package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		g, n string
		want bool
	}{
		{"**/*", "a/b/c.go", true}, {"**/*.go", "main.go", true}, {"**/*.go", "x/y/main.go", true},
		{"**/*.go", "x/main.c", false}, {"go.mod", "go.mod", true}, {"go.mod", "x/go.mod", false},
		{"src/**/*.c", "src/a/b.c", true}, {"src/**/*.c", "lib/b.c", false},
	}
	for _, c := range cases {
		if got := Match(c.g, c.n); got != c.want {
			t.Errorf("Match(%q,%q)=%v", c.g, c.n, got)
		}
	}
}

func TestHashInputs(t *testing.T) {
	d := t.TempDir()
	w := func(p, s string, m os.FileMode) {
		os.MkdirAll(filepath.Dir(filepath.Join(d, p)), 0o755)
		os.WriteFile(filepath.Join(d, p), []byte(s), m)
	}
	w("main.go", "package main", 0o644)
	w("README.md", "hi", 0o644)
	w(".git/HEAD", "ref", 0o644)
	h1, n, _ := HashInputs(d, []string{"**/*.go"})
	if n != 1 {
		t.Fatalf("hashed %d files", n)
	}
	w("README.md", "changed", 0o644)
	w(".git/HEAD", "other", 0o644)
	if h2, _, _ := HashInputs(d, []string{"**/*.go"}); h2 != h1 {
		t.Fatal("unmatched or skipped files changed the hash")
	}
	all1, _, _ := HashInputs(d, []string{"**/*"})
	os.Chmod(filepath.Join(d, "main.go"), 0o755)
	if all2, _, _ := HashInputs(d, []string{"**/*"}); all2 == all1 {
		t.Fatal("executable bit should change the hash")
	}
	w("main.go", "package main // edit", 0o755)
	if h3, _, _ := HashInputs(d, []string{"**/*.go"}); h3 == h1 {
		t.Fatal("content change not detected")
	}
}

func TestStore(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Get("k"); ok {
		t.Fatal("empty cache hit")
	}
	blob := []byte("layer")
	e := Entry{Digest: "sha256:3d7bd7ee05e2fd7ed0c3dc4d30a3d3bb3eaac5e6c6eb6b9b8fd1d6d7e8b6d6b2", Size: 5}
	// Put with a wrong digest must not produce a hit.
	s.Put("bad", e, blob)
	if _, _, ok := s.Get("bad"); ok {
		t.Fatal("corrupt blob accepted")
	}
	e.Digest = "sha256:" + Key()[:0] + sha(blob)
	if err := s.Put("k", e, blob); err != nil {
		t.Fatal(err)
	}
	got, b, ok := s.Get("k")
	if !ok || string(b) != "layer" || got.Size != 5 {
		t.Fatalf("got %v %q %v", got, b, ok)
	}
	if Key("a", "b") == Key("ab") {
		t.Fatal("key parts must be separated")
	}
}

func sha(b []byte) string {
	h := sha256New()
	h.Write(b)
	return hexEncode(h.Sum(nil))
}

func TestUsageAndClean(t *testing.T) {
	s, _ := Open(t.TempDir())
	put := func(key string, data string) string {
		d := "sha256:" + sha([]byte(data))
		s.Put(key, Entry{Digest: d, Size: int64(len(data))}, []byte(data))
		return d
	}
	put("old", "old-layer")
	put("new", "new-layer")
	s.PutBlob("sha256:"+sha([]byte("base")), []byte("base"))
	if k, b, n, _ := s.Usage(); k != 2 || b != 3 || n != int64(len("old-layer")+len("new-layer")+4) {
		t.Fatalf("usage %d %d %d", k, b, n)
	}
	past := time.Now().Add(-60 * 24 * time.Hour)
	os.Chtimes(filepath.Join(s.Dir, "keys", "old.json"), past, past)
	os.Chtimes(filepath.Join(s.Dir, "blobs", "sha256", sha([]byte("old-layer"))), past, past)
	removed, freed, _ := s.Clean(30 * 24 * time.Hour)
	if removed != 1 || freed != int64(len("old-layer")) {
		t.Fatalf("clean: %d %d", removed, freed)
	}
	if _, _, ok := s.Get("new"); !ok {
		t.Fatal("recent entry removed")
	}
	if _, _, ok := s.Get("old"); ok {
		t.Fatal("old entry kept")
	}
	s.Clean(0)
	if k, b, _, _ := s.Usage(); k != 0 || b != 0 {
		t.Fatalf("clean all left %d keys %d blobs", k, b)
	}
}
