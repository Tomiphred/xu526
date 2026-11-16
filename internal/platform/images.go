package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"xu526/internal/runtime"
)

// uploadRef is the only kind of name an uploaded image may take, so an
// upload can never shadow a registry image such as nginx:latest.
var uploadRef = regexp.MustCompile(`^xu/[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?:[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// LoadImage imports an image tarball (from `xu build`) into Docker. When
// want is given, the loaded image's ID must be one of those digests, which
// catches truncated or altered uploads. Docker's classic image store names
// images by config digest and its containerd store by manifest digest, so
// clients send both.
func (p *Platform) LoadImage(ctx context.Context, r io.Reader, ref string, want ...string) (string, error) {
	if !uploadRef.MatchString(ref) {
		return "", fmt.Errorf("uploaded images must be named xu/<name>:<tag>, not %q", ref)
	}
	d, ok := p.cfg.Docker.(*runtime.Docker)
	if !ok {
		return "", errors.New("loading images needs Docker")
	}
	id, err := d.Load(ctx, r, ref)
	if err != nil {
		return "", err
	}
	if len(want) > 0 {
		ok := false
		for _, w := range want {
			ok = ok || (w != "" && w == id)
		}
		if !ok {
			return "", fmt.Errorf("the uploaded image is %s, not the image that was built (%v)", id, want)
		}
	}
	p.event("", "image", "loaded %s (%s)", ref, id[:min(len(id), 19)])
	return id, nil
}

func (p *Platform) imageAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/images", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id, err := p.LoadImage(r.Context(), http.MaxBytesReader(w, r.Body, 16<<30), q.Get("ref"), q["id"]...)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ref": q.Get("ref"), "id": id})
	})
}
