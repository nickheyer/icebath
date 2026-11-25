package pm

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"

	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Joined parts made absolute, left joined when the working directory is gone, with the normcase form
func sitePath(parts ...string) (string, string) {
	dir := pathutil.Join(parts[0], parts[1:]...)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir, pathutil.FoldCase(dir)
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
	encoded, err := charsetutil.Encode(text, charsetutil.Locale())
	return string(encoded), err == nil
}

// Python exception a pth file whose bytes no fallback codec decodes raises out of site processing
func pthDecodeError(err error) error {
	var decode *charsetutil.DecodeError
	if errors.As(err, &decode) {
		return &ClassError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	if errors.Is(err, charsetutil.ErrUnknown) {
		return &ClassError{Class: "LookupError", Message: err.Error()}
	}
	return err
}

// Appends the existing directories one pth file of sitedir names, import statement lines adding no entry
func addSitePackage(entries []string, sitedir, name string, known map[string]bool) ([]string, error) {
	if known == nil {
		known = knownSitePaths(entries)
	}
	fullname := pathutil.Join(sitedir, name)
	st, err := os.Lstat(fullname)
	if err != nil || pthHidden(st) {
		return entries, nil
	}
	raw, err := os.ReadFile(fullname)
	if err != nil {
		return entries, nil
	}
	content, err := charsetutil.Decode(raw, "utf-8-sig")
	if err != nil {
		if content, err = charsetutil.Decode(raw, charsetutil.Locale()); err != nil {
			return entries, pthDecodeError(err)
		}
	}
	for _, line := range textutil.Lines(content) {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" || strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "import\t") {
			continue
		}
		path, ok := fsEncodeLine(strings.TrimRightFunc(line, unicode.IsSpace))
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
