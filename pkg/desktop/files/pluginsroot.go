package files

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	DesktopPluginsDir   = "desktop-plugins"
	PackageMarker       = ".hermes-package.json"
	desktopTreeMaxBytes = 64 * 1024 * 1024
)

// Warning sink of the desktop plugin passes, the default logger in production
var warn = func(msg string) { slog.Warn(msg) }

// Tree copy of a publication, replaced only where a case makes the copy fail
var copyTreeFn = copyTree

var gitOriginRE = regexp.MustCompile(`\[remote "origin"\][^\[]*?url\s*=\s*(\S+)`)

// One writer lock per app root, every reconcile, install and remove of that root runs under it
var rootWriters = struct {
	sync.Mutex
	locks map[string]*sync.Mutex
}{locks: map[string]*sync.Mutex{}}

// Runs task once every earlier writer of the same app root has finished
func WithDesktopPluginsRoot[T any](root string, task func() (T, error)) (T, error) {
	key := resolvePath(root)
	rootWriters.Lock()
	lock := rootWriters.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		rootWriters.locks[key] = lock
	}
	rootWriters.Unlock()
	lock.Lock()
	defer lock.Unlock()
	return task()
}

// Creates dir when it can and answers it either way
func EnsureDir(dir string) string {
	os.MkdirAll(dir, 0o777)
	return dir
}

// Folder names in dir, links to folders included when followLinks
func listDirs(dir string, followLinks bool) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !isLinkMode(e.Type()) {
			names = append(names, e.Name())
			continue
		}
		if followLinks && isLinkMode(e.Type()) {
			if st, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && st.IsDir() {
				names = append(names, e.Name())
			}
		}
	}
	return names
}

// Every local icebath home, the default then each profile
func LocalHomes(hermesHome string) []string {
	homes := []string{hermesHome}
	for _, name := range listDirs(filepath.Join(hermesHome, "profiles"), false) {
		homes = append(homes, filepath.Join(hermesHome, "profiles", name))
	}
	return homes
}

// Moves every profile scoped desktop plugin folder into the app level root, a present one winning
func MigrateProfileScopedDesktopPlugins(hermesHome, appRoot string) []string {
	moved, _ := WithDesktopPluginsRoot(appRoot, func() ([]string, error) {
		var moved []string
		for _, profile := range listDirs(filepath.Join(hermesHome, "profiles"), false) {
			scoped := filepath.Join(hermesHome, "profiles", profile, DesktopPluginsDir)
			for _, entry := range listDirs(scoped, false) {
				from, to := filepath.Join(scoped, entry), filepath.Join(appRoot, entry)
				if _, err := os.Stat(to); err == nil {
					continue
				}
				if os.Rename(from, to) == nil {
					moved = append(moved, to)
				}
			}
			if rest, err := os.ReadDir(scoped); err == nil && len(rest) == 0 {
				os.Remove(scoped)
			}
		}
		return moved, nil
	})
	return moved
}

// Package marker of a folder, nil when absent, unreadable or without package and source
func readMarker(dir string) map[string]any {
	raw, err := os.ReadFile(filepath.Join(dir, PackageMarker))
	if err != nil {
		return nil
	}
	parsed, err := jsonParse(string(raw))
	if err != nil {
		return nil
	}
	m, ok := parsed.(map[string]any)
	if !ok || !jsTruthy(m["package"]) || !jsTruthy(m["source"]) {
		return nil
	}
	return m
}

// True when both files hold the same bytes, false when either cannot be read
func sameFileBytes(a, b string) bool {
	x, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	y, err := os.ReadFile(b)
	return err == nil && string(x) == string(y)
}

// Writes the package marker as indented JSON with a trailing newline
func WriteDesktopHalfMarker(dir string, marker *OrderedObject) error {
	target := filepath.Join(dir, PackageMarker)
	if err := os.WriteFile(target, []byte(jsonStringify(marker, "  ", "")+"\n"), 0o666); err != nil {
		return nodeFsError(err, "open", target)
	}
	return nil
}

var desktopSourceDirs = []string{".", "desktop"}

// Folder holding the package's desktop plugin, the root with a plugin.js else desktop, empty when neither
func DesktopHalfSource(packageDir string) string {
	for _, sub := range desktopSourceDirs {
		if st, err := os.Stat(filepath.Join(packageDir, sub, "plugin.js")); err == nil && st.Mode().IsRegular() {
			return filepath.Join(packageDir, sub)
		}
	}
	return ""
}

// Desktop half folder and plugin.js stat, a candidate the app may not stat ending the search
func desktopHalfEntry(packageDir, packageName string) (string, fs.FileInfo, bool) {
	for _, sub := range desktopSourceDirs {
		sourceDir := filepath.Join(packageDir, sub)
		st, err := os.Stat(filepath.Join(sourceDir, "plugin.js"))
		if err != nil {
			if !isMissingErr(err) {
				warn(fmt.Sprintf("[desktop-plugins] cannot read %s: %s", packageName, nodeErrorString(nodeFsError(err, "stat", filepath.Join(sourceDir, "plugin.js")))))
				return "", nil, false
			}
			continue
		}
		if st.Mode().IsRegular() {
			return sourceDir, st, true
		}
	}
	return "", nil, false
}

// String(error) of a Node error, its name then message
func nodeErrorString(err error) string { return "Error: " + err.Error() }

var rootLayoutSkip = map[string]bool{".git": true, "__pycache__": true}

// Copy filter of a root layout half, the clone's .git and the agent's __pycache__ left out
func RootLayoutFilter(src string) bool { return !rootLayoutSkip[filepath.Base(src)] }

// Origin of a package, the catalog sidecar first and then the git remote
func packageOrigin(packageDir string) [][2]any {
	if raw, err := os.ReadFile(filepath.Join(packageDir, ".hermes-catalog.json")); err == nil {
		if parsed, err := jsonParse(string(raw)); err == nil {
			if sidecar, ok := parsed.(map[string]any); ok && jsTruthy(sidecar["repo"]) {
				return [][2]any{{"catalogName", sidecar["catalog_name"]}, {"repo", sidecar["repo"]}, {"sha", sidecar["sha"]}}
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(packageDir, ".git", "config")); err == nil {
		if m := gitOriginRE.FindStringSubmatch(string(raw)); m != nil {
			return [][2]any{{"repo", m[1]}}
		}
	}
	return nil
}

func isLinkPath(target string) bool {
	st, err := os.Lstat(target)
	return err == nil && isLinkMode(st.Mode())
}

// Copies one package's desktop half into the app root under its name, empty when current, absent or the user's
func MaterializeDesktopHalf(packageDir, appRoot, packageName string) (string, error) {
	if packageName == "" {
		packageName = filepath.Base(packageDir)
	}
	sourceDir, st, ok := desktopHalfEntry(packageDir, packageName)
	if !ok {
		return "", nil
	}
	entry := filepath.Join(sourceDir, "plugin.js")
	rootLayout := sourceDir == filepath.Join(packageDir, ".")
	target := filepath.Join(appRoot, packageName)
	existing := readMarker(target)
	pst, err := os.Lstat(packageDir)
	if err != nil {
		return "", nodeFsError(err, "lstat", packageDir)
	}
	linked := isLinkMode(pst.Mode())
	targetIsLink := isLinkPath(target)
	if targetIsLink && existing == nil {
		return "", nil
	}
	marker := NewOrderedObject("package", packageName, "source", sourceDir, "sourceMtimeMs", mtimeMs(st))
	if linked {
		marker.Set("linked", true)
	}
	for _, kv := range packageOrigin(packageDir) {
		marker.Set(kv[0].(string), kv[1])
	}
	if _, err := os.Stat(target); err == nil {
		if existing == nil {
			if _, err := os.Stat(filepath.Join(target, "plugin.js")); err == nil {
				if sameFileBytes(filepath.Join(target, "plugin.js"), entry) {
					if err := WriteDesktopHalfMarker(target, marker); err != nil {
						return "", err
					}
					return target, nil
				}
				return "", nil
			}
		}
		if existing != nil && !targetIsLink && existing["source"] == sourceDir && jsTruthy(existing["linked"]) == linked {
			current := false
			if linked {
				current = sameFileBytes(filepath.Join(target, "plugin.js"), entry)
			} else if v, present := existing["sourceMtimeMs"]; present {
				if n, ok := jsToNumber(v); ok {
					current = n >= mtimeMs(st)
				}
			}
			if current {
				return "", nil
			}
		}
	}
	plainWithin := packageDir
	if linked && !rootLayout {
		plainWithin = ""
	}
	var filter func(string) bool
	if rootLayout {
		filter = RootLayoutFilter
	}
	err = PublishDesktopTree(DesktopTreeSource{Dir: sourceDir, PlainWithin: plainWithin}, target,
		func(staged string) error { return WriteDesktopHalfMarker(staged, marker) }, filter)
	if err != nil {
		return "", err
	}
	return target, nil
}

// DesktopTreeSource names the tree to publish and the clone it must stay inside as plain files, empty for a linked checkout
type DesktopTreeSource struct {
	Dir         string
	PlainWithin string
}

// Refuses a desktop tree reaching outside its package through a link, or larger than the cap
func assertPlainDesktopTree(root, sourceDir string, filter func(string) bool) error {
	if filter == nil {
		filter = func(string) bool { return true }
	}
	walked := root
	rel, err := filepath.Rel(root, sourceDir)
	if err != nil {
		rel = ""
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		walked = filepath.Join(walked, part)
		st, err := os.Lstat(walked)
		if err != nil {
			return nodeFsError(err, "lstat", walked)
		}
		if isLinkMode(st.Mode()) {
			return errors.New("The desktop plugin folder is a symbolic link; it is not installed.")
		}
	}
	var total int64
	pending := []string{sourceDir}
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nodeFsError(err, "scandir", dir)
		}
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			if !filter(full) {
				continue
			}
			if isLinkMode(e.Type()) {
				r, _ := filepath.Rel(sourceDir, full)
				return fmt.Errorf("The desktop plugin contains a symbolic link (%s); it is not installed.", r)
			}
			if e.IsDir() {
				pending = append(pending, full)
				continue
			}
			st, err := os.Lstat(full)
			if err != nil {
				return nodeFsError(err, "lstat", full)
			}
			total += st.Size()
			if total > desktopTreeMaxBytes {
				return fmt.Errorf("The desktop plugin is larger than %d MB.", desktopTreeMaxBytes/(1024*1024))
			}
		}
	}
	return nil
}

// Copies the tree through a staging sibling and renames the finished copy into place, finalize run on the staged tree
func PublishDesktopTree(source DesktopTreeSource, target string, finalize func(staged string) error, filter func(string) bool) error {
	if source.PlainWithin != "" {
		if err := assertPlainDesktopTree(source.PlainWithin, source.Dir, filter); err != nil {
			return err
		}
	}
	parent, name := filepath.Dir(target), filepath.Base(target)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return nodeFsError(err, "mkdir", parent)
	}
	stagingRoot, err := os.MkdirTemp(parent, "."+name+".staging-")
	if err != nil {
		return nodeFsError(err, "mkdtemp", filepath.Join(parent, "."+name+".staging-XXXXXX"))
	}
	defer os.RemoveAll(stagingRoot)
	staged := filepath.Join(stagingRoot, name)
	from, err := pfs.realpath(source.Dir)
	if err != nil {
		return nodeFsError(err, "realpath", source.Dir)
	}
	if err := copyTreeFn(from, staged, filter); err != nil {
		return err
	}
	if finalize != nil {
		if err := finalize(staged); err != nil {
			return err
		}
	}
	if err := removeAll(target); err != nil {
		return nodeFsError(err, "rm", target)
	}
	if err := os.Rename(staged, target); err != nil {
		return nodeFsError(err, "rename", staged, target)
	}
	return nil
}

// Removes a path recursively, a link removed as the link
func removeAll(target string) error {
	if isLinkPath(target) {
		err := os.Remove(target)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.RemoveAll(target)
}

// Recursive copy as fs.cp force recursive copies, links kept as absolute links and the filter asked for every path
func copyTree(src, dst string, filter func(string) bool) error {
	if filter != nil && !filter(src) {
		return nil
	}
	st, err := os.Lstat(src)
	if err != nil {
		return nodeFsError(err, "lstat", src)
	}
	switch {
	case isLinkMode(st.Mode()):
		link, err := os.Readlink(src)
		if err != nil {
			return nodeFsError(err, "readlink", src)
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(src), link)
		}
		os.Remove(dst)
		if err := os.Symlink(link, dst); err != nil {
			return nodeFsError(err, "symlink", link, dst)
		}
		return nil
	case st.IsDir():
		if err := os.MkdirAll(dst, 0o777); err != nil {
			return nodeFsError(err, "mkdir", dst)
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return nodeFsError(err, "scandir", src)
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), filter); err != nil {
				return err
			}
		}
		if err := os.Chmod(dst, st.Mode().Perm()); err != nil {
			return nodeFsError(err, "chmod", dst)
		}
		return nil
	default:
		return copyFile(src, dst, st.Mode().Perm())
	}
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return nodeFsError(err, "copyfile", src, dst)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return nodeFsError(err, "copyfile", src, dst)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return nodeFsError(err, "copyfile", src, dst)
	}
	if err := out.Close(); err != nil {
		return nodeFsError(err, "copyfile", src, dst)
	}
	return os.Chmod(dst, mode)
}

// True only when the package's plugin.js is really gone, an unreadable one kept
func sourceGone(name, entry string) bool {
	_, err := os.Stat(entry)
	if err == nil {
		return false
	}
	if isMissingErr(err) {
		return true
	}
	warn(fmt.Sprintf("[desktop-plugins] keeping desktop half of unreadable package %s: %s", name, nodeErrorString(nodeFsError(err, "stat", entry))))
	return false
}

// RefusedDesktopHalf is a package whose desktop half a pass did not publish
type RefusedDesktopHalf struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// DesktopReconcileReport names the root copies written or pruned and the halves refused
type DesktopReconcileReport struct {
	Published []string             `json:"published"`
	Refused   []RefusedDesktopHalf `json:"refused"`
}

// Root copies written or pruned by one reconcile pass
func ReconcileUnifiedDesktopHalves(hermesHome, appRoot string) []string {
	return ReconcileDesktopHalvesReport(hermesHome, appRoot).Published
}

// Materializes every local home's desktop halves, first home first, and prunes copies whose package is gone
func ReconcileDesktopHalvesReport(hermesHome, appRoot string) DesktopReconcileReport {
	report, _ := WithDesktopPluginsRoot(appRoot, func() (DesktopReconcileReport, error) {
		return reconcilePass(hermesHome, appRoot), nil
	})
	return report
}

func reconcilePass(hermesHome, appRoot string) DesktopReconcileReport {
	touched := []string{}
	var refused []RefusedDesktopHalf
	seen := map[string]bool{}
	for _, home := range LocalHomes(hermesHome) {
		pluginsRoot := filepath.Join(home, "plugins")
		for _, name := range listDirs(pluginsRoot, true) {
			if seen[name] {
				continue
			}
			result, err := MaterializeDesktopHalf(filepath.Join(pluginsRoot, name), appRoot, name)
			if err != nil {
				warn(fmt.Sprintf("[desktop-plugins] skipping package %s: %s", name, nodeErrorString(err)))
				refused = append(refused, RefusedDesktopHalf{Name: name, Reason: err.Error()})
				continue
			}
			if result != "" || DesktopHalfSource(filepath.Join(pluginsRoot, name)) != "" {
				seen[name] = true
			}
			if result != "" {
				touched = append(touched, result)
			}
		}
	}
	for _, name := range listDirs(appRoot, true) {
		dir := filepath.Join(appRoot, name)
		marker := readMarker(dir)
		if marker == nil {
			continue
		}
		source := jsString(marker["source"])
		if sourceGone(name, filepath.Join(source, "plugin.js")) {
			removeAll(dir)
			touched = append(touched, dir)
		}
	}
	kept := []RefusedDesktopHalf{}
	for _, r := range refused {
		if !seen[r.Name] {
			kept = append(kept, r)
		}
	}
	return DesktopReconcileReport{Published: touched, Refused: kept}
}
