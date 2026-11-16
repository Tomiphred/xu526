// Package dotenv reads .env files so their values reach an app as
// environment variables at deploy time. They are never put in images.
//
// Supported syntax (what dotenv/Next.js/Docker Compose users write):
//
//	KEY=value                 # trailing comments after a space are dropped
//	export KEY=value
//	KEY="double quoted\nwith escapes and ${OTHER} expansion"
//	KEY='single quoted, taken literally'
//	KEY="a value that
//	spans lines"
//	KEY=                      # empty value
package dotenv

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ProductionFiles are read in this order; later files override earlier
// ones (the order Next.js and most dotenv tools use for production).
var ProductionFiles = []string{".env", ".env.production", ".env.local", ".env.production.local"}

var keyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// Parse reads one file's contents. lookup resolves ${VAR} in unquoted and
// double-quoted values (pass the variables loaded so far; may be nil).
func Parse(text string, lookup func(string) (string, bool)) (map[string]string, error) {
	out := map[string]string{}
	get := func(k string) (string, bool) {
		if v, ok := out[k]; ok {
			return v, true
		}
		if lookup != nil {
			return lookup(k)
		}
		return "", false
	}
	text = strings.ReplaceAll(strings.TrimPrefix(text, "\xef\xbb\xbf"), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, rest, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !keyRE.MatchString(key) {
			return nil, fmt.Errorf("line %d: expected KEY=value", i+1)
		}
		rest = strings.TrimLeft(rest, " \t")
		var val string
		switch {
		case strings.HasPrefix(rest, `"`) || strings.HasPrefix(rest, `'`) || strings.HasPrefix(rest, "`"):
			q := rest[:1]
			body := rest[1:]
			start := i
			for {
				if end := closingQuote(body, q); end >= 0 {
					body = body[:end]
					break
				}
				i++
				if i >= len(lines) {
					return nil, fmt.Errorf("line %d: %s has no closing %s", start+1, key, q)
				}
				body += "\n" + lines[i]
			}
			val = body
			if q == `"` {
				val = expand(unescape(val), get)
			}
		default:
			if j := strings.Index(rest, " #"); j >= 0 {
				rest = rest[:j]
			}
			val = expand(strings.TrimSpace(rest), get)
		}
		out[key] = val
	}
	return out, nil
}

// closingQuote finds the unescaped closing quote q in s.
func closingQuote(s, q string) int {
	for i := 0; i < len(s); i++ {
		if q == `"` && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == q[0] {
			return i
		}
	}
	return -1
}

func unescape(s string) string {
	r := strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\"`, `"`, `\\`, `\`, `\$`, "\x00")
	return r.Replace(s)
}

var refRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expand resolves ${VAR}, ${VAR:-default} and $VAR. Unknown names become "".
// An escaped \$ (marked as \x00 by unescape) stays a literal $.
func expand(s string, get func(string) (string, bool)) string {
	s = refRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := refRE.FindStringSubmatch(m)
		name := sub[1]
		if name == "" {
			name = sub[4]
		}
		if v, ok := get(name); ok && v != "" {
			return v
		}
		return sub[3]
	})
	return strings.ReplaceAll(s, "\x00", "$")
}

// Load reads the files that exist in dir, in order, each overriding the
// last. It returns the merged variables and the files it read.
func Load(dir string, files []string) (map[string]string, []string, error) {
	vars := map[string]string{}
	var read []string
	for _, f := range files {
		p := f
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, f)
		}
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		got, err := Parse(string(b), func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f, err)
		}
		for k, v := range got {
			vars[k] = v
		}
		read = append(read, f)
	}
	return vars, read, nil
}

// Keys lists variable names, sorted (for messages: never print values).
func Keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Summary describes what was loaded without revealing values.
func Summary(vars map[string]string, files []string) string {
	if len(files) == 0 {
		return ""
	}
	ks := Keys(vars)
	if len(ks) > 8 {
		ks = append(ks[:8], fmt.Sprintf("and %d more", len(vars)-8))
	}
	noun := "variables"
	if len(vars) == 1 {
		noun = "variable"
	}
	return fmt.Sprintf("loaded %d %s from %s (%s)", len(vars), noun, strings.Join(files, ", "), strings.Join(ks, ", "))
}
