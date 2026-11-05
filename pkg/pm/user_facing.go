package pm

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Packages whose packaged dirs a sealed Windows payload swaps for writable copies
var UserFacingCopied = []string{"npm", "node", "python"}

// Reports a sealed payload on Windows, the only layout whose packaged executables user commands cannot start
func UserFacingApplies(ctx context.Context) (bool, error) {
	target, err := CurrentTarget()
	if err != nil {
		return false, err
	}
	if !strings.HasPrefix(target, "win32") {
		return false, nil
	}
	return Sealed(ctx)
}

// Sealed Windows layout check the copy swap consults, replaced by tests
var userFacingApplies = UserFacingApplies

// WritableCopyOptions shape a writable copy lookup, NoRealize keeping it read only
type WritableCopyOptions struct {
	NoRealize bool
	Explicit  bool
	Why       string
}

// Location of PM's verified copy of an installed package in the writable store, copying it on first use
func WritableCopy(ctx context.Context, name string, opts WritableCopyOptions) (*Location, error) {
	why := opts.Why
	if why == "" {
		why = "a copy outside the package is required"
	}
	p, err := GetPackage(name)
	if err != nil {
		return nil, err
	}
	lock := currentLockfile()
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	location, err := InstalledLocation(ctx, p, lock, target, LocationOptions{})
	if err != nil || location == nil {
		return nil, err
	}
	writableRoot, err := WritableStoreRoot(ctx)
	if err != nil {
		return nil, err
	}
	writable := LongRoot(writableRoot)
	if location.Store.Root == writable {
		return location, nil
	}
	copied, err := InstalledLocation(ctx, p, lock, target, LocationOptions{Verify: opts.Explicit, Roots: []string{writable}})
	if err != nil {
		return nil, err
	}
	if copied != nil || opts.NoRealize {
		return copied, nil
	}
	if !opts.Explicit && !LazyInstallsAllowedFor(ctx) {
		return nil, RefuseLazyFor(ctx, name, why)
	}
	copyStore := NewStore(writable)
	copyFacts, err := NewFacts(filepath.Join(writable, "facts.json"), false)
	if err != nil {
		return nil, err
	}
	if _, err := InstallPackage(ctx, p, lock, copyFacts, copyStore, target, InstallPackageOptions{CopyFrom: location}); err != nil {
		return nil, err
	}
	return &Location{Facts: copyFacts, Store: copyStore}, nil
}

// PATH dirs a store's install of a package contributes, natively spelled
func locationDirs(facts *Facts, store *Store, name string) []string {
	value, present := facts.EnvFor(name, store.Root)["PATH"]
	if !present || !pyrepr.Truthy(value) {
		return nil
	}
	var out []string
	for _, dir := range envDirs(value) {
		if dir != "" {
			out = append(out, Native(dir))
		}
	}
	return out
}

// PATH dirs of the sealed store's install of a package, none when it is not installed there
// Sealed payload dirs holding a package's executables, the exported face for pkg/mcp
func SealedDirs(ctx context.Context, name string) ([]string, error) { return sealedDirs(ctx, name) }

func sealedDirs(ctx context.Context, name string) ([]string, error) {
	facts, err := storeFacts(ctx)
	if err != nil {
		return nil, err
	}
	store, err := defaultStore(ctx)
	if err != nil {
		return nil, err
	}
	if !facts.Installed(name, nil, store.Root, nil) {
		return nil, nil
	}
	return locationDirs(facts, store, name), nil
}

// Writable copy lookup the copy swap makes, replaced by tests
var userFacingWritableCopy = func(ctx context.Context, name string) (*Location, error) {
	return WritableCopy(ctx, name, WritableCopyOptions{})
}

// PATH dirs of a package's writable copy made on first use, none when PM may not make one
func copyDirs(ctx context.Context, name string) ([]string, error) {
	location, err := userFacingWritableCopy(ctx, name)
	if err != nil {
		if isInstallOrOSError(err) {
			slog.Debug("user-facing " + name + " stays the packaged copy: " + err.Error())
			return nil, nil
		}
		return nil, err
	}
	if location == nil {
		return nil, nil
	}
	return locationDirs(location.Facts, location.Store, name), nil
}

// Reports an InstallError or an error an operating system call raised, the classes Python catches as OSError
func isInstallOrOSError(err error) bool {
	var installErr *InstallError
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var syscallErr *os.SyscallError
	var errno syscall.Errno
	return errors.As(err, &installErr) || errors.As(err, &pathErr) || errors.As(err, &linkErr) ||
		errors.As(err, &syscallErr) || errors.As(err, &errno)
}

// Dirs with each packaged npm, node and python dir swapped for its writable copy on a sealed Windows payload
func Copies(ctx context.Context, dirs []string) ([]string, error) {
	applies, err := userFacingApplies(ctx)
	if err != nil {
		return nil, err
	}
	if !applies {
		return append([]string{}, dirs...), nil
	}
	swap := map[string][]string{}
	for _, name := range UserFacingCopied {
		sealed, err := sealedDirs(ctx, name)
		if err != nil {
			return nil, err
		}
		if len(sealed) == 0 {
			continue
		}
		copied, err := copyDirs(ctx, name)
		if err != nil {
			return nil, err
		}
		if len(copied) > 0 {
			for _, dir := range sealed {
				swap[osNormcase(dir)] = copied
			}
		}
	}
	var expanded []string
	for _, dir := range dirs {
		if replacement, ok := swap[osNormcase(dir)]; ok {
			expanded = append(expanded, replacement...)
		} else {
			expanded = append(expanded, dir)
		}
	}
	return uniqueStrings(expanded), nil
}

// Order preserving dedupe, Python dict fromkeys
func uniqueStrings(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

// Reports the Windows app alias dir whose python.exe only opens the Store, a real Store Python shipping python3.N.exe
func pythonStoreAliasDir(directory string) bool {
	parts := pathParts(directory)
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	if len(parts) != 2 || osNormcase(parts[0]) != osNormcase("Microsoft") || osNormcase(parts[1]) != osNormcase("WindowsApps") {
		return false
	}
	found, err := globChildren(directory, "python3.*.exe", false)
	return err != nil || len(found) == 0
}

// Components of a path the way pathlib parts lists them, the anchor first
func pathParts(path string) []string {
	if runtime.GOOS == "windows" {
		drive, root, tail := pypath.NtSplitRoot(strings.ReplaceAll(path, "/", `\`))
		var parts []string
		if drive+root != "" {
			parts = append(parts, drive+root)
		}
		for _, part := range strings.Split(tail, `\`) {
			if part != "" && part != "." {
				parts = append(parts, part)
			}
		}
		return parts
	}
	var parts []string
	switch {
	case strings.HasPrefix(path, "//") && !strings.HasPrefix(path, "///"):
		parts = append(parts, "//")
	case strings.HasPrefix(path, "/"):
		parts = append(parts, "/")
	}
	for _, part := range strings.Split(path, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// PM Python's own PATH dirs composed over an empty PATH, replaced by tests
var userFacingPythonDirs = func(ctx context.Context) ([]string, error) {
	env, err := envFor(ctx, []string{"python"}, map[string]string{"PATH": ""})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, dir := range strings.Split(env["PATH"], string(os.PathListSeparator)) {
		if dir != "" {
			out = append(out, dir)
		}
	}
	return out, nil
}

// Store tool dirs of every installed package, replaced by tests the way they patch install's store path dirs
var userFacingStorePathDirs = StorePathDirs

// PATH with the store's tool dirs moved behind the user's entries, PM Python stopping just before the Store alias dir
func UserFacingPath(ctx context.Context, path string) (string, error) {
	store, err := userFacingStorePathDirs(ctx)
	if err != nil {
		return "", err
	}
	if len(store) == 0 {
		return path, nil
	}
	ours, err := Copies(ctx, store)
	if err != nil {
		return "", err
	}
	keys := map[string]bool{}
	for _, dir := range append(append([]string{}, store...), ours...) {
		keys[osNormcase(dir)] = true
	}
	var kept []string
	for _, entry := range strings.Split(path, string(os.PathListSeparator)) {
		if entry != "" && !keys[osNormcase(entry)] {
			kept = append(kept, entry)
		}
	}
	pythonDirs, err := userFacingPythonDirs(ctx)
	if err != nil {
		return "", err
	}
	pythonCopies, err := Copies(ctx, pythonDirs)
	if err != nil {
		return "", err
	}
	python := map[string]bool{}
	for _, dir := range pythonCopies {
		python[osNormcase(dir)] = true
	}
	cut := len(kept)
	for i, entry := range kept {
		if pythonStoreAliasDir(entry) {
			cut = i
			break
		}
	}
	var early, late []string
	for _, dir := range ours {
		if python[osNormcase(dir)] {
			early = append(early, dir)
		} else {
			late = append(late, dir)
		}
	}
	ordered := append(append(append(append([]string{}, kept[:cut]...), early...), kept[cut:]...), late...)
	return strings.Join(uniqueStrings(ordered), string(os.PathListSeparator)), nil
}
