// Package oci writes OCI images directly: reproducible tar layers, image
// config, manifest, and a single tarball that `docker load` and
// `podman load` both accept. No Docker daemon is involved.
package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// File is one entry to put in a layer.
type File struct {
	Src  string // host file to copy, or
	Data []byte // inline content
	Dst  string // absolute path in the image
	Mode fs.FileMode
	Link string // if set, a symlink to this target instead of a file
}

// Layer is a finished, compressed layer.
type Layer struct {
	Blob      []byte // gzip-compressed tar
	Digest    string // sha256 of Blob ("sha256:…")
	DiffID    string // sha256 of the uncompressed tar
	Size      int64
	MediaType string // "" means MediaLayer (tar+gzip); base layers may be zstd
}

// BuildLayer writes files as a reproducible layer: entries sorted by path,
// parent directories added, owner 0:0, every timestamp set to epoch, and
// deterministic gzip. The same files always give the same digest.
func BuildLayer(files []File, epoch int64) (*Layer, error) {
	mtime := time.Unix(epoch, 0).UTC()
	type item struct {
		name string
		dir  bool
		f    File
	}
	items := map[string]item{}
	for _, f := range files {
		name := strings.TrimPrefix(path.Clean("/"+f.Dst), "/")
		if name == "" || name == "." {
			return nil, fmt.Errorf("invalid destination %q", f.Dst)
		}
		if _, dup := items[name]; dup {
			return nil, fmt.Errorf("two files want %s", f.Dst)
		}
		items[name] = item{name: name, f: f}
		for d := path.Dir(name); d != "." && d != "/"; d = path.Dir(d) {
			if _, ok := items[d]; !ok {
				items[d] = item{name: d, dir: true}
			}
		}
	}
	names := make([]string, 0, len(items))
	for n := range items {
		names = append(names, n)
	}
	sort.Strings(names)

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, n := range names {
		it := items[n]
		hdr := &tar.Header{Name: n, ModTime: mtime, Uid: 0, Gid: 0, Format: tar.FormatPAX}
		if it.dir {
			hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, n+"/", 0o755
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, err
			}
			continue
		}
		if it.f.Link != "" {
			hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, it.f.Link, 0o777
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, err
			}
			continue
		}
		data := it.f.Data
		if it.f.Src != "" {
			b, err := os.ReadFile(it.f.Src)
			if err != nil {
				return nil, err
			}
			data = b
		}
		mode := it.f.Mode.Perm()
		if mode == 0 {
			mode = 0o644
		}
		hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeReg, int64(mode), int64(len(data))
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return Compress(raw.Bytes())
}

// Compress gzips an uncompressed layer tar deterministically.
func Compress(rawTar []byte) (*Layer, error) {
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression) // header has no name and zero mtime
	if _, err := zw.Write(rawTar); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return &Layer{Blob: gz.Bytes(), Digest: Digest(gz.Bytes()), DiffID: Digest(rawTar), Size: int64(gz.Len())}, nil
}

// Digest is the OCI digest of b.
func Digest(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ReadLayerFiles lists a compressed layer's entries (for tests and inspection).
func ReadLayerFiles(blob []byte) (map[string]*tar.Header, error) {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	out := map[string]*tar.Header{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out[h.Name] = h
	}
}
