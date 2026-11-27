package packages

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/fsx"
)

var (
	failedInstall  = regexp.MustCompile(`(?i)failed to install: `)
	missingArchive = regexp.MustCompile(`(?i)archive-v0[\\/][^\s` + "`" + `'"]+.*\(os error [23]\)`)
	projectName    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	distInfo       = regexp.MustCompile(`^(.+?)-[^-]+\.dist-info$`)
	pep503         = regexp.MustCompile(`[-_.]+`)
	wheelVersion   = regexp.MustCompile(`(?m)^[wW][hH][eE][eE][lL]-[vV][eE][rR][sS][iI][oO][nN][ \t]*:`)
)

// Files of a cached dist-info folder checked for a torn write, with the header the first must carry
var cacheFiles = []struct {
	name   string
	header *regexp.Regexp
}{{"WHEEL", wheelVersion}, {"METADATA", nil}, {"RECORD", nil}}

// Normal form of a package name, the spelling uv's cache uses
func normalName(name string) string {
	return strings.ToLower(pep503.ReplaceAllString(name, "-"))
}

// Packages uv could not install because their cached copy is corrupt, none for any other failure
func failedCached(output string) map[string]bool {
	names := map[string]bool{}
	lowered := strings.ToLower(output)
	if !missingArchive.MatchString(output) && !strings.Contains(lowered, "the wheel is invalid") && !strings.Contains(lowered, "`record` file is invalid") {
		return names
	}
	for pos := 0; pos < len(output); {
		loc := failedInstall.FindStringIndex(output[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[1]
		end := wheelEnd(output, start)
		if end < 0 {
			pos += loc[0] + 1
			continue
		}
		head, _, _ := strings.Cut(output[start:end], "-")
		if projectName.MatchString(head) {
			names[normalName(head)] = true
		}
		pos = end
	}
	return names
}

// End of the shortest wheel file name from start, minus one when none ends at a word boundary before a space or parenthesis
func wheelEnd(text string, start int) int {
	for i := start; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if unicode.IsSpace(r) || r == '(' || r == ')' {
			return -1
		}
		i += size
		if len(text)-i >= 4 && strings.EqualFold(text[i:i+4], ".whl") {
			after, _ := utf8.DecodeRuneInString(text[i+4:])
			if i+4 == len(text) || !(after == '_' || unicode.IsLetter(after) || unicode.IsNumber(after)) {
				return i + 4
			}
		}
	}
	return -1
}

// The cache's unpacked archive folder when it is a real folder, empty otherwise
func archiveRoot(cache string) string {
	archive := filepath.Join(cache, "archive-v0")
	if st, err := os.Lstat(archive); err != nil || !st.IsDir() || junction(archive) {
		return ""
	}
	return archive
}

// Unpacked cache entries uv cannot install from, by package; read only
func corruptEntries(cache string) map[string][]string {
	found := map[string][]string{}
	archive := archiveRoot(cache)
	if archive == "" {
		return found
	}
	buckets, _ := os.ReadDir(archive)
	for _, bucket := range buckets {
		dir := filepath.Join(archive, bucket.Name())
		if !bucket.IsDir() || bucket.Type()&fs.ModeSymlink != 0 || junction(dir) {
			continue
		}
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			m := distInfo.FindStringSubmatch(entry.Name())
			if m == nil || !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 || !projectName.MatchString(m[1]) {
				continue
			}
			for _, file := range cacheFiles {
				if torn(filepath.Join(dir, entry.Name(), file.name), file.header) {
					name := normalName(m[1])
					found[name] = append(found[name], bucket.Name())
					break
				}
			}
		}
	}
	return found
}

// Packages with a corrupt unpacked cache entry
func corruptCached(cache string) map[string]bool {
	out := map[string]bool{}
	for name := range corruptEntries(cache) {
		out[name] = true
	}
	return out
}

// Whether a cached file shows a torn write: missing, empty, holding a NUL byte, or never carrying its header; a link or an unreadable file
// counts as whole
func torn(path string, header *regexp.Regexp) bool {
	if st, err := os.Lstat(path); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		return false
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	defer f.Close()
	seen, empty := header == nil, true
	var carry []byte
	buf := make([]byte, 65536)
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			empty = false
			data := buf[:n]
			if bytes.IndexByte(data, 0) >= 0 {
				return true
			}
			if !seen {
				window := append(carry, data...)
				seen = header.Match(window)
				carry = bytes.Clone(window[max(0, len(window)-64):])
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return false
		}
	}
	return empty || !seen
}

// Deletes the corrupt entries of the packages that uv's cache clean left, under uv's cache lock
func removeCorrupt(cache string, names []string) error {
	archive := archiveRoot(cache)
	if len(names) == 0 || runtime.GOOS == "windows" || archive == "" {
		return nil
	}
	release, err := fsx.Lock(context.Background(), filepath.Join(cache, ".lock"), 10*time.Second)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	for name, buckets := range corruptEntries(cache) {
		if wanted[name] {
			for _, bucket := range buckets {
				removeQuiet(filepath.Join(archive, bucket))
			}
		}
	}
	return nil
}
