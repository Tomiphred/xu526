// Package version is the xu526 release, set at build time:
//
//	go build -ldflags "-X xu526/internal/version.Version=1.2.0"
package version

// Version is "dev" for builds from a source checkout.
var Version = "dev"
