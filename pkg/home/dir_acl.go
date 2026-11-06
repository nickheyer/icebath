package home

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

const (
	FileAttributeDirectory    = 0x10
	FileAttributeReparsePoint = 0x400
	ioReparseTagMountPoint    = 0xA0000003
	ioReparseTagSymlink       = 0xA000000C
)

// Refusal to grant through a junction, symlink, other reparse point or hard link
type ReparsePointRefused struct{ Msg string }

func (e *ReparsePointRefused) Error() string { return e.Msg }

// Outcome of one failed grant in a tree walk, the path and the error text
type GrantFailure struct {
	Path   string
	Reason string
}

type aclStat struct {
	link  bool
	dir   bool
	attrs uint32
	tag   uint32
	nlink uint64
	dev   uint64
	ino   uint64
}

type aclIdentity struct {
	path string
	dev  uint64
	ino  uint64
}

type subtreeLinks int8

const (
	linksUnwalked subtreeLinks = iota
	linksAbsent
	linksPresent
)

var (
	errNoNativeACL    = errors.New("native ACL writer unavailable on " + runtime.GOOS)
	processIsElevated = sync.OnceValue(probeProcessElevated)
	aclLstat          = lstatForACL
	grantNative       = grantNativeACL
	whoamiSID         = cachedWhoamiSID
	runACLCommand     = func(argv []string) error { _, _, err := pysubprocess.Run(argv, 60, true); return err }
	iterTree          = IterTreeWithoutReparsePoints
	grantUserAccessFn = GrantUserAccess
	grantedMu         sync.Mutex
	grantedDirs       = map[aclIdentity]bool{}
	whoamiMu          sync.Mutex
	whoamiCached      string
)

// True for an elevated Windows process, false on every other host or on any probe error
func ProcessIsElevated() bool { return processIsElevated() }

// True when path itself is a symlink, junction or other reparse point, or cannot be lstat-ed
func IsReparsePoint(path string) bool {
	st, err := aclLstat(path)
	if err != nil {
		return true
	}
	return st.link || st.attrs&FileAttributeReparsePoint != 0 || st.tag == ioReparseTagMountPoint
}

func hardLinkedFile(path string) bool {
	st, err := aclLstat(path)
	if err != nil {
		return true
	}
	return !st.dir && st.nlink > 1
}

// Current user SID from whoami, cached once it resolves
func cachedWhoamiSID() (string, error) {
	whoamiMu.Lock()
	defer whoamiMu.Unlock()
	if whoamiCached != "" {
		return whoamiCached, nil
	}
	out, _, err := pysubprocess.Run([]string{"whoami", "/user", "/fo", "csv", "/nh"}, 30, true)
	if err != nil {
		return "", err
	}
	text := strings.ReplaceAll(strings.ReplaceAll(pystr.DecodeUTF8Replace(out), "\r\n", "\n"), "\r", "\n")
	stripped := pystr.Strip(text)
	sid := strings.Trim(pystr.Strip(stripped[strings.LastIndex(stripped, ",")+1:]), `"`)
	if !strings.HasPrefix(strings.ToUpper(sid), "S-1-") {
		return "", fmt.Errorf("could not resolve the current user SID from whoami: %s", pyrepr.StrRepr(text))
	}
	whoamiCached = sid
	return sid, nil
}

// Grants through icacls by name, refusing any path without a trusted root or a subtree holding a link
func grantWithIcacls(path string, isDir bool, expected string, links subtreeLinks) error {
	if expected == "" {
		return &ReparsePointRefused{fmt.Sprintf("%s has no trusted root to check (icacls fallback, no pywin32)", path)}
	}
	if isDir && links == linksPresent {
		return &ReparsePointRefused{fmt.Sprintf("%s holds a junction, symlink or hard link (icacls fallback)", path)}
	}
	if isDir && links == linksUnwalked {
		var nested, linked []string
		for entry, entryIsDir := range iterTree(path, &nested) {
			if !entryIsDir && hardLinkedFile(entry) {
				linked = append(linked, entry)
			}
		}
		if bad := append(nested, linked...); len(bad) > 0 {
			return &ReparsePointRefused{fmt.Sprintf("%s under %s is a junction, symlink or hard link (icacls fallback)", bad[0], path)}
		}
	}
	sid, err := whoamiSID()
	if err != nil {
		return err
	}
	ace := "*" + sid + ":F"
	if isDir {
		ace = "*" + sid + ":(OI)(CI)F"
	}
	return runACLCommand([]string{"icacls", path, "/grant", ace, "/Q", "/L"})
}

// Grants the user full control on path itself, true when the existing children are still owed
func grant(path string, isDir bool, expected string, links subtreeLinks) (bool, error) {
	if IsReparsePoint(path) || hardLinkedFile(path) {
		return false, &ReparsePointRefused{fmt.Sprintf("%s is a junction, symlink, other reparse point or hard link", path)}
	}
	owed, err := grantNative(path, isDir, expected)
	if errors.Is(err, errNoNativeACL) {
		return false, grantWithIcacls(path, isDir, expected, links)
	}
	if err != nil {
		return false, err
	}
	return owed && isDir, nil
}

// Path and each ancestor strictly below the process home, outermost first, empty off the home
func componentsBelowHome(path string) []string {
	h, err := pypath.Abspath(Dir(context.Background()))
	if err != nil {
		return nil
	}
	abs, err := pypath.Abspath(path)
	if err != nil {
		return nil
	}
	rel, ok := pypath.PathRelativeTo(abs, h)
	if !ok {
		return nil
	}
	h = pypath.PathStr(h)
	_, parts := pypath.PathParts(rel)
	out := make([]string, 0, len(parts))
	for i := 1; i <= len(parts); i++ {
		out = append(out, pypath.PathJoin(h, parts[:i]...))
	}
	return out
}

// Real home path plus the relative parts, empty for a path off the home
func expectedFinal(path string) (string, error) {
	components := componentsBelowHome(path)
	if len(components) == 0 {
		return "", nil
	}
	h := pypath.PathParent(components[0])
	real, err := pypath.Realpath(h, pypath.Lax)
	if err != nil {
		return "", err
	}
	abs, err := pypath.Abspath(path)
	if err != nil {
		return "", err
	}
	rel, _ := pypath.PathRelativeTo(abs, h)
	_, parts := pypath.PathParts(rel)
	return pypath.Join(real, parts...), nil
}

func excText(err error) string {
	_, text := excInfo(err)
	return text
}

func warnRefused(path string, err error) {
	slog.Warn(fmt.Sprintf("Not granting the current user access to %s while elevated (%s): an elevated grant "+
		"never follows a junction or link out of the Icebath home. If this is your own link, "+
		"point ICEBATH_HOME (or the link) at the real directory.", path, excText(err)))
}

// Gives the user an inheriting full control ACE on path when elevated, true when the grant took
func GrantUserAccess(path string, elevated, repairChildren bool) bool {
	if !elevated {
		return false
	}
	target := pypath.PathStr(path)
	expected := ""
	owed, err := func() (bool, error) {
		for _, component := range componentsBelowHome(target) {
			if IsReparsePoint(component) {
				return false, &ReparsePointRefused{fmt.Sprintf("%s is a junction, symlink or other reparse point", component)}
			}
		}
		exp, err := expectedFinal(target)
		if err != nil {
			return false, err
		}
		expected = exp
		return grant(target, true, expected, linksUnwalked)
	}()
	var refused *ReparsePointRefused
	if errors.As(err, &refused) {
		warnRefused(target, err)
		return false
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not grant the current user access to %s created while elevated (%s); a "+
			"non-elevated Icebath may not be able to use it. To repair, from an elevated prompt run: "+
			"icacls \"%s\" /reset /t", target, excText(err), target))
		return false
	}
	if repairChildren && owed {
		real := expected
		if real == "" {
			real, _ = pypath.Realpath(target, pypath.Lax)
		}
		var skipped []string
		var entries []treeEntry
		for p, d := range iterTree(target, &skipped) {
			if p != target {
				entries = append(entries, treeEntry{p, d})
			}
		}
		if _, _, failed := grantEntries(target, real, entries, skipped); len(failed) > 0 {
			slog.Debug(fmt.Sprintf("Granting the existing entries under %s failed for %d, e.g. %s: %s",
				target, len(failed), failed[0].Path, failed[0].Reason))
		}
	}
	return true
}

type treeEntry struct {
	path  string
	isDir bool
}

// Python sort key of a sibling path, case folded on Windows and code point ordered elsewhere
func siblingKey(p string) string {
	name := pypath.PathName(p)
	if runtime.GOOS == "windows" {
		return pystr.Lower(name)
	}
	return pystr.SurrogateEscapeDecode([]byte(name))
}

// Yields root and everything under it, never entering or yielding a link, which lands in skipped
func IterTreeWithoutReparsePoints(root string, skipped *[]string) iter.Seq2[string, bool] {
	return func(yield func(string, bool) bool) {
		r := pypath.PathStr(root)
		if IsReparsePoint(r) {
			*skipped = append(*skipped, r)
			return
		}
		stack := []string{r}
		for len(stack) > 0 {
			directory := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if !yield(directory, true) {
				return
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				continue
			}
			children := make([]string, len(entries))
			for i, entry := range entries {
				children[i] = pypath.PathJoin(directory, entry.Name())
			}
			slices.SortFunc(children, func(a, b string) int { return strings.Compare(siblingKey(a), siblingKey(b)) })
			for _, child := range children {
				if IsReparsePoint(child) {
					*skipped = append(*skipped, child)
				} else if st, err := os.Stat(child); err == nil && st.IsDir() {
					stack = append(stack, child)
				} else if !yield(child, false) {
					return
				}
			}
		}
	}
}

// Elevated recursive grant over root and everything under it, never through a link
func GrantTreeUserAccess(root string, pinRoot, elevated bool) (int, []string, []GrantFailure, error) {
	if !elevated {
		return 0, nil, nil, nil
	}
	var r string
	var err error
	if pinRoot {
		r, err = pypath.Realpath(root, pypath.Lax)
	} else {
		r, err = pypath.Abspath(root)
	}
	if err != nil {
		return 0, nil, nil, err
	}
	r = pypath.PathStr(r)
	var skipped []string
	var entries []treeEntry
	for p, d := range iterTree(r, &skipped) {
		entries = append(entries, treeEntry{p, d})
	}
	realRoot, err := pypath.Realpath(r, pypath.Lax)
	if err != nil {
		return 0, nil, nil, err
	}
	granted, skipped, failed := grantEntries(r, realRoot, entries, skipped)
	if len(skipped) > 0 {
		suffix, shown := "ies", skipped
		if len(skipped) == 1 {
			suffix = "y"
		}
		if len(shown) > 5 {
			shown = shown[:5]
		}
		list := strings.Join(shown, ", ")
		if len(skipped) > 5 {
			list += " ..."
		}
		slog.Warn(fmt.Sprintf("Elevated ACL repair skipped %d junction/symlink/hard-link entr%s under %s (never "+
			"granted through): %s", len(skipped), suffix, r, list))
	}
	return granted, skipped, failed, nil
}

// Grants each walked entry on its own, one walk deciding which directories hold a link
func grantEntries(root, realRoot string, entries []treeEntry, skipped []string) (int, []string, []GrantFailure) {
	linked := map[string]bool{root: true}
	for _, s := range skipped {
		linked[s] = true
	}
	for _, e := range entries {
		if !e.isDir && hardLinkedFile(e.path) {
			linked[e.path] = true
		}
	}
	holdsLink := map[string]bool{}
	for bad := range linked {
		if bad == root {
			continue
		}
		for _, parent := range pypath.PathParents(bad) {
			if parent == root || slices.Contains(pypath.PathParents(parent), root) {
				holdsLink[parent] = true
			}
		}
	}
	if slices.Contains(skipped, root) {
		holdsLink[root] = true
	}
	granted := 0
	var failed []GrantFailure
	for _, e := range entries {
		rel, _ := pypath.PathRelativeTo(e.path, root)
		_, parts := pypath.PathParts(rel)
		links := linksAbsent
		if e.isDir && holdsLink[e.path] {
			links = linksPresent
		}
		_, err := grant(e.path, e.isDir, pypath.Join(realRoot, parts...), links)
		var refused *ReparsePointRefused
		switch {
		case errors.As(err, &refused):
			skipped = append(skipped, e.path)
		case err != nil:
			cls, text := excInfo(err)
			if text == "" {
				text = cls
			}
			failed = append(failed, GrantFailure{e.path, text})
		default:
			granted++
		}
	}
	return granted, skipped, failed
}

func identity(path string) (aclIdentity, bool) {
	st, err := aclLstat(path)
	if err != nil {
		return aclIdentity{}, false
	}
	return aclIdentity{path, st.dev, st.ino}, true
}

// pathlib mkdir that keeps a directory an elevated Windows process creates usable by the same user unelevated
func MakeUserDir(path string, mode os.FileMode, parents, existOK bool) (string, error) {
	return makeUserDir(path, mode, parents, existOK, ProcessIsElevated())
}

func makeUserDir(path string, mode os.FileMode, parents, existOK, elevated bool) (string, error) {
	target := pypath.PathStr(path)
	if !elevated {
		return target, pathMkdir(target, mode, parents, existOK)
	}
	var created []string
	for probe := target; !pathExists(probe) && pypath.PathParent(probe) != probe; probe = pypath.PathParent(probe) {
		created = append(created, probe)
	}
	if err := pathMkdir(target, mode, parents, existOK); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if st, serr := os.Stat(target); serr == nil && st.IsDir() && !IsReparsePoint(target) {
				grantDirs(target, []string{target}, nil)
			}
		}
		return target, err
	}
	toGrant := []string{target}
	fresh := map[string]bool{}
	if len(created) > 0 {
		toGrant = make([]string, 0, len(created))
		for i := len(created) - 1; i >= 0; i-- {
			toGrant = append(toGrant, created[i])
			fresh[created[i]] = true
		}
	}
	grantDirs(target, toGrant, fresh)
	return target, nil
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Grants every fresh directory and each existing one once per object identity, nothing past a link
func grantDirs(target string, toGrant []string, fresh map[string]bool) {
	seen := map[string]bool{}
	for _, component := range append(componentsBelowHome(target), toGrant...) {
		if seen[component] {
			continue
		}
		seen[component] = true
		if IsReparsePoint(component) {
			warnRefused(component, &ReparsePointRefused{"junction, symlink or other reparse point"})
			return
		}
	}
	for _, directory := range toGrant {
		key, ok := identity(directory)
		if !ok {
			continue
		}
		grantedMu.Lock()
		done := grantedDirs[key]
		grantedMu.Unlock()
		if !fresh[directory] && done {
			continue
		}
		if grantUserAccessFn(directory, true, !fresh[directory]) {
			grantedMu.Lock()
			grantedDirs[key] = true
			grantedMu.Unlock()
		}
	}
}

// pathlib mkdir, missing parents made with the default mode
func pathMkdir(p string, mode os.FileMode, parents, existOK bool) error {
	err := osMkdir(p, mode)
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		parent := pypath.PathParent(p)
		if !parents || parent == p {
			return err
		}
		if perr := pathMkdir(parent, 0o777, true, true); perr != nil {
			return perr
		}
		return pathMkdir(p, mode, false, existOK)
	}
	if st, serr := os.Stat(p); !existOK || serr != nil || !st.IsDir() {
		return err
	}
	return nil
}

// One line repair for a directory this process may not use
func UnusableDirRemedy(path string) string { return unusableDirRemedy(path, runtime.GOOS == "windows") }

func unusableDirRemedy(path string, windows bool) string {
	if windows {
		return fmt.Sprintf(`from an elevated prompt run: icacls "%s" /reset /t`, path)
	}
	return fmt.Sprintf(`run: sudo chown -R "$(id -un)" "%s"`, path)
}
