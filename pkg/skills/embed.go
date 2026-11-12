// Package skills ports the skills system, bundled skills, slash commands, hub, guard, and skill tools
package skills

import (
	_ "embed"
	"io/fs"

	"github.com/nickheyer/icebath/pkg/tarzst"
)

//go:generate go run ../../cmd/packtree -out bundled.tar.zst -manifest bundled_manifest.txt
//go:generate go run ../../cmd/packtree -out optional.tar.zst -manifest optional_manifest.txt

//go:embed bundled.tar.zst
var bundledArchive []byte

//go:embed optional.tar.zst
var optionalArchive []byte

// Bundled holds every skill seeded into a home by default
var bundledRoot = tarzst.NewFS(bundledArchive, "bundled")

// Optional holds the heavier official skills installed on demand through the hub
var optionalRoot = tarzst.NewFS(optionalArchive, "optional")

// Bundled skills tree rooted at the category directories
func BundledFS() fs.FS {
	sub, err := fs.Sub(bundledRoot, "bundled")
	if err != nil {
		panic("skills: bundled tree missing from the binary")
	}
	return sub
}

// Optional skills tree rooted at the category directories
func OptionalFS() fs.FS {
	sub, err := fs.Sub(optionalRoot, "optional")
	if err != nil {
		panic("skills: optional tree missing from the binary")
	}
	return sub
}
