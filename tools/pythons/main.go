// pythons prints the python-build-standalone release table with SHA256
// checksums filled in from each release's SHA256SUMS, for pinning them in
// internal/bundle/plugins/python/versions.go. Run it on a machine that can
// reach GitHub:
//
//	go run ./tools/pythons
//
// Until checksums are pinned in code, xu verifies each tarball against the
// release's SHA256SUMS on first use and records it in the project's xu.lock.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	"xu526/internal/bundle/plugins/python"
)

func main() {
	sums := map[string]string{} // tag → SHA256SUMS
	var minors []int
	for m := range python.Releases {
		minors = append(minors, m)
	}
	sort.Ints(minors)
	fmt.Println("var Releases = map[int]Release{")
	for _, m := range minors {
		r := python.Releases[m]
		if _, ok := sums[r.Tag]; !ok {
			resp, err := http.Get(python.ReleaseBase + "/" + r.Tag + "/SHA256SUMS")
			if err != nil {
				log.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				log.Fatalf("%s SHA256SUMS: %s", r.Tag, resp.Status)
			}
			sums[r.Tag] = string(b)
		}
		var pins []string
		for _, arch := range []string{"amd64", "arm64"} {
			cpu := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch]
			name := fmt.Sprintf("cpython-%s+%s-%s-unknown-linux-gnu-install_only_stripped.tar.gz", r.Version, r.Tag, cpu)
			found := ""
			for _, line := range strings.Split(sums[r.Tag], "\n") {
				if f := strings.Fields(line); len(f) == 2 && f[1] == name {
					found = f[0]
				}
			}
			if found == "" {
				log.Fatalf("%s not in %s SHA256SUMS", name, r.Tag)
			}
			pins = append(pins, fmt.Sprintf("%q: %q", arch, found))
		}
		fmt.Printf("\t%d: {Version: %q, Tag: %q, SHA256: map[string]string{%s}},\n", m, r.Version, r.Tag, strings.Join(pins, ", "))
	}
	fmt.Println("}")
}
