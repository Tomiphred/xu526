package main

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/oci"
	"xu526/internal/platform"
	"xu526/internal/runtime"
)

const imagesUsage = `xu images [APP]                       list images built by xu, and what uses each
xu images inspect REF                 base, layers, command, port, user, env names
xu images rm REF... [--force]         remove (never one an app is running; --force for rollback images)
xu images prune [--keep 5] [--dry-run] [-y]
                                      remove images nothing runs, keeping each app's last N releases
xu images tag REF xu/NAME:TAG         copy under another name on the server
xu images push REF REGISTRY/REPO:TAG  copy to a registry (uses the daemon's registry logins)
xu images save REF [-o FILE]          copy out to a tarball (docker load works on it)
xu images load FILE [--as xu/NAME:TAG] copy in from a tarball (xu build or docker save)
xu images df                          disk used by images, volumes, build cache and logs

REF is a name (xu/pof:0836fe631e37), an image ID (12+ hex digits), or xu/APP
when the app has only one image.
`

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// confirm asks before destructive work; -y skips it, and without a
// terminal to ask on, it refuses.
func confirm(yes bool, question string) bool {
	if yes {
		return true
	}
	if info, err := os.Stdin.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		fmt.Println(question + " (not a terminal: pass -y to confirm)")
		return false
	}
	fmt.Print(question + " [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes")
}

// parse splits flags from positional arguments in any order.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return pos, nil
}

func imagesCmd(args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "ls", "list", "inspect", "rm", "remove", "prune", "tag", "copy", "cp", "push", "save", "export", "load", "import", "df", "help":
			sub, args = args[0], args[1:]
		}
	}
	switch sub {
	case "help":
		fmt.Print(imagesUsage)
		return nil
	case "ls", "list":
		app := ""
		if len(args) > 0 {
			app = args[0]
		}
		var list []platform.XuImage
		if err := call("GET", "/v1/images?app="+url.QueryEscape(app), nil, &list); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "IMAGE\tID\tSIZE\tADDED\tUSED BY")
		for _, im := range list {
			used := "unused"
			switch im.Status {
			case "live":
				used = "running: " + strings.Join(im.Live, ", ")
			case "rollback":
				used = "rollback: " + strings.Join(im.Releases, ", ")
			}
			for i, t := range im.Tags {
				if i == 0 {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t, shortID(im.ID), human(im.Unique()), ago(im.Added), used)
				} else {
					fmt.Fprintf(tw, "%s\t%s\t\t\t(same image)\n", t, shortID(im.ID))
				}
			}
		}
		tw.Flush()
		if len(list) == 0 {
			fmt.Println("(no images built by xu yet: xu build . --deploy NAME)")
		}
		return nil
	case "inspect":
		if len(args) != 1 {
			return errors.New("usage: xu images inspect REF")
		}
		var res struct {
			Image   platform.XuImage     `json:"image"`
			Details runtime.ImageDetails `json:"details"`
		}
		if err := call("GET", "/v1/images/inspect?ref="+url.QueryEscape(args[0]), nil, &res); err != nil {
			return err
		}
		d := res.Details
		sort.Strings(d.EnvNames)
		created := d.Created.Local().Format(time.RFC1123) + " (" + ago(d.Created) + ")"
		if d.Created.Unix() <= 0 {
			created = "fixed to the epoch (reproducible build)"
		}
		fmt.Printf("Image     %s\nID        %s\nAdded     %s\nCreated   %s\nPlatform  %s/%s\nSize      %s\n", strings.Join(d.Tags, ", "), d.ID,
			ago(res.Image.Added), created, d.OS, d.Architecture, human(d.Size))
		if b := d.Labels["org.opencontainers.image.base.name"]; b != "" {
			fmt.Printf("Base      %s@%s\n", b, d.Labels["org.opencontainers.image.base.digest"])
		}
		if bl := d.Labels["dev.xu526.builder"]; bl != "" {
			fmt.Printf("Built by  xu build (%s plugin)\n", bl)
		}
		fmt.Printf("Command   %s\n", strings.Join(append(append([]string{}, d.Entrypoint...), d.Cmd...), " "))
		fmt.Printf("WorkDir   %s\nUser      %s\nPorts     %s\n", firstNonEmptyStr(d.WorkingDir, "/"), firstNonEmptyStr(d.User, "root"), strings.Join(d.Ports, ", "))
		fmt.Printf("Env       %s  (values not shown)\n", strings.Join(d.EnvNames, ", "))
		status := res.Image.Status
		if len(res.Image.Live) > 0 {
			status += ": " + strings.Join(res.Image.Live, ", ")
		}
		if len(res.Image.Releases) > 0 {
			status += "; releases " + strings.Join(res.Image.Releases, ", ")
		}
		fmt.Printf("Used      %s\n\nLayers (newest first):\n", status)
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		for _, l := range d.Layers {
			what := firstNonEmptyStr(l.Comment, l.CreatedBy)
			if len(what) > 70 {
				what = what[:67] + "..."
			}
			fmt.Fprintf(tw, "  %s\t%s\n", human(l.Size), what)
		}
		tw.Flush()
		return nil
	case "rm", "remove":
		fs := flag.NewFlagSet("rm", flag.ContinueOnError)
		force := fs.Bool("force", false, "")
		fs.BoolVar(force, "f", false, "")
		refs, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(refs) == 0 {
			return errors.New("usage: xu images rm REF... [--force]")
		}
		failed := 0
		for _, ref := range refs {
			var im platform.XuImage
			err := call("DELETE", fmt.Sprintf("/v1/images?ref=%s&force=%t", url.QueryEscape(ref), *force), nil, &im)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", ref, err)
				failed++
				continue
			}
			fmt.Printf("removed %s (%s)\n", ref, human(im.Unique()))
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d not removed", failed, len(refs))
		}
		return nil
	case "prune":
		fs := flag.NewFlagSet("prune", flag.ContinueOnError)
		keep := fs.Int("keep", 5, "")
		dry := fs.Bool("dry-run", false, "")
		yes := fs.Bool("y", false, "")
		fs.BoolVar(yes, "yes", false, "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		return pruneImages(*keep, *dry, *yes)
	case "tag", "copy", "cp":
		if len(args) != 2 {
			return errors.New("usage: xu images tag REF xu/NAME:TAG")
		}
		if err := call("POST", "/v1/images/tag", map[string]string{"src": args[0], "dst": args[1]}, nil); err != nil {
			return err
		}
		fmt.Printf("%s is now also %s\n", args[0], args[1])
		return nil
	case "push":
		if len(args) != 2 {
			return errors.New("usage: xu images push REF REGISTRY/REPO:TAG (log in with xu's registry settings first)")
		}
		resp, err := rawCall("POST", "/v1/images/push", map[string]string{"ref": args[0], "dest": args[1]}, "")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var last string
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			last = sc.Text()
			fmt.Println(last)
		}
		if strings.HasPrefix(last, "ERROR:") {
			return errors.New("push failed")
		}
		return nil
	case "save", "export":
		fs := flag.NewFlagSet("save", flag.ContinueOnError)
		out := fs.String("o", "", "")
		pos, err := parse(fs, args)
		if err != nil || len(pos) != 1 {
			return errors.New("usage: xu images save REF [-o FILE]")
		}
		if *out == "" {
			name := strings.TrimPrefix(runtime.ShortName(pos[0]), "xu/")
			*out = strings.NewReplacer(":", "-", "/", "-").Replace(name) + ".tar"
		}
		resp, err := rawCall("GET", "/v1/images/save?ref="+url.QueryEscape(pos[0]), nil, "")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		f, err := os.Create(*out + ".tmp")
		if err != nil {
			return err
		}
		n, err := io.Copy(f, resp.Body)
		f.Close()
		if err != nil {
			os.Remove(*out + ".tmp")
			return err
		}
		if err := os.Rename(*out+".tmp", *out); err != nil {
			return err
		}
		fmt.Printf("saved %s to %s (%s); load it anywhere with docker load -i %s\n", pos[0], *out, human(n), *out)
		return nil
	case "load", "import":
		fs := flag.NewFlagSet("load", flag.ContinueOnError)
		as := fs.String("as", "", "")
		pos, err := parse(fs, args)
		if err != nil || len(pos) != 1 {
			return errors.New("usage: xu images load FILE [--as xu/NAME:TAG]")
		}
		return loadImage(pos[0], *as)
	case "df":
		return systemDF()
	}
	fmt.Print(imagesUsage)
	return nil
}

func pruneImages(keep int, dry, yes bool) error {
	var plan platform.PruneResult
	if err := call("POST", "/v1/images/prune", map[string]any{"keep": keep, "dry_run": true}, &plan); err != nil {
		return err
	}
	if len(plan.Removed) == 0 {
		fmt.Printf("nothing to remove: every xu image is running or among its app's last %d releases\n", keep)
		return nil
	}
	fmt.Printf("These %d images aren't running and aren't among their app's last %d releases:\n", len(plan.Removed), keep)
	for _, im := range plan.Removed {
		note := ""
		if len(xuOnly(im.Tags)) < len(im.Tags) {
			note = fmt.Sprintf("  (only the xu/ names go: it's also %s, so no space is freed)", strings.Join(otherTags(im.Tags), ", "))
		}
		fmt.Printf("  %s  %s  added %s%s\n", strings.Join(xuOnly(im.Tags), ", "), human(im.Unique()), ago(im.Added), note)
	}
	fmt.Printf("That frees about %s. Releases using them can no longer be rolled back to.\n", human(plan.Freed))
	if dry || !confirm(yes, "Remove them?") {
		return nil
	}
	var res platform.PruneResult
	if err := call("POST", "/v1/images/prune", map[string]any{"keep": keep}, &res); err != nil {
		return err
	}
	fmt.Printf("removed %d images, freed %s\n", len(res.Removed), human(res.Freed))
	for _, e := range res.Errors {
		fmt.Fprintln(os.Stderr, "  "+e)
	}
	return nil
}

// rawCall is call() for streaming endpoints.
func rawCall(method, path string, in any, contentType string) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, apiBase()+path, body)
	authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the xu526 daemon (is it running? sudo systemctl start xu526): %w", err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		return nil, fmt.Errorf("%s", firstNonEmptyStr(e.Error, resp.Status))
	}
	return resp, nil
}

// tarballName reads the image name inside a docker-save or OCI tarball.
func tarballName(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("%s isn't an image tarball: %w", path, err)
		}
		if h.Name == "manifest.json" {
			var m []struct{ RepoTags []string }
			if json.NewDecoder(tr).Decode(&m) == nil && len(m) > 0 {
				if len(m) > 1 {
					return "", errors.New("the tarball holds several images; load one at a time")
				}
				if len(m[0].RepoTags) > 0 {
					return runtime.ShortName(m[0].RepoTags[0]), nil
				}
			}
			return "", nil
		}
	}
}

// retag copies a tarball, naming the image ref in manifest.json and
// index.json, so loading it can't take over another image's name.
func retag(path, ref string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	tr, tw := tar.NewReader(f), tar.NewWriter(w)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Name != "manifest.json" && h.Name != "index.json" {
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if h.Name == "manifest.json" {
			var m []map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				return err
			}
			for _, e := range m {
				e["RepoTags"] = []string{ref}
			}
			b, _ = json.Marshal(m)
		} else {
			var idx map[string]any
			if err := json.Unmarshal(b, &idx); err != nil {
				return err
			}
			if ms, ok := idx["manifests"].([]any); ok {
				for _, x := range ms {
					if d, ok := x.(map[string]any); ok {
						ann, _ := d["annotations"].(map[string]any)
						if ann == nil {
							ann = map[string]any{}
						}
						ann["io.containerd.image.name"] = oci.NormalizeRef(ref)
						ann["org.opencontainers.image.ref.name"] = ref[strings.LastIndex(ref, ":")+1:]
						d["annotations"] = ann
					}
				}
			}
			b, _ = json.Marshal(idx)
		}
		h.Size = int64(len(b))
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	return tw.Close()
}

func loadImage(path, as string) error {
	name, err := tarballName(path)
	if err != nil {
		return err
	}
	ref := as
	if ref == "" {
		if strings.HasPrefix(name, "xu/") {
			ref = name
		} else {
			base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			app := strings.Trim(nonName.ReplaceAllString(strings.ToLower(base), "-"), "-.")
			return fmt.Errorf("the image is named %q; xu526 only keeps images named xu/…, so choose one: xu images load %s --as xu/%s:%s",
				firstNonEmptyStr(name, "(nothing)"), path, firstNonEmptyStr(app, "app"), time.Now().Format("20060102"))
		}
	}
	ref = runtime.ShortName(ref)
	if !strings.HasPrefix(ref, "xu/") || !strings.Contains(ref, ":") {
		return errors.New("--as must look like xu/NAME:TAG")
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(retag(path, ref, pw)) }()
	req, _ := http.NewRequest("POST", apiBase()+"/v1/images?ref="+url.QueryEscape(ref), pr)
	req.Header.Set("Content-Type", "application/x-tar")
	authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the xu526 daemon (is it running? sudo systemctl start xu526): %w", err)
	}
	defer resp.Body.Close()
	var res struct{ Ref, ID, Error string }
	json.NewDecoder(resp.Body).Decode(&res)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the daemon refused the image: %s", res.Error)
	}
	fmt.Printf("loaded %s (%s). Deploy it with: xu deploy NAME --image %s --port PORT\n", ref, shortID(res.ID), ref)
	return nil
}

// ---- disk usage, volumes, build cache ----

func localCache() (*cache.Store, error) { return cache.Default() }

func systemDF() error {
	var u platform.DiskUsage
	if err := call("GET", "/v1/system/df", nil, &u); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "WHAT\tCOUNT\tSIZE\tRECLAIMABLE\tHOW")
	fmt.Fprintf(tw, "images (server)\t%d\t%s\t%s\txu images prune\n", u.Images, human(u.ImagesBytes), human(u.ReclaimableBytes))
	fmt.Fprintf(tw, "  running\t\t%s\t\t\n", human(u.LiveBytes))
	fmt.Fprintf(tw, "  kept for rollback\t\t%s\t\t\n", human(u.RollbackBytes))
	fmt.Fprintf(tw, "volumes (server)\t%d\t%s\t%s\txu volumes prune (deletes data)\n", u.Volumes, human(u.VolumesBytes), human(u.OrphanVolumeBytes))
	fmt.Fprintf(tw, "git build cache (server)\t\t%s\t%s\txu system prune\n", human(u.BuildCacheBytes), human(u.BuildCacheBytes))
	fmt.Fprintf(tw, "app logs (server)\t\t%s\t\trotated automatically\n", human(u.LogsBytes))
	if s, err := localCache(); err == nil {
		keys, _, n, _ := s.Usage()
		fmt.Fprintf(tw, "xu build cache (this machine)\t%d layers\t%s\t%s\txu cache clean\n", keys, human(n), human(n))
	}
	tw.Flush()
	return nil
}

func volumesCmd(args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("volumes", flag.ContinueOnError)
	yes := fs.Bool("y", false, "")
	fs.BoolVar(yes, "yes", false, "")
	dry := fs.Bool("dry-run", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	var vols []platform.XuVolume
	if err := call("GET", "/v1/volumes", nil, &vols); err != nil {
		return err
	}
	size := func(v platform.XuVolume) string {
		if v.Size < 0 {
			return "?"
		}
		return human(v.Size)
	}
	switch sub {
	case "ls", "list":
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "VOLUME\tSTACK\tSIZE\tSTATUS")
		for _, v := range vols {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", v.Name, v.Stack, size(v), v.Status)
		}
		tw.Flush()
		if len(vols) == 0 {
			fmt.Println("(no volumes: stacks create them for services with named volumes)")
		}
		return nil
	case "rm", "remove":
		if len(pos) == 0 {
			return errors.New("usage: xu volumes rm NAME... [-y]")
		}
		if !confirm(*yes, fmt.Sprintf("Delete %s and all data in it?", strings.Join(pos, ", "))) {
			return nil
		}
		for _, n := range pos {
			if err := call("DELETE", "/v1/volumes/"+url.PathEscape(n), nil, nil); err != nil {
				return fmt.Errorf("%s: %w", n, err)
			}
			fmt.Printf("removed volume %s\n", n)
		}
		return nil
	case "prune":
		var orphans []platform.XuVolume
		var total int64
		for _, v := range vols {
			if !v.InUse {
				orphans = append(orphans, v)
				if v.Size > 0 {
					total += v.Size
				}
			}
		}
		if len(orphans) == 0 {
			fmt.Println("nothing to remove: every volume belongs to a stack that still exists")
			return nil
		}
		fmt.Println("These volumes belong to stacks that were deleted:")
		for _, v := range orphans {
			fmt.Printf("  %s  (stack %s, %s)\n", v.Name, v.Stack, size(v))
		}
		fmt.Printf("Removing them deletes their data for good (%s).\n", human(total))
		if *dry || !confirm(*yes, "Delete them?") {
			return nil
		}
		for _, v := range orphans {
			if err := call("DELETE", "/v1/volumes/"+url.PathEscape(v.Name), nil, nil); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", v.Name, err)
				continue
			}
			fmt.Printf("removed volume %s\n", v.Name)
		}
		return nil
	}
	fmt.Print("xu volumes [ls] | xu volumes rm NAME... [-y] | xu volumes prune [--dry-run] [-y]\n")
	return nil
}

func cacheCmd(args []string) error {
	sub := "df"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	s, err := localCache()
	if err != nil {
		return err
	}
	switch sub {
	case "df", "ls":
		keys, blobs, n, err := s.Usage()
		if err != nil {
			return err
		}
		fmt.Printf("xu build cache: %s\n  %d cached layers, %d stored blobs (layers and base images), %s\n", s.Dir, keys, blobs, human(n))
		return nil
	case "clean", "prune":
		fs := flag.NewFlagSet("clean", flag.ContinueOnError)
		older := fs.Duration("older-than", 30*24*time.Hour, "")
		all := fs.Bool("all", false, "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		if *all {
			*older = 0
		}
		removed, freed, err := s.Clean(*older)
		if err != nil {
			return err
		}
		what := "everything"
		if *older > 0 {
			what = "entries unused for " + older.String()
		}
		fmt.Printf("removed %s: %d cached layers, %s freed. The next builds rebuild what they need.\n", what, removed, human(freed))
		return nil
	}
	fmt.Print("xu cache [df] | xu cache clean [--older-than 720h | --all]   (the build cache on this machine)\n")
	return nil
}

func systemCmd(args []string) error {
	sub := "df"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "df":
		return systemDF()
	case "prune":
		fs := flag.NewFlagSet("prune", flag.ContinueOnError)
		keep := fs.Int("keep", 5, "")
		vols := fs.Bool("volumes", false, "")
		dry := fs.Bool("dry-run", false, "")
		yes := fs.Bool("y", false, "")
		fs.BoolVar(yes, "yes", false, "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		fmt.Println("== images")
		if err := pruneImages(*keep, *dry, *yes); err != nil {
			return err
		}
		fmt.Println("\n== git build cache on the server")
		var u platform.DiskUsage
		if err := call("GET", "/v1/system/df", nil, &u); err == nil {
			fmt.Printf("%s of checkouts and build layers\n", human(u.BuildCacheBytes))
			if !*dry && u.BuildCacheBytes > 0 && confirm(*yes, "Clear it?") {
				var res map[string]int64
				if err := call("POST", "/v1/system/buildcache/clean", nil, &res); err != nil {
					fmt.Fprintln(os.Stderr, err)
				} else {
					fmt.Printf("freed %s\n", human(res["freed_bytes"]))
				}
			}
		}
		fmt.Println("\n== xu build cache on this machine (entries unused for 30 days)")
		if s, err := localCache(); err == nil {
			if *dry {
				_, _, n, _ := s.Usage()
				fmt.Printf("would remove entries unused for 30 days (the cache holds %s in all)\n", human(n))
			} else {
				removed, freed, _ := s.Clean(30 * 24 * time.Hour)
				fmt.Printf("removed %d cached layers, freed %s\n", removed, human(freed))
			}
		}
		if *vols {
			fmt.Println("\n== volumes of deleted stacks")
			flags := []string{"prune"}
			if *dry {
				flags = append(flags, "--dry-run")
			}
			if *yes {
				flags = append(flags, "-y")
			}
			return volumesCmd(flags)
		}
		return nil
	}
	fmt.Print("xu system df | xu system prune [--keep 5] [--volumes] [--dry-run] [-y]\n")
	return nil
}

// ---- stats ----

func statsCmd(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	watch := fs.Bool("watch", false, "")
	fs.BoolVar(watch, "w", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	app := ""
	if len(pos) > 0 {
		app = pos[0]
	}
	for {
		var list []platform.AppStats
		if err := call("GET", "/v1/stats?app="+url.QueryEscape(app), nil, &list); err != nil {
			return err
		}
		if *watch {
			fmt.Print("\033[H\033[2J")
			fmt.Printf("xu stats  %s  (Ctrl-C to stop)\n\n", time.Now().Format("15:04:05"))
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "APP\tINSTANCE\tCPU\tMEMORY\tIN-FLIGHT\tUPTIME\tREQ/S\tP95\tERRORS")
		for _, a := range list {
			traffic := fmt.Sprintf("%.1f\t%s\t%.1f%%", a.RPS, fmtMs(a.P95), a.ErrorRate*100)
			if len(a.Instances) == 0 {
				fmt.Fprintf(tw, "%s\t(scaled to zero)\t\t\t%d\t\t%s\n", a.App, a.InFlight, traffic)
				continue
			}
			for i, in := range a.Instances {
				name, tr := a.App, traffic
				if i > 0 {
					name, tr = "", "\t\t"
				}
				mem := human(int64(in.MemBytes))
				if a.MemLimit > 0 {
					mem += fmt.Sprintf(" / %d MB", a.MemLimit)
				}
				fmt.Fprintf(tw, "%s\t%s\t%.1f%%\t%s\t%d\t%s\t%s\n", name, shortID(in.ID), in.CPU, mem, in.InFlight, in.Uptime, tr)
			}
		}
		tw.Flush()
		if len(list) == 0 {
			fmt.Println("(no apps)")
		}
		if !*watch {
			return nil
		}
		time.Sleep(time.Second) // the server already samples for about a second
	}
}

func fmtMs(ms float64) string {
	if ms == 0 {
		return "-"
	}
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", ms/1000)
	}
	return fmt.Sprintf("%.0fms", ms)
}

func xuOnly(tags []string) []string {
	var out []string
	for _, t := range tags {
		if strings.HasPrefix(t, "xu/") {
			out = append(out, t)
		}
	}
	return out
}

func otherTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if !strings.HasPrefix(t, "xu/") {
			out = append(out, t)
		}
	}
	return out
}
