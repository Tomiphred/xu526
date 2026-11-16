// Package plugins lists the language plugins, in detection order.
package plugins

import (
	"xu526/internal/bundle"
	"xu526/internal/bundle/plugins/clang"
	"xu526/internal/bundle/plugins/golang"
	"xu526/internal/bundle/plugins/node"
	"xu526/internal/bundle/plugins/python"
	"xu526/internal/bundle/plugins/static"
)

// All returns every plugin, in detection order.
func All() []bundle.Plugin {
	return []bundle.Plugin{golang.Plugin{}, node.Plugin{}, python.Plugin{}, clang.Plugin{}, static.Plugin{}}
}
