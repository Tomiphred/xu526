package dotenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	text := "\xef\xbb\xbf# comment\r\n" + `
DATABASE_URL=postgres://u:p@db:5432/app?sslmode=disable
export API_KEY=abc123   # trailing comment
EMPTY=
SPACED =  value with spaces
SINGLE='literal $HOME and \n'
DOUBLE="line1\nline2 \"quoted\" costs \$5"
MULTI="-----BEGIN KEY-----
abc
-----END KEY-----"
HOST=example.com
URL=https://${HOST}/api
URL2="$HOST:${PORT:-8080}"
HASH=abc#notacomment
NEXT_PUBLIC_SITE=https://site.example
`
	got, err := Parse(text, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DATABASE_URL":     "postgres://u:p@db:5432/app?sslmode=disable",
		"API_KEY":          "abc123",
		"EMPTY":            "",
		"SPACED":           "value with spaces",
		"SINGLE":           `literal $HOME and \n`,
		"DOUBLE":           "line1\nline2 \"quoted\" costs $5",
		"MULTI":            "-----BEGIN KEY-----\nabc\n-----END KEY-----",
		"HOST":             "example.com",
		"URL":              "https://example.com/api",
		"URL2":             "example.com:8080",
		"HASH":             "abc#notacomment",
		"NEXT_PUBLIC_SITE": "https://site.example",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d keys, want %d: %v", len(got), len(want), Keys(got))
	}
	for _, bad := range []string{"NOEQUALS", "1BAD=x", `Q="unterminated`} {
		if _, err := Parse(bad, nil); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLoadOrder(t *testing.T) {
	d := t.TempDir()
	w := func(n, s string) { os.WriteFile(filepath.Join(d, n), []byte(s), 0o600) }
	w(".env", "A=base\nB=base\nHOST=h\n")
	w(".env.production", "B=prod\nURL=http://${HOST}\n")
	w(".env.production.local", "C=local-secret\n")
	vars, files, err := Load(d, ProductionFiles)
	if err != nil {
		t.Fatal(err)
	}
	if vars["A"] != "base" || vars["B"] != "prod" || vars["C"] != "local-secret" || vars["URL"] != "http://h" {
		t.Fatalf("%v", vars)
	}
	if strings.Join(files, ",") != ".env,.env.production,.env.production.local" {
		t.Fatalf("%v", files)
	}
	s := Summary(vars, files)
	if strings.Contains(s, "local-secret") || !strings.Contains(s, "loaded 5 variables") || !strings.Contains(s, "C, HOST") {
		t.Fatalf("summary leaks or is wrong: %s", s)
	}
	if _, f, _ := Load(t.TempDir(), ProductionFiles); len(f) != 0 {
		t.Fatal("no files should load nothing")
	}
}
