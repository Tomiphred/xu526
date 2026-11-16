package platform

import (
	"strings"
	"testing"
	"time"

	"xu526/internal/runtime"
)

func img(id string, tags ...string) runtime.ImageSummary {
	return runtime.ImageSummary{ID: id, Tags: tags, Size: 100, SharedSize: 40, Created: time.Now()}
}

func TestClassifyAndPrune(t *testing.T) {
	imgs := []runtime.ImageSummary{
		img("sha256:a1", "xu/pof:v1"), img("sha256:a2", "xu/pof:v2"), img("sha256:a3", "xu/pof:v3"),
		img("sha256:a4", "xu/pof:v4"), img("sha256:a5", "xu/pof:v5"),
		img("sha256:b1", "xu/shop-web:c1"), img("sha256:b2", "xu/shop-web:c2"),
		img("sha256:z9", "xu/old:x"),
	}
	live := map[string][]string{"sha256:a5": {"pof"}, "xu/shop-web:c2": {"shop/web"}}
	rels := []relRef{
		{key: "pof", version: 1, ids: []string{"sha256:a1"}}, {key: "pof", version: 2, ids: []string{"sha256:a2"}},
		{key: "pof", version: 3, ids: []string{"sha256:a3"}}, {key: "pof", version: 4, ids: []string{"sha256:a4"}},
		{key: "pof", version: 5, ids: []string{"sha256:a5"}},
		{key: "shop", version: 1, ids: []string{"sha256:b1"}}, // stack release pins
		{key: "shop", version: 2, ids: []string{"sha256:b2"}},
	}
	xs := classify(imgs, live, rels)
	status := map[string]string{}
	for _, x := range xs {
		status[x.Tags[0]] = x.Status
	}
	if status["xu/pof:v5"] != "live" || status["xu/shop-web:c2"] != "live" || status["xu/pof:v1"] != "rollback" || status["xu/old:x"] != "unused" {
		t.Fatalf("%v", status)
	}
	var pof *XuImage
	for i := range xs {
		if xs[i].Tags[0] == "xu/pof:v3" {
			pof = &xs[i]
		}
	}
	if pof.App != "pof" || strings.Join(pof.Releases, ",") != "pof v3" {
		t.Fatalf("%+v", pof)
	}
	// keep=2: pof keeps v5 (live) and v4; shop keeps both; old goes.
	plan := planPrune(xs, rels, 2)
	var gone []string
	var freed int64
	for _, x := range plan {
		gone = append(gone, x.Tags[0])
		freed += x.Unique()
	}
	if strings.Join(gone, " ") != "xu/old:x xu/pof:v3 xu/pof:v2 xu/pof:v1" && strings.Join(gone, " ") != "xu/old:x xu/pof:v1 xu/pof:v2 xu/pof:v3" {
		// classify sorts by app then newest first; accept either release order
		t.Fatalf("prune plan: %v", gone)
	}
	if freed != 4*60 {
		t.Fatalf("freed %d, want unique sizes only", freed)
	}
	// A live image is never pruned, even with keep=1 and no release for it.
	for _, x := range planPrune(xs, nil, 1) {
		if len(x.Live) > 0 {
			t.Fatalf("pruned live image %v", x.Tags)
		}
	}
}

func TestMatchImage(t *testing.T) {
	xs := classify([]runtime.ImageSummary{
		img("sha256:0836fe631e376ff1c7f2", "xu/pof:0836fe631e37"), img("sha256:110489e77878aaaa", "xu/pof:110489e77878"),
		img("sha256:ffff00001111", "xu/solo:abc"),
	}, nil, nil)
	cases := map[string]string{
		"xu/pof:0836fe631e37": "xu/pof:0836fe631e37", "docker.io/xu/pof:110489e77878": "xu/pof:110489e77878",
		"0836fe631e37": "xu/pof:0836fe631e37", "sha256:110489e77878": "xu/pof:110489e77878", "xu/solo": "xu/solo:abc",
	}
	for in, want := range cases {
		m, err := matchImage(xs, in)
		if err != nil || m.Tags[0] != want {
			t.Errorf("%s -> %v %v, want %s", in, m, err, want)
		}
	}
	for in, msg := range map[string]string{"xu/pof": "matches 2 images", "nginx:latest": "isn't an xu image", "xu/nope:1": "no xu image", "0836": "isn't an xu image"} {
		if _, err := matchImage(xs, in); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: want %q, got %v", in, msg, err)
		}
	}
}
