package pm

import (
	"bytes"
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
)

// Characters Python str patterns match with a backslash s class
const pyWhitespaceClass = `\t\n\x0b\x0c\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	failedInstallPrefix = regexp.MustCompile(`(?i)failed to install: `)
	corruptCauses       = []string{"the wheel is invalid", "`record` file is invalid"}
	missingArchive      = regexp.MustCompile("(?i)archive-v0[\\\\/][^" + pyWhitespaceClass + "`'\"]+.*\\(os error [23]\\)")
	pep508Name          = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	distInfoName        = regexp.MustCompile(`^(.+?)-[^-]+\.dist-info$`)
	pep503Runs          = regexp.MustCompile(`[-_.]+`)
	wheelVersion        = regexp.MustCompile(`(?m)^[wW][hH][eE][eE][lL]-[vV][eE][rR][sS][iI][oO][nN][ \t]*:`)
)

const (
	cacheHeadChunk = 65536
	cacheLockWait  = 10 * time.Second
)

// Checked dist-info files with the header each must carry
var cacheFiles = []struct {
	name   string
	header *regexp.Regexp
}{{"WHEEL", wheelVersion}, {"METADATA", nil}, {"RECORD", nil}}

// PEP 503 normal form, the spelling uv's cache uses
func NormalizePackageName(name string) string {
	return strings.ToLower(pep503Runs.ReplaceAllString(name, "-"))
}

// Normalized name of a valid PEP 508 project name, empty otherwise
func validPackageName(name string) string {
	if pep508Name.MatchString(name) {
		return NormalizePackageName(name)
	}
	return ""
}

// Reports a character Python str patterns count as a word character, a letter, a number or underscore
func pyIsWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// Lowercases with Python str lower full mapping, dotted capital I becoming i and a combining dot
func pyLower(s string) string { return strings.ToLower(strings.ReplaceAll(s, "\u0130", "i\u0307")) }

// Wheel names uv's install step reports, the shortest run ending in a whl suffix at a word boundary
func failedWheels(output string) []string {
	var wheels []string
	for pos := 0; pos < len(output); {
		loc := failedInstallPrefix.FindStringIndex(output[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[1]
		if end := wheelEnd(output, start); end >= 0 {
			wheels = append(wheels, output[start:end])
			pos = end
			continue
		}
		pos += loc[0] + 1
	}
	return wheels
}

// End of the shortest wheel name from start whose suffix sits at a word boundary, minus one when none fits
func wheelEnd(text string, start int) int {
	for i := start; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if pyIsSpace(r) || r == '(' || r == ')' {
			return -1
		}
		i += size
		if len(text)-i >= 4 && strings.EqualFold(text[i:i+4], ".whl") {
			after, _ := utf8.DecodeRuneInString(text[i+4:])
			if i+4 == len(text) || !pyIsWord(after) {
				return i + 4
			}
		}
	}
	return -1
}

// Packages uv failed to install because their cached copy is corrupt, empty for any other failure
func FailedCachedPackages(output string) map[string]bool {
	lowered := pyLower(output)
	corrupt := missingArchive.MatchString(output)
	for _, cause := range corruptCauses {
		if strings.Contains(lowered, cause) {
			corrupt = true
		}
	}
	names := map[string]bool{}
	if !corrupt {
		return names
	}
	for _, wheel := range failedWheels(output) {
		head, _, _ := strings.Cut(wheel, "-")
		if name := validPackageName(head); name != "" {
			names[name] = true
		}
	}
	return names
}

// Reports positive evidence of a torn write, empty, a NUL byte anywhere, or the header never seen
func streamDamaged(r io.Reader, header *regexp.Regexp) (bool, error) {
	seen := header == nil
	var carry []byte
	empty := true
	buf := make([]byte, cacheHeadChunk)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			empty = false
			chunk := buf[:n]
			if bytes.IndexByte(chunk, 0) >= 0 {
				return true, nil
			}
			if !seen {
				window := append(append([]byte{}, carry...), chunk...)
				seen = header.Match(window)
				carry = window[max(0, len(window)-64):]
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return false, err
		}
	}
	return empty || !seen, nil
}

// Reports a missing or torn file, a link or an unreadable file counting as healthy
func cacheFileDamaged(path string, header *regexp.Regexp) bool {
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
	damaged, err := streamDamaged(f, header)
	if err != nil {
		return false
	}
	return damaged
}

// Package name of a dist-info directory, empty when it is not one
func distName(info string) string {
	m := distInfoName.FindStringSubmatch(info)
	if m == nil {
		return ""
	}
	return validPackageName(m[1])
}

// The cache's archive-v0 when it is a real directory, empty otherwise
func archiveRoot(cache string) string {
	archive := filepath.Join(cache, "archive-v0")
	if st, err := os.Lstat(archive); err != nil || st.Mode()&fs.ModeSymlink != 0 || IsJunction(archive) {
		return ""
	}
	if st, err := os.Stat(archive); err != nil || !st.IsDir() {
		return ""
	}
	return archive
}

// Unpacked cache entries uv cannot install from, package to archive entry names, read only
func CorruptCacheEntries(cache string) map[string][]string {
	found := map[string][]string{}
	archive := archiveRoot(cache)
	if archive == "" {
		return found
	}
	buckets, err := os.ReadDir(archive)
	if err != nil {
		return found
	}
	for _, bucket := range buckets {
		bucketPath := filepath.Join(archive, bucket.Name())
		if st, err := os.Lstat(bucketPath); err != nil || st.Mode()&fs.ModeSymlink != 0 || IsJunction(bucketPath) || !st.IsDir() {
			continue
		}
		entries, err := os.ReadDir(bucketPath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".dist-info") || entry.Type()&fs.ModeSymlink != 0 || !entry.IsDir() {
				continue
			}
			name := distName(entry.Name())
			if name == "" {
				continue
			}
			for _, file := range cacheFiles {
				if cacheFileDamaged(filepath.Join(bucketPath, entry.Name(), file.name), file.header) {
					found[name] = append(found[name], bucket.Name())
					break
				}
			}
		}
	}
	return found
}

// Packages with at least one corrupt unpacked cache entry
func CorruptCachedPackages(cache string) map[string]bool {
	out := map[string]bool{}
	for name := range CorruptCacheEntries(cache) {
		out[name] = true
	}
	return out
}

// Deletes the corrupt archive entries of names that uv cache clean left behind, under uv's cache lock
func RemoveCorruptEntries(cache string, names map[string]bool) error {
	archive := archiveRoot(cache)
	if len(names) == 0 || runtime.GOOS == "windows" || archive == "" {
		return nil
	}
	lock, err := os.OpenFile(filepath.Join(cache, ".lock"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	defer lock.Close()
	held, err := LockFD(lock, true, cacheLockWait)
	if err != nil || !held {
		return err
	}
	for pkg, buckets := range CorruptCacheEntries(cache) {
		if names[pkg] {
			for _, bucket := range buckets {
				removeTreeQuiet(filepath.Join(archive, bucket))
			}
		}
	}
	return nil
}
