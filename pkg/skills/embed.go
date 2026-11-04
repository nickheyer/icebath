// Package skills ports the skills system, bundled skills, slash commands, hub, guard, and skill tools
package skills

import (
	"embed"
	"io/fs"
)

// Bundled holds every skill seeded into a home by default
//
//go:embed all:bundled
var bundledRoot embed.FS

// Optional holds the heavier official skills installed on demand through the hub
//
//go:embed all:optional
var optionalRoot embed.FS

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
