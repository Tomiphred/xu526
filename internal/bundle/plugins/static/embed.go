package static

import (
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"strings"
)

//go:generate go run ./gen

//go:embed bin/*.gz bin/VERSION
var bins embed.FS

// ServerBinary returns the xu-static binary for an architecture.
func ServerBinary(arch string) ([]byte, error) {
	gz, err := bins.ReadFile("bin/xu-static-linux-" + arch + ".gz")
	if err != nil {
		return nil, fmt.Errorf("no static file server built for %s", arch)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

// ServerVersion is the Go version the embedded servers were built with.
func ServerVersion() string {
	b, _ := bins.ReadFile("bin/VERSION")
	return strings.TrimSpace(string(b))
}
