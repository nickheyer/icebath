package pm

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// File names MakeAppx refuses anywhere in an MSIX payload, lower cased
var msixReservedNames = []string{"[content_types].xml"}

// Trees no runtime code reads that carry a reserved name, as lower cased path parts
var msixDeadTrees = [][]string{{"docx", "templates", "default-docx-template"}}

// Removes the dead trees holding reserved names, returning them relative to root, refusing any other reserved file
func dropMSIXReservedNames(out string) ([]string, error) {
	var removed, offending []string
	var failure error
	pywalk(out, func(directory string, dirs, names []string) []string {
		if failure != nil {
			return nil
		}
		var reserved []string
		for _, name := range names {
			if slices.Contains(msixReservedNames, strings.ToLower(name)) {
				reserved = append(reserved, name)
			}
		}
		if len(reserved) == 0 {
			return dirs
		}
		relative, _ := filepath.Rel(out, directory)
		var parts []string
		if relative != "." {
			parts = strings.Split(relative, string(filepath.Separator))
		}
		tree := msixDeadTree(parts)
		if tree == "" {
			for _, name := range reserved {
				offending = append(offending, filepath.ToSlash(filepath.Join(relative, name)))
			}
			return dirs
		}
		if err := pyfs.RmTree(filepath.Join(out, tree), nil); err != nil {
			failure = err
			return nil
		}
		removed = append(removed, tree)
		if tree == relative {
			return nil
		}
		return dirs
	})
	if failure != nil {
		return nil, failure
	}
	if len(offending) > 0 {
		sort.Strings(offending)
		return nil, NewInstallError("msix", "MSIX reserves [Content_Types].xml; MakeAppx cannot pack "+strings.Join(offending, ", "),
			"drop the tree in DEAD_TREES if no runtime code reads it")
	}
	return removed, nil
}

// Leading path through the dead tree entry that contains parts, empty when none does
func msixDeadTree(parts []string) string {
	folded := make([]string, len(parts))
	for i, part := range parts {
		folded[i] = strings.ToLower(part)
	}
	for _, tree := range msixDeadTrees {
		for start := 0; start <= len(folded)-len(tree); start++ {
			if slices.Equal(folded[start:start+len(tree)], tree) {
				return filepath.Join(parts[:start+len(tree)]...)
			}
		}
	}
	return ""
}
