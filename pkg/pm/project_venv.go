package pm

import (
	"archive/zip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Parent of the resolved binary, the tree the running code was loaded from
var moduleDir = home.InstallRoot

// Venv prefix the running binary starts in, ok false outside any venv
var runningVenvPrefix = func() (string, bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	return venvPrefixOf(exe)
}

// Directory holding pyvenv.cfg one level above the resolved executable's directory, else beside it, like getpath
func venvPrefixOf(executable string) (string, bool, error) {
	resolved, err := pypath.Resolve(executable)
	if err != nil {
		return "", false, err
	}
	exeDir := filepath.Dir(resolved)
	for _, prefix := range []string{filepath.Dir(exeDir), exeDir} {
		_, err := os.ReadFile(filepath.Join(prefix, "pyvenv.cfg"))
		if err == nil {
			return prefix, true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) {
			return "", false, err
		}
	}
	return "", false, nil
}

// In tree venv or .venv of a checkout, venv winning, else the running venv when it installs this very checkout
func projectVenvDir(root string) (string, error) {
	for _, name := range []string{"venv", ".venv"} {
		if candidate := pypath.PathJoin(root, name); isDirectory(candidate) {
			return candidate, nil
		}
	}
	resolvedRoot, err := pypath.Resolve(root)
	if err != nil {
		return "", err
	}
	if !samePurePath(moduleDir(), resolvedRoot) {
		return "", nil
	}
	running, inVenv, err := runningVenvPrefix()
	if err != nil || !inVenv || !isRegularFile(VenvPython(running)) {
		return "", err
	}
	installs, err := venvInstallsCheckout(running, resolvedRoot)
	if err != nil || !installs {
		return "", err
	}
	return running, nil
}

// pathlib equality, the str form on POSIX and its lowercase on Windows
func samePurePath(a, b string) bool {
	a, b = pypath.PathStr(a), pypath.PathStr(b)
	if runtime.GOOS == "windows" {
		return pystr.Lower(a) == pystr.Lower(b)
	}
	return a == b
}

// Reports whether the venv's own icebath-agent distribution records the resolved root as its direct_url source tree
func venvInstallsCheckout(venv, resolvedRoot string) (bool, error) {
	site, err := SitePackages(venv)
	if err != nil {
		return false, err
	}
	for _, dist := range metadataDistributions(site, "icebath-agent") {
		raw, err := dist.readText("direct_url.json")
		if err != nil {
			var decodeErr *pystr.UnicodeDecodeError
			if errors.As(err, &decodeErr) {
				continue
			}
			return false, err
		}
		if raw == "" {
			raw = "{}"
		}
		value, err := pyjson.Loads(raw)
		if err != nil {
			var jsonErr *pyjson.DecodeError
			if errors.As(err, &jsonErr) {
				continue
			}
			return false, err
		}
		object, ok := value.(*pyjson.Object)
		if !ok {
			return false, &PyError{Class: "AttributeError", Message: "'" + pyrepr.TypeName(value) + "' object has no attribute 'get'"}
		}
		var url any = ""
		if got, ok := object.Get("url"); ok {
			url = got
		}
		text, ok := url.(string)
		if !ok {
			return false, &PyError{Class: "AttributeError", Message: "'" + pyrepr.TypeName(url) + "' object has no attribute 'startswith'"}
		}
		if !strings.HasPrefix(text, "file:") {
			continue
		}
		parsed, err := pyurl.Parse(text)
		if err != nil {
			return false, err
		}
		local, err := pyurl.URL2Pathname(parsed.Path, false, false)
		if err != nil {
			return false, err
		}
		source, err := pypath.Resolve(local)
		if err != nil {
			return false, err
		}
		if samePurePath(source, resolvedRoot) {
			return true, nil
		}
	}
	return false, nil
}

// One importlib.metadata PathDistribution, a directory or a member prefix inside a zip root
type metadataDistribution struct {
	dir     string
	archive string
	member  string
}

var metadataNameRuns = regexp.MustCompile(`[-_.]+`)

// Distributions importlib.metadata finds for a name on one path entry, metadata dirs first then legacy eggs
func metadataDistributions(root, name string) []metadataDistribution {
	normalized := strings.ReplaceAll(pystr.Lower(metadataNameRuns.ReplaceAllString(name, "-")), "-", "_")
	legacy := strings.ReplaceAll(pystr.Lower(name), "-", "_")
	children, archive := metadataChildren(root)
	base := pystr.Lower(pypath.Basename(root))
	var infos, eggs []metadataDistribution
	for _, child := range children {
		dist := metadataDistribution{dir: filepath.Join(root, child), archive: archive, member: child}
		low := pystr.Lower(child)
		if strings.HasSuffix(low, ".dist-info") || strings.HasSuffix(low, ".egg-info") {
			stem, _ := cutLast(low, ".")
			stem, _, _ = strings.Cut(stem, "-")
			if strings.ReplaceAll(pystr.Lower(metadataNameRuns.ReplaceAllString(stem, "-")), "-", "_") == normalized {
				infos = append(infos, dist)
			}
		} else if strings.HasSuffix(base, ".egg") && low == "egg-info" {
			stem, _ := cutLast(base, ".")
			stem, _, _ = strings.Cut(stem, "-")
			if strings.ReplaceAll(pystr.Lower(stem), "-", "_") == legacy {
				eggs = append(eggs, dist)
			}
		}
	}
	return append(infos, eggs...)
}

// Text before the last separator, the whole text empty when there is none like str.rpartition
func cutLast(s, sep string) (string, bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], true
	}
	return "", false
}

// Directory entries in listing order, else the top level names of a zip root, else nothing
func metadataChildren(root string) ([]string, string) {
	dir := root
	if dir == "" {
		dir = "."
	}
	if f, err := os.Open(dir); err == nil {
		names, err := f.Readdirnames(-1)
		f.Close()
		if err == nil {
			return names, ""
		}
	}
	reader, err := zip.OpenReader(root)
	if err != nil {
		return nil, ""
	}
	defer reader.Close()
	seen := map[string]bool{}
	var names []string
	for _, file := range reader.File {
		top, _, _ := strings.Cut(file.Name, "/")
		if !seen[top] {
			seen[top] = true
			names = append(names, top)
		}
	}
	return names, root
}

// Metadata file text decoded as strict utf-8 with universal newlines, empty when the file cannot be opened
func (d metadataDistribution) readText(filename string) (string, error) {
	var data []byte
	var err error
	if d.archive != "" {
		data, err = zipMemberBytes(d.archive, d.member+"/"+filename)
	} else {
		data, err = os.ReadFile(filepath.Join(d.dir, filename))
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENOTDIR) {
			return "", nil
		}
		return "", err
	}
	if !utf8.Valid(data) {
		return "", pystr.UTF8DecodeError(data, firstInvalidUTF8(data))
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n"), nil
}

// Offset of the first byte strict utf-8 rejects, encoded surrogates included
func firstInvalidUTF8(data []byte) int {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return len(data)
}

// Bytes of one zip member, not found for a missing name and is a directory for a directory entry
func zipMemberBytes(archive, name string) ([]byte, error) {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.Name == name+"/" || strings.HasPrefix(file.Name, name+"/") {
			return nil, syscall.EISDIR
		}
	}
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		member, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer member.Close()
		return io.ReadAll(member)
	}
	return nil, fs.ErrNotExist
}
