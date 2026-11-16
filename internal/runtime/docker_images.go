package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ImageSummary is one image on the host.
type ImageSummary struct {
	ID         string            `json:"id"`
	Tags       []string          `json:"tags"`
	Size       int64             `json:"size"`        // including layers shared with other images
	SharedSize int64             `json:"shared_size"` // -1 when unknown
	Created    time.Time         `json:"created"`     // the epoch for reproducible builds
	Added      time.Time         `json:"added"`       // when it got its current name on this host
	Labels     map[string]string `json:"labels,omitempty"`
}

// Unique is the space removing this image alone would free (approximately).
func (s ImageSummary) Unique() int64 {
	if s.SharedSize > 0 && s.SharedSize <= s.Size {
		return s.Size - s.SharedSize
	}
	return s.Size
}

// ShortName drops Docker's implicit docker.io/ prefix from a tag.
func ShortName(tag string) string {
	tag = strings.TrimPrefix(tag, "docker.io/")
	return strings.TrimPrefix(tag, "library/")
}

type dockerImage struct {
	ID          string `json:"Id"`
	RepoTags    []string
	Size        int64
	SharedSize  int64
	Created     int64
	Labels      map[string]string
	Containers  int64
	RepoDigests []string
}

func (i dockerImage) summary() ImageSummary {
	s := ImageSummary{ID: i.ID, Size: i.Size, SharedSize: i.SharedSize, Created: time.Unix(i.Created, 0), Labels: i.Labels}
	for _, t := range i.RepoTags {
		if t != "<none>:<none>" {
			s.Tags = append(s.Tags, ShortName(t))
		}
	}
	return s
}

// Images lists images whose name matches reference (e.g. "xu/*"), with
// disk usage. It uses /system/df so shared layers are accounted for.
func (d *Docker) Images(ctx context.Context, reference string) ([]ImageSummary, error) {
	var df struct{ Images []dockerImage }
	if err := d.do(ctx, "GET", "/system/df?type=image", nil, &df); err != nil || df.Images == nil {
		// Older engines don't support type=; fall back to the plain list.
		f, _ := json.Marshal(map[string][]string{"reference": {reference}})
		var list []dockerImage
		if err := d.do(ctx, "GET", "/images/json?filters="+url.QueryEscape(string(f)), nil, &list); err != nil {
			return nil, err
		}
		df.Images = list
	}
	prefix := strings.TrimSuffix(reference, "*")
	var out []ImageSummary
	for _, img := range df.Images {
		s := img.summary()
		match := false
		for _, t := range s.Tags {
			match = match || strings.HasPrefix(t, prefix)
		}
		if !match {
			continue
		}
		// Names with the prefix first (xu/… before go-hello:latest).
		sort.SliceStable(s.Tags, func(i, j int) bool {
			return strings.HasPrefix(s.Tags[i], prefix) && !strings.HasPrefix(s.Tags[j], prefix)
		})
		var meta struct {
			Metadata struct{ LastTagTime time.Time }
		}
		if d.do(ctx, "GET", "/images/"+url.PathEscape(s.ID)+"/json", nil, &meta) == nil && !meta.Metadata.LastTagTime.IsZero() {
			s.Added = meta.Metadata.LastTagTime
		} else {
			s.Added = s.Created
		}
		out = append(out, s)
	}
	return out, nil
}

// ImageDetails is what `xu images inspect` shows.
type ImageDetails struct {
	ID           string            `json:"id"`
	Tags         []string          `json:"tags"`
	Digests      []string          `json:"digests,omitempty"`
	Created      time.Time         `json:"created"`
	Size         int64             `json:"size"`
	Architecture string            `json:"architecture"`
	OS           string            `json:"os"`
	Entrypoint   []string          `json:"entrypoint,omitempty"`
	Cmd          []string          `json:"cmd,omitempty"`
	WorkingDir   string            `json:"working_dir,omitempty"`
	User         string            `json:"user,omitempty"`
	Ports        []string          `json:"ports,omitempty"`
	EnvNames     []string          `json:"env_names"` // values are left out on purpose
	Labels       map[string]string `json:"labels,omitempty"`
	Layers       []LayerInfo       `json:"layers"`
}

// LayerInfo is one history entry.
type LayerInfo struct {
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by"`
	Comment   string    `json:"comment,omitempty"`
	Size      int64     `json:"size"`
}

// InspectImage returns an image's configuration and layers. Environment
// values are not returned, only names.
func (d *Docker) InspectImage(ctx context.Context, ref string) (*ImageDetails, error) {
	var img struct {
		ID           string `json:"Id"`
		RepoTags     []string
		RepoDigests  []string
		Created      time.Time
		Size         int64
		Architecture string
		Os           string
		Config       struct {
			Entrypoint   []string
			Cmd          []string
			WorkingDir   string
			User         string
			Env          []string
			ExposedPorts map[string]any
			Labels       map[string]string
		}
	}
	if err := d.do(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil, &img); err != nil {
		return nil, err
	}
	out := &ImageDetails{ID: img.ID, Digests: img.RepoDigests, Created: img.Created, Size: img.Size,
		Architecture: img.Architecture, OS: img.Os, Entrypoint: img.Config.Entrypoint, Cmd: img.Config.Cmd,
		WorkingDir: img.Config.WorkingDir, User: img.Config.User, Labels: img.Config.Labels, EnvNames: []string{}}
	for _, t := range img.RepoTags {
		out.Tags = append(out.Tags, ShortName(t))
	}
	for p := range img.Config.ExposedPorts {
		out.Ports = append(out.Ports, p)
	}
	for _, kv := range img.Config.Env {
		k, _, _ := strings.Cut(kv, "=")
		out.EnvNames = append(out.EnvNames, k)
	}
	var hist []struct {
		Created   int64
		CreatedBy string
		Comment   string
		Size      int64
	}
	if err := d.do(ctx, "GET", "/images/"+url.PathEscape(ref)+"/history", nil, &hist); err == nil {
		for _, h := range hist {
			out.Layers = append(out.Layers, LayerInfo{Created: time.Unix(h.Created, 0), CreatedBy: h.CreatedBy, Comment: h.Comment, Size: h.Size})
		}
	}
	return out, nil
}

// RemoveImage untags ref, deleting the image when it was the last tag.
func (d *Docker) RemoveImage(ctx context.Context, ref string, force bool) error {
	q := "?noprune=false"
	if force {
		q += "&force=true"
	}
	return d.do(ctx, "DELETE", "/images/"+url.PathEscape(ref)+q, nil, nil)
}

// TagImage gives an image another name (repo:tag).
func (d *Docker) TagImage(ctx context.Context, ref, repo, tag string) error {
	q := url.Values{"repo": {repo}, "tag": {tag}}
	return d.do(ctx, "POST", "/images/"+url.PathEscape(ref)+"/tag?"+q.Encode(), nil, nil)
}

// SaveImage streams an image as a tarball (docker save format).
func (d *Docker) SaveImage(ctx context.Context, ref string) (io.ReadCloser, error) {
	resp, err := d.raw(ctx, "GET", "/images/get?names="+url.QueryEscape(ref), nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// VolumeInfo is a named volume.
type VolumeInfo struct {
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt string            `json:"created_at"`
	Size      int64             `json:"size"` // -1 when unknown
	RefCount  int64             `json:"containers"`
}

// Volumes lists volumes carrying label (e.g. "xu.managed=true") with sizes.
func (d *Docker) Volumes(ctx context.Context, label string) ([]VolumeInfo, error) {
	var df struct {
		Volumes []struct {
			Name      string
			Labels    map[string]string
			CreatedAt string
			UsageData struct{ Size, RefCount int64 }
		}
	}
	if err := d.do(ctx, "GET", "/system/df?type=volume", nil, &df); err != nil {
		return nil, err
	}
	k, v, _ := strings.Cut(label, "=")
	var out []VolumeInfo
	for _, vol := range df.Volumes {
		if label != "" && vol.Labels[k] != v {
			continue
		}
		out = append(out, VolumeInfo{Name: vol.Name, Labels: vol.Labels, CreatedAt: vol.CreatedAt, Size: vol.UsageData.Size, RefCount: vol.UsageData.RefCount})
	}
	return out, nil
}

// RemoveVolume deletes a volume and its data.
func (d *Docker) RemoveVolume(ctx context.Context, name string) error {
	if err := d.do(ctx, "DELETE", "/volumes/"+url.PathEscape(name), nil, nil); err != nil {
		return fmt.Errorf("remove volume %s: %w", name, err)
	}
	return nil
}
