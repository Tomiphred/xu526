// Package static packages a folder of HTML, CSS, JS and assets into a
// scratch image served by xu-static, a small embedded Go file server.
package static

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"xu526/internal/bundle"
)

// Plugin is the static site plugin.
type Plugin struct{}

func (Plugin) Name() string    { return "static" }
func (Plugin) Version() string { return "1" }

// builtDirs are where site generators and bundlers put finished output.
var builtDirs = []string{"dist", "build", "out", "public", "_site", "site", "www", "docs"}

// Detect claims a folder with an index.html. Other plugins outrank it:
// a Go project that ships a public/ folder is still a Go project.
func (Plugin) Detect(src *bundle.Source) (*bundle.Detection, error) {
	hasPkg := src.Has("package.json")
	dir, conf := "", 0
	// In a Node project the root index.html is usually a bundler's source
	// template (Vite), not the finished site, so only built folders count.
	if !hasPkg && src.Has("index.html") {
		dir, conf = ".", 50
	} else {
		for _, d := range builtDirs {
			if src.Has(d + "/index.html") {
				dir, conf = d, 40
				break
			}
		}
	}
	if dir == "" {
		return nil, nil
	}
	marker := path.Join(dir, "index.html")
	if dir == "." {
		marker = "index.html"
	}
	det := &bundle.Detection{Confidence: conf, Markers: []string{marker}, Details: map[string]string{"dir": dir}}
	if hasPkg {
		det.Details["prebuilt"] = "1"
	}
	files, skipped, err := SiteFiles(src, dir)
	if err != nil {
		return nil, err
	}
	if IsSPA(src, dir, files) {
		det.Details["spa"] = "1"
	}
	det.Details["files"] = fmt.Sprint(len(files))
	det.Details["skipped"] = fmt.Sprint(skipped)
	return det, nil
}

// SiteFiles lists what will be served, relative to the project: regular
// files only, no dotfiles (.env, .git) except .well-known, no node_modules.
// Symlinks are skipped so a link can't pull in files from outside the site.
func SiteFiles(src *bundle.Source, dir string) (files []string, skipped int, err error) {
	err = fs.WalkDir(src.FS, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if p != dir && strings.HasPrefix(name, ".") && name != ".well-known" {
			if d.IsDir() {
				return fs.SkipDir
			}
			skipped++
			return nil
		}
		if d.IsDir() {
			if name == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasSuffix(name, ".tar") || (p == bundle.LockFile && dir == ".") {
			skipped++
			return nil
		}
		files = append(files, p)
		return nil
	})
	sort.Strings(files)
	return files, skipped, err
}

// IsSPA guesses whether unknown paths should get index.html: an explicit
// 200.html (surge) or an /index.html 200 rewrite (_redirects), or a site
// that is one HTML page plus scripts, the shape every SPA bundler emits.
func IsSPA(src *bundle.Source, dir string, files []string) bool {
	if src.Has(path.Join(dir, "200.html")) {
		return true
	}
	if b, err := fs.ReadFile(src.FS, path.Join(dir, "_redirects")); err == nil && strings.Contains(string(b), "/index.html") && strings.Contains(string(b), "200") {
		return true
	}
	html, js := 0, 0
	for _, f := range files {
		switch path.Ext(f) {
		case ".html", ".htm":
			if path.Base(f) != "404.html" {
				html++
			}
		case ".js", ".mjs":
			js++
		}
	}
	return html == 1 && js > 0
}

func (Plugin) Plan(src *bundle.Source, d *bundle.Detection, opt bundle.Options) (*bundle.Plan, error) {
	bin, err := ServerBinary(opt.Platform.Arch)
	if err != nil {
		return nil, err
	}
	dir := d.Details["dir"]
	files, _, err := SiteFiles(src, dir)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bin)
	env := map[string]string{"PORT": "8080", "XU_ROOT": "/site"}
	where := "the project folder"
	if dir != "." {
		where = dir + "/"
	}
	summary := []string{fmt.Sprintf("serve %d files from %s with xu-static (built with %s)", len(files), where, ServerVersion())}
	if d.Details["skipped"] != "0" {
		summary = append(summary, fmt.Sprintf("left out %s dotfiles, symlinks and tarballs", d.Details["skipped"]))
	}
	if d.Details["spa"] == "1" {
		env["XU_SPA"] = "1"
		summary = append(summary, "single-page app: unknown paths get index.html (set XU_SPA=0 to turn off)")
	}
	if d.Details["prebuilt"] == "1" {
		summary = append(summary, fmt.Sprintf("uses the already-built %s/ as is; run your build first", dir))
	}
	summary = append(summary, "runs as user 65532 (non-root); listens on $PORT (8080 by default)")
	return &bundle.Plan{
		Strategy: bundle.StaticAssets, Base: "scratch",
		Entrypoint: []string{"/xu-static"}, Env: env, User: "65532:65532", Port: 8080,
		Layers: []bundle.LayerPlan{
			{Name: "server", Salt: hex.EncodeToString(sum[:])},
			{Name: "site", Files: files, Salt: dir},
		},
		Summary: summary,
	}, nil
}

// Build has nothing to compile: it only unpacks the server for the target
// architecture. The site is used as it is.
func (Plugin) Build(ctx context.Context, env *bundle.BuildEnv, p *bundle.Plan) (*bundle.Output, error) {
	bin, err := ServerBinary(env.Options.Platform.Arch)
	if err != nil {
		return nil, err
	}
	server := filepath.Join(env.WorkDir, "xu-static")
	if err := os.WriteFile(server, bin, 0o755); err != nil {
		return nil, err
	}
	return &bundle.Output{Files: map[string]string{"root": env.Source.Dir, "server": server}}, nil
}

func (Plugin) Layers(p *bundle.Plan, out *bundle.Output) ([]bundle.Layer, error) {
	var layers []bundle.Layer
	for _, lp := range p.Layers {
		switch lp.Name {
		case "server":
			layers = append(layers, bundle.Layer{Name: "server", Files: []bundle.Entry{{Src: out.Files["server"], Dst: "/xu-static", Mode: 0o755}}})
		case "site":
			dir := lp.Salt
			entries := make([]bundle.Entry, 0, len(lp.Files))
			for _, f := range lp.Files {
				rel := f
				if dir != "." {
					rel = strings.TrimPrefix(f, dir+"/")
				}
				entries = append(entries, bundle.Entry{Src: filepath.Join(out.Files["root"], filepath.FromSlash(f)), Dst: "/site/" + rel, Mode: 0o644})
			}
			layers = append(layers, bundle.Layer{Name: "site", Files: entries})
		}
	}
	return layers, nil
}
