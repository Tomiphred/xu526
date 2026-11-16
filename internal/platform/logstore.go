package platform

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"xu526/internal/cloud"
)

// LogStore keeps every app's output (all instances plus router access logs)
// in one file per app, rotates it by size or age into gzip chunks, and a
// shipper uploads finished chunks to external storage.
//
//	<dir>/<app>/current.log
//	<dir>/<app>/20261111T090000Z_20261111T100000Z.log.gz   (+ .uploaded once shipped)
type LogStore struct {
	dir       string
	maxBytes  int64
	maxAge    time.Duration
	keepLocal int // uploaded chunks kept on disk per app
	keepMax   int // chunks kept when nothing is uploading them

	mu    sync.Mutex
	files map[string]*logFile

	shipMu   sync.Mutex
	storage  cloud.StorageConfig
	host     string
	lastOK   time.Time
	lastErr  string
	shipped  int
	failures int
	wake     chan struct{}
}

type logFile struct {
	f      *os.File
	w      *bufio.Writer
	size   int64
	opened time.Time
}

// LogShipStatus summarises uploads.
type LogShipStatus struct {
	Storage     string    `json:"storage"`
	Configured  bool      `json:"configured"`
	Pending     int       `json:"pending"`
	Shipped     int       `json:"shipped"`
	Failures    int       `json:"failures"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	MaxChunkMB  int64     `json:"max_chunk_mb"`
	MaxAgeSec   int       `json:"max_age_sec"`
}

// ChunkInfo is one rotated, compressed log file.
type ChunkInfo struct {
	Name     string    `json:"name"`
	Bytes    int64     `json:"bytes"`
	Uploaded bool      `json:"uploaded"`
	Time     time.Time `json:"time"`
}

func NewLogStore(dir string, maxBytes int64, maxAge time.Duration, host string) *LogStore {
	_ = os.MkdirAll(dir, 0o755)
	return &LogStore{dir: dir, maxBytes: maxBytes, maxAge: maxAge, keepLocal: 10, keepMax: 200,
		files: map[string]*logFile{}, host: host, wake: make(chan struct{}, 1)}
}

// Write appends one line: "<RFC3339 time> <source> <stream> <message>".
func (s *LogStore) Write(app, source, stream, msg string) {
	if s == nil || app == "" {
		return
	}
	line := time.Now().UTC().Format("2006-01-02T15:04:05.000Z") + " " + source + " " + stream + " " + msg + "\n"
	s.mu.Lock()
	defer s.mu.Unlock()
	lf, err := s.open(app)
	if err != nil {
		return
	}
	if lf.size > 0 && (lf.size+int64(len(line)) > s.maxBytes || time.Since(lf.opened) > s.maxAge) {
		s.rotateLocked(app, lf)
		if lf, err = s.open(app); err != nil {
			return
		}
	}
	n, _ := lf.w.WriteString(line)
	lf.size += int64(n)
}

func (s *LogStore) open(app string) (*logFile, error) {
	if lf := s.files[app]; lf != nil {
		return lf, nil
	}
	dir := filepath.Join(s.dir, app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "current.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	st, _ := f.Stat()
	opened := time.Now()
	if st.Size() > 0 {
		opened = st.ModTime() // resume after a restart without resetting the age clock forever
		if b, err := os.ReadFile(path + ".opened"); err == nil {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil {
				opened = t
			}
		}
	} else {
		_ = os.WriteFile(path+".opened", []byte(opened.UTC().Format(time.RFC3339)), 0o644)
	}
	lf := &logFile{f: f, w: bufio.NewWriterSize(f, 64<<10), size: st.Size(), opened: opened}
	s.files[app] = lf
	return lf, nil
}

// rotateLocked closes current.log, renames it after its time span, and
// compresses it in the background.
func (s *LogStore) rotateLocked(app string, lf *logFile) {
	_ = lf.w.Flush()
	_ = lf.f.Close()
	delete(s.files, app)
	dir := filepath.Join(s.dir, app)
	base := lf.opened.UTC().Format("20060102T150405Z") + "_" + time.Now().UTC().Format("20060102T150405Z")
	src := filepath.Join(dir, "current.log")
	dst := filepath.Join(dir, base+".log")
	// Busy apps can rotate several times a second: never overwrite a chunk.
	for i := 2; fileExists(dst) || fileExists(dst+".gz"); i++ {
		dst = filepath.Join(dir, fmt.Sprintf("%s-%d.log", base, i))
	}
	if err := os.Rename(src, dst); err != nil {
		return
	}
	_ = os.Remove(src + ".opened")
	go func() {
		if err := gzipFile(dst); err == nil {
			_ = os.Remove(dst)
			s.kick()
		}
	}()
}

func gzipFile(path string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := path + ".gz.part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	zw.Name = filepath.Base(path)
	if _, err := io.Copy(zw, in); err != nil {
		out.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path+".gz")
}

// Flush writes buffered lines to disk and rotates files that are too old.
func (s *LogStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for app, lf := range s.files {
		_ = lf.w.Flush()
		if lf.size > 0 && time.Since(lf.opened) > s.maxAge {
			s.rotateLocked(app, lf)
		}
	}
}

// Rotate forces a rotation of an app's current file (e.g. before deleting).
func (s *LogStore) Rotate(app string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lf := s.files[app]; lf != nil && lf.size > 0 {
		s.rotateLocked(app, lf)
	}
}

// Tail returns up to n recent lines containing query (case-insensitive),
// reading the current file and, if needed, the newest chunks.
func (s *LogStore) Tail(app string, n int, query string) []string {
	s.mu.Lock()
	if lf := s.files[app]; lf != nil {
		_ = lf.w.Flush()
	}
	s.mu.Unlock()
	q := strings.ToLower(query)
	match := func(l string) bool { return q == "" || strings.Contains(strings.ToLower(l), q) }
	var out []string
	dir := filepath.Join(s.dir, app)
	if b, err := readTail(filepath.Join(dir, "current.log"), 4<<20); err == nil {
		out = filterLines(b, match)
	}
	chunks := s.Chunks(app)
	for i := len(chunks) - 1; i >= 0 && len(out) < n && i >= len(chunks)-3; i-- {
		f, err := os.Open(filepath.Join(dir, chunks[i].Name))
		if err != nil {
			continue
		}
		zr, err := gzip.NewReader(f)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(zr, 16<<20))
			out = append(filterLines(b, match), out...)
		}
		f.Close()
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func filterLines(b []byte, match func(string) bool) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l != "" && match(l) {
			out = append(out, l)
		}
	}
	return out
}

func readTail(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, _ := f.Stat()
	off := st.Size() - max
	if off < 0 {
		off = 0
	}
	b := make([]byte, st.Size()-off)
	_, err = f.ReadAt(b, off)
	if off > 0 { // drop the partial first line
		if i := strings.IndexByte(string(b), '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b, err
}

// Chunks lists an app's compressed chunks, oldest first.
func (s *LogStore) Chunks(app string) []ChunkInfo {
	dir := filepath.Join(s.dir, app)
	entries, _ := os.ReadDir(dir)
	out := []ChunkInfo{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".log.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		_, err = os.Stat(filepath.Join(dir, e.Name()+".uploaded"))
		out = append(out, ChunkInfo{Name: e.Name(), Bytes: info.Size(), Uploaded: err == nil, Time: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ChunkPath returns the path of a chunk, refusing anything outside the store.
func (s *LogStore) ChunkPath(app, name string) (string, error) {
	if strings.ContainsAny(name, "/\\") || !strings.HasSuffix(name, ".log.gz") || !nameRE.MatchString(app) && app != "xu" {
		return "", fmt.Errorf("invalid chunk name")
	}
	p := filepath.Join(s.dir, app, name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("no such chunk")
	}
	return p, nil
}

// ---- shipping ----

func (s *LogStore) SetStorage(c cloud.StorageConfig) {
	s.shipMu.Lock()
	s.storage = c
	s.lastErr = ""
	s.shipMu.Unlock()
	s.kick()
}

func (s *LogStore) Storage() cloud.StorageConfig {
	s.shipMu.Lock()
	defer s.shipMu.Unlock()
	return s.storage
}

func (s *LogStore) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run flushes every second, checks rotation every 30 s, and ships chunks.
func (s *LogStore) Run(ctx context.Context) {
	flush := time.NewTicker(time.Second)
	ship := time.NewTicker(30 * time.Second)
	defer flush.Stop()
	defer ship.Stop()
	backoff := time.Duration(0)
	var nextTry time.Time
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			for _, lf := range s.files {
				_ = lf.w.Flush()
			}
			s.mu.Unlock()
			return
		case <-flush.C:
			s.Flush()
			continue
		case <-ship.C:
		case <-s.wake:
		}
		if time.Now().Before(nextTry) {
			continue
		}
		if err := s.shipOnce(ctx); err != nil {
			backoff = min(max(backoff*2, 30*time.Second), 10*time.Minute)
			nextTry = time.Now().Add(backoff)
		} else {
			backoff = 0
		}
	}
}

// ShipNow uploads pending chunks immediately (used by the Test button too).
func (s *LogStore) ShipNow(ctx context.Context) error { return s.shipOnce(ctx) }

func (s *LogStore) shipOnce(ctx context.Context) error {
	s.shipMu.Lock()
	cfg := s.storage
	s.shipMu.Unlock()
	apps, _ := os.ReadDir(s.dir)
	for _, a := range apps {
		if !a.IsDir() {
			continue
		}
		app := a.Name()
		chunks := s.Chunks(app)
		if cfg.Type != "" {
			for _, c := range chunks {
				if c.Uploaded {
					continue
				}
				path := filepath.Join(s.dir, app, c.Name)
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				t, _ := time.Parse("20060102T150405Z", strings.SplitN(c.Name, "_", 2)[0])
				key := cloud.ObjectKey(s.host, app, t, c.Name)
				uctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				err = cfg.Put(uctx, key, data, "application/gzip")
				cancel()
				s.shipMu.Lock()
				if err != nil {
					s.failures++
					s.lastErr = fmt.Sprintf("%s: %v", c.Name, err)
					s.shipMu.Unlock()
					return err
				}
				s.shipped++
				s.lastOK, s.lastErr = time.Now(), ""
				s.shipMu.Unlock()
				_ = os.WriteFile(path+".uploaded", []byte(key), 0o644)
			}
		}
		s.prune(app, cfg.Type != "")
	}
	return nil
}

// prune keeps local disk bounded: a few uploaded chunks, or a hard cap when
// nothing ships them.
func (s *LogStore) prune(app string, shipping bool) {
	chunks := s.Chunks(app)
	dir := filepath.Join(s.dir, app)
	var uploaded []ChunkInfo
	for _, c := range chunks {
		if c.Uploaded {
			uploaded = append(uploaded, c)
		}
	}
	remove := func(c ChunkInfo) {
		_ = os.Remove(filepath.Join(dir, c.Name))
		_ = os.Remove(filepath.Join(dir, c.Name+".uploaded"))
	}
	for len(uploaded) > s.keepLocal {
		remove(uploaded[0])
		uploaded = uploaded[1:]
	}
	if !shipping {
		for len(chunks) > s.keepMax {
			remove(chunks[0])
			chunks = chunks[1:]
		}
	}
}

// Status reports shipping progress.
func (s *LogStore) Status() LogShipStatus {
	pending := 0
	apps, _ := os.ReadDir(s.dir)
	for _, a := range apps {
		for _, c := range s.Chunks(a.Name()) {
			if !c.Uploaded {
				pending++
			}
		}
	}
	s.shipMu.Lock()
	defer s.shipMu.Unlock()
	return LogShipStatus{Storage: s.storage.Describe(), Configured: s.storage.Type != "", Pending: pending,
		Shipped: s.shipped, Failures: s.failures, LastSuccess: s.lastOK, LastError: s.lastErr,
		MaxChunkMB: s.maxBytes >> 20, MaxAgeSec: int(s.maxAge / time.Second)}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
