package runtime

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ImageInfo identifies an exact image.
type ImageInfo struct {
	ID     string // sha256:… on this host
	Digest string // repo@sha256:… when it came from a registry
}

// Resolve pulls ref (to pick up a moved tag) and returns its exact identity.
// Images that exist only on this server are used as they are.
func (d *Docker) Resolve(ctx context.Context, ref, auth string) (ImageInfo, error) {
	var pullErr error
	if !strings.HasPrefix(ref, "xu/") && !strings.HasPrefix(ref, "sha256:") {
		pullErr = d.pull(ctx, ref, auth)
	}
	var img struct {
		Id          string
		RepoDigests []string
	}
	if err := d.do(ctx, "GET", "/images/"+ref+"/json", nil, &img); err != nil {
		if pullErr != nil {
			return ImageInfo{}, pullErr
		}
		return ImageInfo{}, err
	}
	info := ImageInfo{ID: img.Id}
	if len(img.RepoDigests) > 0 {
		info.Digest = img.RepoDigests[0]
	}
	return info, nil
}

// Build builds dir (a Docker build context) and tags the result. Build output
// is written to logw. registryConfig is base64 JSON for X-Registry-Config.
func (d *Docker) Build(ctx context.Context, dir, dockerfile, tag, registryConfig string, logw io.Writer) (string, error) {
	var buf bytes.Buffer
	if err := tarContext(dir, &buf); err != nil {
		return "", fmt.Errorf("pack build context: %w", err)
	}
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	q := url.Values{"t": {tag}, "dockerfile": {dockerfile}, "rm": {"1"}, "forcerm": {"1"}}
	resp, err := d.rawReq(ctx, "POST", "/build?"+q.Encode(), &buf, map[string]string{
		"Content-Type": "application/x-tar", "X-Registry-Config": registryConfig,
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := streamJSON(resp.Body, logw); err != nil {
		return "", err
	}
	var img struct{ Id string }
	if err := d.do(ctx, "GET", "/images/"+tag+"/json", nil, &img); err != nil {
		return "", err
	}
	return img.Id, nil
}

// Load imports an image tarball (docker save / OCI layout, as written by
// xu build) and returns the image ID that ref now names.
func (d *Docker) Load(ctx context.Context, tarball io.Reader, ref string) (string, error) {
	resp, err := d.rawReq(ctx, "POST", "/images/load?quiet=1", tarball, map[string]string{"Content-Type": "application/x-tar"})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := streamJSON(resp.Body, io.Discard); err != nil {
		return "", fmt.Errorf("load image: %w", err)
	}
	var img struct{ Id string }
	if err := d.do(ctx, "GET", "/images/"+ref+"/json", nil, &img); err != nil {
		return "", fmt.Errorf("loaded image has no tag %s: %w", ref, err)
	}
	return img.Id, nil
}

// Push tags image as repo:tag and pushes it to its registry.
func (d *Docker) Push(ctx context.Context, image, repo, tag, auth string, logw io.Writer) error {
	q := url.Values{"repo": {repo}, "tag": {tag}}
	if err := d.do(ctx, "POST", "/images/"+image+"/tag?"+q.Encode(), nil, nil); err != nil {
		return fmt.Errorf("tag: %w", err)
	}
	resp, err := d.rawReq(ctx, "POST", "/images/"+repo+"/push?tag="+url.QueryEscape(tag), nil,
		map[string]string{"X-Registry-Auth": auth})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return streamJSON(resp.Body, logw)
}

// streamJSON copies Docker's JSON progress stream to w and returns the first error in it.
func streamJSON(r io.Reader, w io.Writer) error {
	dec := json.NewDecoder(r)
	for {
		var m struct {
			Stream, Status, Error, Progress string
			ErrorDetail                     struct{ Message string }
		}
		if err := dec.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		switch {
		case m.Error != "":
			fmt.Fprintln(w, "ERROR:", m.Error)
			return errors.New(m.Error)
		case m.Stream != "":
			io.WriteString(w, m.Stream)
		case m.Status != "" && m.Progress == "":
			fmt.Fprintln(w, m.Status)
		}
	}
}

// tarContext packs dir, honouring simple .dockerignore patterns and skipping .git.
func tarContext(dir string, w io.Writer) error {
	var ignore []string
	if b, err := os.ReadFile(filepath.Join(dir, ".dockerignore")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "!") {
				ignore = append(ignore, strings.TrimPrefix(strings.TrimSuffix(l, "/"), "./"))
			}
		}
	}
	skip := func(rel string) bool {
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			return true
		}
		for _, p := range ignore {
			if ok, _ := filepath.Match(p, rel); ok || strings.HasPrefix(rel, p+"/") {
				return true
			}
		}
		return false
	}
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skip(rel) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		hdr.ModTime = time.Unix(0, 0) // reproducible contexts build-cache better
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Follow streams an instance's output line by line until it exits or ctx ends.
func (d *Docker) Follow(ctx context.Context, inst *Instance, since time.Time, fn func(stream, line string)) error {
	q := "follow=true&stdout=true&stderr=true&since=" + strconv.FormatInt(since.Unix(), 10)
	resp, err := d.raw(ctx, "GET", "/containers/"+inst.ID+"/logs?"+q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	hdr := make([]byte, 8)
	partial := map[byte]string{}
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			for k, v := range partial {
				if v != "" {
					fn(streamName(k), v)
				}
			}
			return nil
		}
		buf := make([]byte, binary.BigEndian.Uint32(hdr[4:]))
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil
		}
		text := partial[hdr[0]] + string(buf)
		lines := strings.Split(text, "\n")
		partial[hdr[0]] = lines[len(lines)-1]
		for _, l := range lines[:len(lines)-1] {
			fn(streamName(hdr[0]), strings.TrimRight(l, "\r"))
		}
	}
}

func streamName(b byte) string {
	if b == 2 {
		return "stderr"
	}
	return "stdout"
}

// Usage is one resource sample of a container.
type Usage struct {
	CPUTotal, SystemTotal uint64
	CPUs                  int
	MemBytes              uint64
}

// Stats takes one resource sample without waiting.
func (d *Docker) Stats(ctx context.Context, inst *Instance) (Usage, error) {
	var s struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs     int    `json:"online_cpus"`
		} `json:"cpu_stats"`
		MemoryStats struct {
			Usage uint64            `json:"usage"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
	}
	if err := d.do(ctx, "GET", "/containers/"+inst.ID+"/stats?stream=false&one-shot=true", nil, &s); err != nil {
		return Usage{}, err
	}
	mem := s.MemoryStats.Usage
	if c := s.MemoryStats.Stats["inactive_file"]; c < mem { // what `docker stats` shows
		mem -= c
	}
	return Usage{CPUTotal: s.CPUStats.CPUUsage.TotalUsage, SystemTotal: s.CPUStats.SystemCPUUsage,
		CPUs: max(1, s.CPUStats.OnlineCPUs), MemBytes: mem}, nil
}

// Login asks Docker to verify registry credentials.
func (d *Docker) Login(ctx context.Context, user, pass, server string) error {
	if server == "docker.io" {
		server = "https://index.docker.io/v1/"
	}
	return d.do(ctx, "POST", "/auth", map[string]string{"username": user, "password": pass, "serveraddress": server}, nil)
}
