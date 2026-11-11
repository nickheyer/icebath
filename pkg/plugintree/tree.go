// Package plugintree carries the upstream bundled plugin files that the bundled plugin readers consult
package plugintree

import (
	"embed"
	"io/fs"
)

//go:embed all:plugins
var compiled embed.FS

// Upstream bundled plugins tree carried inside the binary, rooted where the shipped plugins directory is
func Compiled() fs.FS {
	sub, err := fs.Sub(compiled, "plugins")
	if err != nil {
		panic("plugintree: plugins tree missing from the binary")
	}
	return sub
}
