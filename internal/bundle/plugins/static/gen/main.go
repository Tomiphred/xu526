// gen rebuilds the embedded xu-static binaries. Run from the static plugin
// folder with `go generate`. The output is reproducible for a given Go
// version, which is recorded in bin/VERSION.
package main

import (
	"bytes"
	"compress/gzip"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	tmp, err := os.MkdirTemp("", "xu-static-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	for _, arch := range []string{"amd64", "arm64"} {
		out := filepath.Join(tmp, arch)
		cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w -buildid=", "-o", out, "./server")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch, "GOFLAGS=")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			log.Fatal(err)
		}
		bin, _ := os.ReadFile(out)
		var gz bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
		zw.Write(bin)
		zw.Close()
		if err := os.WriteFile(filepath.Join("bin", "xu-static-linux-"+arch+".gz"), gz.Bytes(), 0o644); err != nil {
			log.Fatal(err)
		}
		log.Printf("bin/xu-static-linux-%s.gz: %d KB (%d KB unpacked)", arch, gz.Len()>>10, len(bin)>>10)
	}
	v, _ := exec.Command("go", "env", "GOVERSION").Output()
	os.WriteFile(filepath.Join("bin", "VERSION"), []byte(strings.TrimSpace(string(v))+"\n"), 0o644)
}
