//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Anchors maps each extra the binary carries to the imports that prove it installed
var Anchors = lockAnchors()

// Anchor table of the embedded extras lock, aliases sharing their extra's imports
func lockAnchors() map[string][]string {
	out := map[string][]string{}
	for name, entry := range extrasLock().Extras {
		out[name] = entry.Anchors
		for _, alias := range entry.Aliases {
			out[alias] = entry.Anchors
		}
	}
	return out
}

// Imports proving an extra, the extra's own name with underscores when the table lacks it
func anchors(extra string) []string {
	if got, ok := Anchors[extra]; ok {
		return got
	}
	return []string{strings.ReplaceAll(extra, "-", "_")}
}

// Reports whether an extra environment or the selected environment holds an anchor module, read from package folders and PYTHONPATH
var anchorImportable = func(ctx context.Context, anchor string) bool {
	if extraAnchorImportable(ctx, anchor) {
		return true
	}
	venv, err := SelectedVenv(ctx, RepoRoot())
	if err != nil {
		return false
	}
	site, err := SitePackages(venv)
	if err != nil {
		return false
	}
	roots := moduleRoots(site)
	for _, dir := range filepath.SplitList(pyEnviron()["PYTHONPATH"]) {
		if dir != "" {
			roots = append(roots, dir)
		}
	}
	return moduleInstalled(roots, anchor)
}

// Whether a provisioned extra that names the anchor holds it in its own environment
func extraAnchorImportable(ctx context.Context, anchor string) bool {
	for _, site := range AnchorSitePackages(ctx, anchor) {
		if moduleInstalled(moduleRoots(site), anchor) {
			return true
		}
	}
	return false
}

// Site packages of every provisioned extra that names the anchor among its imports
func managedAnchorSitePackages(ctx context.Context, anchor string) []string {
	lock := extrasLock()
	var out []string
	for _, name := range sortedKeys(lock.Extras) {
		if !containsString(lock.Extras[name].Anchors, anchor) {
			continue
		}
		if site, ok := ExtraSitePackages(ctx, name); ok {
			out = append(out, site)
		}
	}
	return out
}

// AnchorImportable reports whether an extra environment or the selected environment holds the module
func managedAnchorImportable(ctx context.Context, anchor string) bool {
	return anchorImportable(ctx, anchor)
}

// Reports every anchor of an extra importable and its platform gate admitting this machine
func managedAvailable(ctx context.Context, extra string) (bool, error) {
	supported, err := extraSupported(ctx, extra, nil, nil)
	if err != nil || !supported {
		return false, err
	}
	for _, anchor := range anchors(extra) {
		if !anchorImportable(ctx, anchor) {
			return false, nil
		}
	}
	return true, nil
}

var platformGatesCache struct {
	sync.Mutex
	gates map[string]string
}

// Extra to marker table of the checkout's pyproject, read once per process and empty when unreadable
func platformGates() (map[string]string, error) {
	platformGatesCache.Lock()
	defer platformGatesCache.Unlock()
	if platformGatesCache.gates != nil {
		return platformGatesCache.gates, nil
	}
	gates := map[string]string{}
	data, err := loadPyproject(RepoRoot())
	if err == nil {
		tool, err := tomlGet(data, "tool", NewOrderedObject())
		if err != nil {
			return nil, err
		}
		own, err := tomlGet(tool, "icebath", NewOrderedObject())
		if err != nil {
			return nil, err
		}
		table, err := tomlGet(own, "extras-platforms", NewOrderedObject())
		if err != nil {
			return nil, err
		}
		if obj, ok := table.(*OrderedObject); ok {
			for _, key := range obj.Keys {
				gates[key] = tomlText(obj.Values[key])
			}
		}
	}
	platformGatesCache.gates = gates
	return gates, nil
}

// Platform marker environment of this machine as Python's platform module reports it
func markerEnvironment() map[string]string {
	system := map[string]string{"linux": "Linux", "darwin": "Darwin", "windows": "Windows", "freebsd": "FreeBSD",
		"openbsd": "OpenBSD", "netbsd": "NetBSD", "android": "Android", "dragonfly": "DragonFly"}[runtime.GOOS]
	osName := "posix"
	if runtime.GOOS == "windows" {
		osName = "nt"
	}
	return map[string]string{"sys_platform": sysPlatform(), "platform_system": system, "platform_machine": platformMachine(), "os_name": osName}
}

// Gate verdict over the environment laid on the interpreter's defaults, a marker that fails to parse or evaluate leaving the extra ungated
func evaluateGate(ctx context.Context, marker string, environment map[string]string) (bool, error) {
	markers, err := parseMarker(marker)
	if err != nil {
		return true, nil
	}
	var defaults map[string]string
	if markerNeedsDefaults(markers, environment) {
		if defaults, err = loaderMarkerEnvironment(ctx); err != nil {
			return false, err
		}
	}
	ok, err := evaluateMarker(markers, defaults, environment)
	return err != nil || ok, nil
}

// Reports an extra installable here, an installed anchor set beating its platform gate
func extraSupported(ctx context.Context, extra string, environment map[string]string, importable func(string) bool) (bool, error) {
	all := true
	for _, anchor := range anchors(extra) {
		if importable != nil {
			all = all && importable(anchor)
		} else {
			all = all && anchorImportable(ctx, anchor)
		}
	}
	if all {
		return true, nil
	}
	if _, entry, known := ResolveExtra(extra); known {
		target, err := CurrentTarget()
		if err != nil {
			return false, err
		}
		if _, gone := entry.Unsupported[target]; gone {
			return false, nil
		}
	}
	gates, err := platformGates()
	if err != nil {
		return false, err
	}
	marker, gated := gates[extra]
	if !gated {
		return true, nil
	}
	if environment == nil {
		environment = markerEnvironment()
	}
	return evaluateGate(ctx, marker, environment)
}

// ExtraSupported reports an extra installable on this machine, an installed anchor set beating its platform gate
func managedExtraSupported(ctx context.Context, extra string) (bool, error) {
	return extraSupported(ctx, extra, nil, nil)
}

// ExtraSupportedIn reports an extra installable under an explicit marker environment and importability probe
func ExtraSupportedIn(ctx context.Context, extra string, environment map[string]string, importable func(string) bool) (bool, error) {
	return extraSupported(ctx, extra, environment, importable)
}

// SwapPlatformGates installs the extra to marker table the gates read, returning the table it replaced
func SwapPlatformGates(gates map[string]string) map[string]string {
	platformGatesCache.Lock()
	defer platformGatesCache.Unlock()
	saved := platformGatesCache.gates
	platformGatesCache.gates = gates
	return saved
}

// SwapAnchorImportable installs the anchor importability probe, returning the one it replaced
func SwapAnchorImportable(fn func(ctx context.Context, anchor string) bool) func(ctx context.Context, anchor string) bool {
	saved := anchorImportable
	anchorImportable = fn
	return saved
}

// Client sync a configured feature install goes through
var clientSyncVenv = SyncVenv

// Anchors of an extra the selected environment cannot import
func Missing(ctx context.Context, extra string) []string {
	var out []string
	for _, anchor := range anchors(extra) {
		if !anchorImportable(ctx, anchor) {
			out = append(out, anchor)
		}
	}
	return out
}

// Reports an anchor installed as a package dir, module file or compiled extension under a site packages tree
func installedIn(sitePackages, anchor string) bool {
	parts := strings.Split(anchor, ".")
	leaf := parts[len(parts)-1]
	parent := filepath.Join(append([]string{sitePackages}, parts[:len(parts)-1]...)...)
	if isDirectory(filepath.Join(parent, leaf)) {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(globEscape(parent), globEscape(leaf)+".*"))
	for _, match := range matches {
		name := filepath.Base(match)
		switch name[strings.LastIndex(name, ".")+1:] {
		case "py", "so", "pyd":
			return true
		}
	}
	return false
}

// DeclinedExtrasFilename is the record of extras dropped with install without
const DeclinedExtrasFilename = "declined-extras.json"

// Declined extras record of an install, this binary's own tree when projectRoot is empty
func DeclinedExtrasPath(ctx context.Context, projectRoot string) string {
	if projectRoot == "" {
		projectRoot = RepoRoot()
	}
	return filepath.Join(InstallStateDir(ctx, projectRoot), DeclinedExtrasFilename)
}

// Extras the user dropped by their PEP 685 names, refusing a damaged record rather than re adding them
func declinedExtras(ctx context.Context, projectRoot string) (map[string]bool, error) {
	path := DeclinedExtrasPath(ctx, projectRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	fail := func(detail string) error {
		return NewInstallError("venv", fmt.Sprintf("cannot read %s: %s", path, detail), "fix or delete the file, then retry")
	}
	if err != nil {
		return nil, fail(pyExceptionText(err))
	}
	data = trimBOM(data)
	if err := utf8Strict(data); err != nil {
		return nil, fail(err.Error())
	}
	doc, err := loadJSON(data)
	if err != nil {
		return nil, fail(err.Error())
	}
	out := map[string]bool{}
	record, _ := doc.(map[string]any)
	if names, ok := record["declined"].([]any); ok {
		for _, name := range names {
			if s, isStr := name.(string); isStr {
				out[ExtraKey(s)] = true
			}
		}
	}
	return out, nil
}

// Why an update left each declined extra out, empty for a user's own without and for any unreadable record
func declinedReasons(ctx context.Context, projectRoot string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(DeclinedExtrasPath(ctx, projectRoot))
	if err != nil {
		return out
	}
	data = trimBOM(data)
	if utf8Strict(data) != nil {
		return out
	}
	doc, err := loadJSON(data)
	if err != nil {
		return out
	}
	record, _ := doc.(map[string]any)
	notes, _ := record["left_out_by_update"].(map[string]any)
	for name, text := range notes {
		if s, ok := text.(string); ok {
			out[name] = s
		}
	}
	return out
}

// Adds and removes declined extras with update reasons, writing only on change and returning the new set
func recordDeclinedExtras(ctx context.Context, add, remove []string, reasons map[string]string, projectRoot string) (map[string]bool, error) {
	current, err := declinedExtras(ctx, projectRoot)
	if err != nil {
		return nil, err
	}
	updated := map[string]bool{}
	for name := range current {
		updated[name] = true
	}
	for _, name := range add {
		updated[ExtraKey(name)] = true
	}
	for _, name := range remove {
		delete(updated, ExtraKey(name))
	}
	known := declinedReasons(ctx, projectRoot)
	merged := map[string]string{}
	for name, text := range known {
		merged[name] = text
	}
	for name, text := range reasons {
		merged[ExtraKey(name)] = pyPrefix(strings.Join(strings.FieldsFunc(text, pystr.IsSpace), " "), 300)
	}
	noted := map[string]string{}
	for name, text := range merged {
		if updated[name] {
			noted[name] = text
		}
	}
	if !sameKeys(updated, current) || !sameNotes(noted, known) {
		record := NewOrderedObject()
		record.Set("schema", 1)
		record.Set("declined", sortedKeys(updated))
		if len(noted) > 0 {
			notes := NewOrderedObject()
			for _, name := range sortedKeys(noted) {
				notes.Set(name, noted[name])
			}
			record.Set("left_out_by_update", notes)
		}
		data, err := dumpsJSON(record, true)
		if err != nil {
			return nil, err
		}
		if err := DurableWriteBytes(DeclinedExtrasPath(ctx, projectRoot), append(data, '\n')); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// Reports two sets equal
func sameKeys(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// Reports two string maps equal
func sameNotes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Declared extras install without may drop, neither all nor anything all pulls in, every carried extra for a binary install
func DroppableExtras(projectRoot string) ([]string, error) {
	if runtimeOnlyProject(projectRoot) {
		return KnownExtras(), nil
	}
	data, err := loadPyproject(projectRoot)
	if err != nil {
		return nil, err
	}
	project, err := tomlGet(data, "project", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	table, err := tomlGet(project, "optional-dependencies", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	nameValue, err := tomlGet(project, "name", "")
	if err != nil {
		return nil, err
	}
	selfRef := regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(tomlText(nameValue)) + `\[([^\]]+)\]`)
	kept, pending := map[string]bool{"all": true}, []string{"all"}
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		requirements, err := tomlGet(table, next, []any{})
		if err != nil {
			return nil, err
		}
		items, err := tomlIter(requirements)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("optional dependency of extra %s must be text, got %s", next, tomlKind(item))}
			}
			match := selfRef.FindStringSubmatch(strings.ReplaceAll(text, " ", ""))
			if match == nil {
				continue
			}
			for _, member := range strings.Split(match[1], ",") {
				if !kept[member] {
					kept[member] = true
					pending = append(pending, member)
				}
			}
		}
	}
	names, err := tomlIter(table)
	if err != nil {
		return nil, err
	}
	declared, err := tomlStrings(names)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, extra := range declared {
		if !kept[extra] {
			out = append(out, extra)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Extras PM's first generation selects when replacing a pre PM venv, all plus every supported opt in it held
func LegacySelection(ctx context.Context, projectRoot string) ([]string, error) {
	var trees []string
	for _, venv := range []string{filepath.Join(projectRoot, "venv"), filepath.Join(projectRoot, ".venv")} {
		candidates := globUnder(filepath.Join(venv, "lib"), filepath.Join("python*", "site-packages"))
		candidates = append(candidates, filepath.Join(venv, "Lib", "site-packages"))
		for _, tree := range candidates {
			if isDirectory(tree) {
				trees = append(trees, tree)
			}
		}
	}
	refused, err := declinedExtras(ctx, projectRoot)
	if err != nil {
		return nil, err
	}
	carried := []string{}
	for _, extra := range sortedKeys(Anchors) {
		if extra == "voice" || extra == "wake" || refused[extra] {
			continue
		}
		held := false
		for _, tree := range trees {
			all := true
			for _, anchor := range anchors(extra) {
				all = all && installedIn(tree, anchor)
			}
			held = held || all
		}
		if !held {
			continue
		}
		supported, err := extraSupported(ctx, extra, nil, func(string) bool { return false })
		if err != nil {
			return nil, err
		}
		if supported {
			carried = append(carried, extra)
		}
	}
	return append([]string{"all"}, carried...), nil
}
