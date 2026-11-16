package ghapp

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Audit is an append-only, hash-chained JSON-lines log: each record carries
// the SHA-256 of the previous line, so editing, reordering or deleting
// lines is detectable with VerifyAudit. Tokens are never written; only
// fingerprints.
type Audit struct {
	mu   sync.Mutex
	path string
	prev string
	now  func() time.Time
}

// OpenAudit opens (or creates, mode 0600) the log and resumes its chain.
func OpenAudit(path string) (*Audit, error) {
	a := &Audit{path: path, now: time.Now}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if line := sc.Bytes(); len(bytes.TrimSpace(line)) > 0 {
			a.prev = lineHash(line)
		}
	}
	return a, sc.Err()
}

func lineHash(line []byte) string {
	h := sha256.Sum256(line)
	return hex.EncodeToString(h[:])
}

// Path is the log file.
func (a *Audit) Path() string { return a.path }

// Log appends one record. Fields "t" and "prev" are set here.
func (a *Audit) Log(action string, fields map[string]any) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := map[string]any{"t": a.now().UTC().Format(time.RFC3339Nano), "action": action}
	for k, v := range fields {
		rec[k] = v
	}
	if a.prev != "" {
		rec["prev"] = a.prev
	}
	line, _ := json.Marshal(sortedMap(rec))
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit: %v\n", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "audit: %v\n", err)
		return
	}
	a.prev = lineHash(line)
}

// sortedMap marshals with a stable key order ("t", "action" first).
type sortedMap map[string]any

func (m sortedMap) MarshalJSON() ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "t" && k != "action" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	keys = append([]string{"t", "action"}, keys...)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// VerifyAudit checks the chain and returns the number of records.
func VerifyAudit(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	prev, n := "", 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		n++
		var rec struct {
			Prev string `json:"prev"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			return n, fmt.Errorf("record %d is not valid JSON", n)
		}
		if rec.Prev != prev {
			return n, fmt.Errorf("record %d doesn't follow record %d: the log was edited, reordered or truncated", n, n-1)
		}
		prev = lineHash(line)
	}
	return n, sc.Err()
}

// fingerprint identifies a token in logs without revealing it.
func fingerprint(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:4])
}
