package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLooksExecutable(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		body string
		want bool
	}{
		"elf": {"\x7fELF\x02\x01", true}, "script": {"#!/usr/bin/env node\n", true},
		"js": {"console.log(1)\n", false}, "tiny": {"#", false},
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(c.body), 0o644)
		if got := looksExecutable(p); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
