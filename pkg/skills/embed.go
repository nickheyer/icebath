package skills

import (
	"io/fs"

	skilldata "github.com/nickheyer/icebath/skills"
)

// Bundled skills tree rooted at the category directories
func BundledFS() fs.FS {
	sub, err := fs.Sub(skilldata.Bundled, "bundled")
	if err != nil {
		panic("skills: bundled tree missing from the binary")
	}
	return sub
}

// Optional skills tree rooted at the category directories
func OptionalFS() fs.FS {
	sub, err := fs.Sub(skilldata.Optional, "optional")
	if err != nil {
		panic("skills: optional tree missing from the binary")
	}
	return sub
}
