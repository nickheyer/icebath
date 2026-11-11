package desktopplugins

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	PackageMarker = ".hermes-package.json"
	TreeMaxBytes  = 64 * 1024 * 1024
	DirName       = "desktop-plugins"
)

var rootLayoutSkip = []string{".git", "__pycache__"}

var gitOriginRE = regexp.MustCompile(`\[remote "origin"\][^\[]*?url\s*=\s*(\S+)`)

// Serializes every writer of the app level root across the server's goroutines
var RootWriter sync.Mutex

func logger() *slog.Logger { return slog.Default().With("logger", "hermes_cli.desktop_plugin_halves") }

// Tree refused for publication, a link inside it or an oversized tree
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string      { return e.Reason }
func (e *RefusedError) PyTypeName() string { return "OSError" }

// App level desktop-plugins folder under the root home, created on demand and resolved
func Root() (string, error) {
	root := filepath.Join(home.Root(), DirName)
	if err := os.MkdirAll(root, 0o777); err != nil {
		return "", err
	}
	return pypath.Resolve(root)
}

// Junction probe of the host, the pm.filesystem attribute hermes tests patch
var isJunction = pm.IsJunction

// True for a symlink or a Windows junction
func isLink(path string) bool {
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return true
	}
	return isJunction(path)
}

// Folder names in folder, links to folders included only when followLinks
func listDirs(folder string, followLinks bool) []string {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
			continue
		}
		if followLinks && e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(folder, e.Name())); err == nil && st.IsDir() {
				names = append(names, e.Name())
			}
		}
	}
	return names
}

// JSON of a file read as utf-8-sig, an error for unreadable or undecodable text
func readJSONSig(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// Origin of the package, the catalog sidecar first and then the git remote
func packageOrigin(packageDir string) (*pyjson.Object, error) {
	if v, err := readJSONSig(filepath.Join(packageDir, ".hermes-catalog.json")); err == nil {
		if sidecar, ok := v.(*pyjson.Object); ok && truthy(sidecar.GetOr("repo", nil)) {
			origin := pyjson.NewObject()
			for _, kv := range [][2]string{{"catalogName", "catalog_name"}, {"repo", "repo"}, {"sha", "sha"}} {
				if val := sidecar.GetOr(kv[1], nil); val != nil {
					origin.Set(kv[0], val)
				}
			}
			return origin, nil
		}
	}
	raw, err := os.ReadFile(filepath.Join(packageDir, ".git", "config"))
	if err != nil {
		return pyjson.NewObject(), nil
	}
	text, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil, err
	}
	if m := gitOriginRE.FindStringSubmatch(text); m != nil {
		return pyjson.ObjectOf("repo", m[1]), nil
	}
	return pyjson.NewObject(), nil
}

// Python truthiness of a decoded JSON value
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *pyjson.Object:
		return x.Len() > 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return true
}

// Modification time in milliseconds as Node computes mtimeMs
func MtimeMs(st fs.FileInfo) float64 {
	ns := st.ModTime().UnixNano()
	seconds, nanoseconds := ns/1_000_000_000, ns%1_000_000_000
	if nanoseconds < 0 {
		seconds, nanoseconds = seconds-1, nanoseconds+1_000_000_000
	}
	return float64(seconds)*1e3 + float64(nanoseconds)/1e6
}

// Package marker of a folder, nil when absent or lacking its package and source
func ReadPackageMarker(folder string) *pyjson.Object {
	v, err := readJSONSig(filepath.Join(folder, PackageMarker))
	if err != nil {
		return nil
	}
	marker, ok := v.(*pyjson.Object)
	if !ok || !truthy(marker.GetOr("package", nil)) || !truthy(marker.GetOr("source", nil)) {
		return nil
	}
	switch marker.GetOr("sourceMtimeMs", nil).(type) {
	case int64, float64:
	default:
		marker.Set("sourceMtimeMs", int64(-1))
	}
	return marker
}

// Writes the marker as indented JSON text with a trailing newline
func writeMarker(folder string, marker *pyjson.Object) error {
	text, err := pyjson.Dumps(marker, pyjson.Indent(2))
	if err != nil {
		return err
	}
	text += "\n"
	if runtime.GOOS == "windows" {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	return os.WriteFile(filepath.Join(folder, PackageMarker), []byte(text), 0o666)
}

// True when both paths hold the same bytes, false when either cannot be read
func sameFile(a, b string) bool {
	x, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	y, err := os.ReadFile(b)
	return err == nil && string(x) == string(y)
}

// True when path is a regular file after following links
func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Folder holding the package's desktop plugin, the root with a plugin.js else desktop, empty when neither
func DesktopSource(packageDir string) string {
	for _, source := range []string{packageDir, filepath.Join(packageDir, "desktop")} {
		if isFile(filepath.Join(source, "plugin.js")) {
			return source
		}
	}
	return ""
}

// Ignore rule of a root layout half, nil for a desktop folder copied whole
func RootLayoutIgnore(sourceDir, packageDir string) func(dir string, names []string) []string {
	if sourceDir == packageDir {
		return pyshutil.IgnorePatterns(rootLayoutSkip...)
	}
	return nil
}

// Relative display path of entry under base
func relTo(entry, base string) string {
	rel, err := filepath.Rel(base, entry)
	if err != nil {
		return entry
	}
	return rel
}

// Reason the tree cannot be published as a desktop plugin, empty when it is a plain tree under the cap
func CheckDesktopTree(source, base string, linkedRoot bool, skip []string) (string, error) {
	if isLink(source) && !linkedRoot {
		return fmt.Sprintf("The desktop plugin contains a link (%s); links are not installed.", relTo(source, base)), nil
	}
	var total int64
	var walk func(folder string) (string, error)
	walk = func(folder string) (string, error) {
		entries, err := os.ReadDir(folder)
		if err != nil {
			return "", nil
		}
		var dirs, files []string
		for _, e := range entries {
			p := filepath.Join(folder, e.Name())
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				skipped := false
				for _, s := range skip {
					if e.Name() == s {
						skipped = true
					}
				}
				if !skipped {
					dirs = append(dirs, e.Name())
				}
			} else {
				files = append(files, e.Name())
			}
		}
		for _, n := range append(append([]string{}, dirs...), files...) {
			if entry := filepath.Join(folder, n); isLink(entry) {
				return fmt.Sprintf("The desktop plugin contains a link (%s); links are not installed.", relTo(entry, base)), nil
			}
		}
		for _, n := range files {
			st, err := os.Stat(filepath.Join(folder, n))
			if err != nil {
				return "", err
			}
			total += st.Size()
		}
		if total > TreeMaxBytes {
			return fmt.Sprintf("The desktop plugin is larger than %d MB.", TreeMaxBytes/(1024*1024)), nil
		}
		for _, d := range dirs {
			if reason, err := walk(filepath.Join(folder, d)); reason != "" || err != nil {
				return reason, err
			}
		}
		return "", nil
	}
	return walk(source)
}

// Copies through a staging sibling and renames the finished tree into place, marker included
func PublishDesktopTree(sourceDir, target string, marker *pyjson.Object, ignore func(dir string, names []string) []string) error {
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(target)+".staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	staged := filepath.Join(staging, filepath.Base(target))
	if ignore == nil {
		ignore = func(string, []string) []string { return nil }
	}
	if err := pyshutil.CopyTreeIgnore(sourceDir, staged, true, false, ignore); err != nil {
		return err
	}
	if marker == nil {
		if err := os.Remove(filepath.Join(staged, PackageMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	} else if err := writeMarker(staged, marker); err != nil {
		return err
	}
	if isLink(target) {
		if err := os.Remove(target); err != nil {
			return err
		}
	} else if _, err := os.Stat(target); err == nil {
		if err := tools.RmtreeReadonly(target, false); err != nil {
			return err
		}
	}
	return os.Rename(staged, target)
}

// True for a missing path or a component that is not a directory
func isMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// Source dir and plugin.js info of the package's desktop half, false when none or unreadable
func desktopEntry(packageDir, name string) (string, fs.FileInfo, bool) {
	for _, sourceDir := range []string{packageDir, filepath.Join(packageDir, "desktop")} {
		st, err := os.Stat(filepath.Join(sourceDir, "plugin.js"))
		if err != nil {
			if isMissing(err) {
				continue
			}
			logger().Warn(fmt.Sprintf("desktop-plugins: cannot read %s: %s", name, pypath.OSErrorText(err)))
			return "", nil, false
		}
		if st.Mode().IsRegular() {
			return sourceDir, st, true
		}
	}
	return "", nil, false
}

// Number of a marker field, int or float
func markerNumber(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

// Copies the package's desktop half to the app root folder of its name, empty when current, absent or user owned
func MaterializeDesktopHalf(packageDir, appRoot, name string) (string, error) {
	sourceDir, entryStat, ok := desktopEntry(packageDir, name)
	if !ok {
		return "", nil
	}
	entry := filepath.Join(sourceDir, "plugin.js")
	target := filepath.Join(appRoot, name)
	existing := ReadPackageMarker(target)
	linked := isLink(packageDir)
	targetIsLink := isLink(target)
	if targetIsLink && existing == nil {
		return "", nil
	}
	marker := pyjson.ObjectOf("package", name, "source", sourceDir, "sourceMtimeMs", MtimeMs(entryStat))
	if linked {
		marker.Set("linked", true)
	}
	origin, err := packageOrigin(packageDir)
	if err != nil {
		return "", err
	}
	marker.Update(origin)
	if _, err := os.Stat(target); err == nil {
		if _, perr := os.Stat(filepath.Join(target, "plugin.js")); existing == nil && perr == nil {
			if sameFile(filepath.Join(target, "plugin.js"), entry) {
				if err := writeMarker(target, marker); err != nil {
					return "", err
				}
				return target, nil
			}
			return "", nil
		}
		if existing != nil && !targetIsLink && pyjson.Equal(existing.GetOr("source", nil), sourceDir) &&
			truthy(existing.GetOr("linked", nil)) == linked {
			current := false
			if linked {
				current = sameFile(filepath.Join(target, "plugin.js"), entry)
			} else {
				current = markerNumber(existing.GetOr("sourceMtimeMs", int64(0))) >= markerNumber(marker.Vals["sourceMtimeMs"])
			}
			if current {
				return "", nil
			}
		}
	}
	ignore := RootLayoutIgnore(sourceDir, packageDir)
	if ignore != nil || !linked {
		var skip []string
		if ignore != nil {
			skip = rootLayoutSkip
		}
		refused, err := CheckDesktopTree(sourceDir, packageDir, linked, skip)
		if err != nil {
			return "", err
		}
		if refused != "" {
			return "", &RefusedError{refused}
		}
	}
	if err := PublishDesktopTree(sourceDir, target, marker, ignore); err != nil {
		return "", err
	}
	return target, nil
}

// True only when the source plugin.js is really gone, an unreadable one kept
func sourceGone(name, entry string) bool {
	_, err := os.Stat(entry)
	if err == nil {
		return false
	}
	if isMissing(err) {
		return true
	}
	logger().Warn(fmt.Sprintf("desktop-plugins: keeping desktop half of unreadable package %s: %s", name, pypath.OSErrorText(err)))
	return false
}

// Python str of a materialize failure, the refusal reason or the OSError text
func errorText(err error) string {
	var refused *RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return pypath.OSErrorText(err)
}

// Folders the reconcile pass wrote or removed
func ReconcileDesktopHalves(hermesRoot, appRoot string) ([]string, error) {
	touched, _, err := reconcile(hermesRoot, appRoot)
	return touched, err
}

// Reconcile result naming the folders touched and each package whose half was refused and not later published
func ReconcileDesktopHalvesReport(hermesRoot, appRoot string) (*pyjson.Object, error) {
	touched, refused, err := reconcile(hermesRoot, appRoot)
	if err != nil {
		return nil, err
	}
	t := []any{}
	for _, s := range touched {
		t = append(t, s)
	}
	return pyjson.ObjectOf("touched", t, "refused", refused), nil
}

// Materializes every local home's halves first home first and prunes copies whose package is gone
func reconcile(hermesRoot, appRoot string) ([]string, []any, error) {
	RootWriter.Lock()
	defer RootWriter.Unlock()
	touched := []string{}
	type refusal struct{ name, reason string }
	var refusedList []refusal
	seen := map[string]bool{}
	homes := []string{hermesRoot}
	for _, name := range listDirs(filepath.Join(hermesRoot, "profiles"), false) {
		homes = append(homes, filepath.Join(hermesRoot, "profiles", name))
	}
	for _, h := range homes {
		pluginsRoot := filepath.Join(h, "plugins")
		for _, name := range listDirs(pluginsRoot, true) {
			if seen[name] {
				continue
			}
			result, err := MaterializeDesktopHalf(filepath.Join(pluginsRoot, name), appRoot, name)
			var decodeErr *pycodecs.UnicodeDecodeError
			if errors.As(err, &decodeErr) {
				return nil, nil, err
			}
			if err != nil {
				reason := errorText(err)
				logger().Warn(fmt.Sprintf("desktop-plugins: skipping package %s: %s", name, reason))
				refusedList = append(refusedList, refusal{name, reason})
				continue
			}
			if result != "" || DesktopSource(filepath.Join(pluginsRoot, name)) != "" {
				seen[name] = true
			}
			if result != "" {
				touched = append(touched, result)
			}
		}
	}
	for _, name := range listDirs(appRoot, true) {
		folder := filepath.Join(appRoot, name)
		marker := ReadPackageMarker(folder)
		if marker == nil {
			continue
		}
		source, _ := marker.GetOr("source", "").(string)
		if sourceGone(name, filepath.Join(source, "plugin.js")) {
			if isLink(folder) {
				if err := os.Remove(folder); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return nil, nil, err
				}
			} else {
				_ = os.RemoveAll(folder)
			}
			touched = append(touched, folder)
		}
	}
	refused := []any{}
	for _, r := range refusedList {
		if !seen[r.name] {
			refused = append(refused, pyjson.ObjectOf("name", r.name, "reason", r.reason))
		}
	}
	return touched, refused, nil
}
