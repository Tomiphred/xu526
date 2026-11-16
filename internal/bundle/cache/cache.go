// Package cache is a content-addressed store for built layers.
//
//	<dir>/blobs/sha256/<hex>     layer blobs, named by their own digest
//	<dir>/keys/<key>.json        input hash → which blob it produced
//
// A key is the hash of everything that decides a layer's content: plugin
// and version, target platform, toolchain, and the bytes of every input
// file. If the key exists, the layer is reused without running anything.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store is a cache directory.
type Store struct{ Dir string }

// Default returns ~/.cache/xu/build (or $XDG_CACHE_HOME/xu/build).
func Default() (*Store, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	return Open(filepath.Join(base, "xu", "build"))
}

func Open(dir string) (*Store, error) {
	for _, d := range []string{"blobs/sha256", "keys"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{Dir: dir}, nil
}

// Entry records the layer a key produced.
type Entry struct {
	Digest string `json:"digest"`
	DiffID string `json:"diff_id"`
	Size   int64  `json:"size"`
}

func (s *Store) blobPath(digest string) string {
	return filepath.Join(s.Dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

// BlobPath is where a blob with this digest is (or would be) stored.
func (s *Store) BlobPath(digest string) string { return s.blobPath(digest) }

// Get returns the cached layer for key, if both the record and blob exist.
func (s *Store) Get(key string) (*Entry, []byte, bool) {
	b, err := os.ReadFile(filepath.Join(s.Dir, "keys", key+".json"))
	if err != nil {
		return nil, nil, false
	}
	var e Entry
	if json.Unmarshal(b, &e) != nil {
		return nil, nil, false
	}
	blob, err := os.ReadFile(s.blobPath(e.Digest))
	if err != nil {
		return nil, nil, false
	}
	if sum := sha256.Sum256(blob); "sha256:"+hex.EncodeToString(sum[:]) != e.Digest {
		return nil, nil, false // corrupted: rebuild
	}
	s.Touch(key)
	return &e, blob, true
}

// GetBlob returns a blob by digest, verifying its content.
func (s *Store) GetBlob(digest string) ([]byte, bool) {
	b, err := os.ReadFile(s.blobPath(digest))
	if err != nil {
		return nil, false
	}
	if sum := sha256.Sum256(b); "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, false
	}
	return b, true
}

// PutBlob stores a blob under its digest (base image layers and configs).
func (s *Store) PutBlob(digest string, b []byte) error {
	return writeAtomic(s.blobPath(digest), b)
}

// Put stores a blob and records it under key.
func (s *Store) Put(key string, e Entry, blob []byte) error {
	if err := writeAtomic(s.blobPath(e.Digest), blob); err != nil {
		return err
	}
	b, _ := json.Marshal(e)
	return writeAtomic(filepath.Join(s.Dir, "keys", key+".json"), b)
}

func writeAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Key hashes the given parts into a cache key.
func Key(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		io.WriteString(h, p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// skipDirs are never part of a build's inputs.
var skipDirs = map[string]bool{".git": true, "node_modules": true, ".venv": true, "__pycache__": true, ".xu": true}

// HashInputs hashes every file under root matching any glob, in sorted
// order, including each file's path and executable bit. "**/" matches any
// number of directories, so "**/*.go" covers the whole tree. Paths in
// exclude (relative, slash-separated) are skipped, e.g. the build's own
// output tarball.
func HashInputs(root string, globs []string, exclude ...string) (string, int, error) {
	if len(globs) == 0 {
		return Key(), 0, nil
	}
	skip := map[string]bool{}
	var skipFolders []string
	for _, e := range exclude {
		if strings.HasSuffix(e, "/") {
			skipFolders = append(skipFolders, strings.TrimSuffix(e, "/"))
		} else {
			skip[e] = true
		}
	}
	var files []string
	err := fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			for _, f := range skipFolders {
				if p == f {
					return fs.SkipDir
				}
			}
			return nil
		}
		if skip[p] || OSJunk(d.Name()) {
			return nil
		}
		for _, g := range globs {
			if Match(g, p) {
				files = append(files, p)
				break
			}
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	h, err := HashFiles(root, files)
	return h, len(files), err
}

// HashFiles hashes an exact list of files (relative, slash-separated).
func HashFiles(root string, files []string) (string, error) {
	files = append([]string(nil), files...)
	sort.Strings(files)
	fsys := os.DirFS(root)
	h := sha256.New()
	for _, f := range files {
		info, err := fs.Stat(fsys, f)
		if err != nil {
			return "", err
		}
		io.WriteString(h, f)
		if info.Mode()&0o111 != 0 {
			io.WriteString(h, "\x00x")
		}
		h.Write([]byte{0})
		fh, err := fsys.Open(f)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, fh)
		fh.Close()
		if err != nil {
			return "", err
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Match is path.Match with "**/" meaning "any number of directories".
func Match(glob, name string) bool {
	if dir, ok := strings.CutSuffix(glob, "/**"); ok && !strings.Contains(dir, "*") {
		return strings.HasPrefix(name, dir+"/") // everything under a folder
	}
	if rest, ok := strings.CutPrefix(glob, "**/"); ok {
		if Match(rest, name) {
			return true
		}
		for i := 0; i < len(name); i++ {
			if name[i] == '/' && Match(rest, name[i+1:]) {
				return true
			}
		}
		return false
	}
	ok, err := path.Match(glob, name)
	return ok && err == nil
}

var ErrMiss = errors.New("cache miss")

// Usage reports the cache's size: cached layers (keys) and stored blobs.
func (s *Store) Usage() (keys int, blobs int, bytes int64, err error) {
	ks, _ := os.ReadDir(filepath.Join(s.Dir, "keys"))
	for _, k := range ks {
		if strings.HasSuffix(k.Name(), ".json") {
			keys++
		}
	}
	bs, err := os.ReadDir(filepath.Join(s.Dir, "blobs", "sha256"))
	if err != nil && !os.IsNotExist(err) {
		return 0, 0, 0, err
	}
	for _, b := range bs {
		if info, err := b.Info(); err == nil && !b.IsDir() {
			blobs++
			bytes += info.Size()
		}
	}
	return keys, blobs, bytes, nil
}

// Clean removes cache entries not used for olderThan (all of them when
// olderThan is 0), then every blob no remaining entry needs. Base-image
// blobs are kept while younger than olderThan.
func (s *Store) Clean(olderThan time.Duration) (removed int, freed int64, err error) {
	cutoff := time.Now().Add(-olderThan)
	keep := map[string]bool{}
	ks, _ := os.ReadDir(filepath.Join(s.Dir, "keys"))
	for _, k := range ks {
		p := filepath.Join(s.Dir, "keys", k.Name())
		info, err := k.Info()
		if err != nil {
			continue
		}
		if olderThan > 0 && info.ModTime().After(cutoff) {
			if b, err := os.ReadFile(p); err == nil {
				var e Entry
				if json.Unmarshal(b, &e) == nil {
					keep[strings.TrimPrefix(e.Digest, "sha256:")] = true
				}
			}
			continue
		}
		if os.Remove(p) == nil {
			removed++
		}
	}
	bs, _ := os.ReadDir(filepath.Join(s.Dir, "blobs", "sha256"))
	for _, b := range bs {
		info, err := b.Info()
		if err != nil || keep[b.Name()] || (olderThan > 0 && info.ModTime().After(cutoff)) {
			continue
		}
		if os.Remove(filepath.Join(s.Dir, "blobs", "sha256", b.Name())) == nil {
			freed += info.Size()
		}
	}
	return removed, freed, nil
}

// Touch marks a cache entry as used now, so Clean keeps it.
func (s *Store) Touch(key string) {
	now := time.Now()
	os.Chtimes(filepath.Join(s.Dir, "keys", key+".json"), now, now)
}

// OSJunk reports files desktop OSes leave everywhere (macOS Finder's
// .DS_Store and AppleDouble ._ files, Windows' Thumbs.db): they change
// without the project changing, so they neither invalidate the cache nor
// go into images.
func OSJunk(name string) bool {
	return name == ".DS_Store" || strings.HasPrefix(name, "._") || name == "Thumbs.db" || name == "desktop.ini"
}
