package pm

import (
	"errors"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Joined parts made absolute, left joined when the working directory is gone, with the normcase form
func sitePath(parts ...string) (string, string) {
	dir := pypath.Join(parts[0], parts[1:]...)
	if abs, err := pypath.Abspath(dir); err == nil {
		dir = abs
	}
	return dir, pypath.Normcase(dir)
}

// Normcase forms of the import path entries that exist on disk
func knownSitePaths(entries []string) map[string]bool {
	known := map[string]bool{}
	for _, item := range entries {
		if lexistsFollow(item) {
			_, itemcase := sitePath(item)
			known[itemcase] = true
		}
	}
	return known
}

// Path bytes the filesystem encoding gives a decoded pth line, false when that encoding cannot spell it
func fsEncodeLine(text string) (string, bool) {
	switch runtime.GOOS {
	case "windows", "darwin", "android":
		return text, true
	}
	if isASCII(text) {
		return text, true
	}
	encoded, err := pycodecs.Encode(text, pycodecs.LocaleEncoding())
	return encoded, err == nil
}

// Python exception a pth file whose bytes no fallback codec decodes raises out of site processing
func pthDecodeError(err error) error {
	var decode *pycodecs.UnicodeDecodeError
	if errors.As(err, &decode) {
		return &PyError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	var lookup *pycodecs.LookupError
	if errors.As(err, &lookup) {
		return &PyError{Class: "LookupError", Message: err.Error()}
	}
	return err
}

// Appends the existing directories one pth file of sitedir names, import statement lines adding no entry
func addSitePackage(entries []string, sitedir, name string, known map[string]bool) ([]string, error) {
	if known == nil {
		known = knownSitePaths(entries)
	}
	fullname := pypath.Join(sitedir, name)
	st, err := os.Lstat(fullname)
	if err != nil || pthHidden(st) {
		return entries, nil
	}
	raw, err := os.ReadFile(fullname)
	if err != nil {
		return entries, nil
	}
	content, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		if content, err = pycodecs.Decode(raw, pycodecs.GetEncoding()); err != nil {
			return entries, pthDecodeError(err)
		}
	}
	for _, line := range pystr.Splitlines(content, false) {
		if strings.HasPrefix(line, "#") || pystr.Strip(line) == "" || strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "import\t") {
			continue
		}
		path, ok := fsEncodeLine(pystr.Rstrip(line))
		if !ok {
			continue
		}
		dir, dircase := sitePath(sitedir, path)
		if !known[dircase] && lexistsFollow(dir) {
			entries = append(entries, dir)
			known[dircase] = true
		}
	}
	return entries, nil
}

// Appends sitedir when absent, then the directories its visible pth files name in sorted file order
func addSiteDir(entries []string, sitedir string, known map[string]bool) ([]string, error) {
	if known == nil {
		known = knownSitePaths(entries)
	}
	sitedir, sitedircase := sitePath(sitedir)
	if !known[sitedircase] {
		entries = append(entries, sitedir)
		known[sitedircase] = true
	}
	dir, err := os.Open(sitedir)
	if err != nil {
		return entries, nil
	}
	all, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return entries, nil
	}
	var names []string
	for _, name := range all {
		if strings.HasSuffix(name, ".pth") && !strings.HasPrefix(name, ".") {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return tarNameLess(names[i], names[j]) })
	for _, name := range names {
		if entries, err = addSitePackage(entries, sitedir, name, known); err != nil {
			return entries, err
		}
	}
	return entries, nil
}

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
