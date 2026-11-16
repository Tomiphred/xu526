package oci

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Media types written by this package.
const (
	MediaManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaConfig   = "application/vnd.oci.image.config.v1+json"
	MediaLayer    = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaIndex    = "application/vnd.oci.image.index.v1+json"
)

// Config is the runtime part of the image config.
type Config struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
	Port       int
	Labels     map[string]string
}

// Image is an assembled image ready to write.
type Image struct {
	OS, Arch     string
	Layers       []*Layer
	LayerHistory []string // one comment per layer xu built ("app", "deps", …)
	BaseLayers   int      // how many of Layers come from the base image
	BaseHistory  []json.RawMessage
	Config       Config
	Epoch        int64

	configJSON   []byte
	manifestJSON []byte
}

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *platform         `json:"platform,omitempty"`
}

type platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

// Seal computes the config and manifest. Call after setting all fields.
func (im *Image) Seal() error {
	created := time.Unix(im.Epoch, 0).UTC().Format(time.RFC3339)
	type history struct {
		Created   string `json:"created"`
		CreatedBy string `json:"created_by"`
		Comment   string `json:"comment,omitempty"`
	}
	cfg := map[string]any{"Env": im.Config.Env}
	if len(im.Config.Entrypoint) > 0 {
		cfg["Entrypoint"] = im.Config.Entrypoint
	}
	if len(im.Config.Cmd) > 0 {
		cfg["Cmd"] = im.Config.Cmd
	}
	if im.Config.WorkingDir != "" {
		cfg["WorkingDir"] = im.Config.WorkingDir
	}
	if im.Config.User != "" {
		cfg["User"] = im.Config.User
	}
	if im.Config.Port != 0 {
		cfg["ExposedPorts"] = map[string]struct{}{strconv.Itoa(im.Config.Port) + "/tcp": {}}
	}
	if len(im.Config.Labels) > 0 {
		cfg["Labels"] = im.Config.Labels
	}
	var diffIDs []string
	hist := []any{}
	for _, h := range im.BaseHistory {
		hist = append(hist, h)
	}
	for i, l := range im.Layers {
		diffIDs = append(diffIDs, l.DiffID)
		if i < im.BaseLayers {
			continue
		}
		c := ""
		if j := i - im.BaseLayers; j < len(im.LayerHistory) {
			c = im.LayerHistory[j]
		}
		hist = append(hist, history{Created: created, CreatedBy: "xu build", Comment: c})
	}
	var err error
	im.configJSON, err = json.Marshal(map[string]any{
		"architecture": im.Arch, "os": im.OS, "created": created, "config": cfg,
		"rootfs":  map[string]any{"type": "layers", "diff_ids": diffIDs},
		"history": hist,
	})
	if err != nil {
		return err
	}
	var layers []descriptor
	for _, l := range im.Layers {
		layers = append(layers, descriptor{MediaType: l.mediaType(), Digest: l.Digest, Size: l.Size})
	}
	if layers == nil {
		layers = []descriptor{}
	}
	im.manifestJSON, err = json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": MediaManifest,
		"config": descriptor{MediaType: MediaConfig, Digest: Digest(im.configJSON), Size: int64(len(im.configJSON))},
		"layers": layers,
	})
	return err
}

// Digest is the image's manifest digest: its identity in a registry.
func (im *Image) Digest() string { return Digest(im.manifestJSON) }

// ConfigDigest is what `docker images` shows as the image ID.
func (im *Image) ConfigDigest() string { return Digest(im.configJSON) }

// Manifest and ConfigJSON expose the sealed documents (for pushing).
func (im *Image) Manifest() []byte   { return im.manifestJSON }
func (im *Image) ConfigJSON() []byte { return im.configJSON }

// WriteTar writes one tarball holding the image twice-described: as an OCI
// image layout (index.json, oci-layout, blobs/sha256/…) and with Docker's
// manifest.json pointing at the same blobs. `docker load` (any version) and
// `podman load` accept it. Entries are sorted and timestamped at the epoch,
// so the tarball itself is reproducible.
func (im *Image) WriteTar(w io.Writer, ref string) error {
	if im.manifestJSON == nil {
		return fmt.Errorf("image not sealed")
	}
	files := map[string][]byte{}
	blob := func(d string, b []byte) string {
		p := "blobs/sha256/" + strings.TrimPrefix(d, "sha256:")
		files[p] = b
		return p
	}
	cfgPath := blob(Digest(im.configJSON), im.configJSON)
	blob(im.Digest(), im.manifestJSON)
	var layerPaths []string
	for _, l := range im.Layers {
		layerPaths = append(layerPaths, blob(l.Digest, l.Blob))
	}
	ann := map[string]string{}
	var repoTags []string
	if ref != "" {
		ann["io.containerd.image.name"] = NormalizeRef(ref)
		if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
			ann["org.opencontainers.image.ref.name"] = ref[i+1:]
		}
		repoTags = []string{ref}
	}
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": MediaIndex,
		"manifests": []descriptor{{MediaType: MediaManifest, Digest: im.Digest(), Size: int64(len(im.manifestJSON)),
			Annotations: ann, Platform: &platform{Architecture: im.Arch, OS: im.OS}}},
	})
	files["index.json"] = index
	files["oci-layout"] = []byte(`{"imageLayoutVersion":"1.0.0"}`)
	dm, _ := json.Marshal([]map[string]any{{"Config": cfgPath, "RepoTags": repoTags, "Layers": layerPaths}})
	files["manifest.json"] = dm

	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	mtime := time.Unix(im.Epoch, 0).UTC()
	tw := tar.NewWriter(w)
	for _, d := range []string{"blobs/", "blobs/sha256/"} {
		if err := tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mtime, Format: tar.FormatPAX}); err != nil {
			return err
		}
	}
	for _, n := range names {
		b := files[n]
		if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b)), ModTime: mtime, Format: tar.FormatPAX}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	return tw.Close()
}

// NormalizeRef expands a short reference the way Docker does:
// "app" → "docker.io/library/app:latest", "me/app:1" → "docker.io/me/app:1".
// containerd (and so Docker's containerd image store) only finds images by
// their full name.
func NormalizeRef(ref string) string {
	name, tag := ref, ""
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i:]
	}
	if !strings.Contains(ref, "@") && tag == "" {
		tag = ":latest"
	}
	first, _, hasSlash := strings.Cut(name, "/")
	switch {
	case !hasSlash:
		name = "docker.io/library/" + name
	case !strings.ContainsAny(first, ".:") && first != "localhost":
		name = "docker.io/" + name
	}
	return name + tag
}

// MediaLayerZstd is a zstd-compressed layer, which some base images use.
const MediaLayerZstd = "application/vnd.oci.image.layer.v1.tar+zstd"

func (l *Layer) mediaType() string {
	switch l.MediaType {
	case "", "application/vnd.docker.image.rootfs.diff.tar.gzip":
		return MediaLayer // same bytes, OCI name
	}
	return l.MediaType
}
