//go:build !icebath_core && !icebath_noextras

package pm

import (
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Processes each pth file of the updated site dir whose name the previous site dir lacks, in path order
func addAdoptedSitePackages(entries []string, previous, updated string) ([]string, error) {
	old, err := pypath.Glob(previous, "*.pth")
	if err != nil {
		return entries, err
	}
	known := map[string]bool{}
	for _, pth := range old {
		known[pypath.PathName(pth)] = true
	}
	current, err := pypath.Glob(updated, "*.pth")
	if err != nil {
		return entries, err
	}
	names := make([]string, len(current))
	for i, pth := range current {
		names[i] = pypath.PathName(pth)
	}
	sortPathNames(names)
	for _, name := range names {
		if known[name] {
			continue
		}
		if entries, err = addSitePackage(entries, updated, name, nil); err != nil {
			return entries, err
		}
	}
	return entries, nil
}
