package bundle

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// CopyTree copies src into dst, keeping symlinks and executable bits.
// skip (optional) leaves out entries; only (optional) limits the copy to
// these slash-separated relative paths. Plugins build in such a copy so the
// developer's tree is never modified.
func CopyTree(src, dst string, skip func(string, fs.DirEntry) bool, only map[string]bool) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if only != nil && !only[filepath.ToSlash(rel)] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if skip != nil && skip(rel, d) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, info.Mode().Perm())
		}
		return nil
	})
}

// FileEntry makes a layer entry for a file or relative symlink at p,
// placed at dst. Absolute symlinks and special files are skipped (ok=false).
// Files keep only their executable bit: 0755 or 0644.
func FileEntry(p, dst string, d fs.DirEntry) (Entry, bool, error) {
	if d.Type()&fs.ModeSymlink != 0 {
		link, err := os.Readlink(p)
		if err != nil {
			return Entry{}, false, err
		}
		if filepath.IsAbs(link) {
			return Entry{}, false, nil
		}
		return Entry{Dst: dst, Link: filepath.ToSlash(link)}, true, nil
	}
	if !d.Type().IsRegular() {
		return Entry{}, false, nil
	}
	info, err := d.Info()
	if err != nil {
		return Entry{}, false, err
	}
	mode := fs.FileMode(0o644)
	if info.Mode()&0o111 != 0 || (runtime.GOOS == "windows" && looksExecutable(p)) {
		mode = 0o755
	}
	return Entry{Src: p, Dst: dst, Mode: mode}, true, nil
}

// looksExecutable stands in for the executable bit, which Windows doesn't
// have: Linux programs (ELF) and scripts (#!) must be executable in the
// image, e.g. esbuild's binary or a downloaded Python.
func looksExecutable(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 4)
	n, _ := io.ReadFull(f, b)
	return (n == 4 && string(b) == "\x7fELF") || (n >= 2 && string(b[:2]) == "#!")
}

// SplitCommand splits a simple command line (an npm script, a Procfile
// entry) into words, honouring quotes. Commands that need a shell (&&,
// pipes, redirects, $variables) are refused, because runtime images have no
// shell.
func SplitCommand(s string) ([]string, error) {
	for _, bad := range []string{"&&", "||", ";", "|", ">", "<", "`", "$"} {
		if strings.Contains(s, bad) {
			if bad == "$" {
				return nil, fmt.Errorf("it uses $variables, which need a shell to expand; read process.env in the app instead")
			}
			return nil, fmt.Errorf("it uses %q, which needs a shell", bad)
		}
	}
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unbalanced quotes")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// ImageCABundle is where images get the CA certificates.
const ImageCABundle = "/etc/ssl/certs/ca-certificates.crt"

// HostCABundle finds this machine's PEM CA bundle (Debian/Ubuntu, Fedora,
// Alpine, macOS, Homebrew), or "" when there is none.
func HostCABundle() string {
	for _, p := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/cert.pem", // macOS, Alpine
		"/opt/homebrew/etc/ca-certificates/cert.pem",
		"/usr/local/etc/ca-certificates/cert.pem",
	} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			return p
		}
	}
	return ""
}
