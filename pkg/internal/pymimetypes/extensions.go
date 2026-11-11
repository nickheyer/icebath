package pymimetypes

import (
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Extensions known for the lowercased type, strict ones first then any non strict extras
func (d *DB) GuessAllExtensions(typ string, strict bool) []string {
	typ = pystr.Lower(typ)
	extensions := slices.Clone(d.typesInv[1][typ])
	if !strict {
		for _, ext := range d.typesInv[0][typ] {
			if !slices.Contains(extensions, ext) {
				extensions = append(extensions, ext)
			}
		}
	}
	return extensions
}

// First known extension of the type, empty standing for None
func (d *DB) GuessExtension(typ string, strict bool) string {
	if extensions := d.GuessAllExtensions(typ, strict); len(extensions) > 0 {
		return extensions[0]
	}
	return ""
}

// mimetypes.guess_extension on the module database, empty standing for None
func GuessExtension(typ string, strict bool) string {
	return Default().GuessExtension(typ, strict)
}
