// Package plugintree carries the upstream bundled plugin files that the bundled plugin readers consult
package plugintree

import (
	"embed"
	"io/fs"

	"github.com/nickheyer/icebath/pkg/tarzst"
)

//go:embed all:plugins
var compiled embed.FS

//go:embed catalog.tar.zst
var catalogArchive []byte

// Renamed catalog plugin trees at their catalog pins, one directory per catalog name
var catalogTrees = tarzst.NewFS(catalogArchive, "")

// Upstream bundled plugins tree carried inside the binary, rooted where the shipped plugins directory is
func Compiled() fs.FS {
	sub, err := fs.Sub(compiled, "plugins")
	if err != nil {
		panic("plugintree: plugins tree missing from the binary")
	}
	return sub
}

// Tree a catalog install of the named plugin publishes at its catalog pin, false when none is compiled in
func CatalogTree(name string) (fs.FS, bool) {
	if !fs.ValidPath(name) || name == "." {
		return nil, false
	}
	if st, err := fs.Stat(catalogTrees, name); err != nil || !st.IsDir() {
		return nil, false
	}
	sub, err := fs.Sub(catalogTrees, name)
	if err != nil {
		return nil, false
	}
	return sub, true
}
