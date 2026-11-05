package pm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// InstallProgress reports download, unpack and verify stages, an error aborting the install
type InstallProgress func(stage string, done, total int64, label string) error

// Environment variable the docker image and hermetic harness set to turn lazy installs off
const EnvDisableLazyInstalls = "ICEBATH_DISABLE_LAZY_INSTALLS"

// Prefix of trees a publish or restore could not delete, reclaimed by the next install or gc
const setAsidePrefix = ".reclaim-"

// Renames an entry tree, swapped by tests to simulate a held Windows tree
var renameEntry = os.Rename

// Publishes a staged tree into a store, swapped by tests that inject publication faults
var publishStaged = func(store *Store, staged, name string) (string, error) { return store.Publish(staged, name) }

// Records a tool fact, swapped by tests that inject facts write faults
var recordToolFact = func(f *Facts, name, version, entry string, env EnvDiff, root string, extra FactRecord) error {
	return f.Record(name, version, entry, env, root, extra)
}

// Fetches a pin's archives, swapped by tests that inject transfer faults
var fetchArtifacts = func(store *Store, artifacts []Artifact, scratch string, progress ProgressFunc, pause *PauseEvent) ([]string, error) {
	return store.FetchMany(artifacts, scratch, progress, pause)
}

// Deletes a store entry, swapped by tests that model a hold outlasting every retry
var removeEntry = removeStoreEntry

type lazyDisabledKey struct{}

// Context whose lazy install policy is off, the worker boundary's ICEBATH_DISABLE_LAZY_INSTALLS
func withLazyInstallsDisabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, lazyDisabledKey{}, true)
}

// Location is a tool package found installed, the facts that vouch for it and its store
type Location struct {
	Facts *Facts
	Store *Store
}

// LocationOptions verify bytes, accept a prior pin, choose roots with nil the default pair, and keep lookups read only
type LocationOptions struct {
	Verify        bool
	AllowOutdated bool
	NoHeal        bool
	Roots         []string
}

// Lockfile this process installs from
func currentLockfile() *Lockfile { return NewLockfile(LockfilePath()) }

// Installed state file of the byte store
func storeFacts(ctx context.Context) (*Facts, error) {
	path, err := FactsPath(ctx)
	if err != nil {
		return nil, err
	}
	return NewFacts(path, false)
}

// Byte store of this process
func defaultStore(ctx context.Context) (*Store, error) {
	root, err := StoreRoot(ctx)
	if err != nil {
		return nil, err
	}
	return NewStore(root), nil
}

// Reports a path that exists without following a final symlink
func lexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Reports a path that exists, following symlinks
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Reports a directory, following symlinks
func isDirectory(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// Restores a dropped exec bit in place, false when the entry may not or cannot be made executable
func healExecBit(binary string, heal bool) bool {
	if runtime.GOOS == "windows" || canExecute(binary) {
		return true
	}
	if !heal {
		return false
	}
	st, err := os.Stat(binary)
	if err != nil {
		return false
	}
	if err := os.Chmod(binary, st.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)|0o111); err != nil {
		return false
	}
	return canExecute(binary)
}

// Store and facts holding the current pin of a package, a prior pin when allowed, nil when none
func InstalledLocation(ctx context.Context, p Package, lockfile *Lockfile, target string, opts LocationOptions) (*Location, error) {
	storeRoot, err := StoreRoot(ctx)
	if err != nil {
		return nil, err
	}
	roots := opts.Roots
	if roots == nil {
		writable, err := WritableStoreRoot(ctx)
		if err != nil {
			return nil, err
		}
		roots = []string{storeRoot, writable}
	}
	var unique []string
	for _, root := range roots {
		if !slices.Contains(unique, root) {
			unique = append(unique, root)
		}
	}
	current, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	name := p.Base().Name
	version, pinned := lockfile.Version(name)
	var expected *string
	if pinned {
		expected = &version
	}
	identity := pinIdentity(lockfile, name, target)
	var fallback *Location
	for _, root := range unique {
		store := NewStore(root)
		var facts *Facts
		if root == storeRoot {
			facts, err = storeFacts(ctx)
		} else {
			facts, err = NewFacts(filepath.Join(root, "facts.json"), false)
		}
		if err != nil {
			return nil, err
		}
		fact, _ := facts.Get(name)
		if len(fact) == 0 || !facts.Installed(name, nil, store.Root, nil) {
			continue
		}
		binary := p.Binary(store.Entry(pyrepr.Str(fact["entry"])), target)
		if binary != "" && !isRegularFile(binary) {
			continue
		}
		if binary != "" && target == current && !healExecBit(binary, !opts.NoHeal) {
			continue
		}
		if opts.Verify && !entryVerified(p, fact, store, target) {
			continue
		}
		if facts.Installed(name, expected, store.Root, identity) {
			return &Location{Facts: facts, Store: store}, nil
		}
		if opts.AllowOutdated && jsonEquals(fact["target"], target) && pyrepr.Truthy(fact["artifacts"]) && pyrepr.Truthy(fact["digest"]) {
			fallback = &Location{Facts: facts, Store: store}
		}
	}
	return fallback, nil
}

// InstalledPackage is a selected PM entry in its ordinary spelling, Binary empty for a package without one
type InstalledPackage struct {
	Path    string
	Version string
	Binary  string
}

// Reads the selected entry of an application package without installing or changing its facts, nil when absent
func Installed(ctx context.Context, name string, allowOutdated bool) (*InstalledPackage, error) {
	p, err := GetPackage(name)
	if err != nil {
		return nil, err
	}
	if p.Base().Internal {
		return nil, &PyError{Class: "ValueError", Message: name + " is internal PM tooling, not an application package"}
	}
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	loc, err := InstalledLocation(ctx, p, currentLockfile(), target, LocationOptions{AllowOutdated: allowOutdated})
	if err != nil || loc == nil {
		return nil, err
	}
	fact, _ := loc.Facts.Get(name)
	entry := loc.Store.Entry(pyrepr.Str(fact["entry"]))
	out := &InstalledPackage{Path: Native(entry), Version: pyrepr.Str(fact["version"])}
	if binary := p.Binary(entry, target); binary != "" {
		out.Binary = Native(binary)
	}
	return out, nil
}

// Installed uv or uvx launcher for a user declared MCP stdio command, empty when it is not installed
func UvLauncher(ctx context.Context, name string) (string, error) {
	if name != "uv" && name != "uvx" {
		return "", &PyError{Class: "ValueError", Message: pyrepr.StrRepr(name) + " is not a uv launcher"}
	}
	p, err := GetPackage("uv")
	if err != nil {
		return "", err
	}
	target, err := CurrentTarget()
	if err != nil {
		return "", err
	}
	loc, err := InstalledLocation(ctx, p, currentLockfile(), target, LocationOptions{})
	if err != nil || loc == nil {
		return "", err
	}
	fact, _ := loc.Facts.Get("uv")
	binary := p.Binary(loc.Store.Entry(pyrepr.Str(fact["entry"])), target)
	if binary == "" {
		return "", nil
	}
	launcher := filepath.Join(filepath.Dir(binary), name+pypath.PathSuffix(filepath.Base(binary)))
	if !isRegularFile(launcher) {
		return "", nil
	}
	return Native(launcher), nil
}

// Target and artifact digests the lock pins for a package, nil when it pins no artifacts
func pinIdentity(lockfile *Lockfile, name, target string) *Identity {
	artifacts := lockfile.Artifacts(name, target)
	if len(artifacts) == 0 {
		return nil
	}
	shas := make([]string, len(artifacts))
	for i, a := range artifacts {
		shas[i] = pyrepr.Str(a["sha256"])
	}
	return &Identity{Target: target, SHA256s: shas}
}

// Reports whether PM may install on demand for the process home and environment
func LazyInstallsAllowed() bool { return LazyInstallsAllowedFor(context.Background()) }

// Reports whether PM may install on demand under a context's home and worker policy, a broken config failing closed
func LazyInstallsAllowedFor(ctx context.Context) bool {
	if disabled, _ := ctx.Value(lazyDisabledKey{}).(bool); disabled {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvDisableLazyInstalls))) {
	case "1", "true", "yes":
		return false
	}
	h := home.Dir(ctx)
	if _, err := config.RequireReadableConfigBeforeWrite(ctx, ""); err != nil {
		return false
	}
	tree, err := config.LoadReadonly(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return false
	}
	value, found := tree.Lookup("security.allow_lazy_installs")
	if !found {
		return true
	}
	allowed, isBool := value.(bool)
	return isBool && allowed
}

// Venv extras recorded in the installed state
func EnabledExtras(ctx context.Context) ([]string, error) {
	runtimeFacts, err := NewFacts(RuntimeFactsPath(ctx), false)
	if err != nil {
		return nil, err
	}
	fact, _ := runtimeFacts.Get("venv")
	if len(fact) == 0 {
		facts, err := storeFacts(ctx)
		if err != nil {
			return nil, err
		}
		fact, _ = facts.Get("venv")
	}
	value, present := fact["extras"]
	if !present {
		return []string{}, nil
	}
	return pyIterStrings(value)
}

// Reports whether the current pin of a package is installed
func IsInstalled(ctx context.Context, name string) (bool, error) {
	p, err := GetPackage(name)
	if err != nil {
		return false, err
	}
	target, err := CurrentTarget()
	if err != nil {
		return false, err
	}
	loc, err := InstalledLocation(ctx, p, currentLockfile(), target, LocationOptions{})
	return loc != nil, err
}

// Reports the bundled layout, a store beside the bundle manifest that ships read only
func Sealed(ctx context.Context) (bool, error) {
	root, err := StoreRoot(ctx)
	if err != nil {
		return false, err
	}
	return isRegularFile(filepath.Join(filepath.Dir(root), "manifest.json")), nil
}

// Records the refusal of an on demand install in the process receipt scope and returns it
func RefuseLazy(name, what string) *InstallError {
	return RefuseLazyFor(context.Background(), name, what)
}

// Records the refusal of an on demand install in a context's receipt scope and returns it
func RefuseLazyFor(ctx context.Context, name, what string) *InstallError {
	err := NewInstallError(name, "not installed and lazy installs are disabled: "+what,
		"enable security.allow_lazy_installs or run `icebath pm install`")
	RecordRefusal(ctx, "lazy-install", err.Error())
	return err
}

// Deletes a replaced or failed entry, retrying transient holds and reporting bytes that survive
func removeStoreEntry(store *Store, entryName string, attempts int) error {
	entry := store.Entry(entryName)
	for attempt := 0; attempt < attempts; attempt++ {
		var err error
		st, lerr := os.Lstat(entry)
		switch {
		case lerr != nil && errors.Is(lerr, fs.ErrNotExist):
			return nil
		case lerr == nil && (st.Mode()&fs.ModeSymlink != 0 || !isDirectory(entry)):
			err = os.Remove(entry)
		default:
			err = RemoveTree(entry)
		}
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if attempt == attempts-1 {
			return err
		}
		time.Sleep(time.Duration(200*(attempt+1)) * time.Millisecond)
	}
	return nil
}

// Drops leftover garbage, renaming a still held tree aside so its slot frees for the next publish
func discardEntry(store *Store, entryName string) {
	err := removeEntry(store, entryName, 5)
	if err == nil {
		return
	}
	if rerr := renameEntry(store.Entry(entryName), store.Entry(setAsidePrefix+randomHex32())); rerr != nil {
		slog.Warn(fmt.Sprintf("could not remove or set aside %s: %s", entryName, err))
		return
	}
	slog.Warn(fmt.Sprintf("%s is still in use (%s); set aside for the next install or `icebath pm gc`", entryName, err))
}

// Deletes set aside trees whose hold is gone under the caller's store lock and counts them
func reclaimSetAside(store *Store) int {
	items, err := os.ReadDir(store.Root)
	if err != nil {
		return 0
	}
	removed := 0
	for _, item := range items {
		if !strings.HasPrefix(item.Name(), setAsidePrefix) {
			continue
		}
		if removeEntry(store, item.Name(), 1) != nil {
			continue
		}
		removed++
	}
	return removed
}

// Releases a package's cached archives after publication under its store lock
func removeDownloads(store *Store, artifacts []map[string]any) {
	for _, artifact := range artifacts {
		discardEntry(store, "fetch-"+pyrepr.Str(artifact["sha256"]))
	}
}

// Recorded digest of a fact, nil when it records none
func factDigest(fact map[string]any) *string {
	if digest, ok := fact["digest"].(string); ok {
		return &digest
	}
	return nil
}

// Reports realized bytes that still verify and match the recorded digest
func entryVerified(p Package, fact map[string]any, store *Store, target string) bool {
	entry := store.Entry(pyrepr.Str(fact["entry"]))
	if p.Verify(entry, target) != "" {
		return false
	}
	matches, err := DigestMatches(entry, factDigest(fact))
	return err == nil && matches
}

// Moves the previous tree back while keeping both versions recoverable until the rename lands
func restorePreviousEntry(store *Store, entry, previous string) error {
	displaced := store.Entry(".displaced-" + randomHex32())
	hadEntry := lexists(entry)
	if hadEntry {
		if err := RetryHeld(func() error { return renameEntry(entry, displaced) }); err != nil {
			return err
		}
	}
	if err := RetryHeld(func() error { return renameEntry(previous, entry) }); err != nil {
		if hadEntry {
			if rerr := RetryHeld(func() error { return renameEntry(displaced, entry) }); rerr != nil {
				return rerr
			}
		}
		return err
	}
	if hadEntry {
		discardEntry(store, filepath.Base(displaced))
	}
	return nil
}

// Publishes a staged tree with rollback kept live through the caller's facts commit
func publishEntry(p Package, store *Store, staged, entry, previousEntry, target string, commit func() error) error {
	keptLive := false
	if lexists(entry) {
		if renameErr := RetryHeld(func() error { return renameEntry(entry, previousEntry) }); renameErr != nil {
			matches := false
			if p.Verify(entry, target) == "" {
				live, lerr := TreeDigest(entry)
				if lerr != nil {
					return renameErr
				}
				fresh, serr := TreeDigest(staged)
				if serr != nil {
					return renameErr
				}
				matches = live == fresh
			}
			if !matches {
				return renameErr
			}
			slog.Warn(fmt.Sprintf("%s entry is in use but already matches the staged bytes; keeping it live", p.Base().Name))
			keptLive = true
		}
	}
	err := func() error {
		defer func() {
			if r := recover(); r != nil {
				if pathExists(previousEntry) {
					if rerr := restorePreviousEntry(store, entry, previousEntry); rerr != nil {
						panic(rerr)
					}
				}
				panic(r)
			}
		}()
		if !keptLive {
			if _, err := publishStaged(store, staged, filepath.Base(entry)); err != nil {
				return err
			}
		}
		if reason := p.Verify(entry, target); reason != "" {
			return NewInstallError(p.Base().Name, "published entry failed verification: "+reason, "")
		}
		return commit()
	}()
	if err != nil {
		if pathExists(previousEntry) {
			if rerr := restorePreviousEntry(store, entry, previousEntry); rerr != nil {
				return rerr
			}
		}
		return err
	}
	if pathExists(previousEntry) {
		discardEntry(store, filepath.Base(previousEntry))
	}
	return nil
}

// Finishes or undoes a publication an earlier install left behind
func settlePreviousEntry(p Package, store *Store, entry, previousEntry string, previous map[string]any, target string) error {
	if !pathExists(previousEntry) {
		return nil
	}
	if previous != nil && jsonEquals(previous["entry"], filepath.Base(entry)) && entryVerified(p, previous, store, target) {
		discardEntry(store, filepath.Base(previousEntry))
		return nil
	}
	return restorePreviousEntry(store, entry, previousEntry)
}

// Reports an entry already realizing the pin, through host facts or a cross target stage marker
func entryCurrent(p Package, lockfile *Lockfile, facts *Facts, store *Store, entry string, previous map[string]any, version, pin, target string) (bool, error) {
	name := p.Base().Name
	if facts != nil {
		return previous != nil && facts.Installed(name, &version, store.Root, pinIdentity(lockfile, name, target)) &&
			entryVerified(p, previous, store, target), nil
	}
	data, err := os.ReadFile(filepath.Join(entry, ".pm-stage-pin.json"))
	if err != nil {
		return false, nil
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if err := strictUTF8(data); err != nil {
		return false, err
	}
	return string(data) == pin && p.Verify(entry, target) == "", nil
}

// Reports equal env diffs, list values compared item by item and scalars by their text
func envDiffEqual(a, b EnvDiff) bool {
	if len(a) != len(b) {
		return false
	}
	for key, av := range a {
		bv, ok := b[key]
		if !ok {
			return false
		}
		al, aList := envList(av)
		bl, bList := envList(bv)
		if aList != bList {
			return false
		}
		if aList && !slices.Equal(al, bl) {
			return false
		}
		if !aList && pyrepr.Str(av) != pyrepr.Str(bv) {
			return false
		}
	}
	return true
}

// String value of a fact field, nil when absent or not a string
func optionalFactString(fact map[string]any, key string) *string {
	if s, ok := fact[key].(string); ok {
		return &s
	}
	return nil
}

// Re-records an entry's env when package code changed it, the recorded digest still vouching for its bytes
func refreshRecordedEnv(p Package, facts *Facts, store *Store, entry string, previous map[string]any, target string) error {
	name := p.Base().Name
	env, err := PackageEnv(p, entry, target)
	if err != nil {
		return err
	}
	if envDiffEqual(facts.EnvFor(name, store.Root), env) {
		return nil
	}
	record := FactRecord{Target: optionalFactString(previous, "target"), Digest: optionalFactString(previous, "digest")}
	if raw, ok := previous["artifacts"]; ok && raw != nil {
		list, err := pyIterStrings(raw)
		if err != nil {
			return err
		}
		record.Artifacts = list
	}
	return facts.Record(name, pyrepr.Str(previous["version"]), pyrepr.Str(previous["entry"]), env, store.Root, record)
}

// Re-records a found entry's env in the writable store under its install lock
func refreshFoundEnv(ctx context.Context, p Package, loc *Location, target string, op *installOperation) error {
	name := p.Base().Name
	fact, _ := loc.Facts.Get(name)
	entry := loc.Store.Entry(pyrepr.Str(fact["entry"]))
	writable, err := WritableStoreRoot(ctx)
	if err != nil {
		return err
	}
	env, err := PackageEnv(p, entry, target)
	if err != nil {
		return err
	}
	if envDiffEqual(loc.Facts.EnvFor(name, loc.Store.Root), env) || loc.Store.Root != NewStore(writable).Root {
		return nil
	}
	if op == nil {
		release, err := loc.Store.InstallLock()
		if err != nil {
			return err
		}
		defer release()
	}
	if err := loc.Facts.Reload(); err != nil {
		return err
	}
	previous, ok := loc.Facts.Get(name)
	if ok && len(previous) > 0 && jsonEquals(previous["entry"], filepath.Base(entry)) {
		return refreshRecordedEnv(p, loc.Facts, loc.Store, entry, previous, target)
	}
	return nil
}

// Copies a verified bundled entry into staging and checks the copy against its recorded digest
func copyVerifiedSource(p Package, lockfile *Lockfile, copyFrom *Location, staged, version, target string) error {
	name := p.Base().Name
	source, _ := copyFrom.Facts.Get(name)
	if !copyFrom.Facts.Installed(name, &version, copyFrom.Store.Root, pinIdentity(lockfile, name, target)) ||
		!entryVerified(p, source, copyFrom.Store, target) {
		return NewInstallError(name, "bundled copy source failed verification", "")
	}
	if err := copyTreeSymlinks(copyFrom.Store.Entry(pyrepr.Str(source["entry"])), staged); err != nil {
		return err
	}
	recorded := factDigest(source)
	matches, err := DigestMatches(staged, recorded)
	if err != nil {
		return err
	}
	if !matches {
		return NewInstallError(name, "copied bytes do not match the bundled source", "")
	}
	return nil
}

// Copies a tree the way shutil copytree with symlinks does, links recreated and file modes and times kept
func copyTreeSymlinks(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(dst, st.Mode().Perm()); err != nil {
		return err
	}
	items, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, item := range items {
		from, to := filepath.Join(src, item.Name()), filepath.Join(dst, item.Name())
		info, err := os.Lstat(from)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(from)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, to); err != nil {
				return err
			}
		case info.IsDir():
			if err := copyTreeSymlinks(from, to); err != nil {
				return err
			}
		default:
			if err := copyFile(from, to); err != nil {
				return err
			}
			if err := os.Chmod(to, info.Mode().Perm()); err != nil {
				return err
			}
			if err := os.Chtimes(to, info.ModTime(), info.ModTime()); err != nil {
				return err
			}
		}
	}
	if err := os.Chmod(dst, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, st.ModTime(), st.ModTime())
}

// First n characters of a string, counted in code points
func pyPrefix(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		runes = runes[:n]
	}
	return string(runes)
}

// Logs a replaced established fact as a repair naming the old and new pins
func logRepair(p Package, previous map[string]any, version string, artifacts []map[string]any) {
	if previous == nil {
		return
	}
	if _, ok := previous["entry"]; !ok {
		return
	}
	oldArtifact := any("?")
	if list, ok := previous["artifacts"].([]any); ok && len(list) > 0 {
		oldArtifact = list[0]
	} else if pyrepr.Truthy(previous["artifacts"]) {
		if items, err := pyIterStrings(previous["artifacts"]); err == nil && len(items) > 0 {
			oldArtifact = items[0]
		}
	}
	oldVersion := "?"
	if v, ok := previous["version"]; ok {
		oldVersion = pyrepr.Str(v)
	}
	old := oldVersion + "/" + pyPrefix(pyrepr.Str(oldArtifact), 12)
	fresh := version
	if len(artifacts) > 0 {
		fresh = version + "/" + pyPrefix(pyrepr.Str(artifacts[0]["sha256"]), 12)
	}
	slog.Info(fmt.Sprintf("repair: %s re-realized %s -> %s", p.Base().Name, old, fresh))
}

// Reports a pure pin package, one that stages no bytes because its consumers verify the digest
func pinOnly(p Package) bool {
	po, ok := p.(PinOnlyPackage)
	return ok && po.PinOnly()
}

// Reports whether a package contributes PATH dirs, honoring a concrete package's own answer
func onPath(p Package) bool {
	if op, ok := p.(interface{ OnPath() bool }); ok {
		return op.OnPath()
	}
	return !p.Base().OffPath
}

// Reports an install failure that keeps its own class instead of becoming an install failed wrap
func isInstallError(err error) bool {
	var ie *InstallError
	return errors.As(err, &ie)
}

// Pin marker JSON a cross target stage carries, target first as Python orders it
func stagePin(target string, artifacts []map[string]any) (string, error) {
	shas := make([]any, len(artifacts))
	for i, a := range artifacts {
		shas[i] = pyrepr.Str(a["sha256"])
	}
	pin := NewOrderedObject()
	pin.Set("target", target)
	pin.Set("sha256", shas)
	out, err := dumpsJSON(pin, false)
	return string(out), err
}

// Archives of a fetch plan in lockfile order
func artifactPlan(artifacts []map[string]any) []Artifact {
	out := make([]Artifact, len(artifacts))
	for i, a := range artifacts {
		out[i] = Artifact{URL: pyrepr.Str(a["url"]), SHA256: pyrepr.Str(a["sha256"])}
	}
	return out
}

// Label of one archive of a multi archive plan, empty for a single archive
func archiveLabel(index, count int) string {
	if count > 1 {
		return fmt.Sprintf("%d/%d", index+1, count)
	}
	return ""
}

// Fetches, unpacks, merges and stages a pin's archives, reporting progress with lockfile order labels
func prepareArtifacts(p Package, store *Store, scratch string, artifacts []map[string]any, version, target string, opts InstallPackageOptions) (string, error) {
	tick := func(done, total int64, ranges []RangeSet) error {
		if opts.Progress != nil {
			if len(ranges) == 0 {
				return errors.New("")
			}
			active := ranges[len(ranges)-1].Key
			index := -1
			for i, artifact := range artifacts {
				if pyrepr.Str(artifact["url"]) == active {
					index = i
					break
				}
			}
			if index < 0 {
				return errors.New("")
			}
			if err := opts.Progress("download", done, total, archiveLabel(index, len(artifacts))); err != nil {
				return err
			}
		}
		if opts.DownloadProgress != nil {
			return opts.DownloadProgress(done, total, ranges)
		}
		return nil
	}
	staged := filepath.Join(scratch, "tree")
	archives, err := fetchArtifacts(store, artifactPlan(artifacts), scratch, tick, opts.PauseEvent)
	if err != nil {
		return "", err
	}
	for index, archive := range archives {
		if opts.PauseEvent != nil && opts.PauseEvent.IsSet() {
			return "", downloadError(DownloadWasPaused, "install paused")
		}
		if opts.Progress != nil {
			if err := opts.Progress("unpack", 0, 0, archiveLabel(index, len(artifacts))); err != nil {
				return "", err
			}
		}
		destination := staged
		if index > 0 {
			destination = filepath.Join(scratch, fmt.Sprintf("extra-%d", index))
		}
		if err := p.Unpack(archive, destination, target); err != nil {
			return "", err
		}
		if index > 0 {
			if err := MergeTree(destination, staged); err != nil {
				return "", err
			}
		}
	}
	if err := p.Stage(store, staged, version, target); err != nil {
		return "", err
	}
	return staged, nil
}

// InstallPackageOptions report progress, pause, copy from a verified source, reuse a held lock and force a fresh copy
type InstallPackageOptions struct {
	Progress         InstallProgress
	PauseEvent       *PauseEvent
	DownloadProgress ProgressFunc
	CopyFrom         *Location
	LockHeld         bool
	FreshCopy        bool
}

// Realizes one pin, host installs committing facts and cross target stages carrying a pin marker
func InstallPackage(ctx context.Context, p Package, lockfile *Lockfile, facts *Facts, store *Store, target string, opts InstallPackageOptions) (string, error) {
	name := p.Base().Name
	version, pinned := lockfile.Version(name)
	if !pinned {
		return "", NewInstallError(name, "not in the lockfile", "add it with `icebath pm lock --bump`")
	}
	if reason := p.MissingReason(target); reason != "" {
		return "", NewInstallError(name, fmt.Sprintf("unavailable on %s: %s", target, reason), "none")
	}
	entryName := p.StoreEntry(version, target)
	entry := store.Entry(entryName)
	if pinOnly(p) {
		return entry, nil
	}
	artifacts := lockfile.Artifacts(name, target)
	pin, err := stagePin(target, artifacts)
	if err != nil {
		return "", err
	}
	if !opts.LockHeld {
		release, err := store.InstallLock()
		if err != nil {
			return "", err
		}
		defer release()
	}
	if opts.PauseEvent != nil && opts.PauseEvent.IsSet() {
		return "", downloadError(DownloadWasPaused, "install paused")
	}
	var previous map[string]any
	if facts != nil {
		if err := facts.Reload(); err != nil {
			return "", err
		}
		previous, _ = facts.Get(name)
	}
	prefix := ".previous-"
	if facts == nil {
		prefix += "stage-"
	}
	previousEntry := store.Entry(prefix + entryName)
	if err := settlePreviousEntry(p, store, entry, previousEntry, previous, target); err != nil {
		return "", err
	}
	reclaimSetAside(store)
	current, err := entryCurrent(p, lockfile, facts, store, entry, previous, version, pin, target)
	if err != nil {
		return "", err
	}
	if current && !opts.FreshCopy {
		if facts != nil {
			if err := refreshRecordedEnv(p, facts, store, entry, previous, target); err != nil {
				return "", err
			}
		}
		removeDownloads(store, artifacts)
		return entry, nil
	}
	if len(artifacts) == 0 {
		return "", NewInstallError(name, fmt.Sprintf("no artifact for %s in the lockfile", target),
			"run `icebath pm lock --bump` for this package")
	}
	scratch, cleanup, err := store.Scratch()
	if err != nil {
		return "", err
	}
	defer cleanup()
	err = func() error {
		staged := filepath.Join(scratch, "tree")
		if opts.CopyFrom != nil {
			if err := copyVerifiedSource(p, lockfile, opts.CopyFrom, staged, version, target); err != nil {
				return err
			}
		} else {
			prepared, err := prepareArtifacts(p, store, scratch, artifacts, version, target, opts)
			if err != nil {
				return err
			}
			staged = prepared
		}
		if opts.PauseEvent != nil && opts.PauseEvent.IsSet() {
			return downloadError(DownloadWasPaused, "install paused")
		}
		if opts.Progress != nil {
			if err := opts.Progress("verify", 0, 0, ""); err != nil {
				return err
			}
		}
		reason, remedy := p.Verify(staged, target), ""
		if reason != "" {
			reason, remedy = p.RepairStagedVerification(staged, target, reason)
		}
		if reason != "" {
			return NewInstallError(name, "staged entry failed verification: "+reason, remedy)
		}
		if facts == nil {
			if err := os.WriteFile(filepath.Join(staged, ".pm-stage-pin.json"), []byte(pin), 0o666); err != nil {
				return err
			}
		}
		if err := publishEntry(p, store, staged, entry, previousEntry, target, func() error {
			if facts == nil {
				return nil
			}
			digest, err := TreeDigest(entry)
			if err != nil {
				return err
			}
			shas := make([]string, len(artifacts))
			for i, a := range artifacts {
				shas[i] = pyrepr.Str(a["sha256"])
			}
			env, err := PackageEnv(p, entry, target)
			if err != nil {
				return err
			}
			return recordToolFact(facts, name, version, entryName, env, store.Root,
				FactRecord{Target: &target, Artifacts: shas, Digest: &digest})
		}); err != nil {
			return err
		}
		removeDownloads(store, artifacts)
		return nil
	}()
	if err != nil {
		if isInstallError(err) || IsDownloadKind(err, DownloadWasPaused) {
			return "", err
		}
		return "", NewInstallError(name, "install failed: "+err.Error(), "")
	}
	logRepair(p, previous, version, artifacts)
	return entry, nil
}

// StageOnlyOptions report the slow stages of a cross target stage and may pause it
type StageOnlyOptions struct {
	Progress         InstallProgress
	PauseEvent       *PauseEvent
	DownloadProgress ProgressFunc
}

// Realizes a cross target pin without host facts or an executable runner
func stageOnly(ctx context.Context, name, target string, opts StageOnlyOptions) (string, error) {
	p, err := GetPackage(name)
	if err != nil {
		return "", err
	}
	store, err := defaultStore(ctx)
	if err != nil {
		return "", err
	}
	return InstallPackage(ctx, p, currentLockfile(), nil, store, target,
		InstallPackageOptions{Progress: opts.Progress, PauseEvent: opts.PauseEvent, DownloadProgress: opts.DownloadProgress})
}

// One operation's hold on the writable store's install lock, observations valid only while it is held
type installOperation struct {
	ctx     context.Context
	store   *Store
	release func()
	checked map[string]bool
}

// Operation that takes the writable store lock on first use
func newInstallOperation(ctx context.Context) *installOperation {
	return &installOperation{ctx: ctx, checked: map[string]bool{}}
}

// Writable store with its install lock held for the rest of the operation
func (o *installOperation) lock() (*Store, error) {
	if o.store == nil {
		root, err := WritableStoreRoot(o.ctx)
		if err != nil {
			return nil, err
		}
		store := NewStore(root)
		release, err := store.InstallLock()
		if err != nil {
			return nil, err
		}
		o.store, o.release = store, release
	}
	return o.store, nil
}

// Drops the lock and every observation made under it
func (o *installOperation) Close() {
	clear(o.checked)
	o.store = nil
	if o.release != nil {
		o.release()
		o.release = nil
	}
}

// EnsureOptions choose the base env, the explicit and verify policy, progress reporting and cancellation
type EnsureOptions struct {
	BaseEnv          map[string]string
	Explicit         bool
	SkipVerify       bool
	Progress         InstallProgress
	PauseEvent       *PauseEvent
	DownloadProgress ProgressFunc
	operation        *installOperation
}

// Observation key of a package pin on a target
func checkedKey(lockfile *Lockfile, name, target string) string {
	version := any(nil)
	if v, ok := lockfile.Version(name); ok {
		version = v
	}
	identity := any(nil)
	if id := pinIdentity(lockfile, name, target); id != nil {
		shas := make([]any, len(id.SHA256s))
		for i, s := range id.SHA256s {
			shas[i] = s
		}
		identity = []any{id.Target, shas}
	}
	encoded, _ := dumpsJSON([]any{name, version, target, identity}, false)
	return string(encoded)
}

// Makes the installed state match the lockfile for a package closure and returns its composed runner
func ensure(ctx context.Context, name string, opts EnsureOptions) (*Runner, error) {
	p, err := GetPackage(name)
	if err != nil {
		return nil, err
	}
	op := opts.operation
	if _, state := p.(StatePackage); state {
		if op != nil {
			op.Close()
		}
		if err := syncVenv(ctx, SyncVenvOptions{Explicit: opts.Explicit}); err != nil {
			return nil, err
		}
		return &Runner{Name: name, Env: ComposeEnv(nil, opts.BaseEnv)}, nil
	}
	lockfile := currentLockfile()
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	chain, err := Walk([]string{name})
	if err != nil {
		return nil, err
	}
	checked := map[string]bool{}
	if op != nil {
		checked = op.checked
		if _, err := op.lock(); err != nil {
			return nil, err
		}
	}
	var missing []Package
	for _, pkg := range chain {
		key := checkedKey(lockfile, pkg.Base().Name, target)
		if checked[key] {
			continue
		}
		loc, err := InstalledLocation(ctx, pkg, lockfile, target, LocationOptions{Verify: opts.Explicit && !opts.SkipVerify})
		if err != nil {
			return nil, err
		}
		if loc == nil {
			missing = append(missing, pkg)
			continue
		}
		if err := refreshFoundEnv(ctx, pkg, loc, target, op); err != nil {
			return nil, err
		}
		checked[key] = true
	}
	if len(missing) > 0 && !opts.Explicit && !LazyInstallsAllowedFor(ctx) {
		return nil, RefuseLazyFor(ctx, name, packageNames(missing))
	}
	if len(missing) > 0 {
		var store *Store
		if op != nil {
			if store, err = op.lock(); err != nil {
				return nil, err
			}
		} else {
			root, err := WritableStoreRoot(ctx)
			if err != nil {
				return nil, err
			}
			store = NewStore(root)
		}
		own, err := defaultStore(ctx)
		if err != nil {
			return nil, err
		}
		var facts *Facts
		if store.Root == own.Root {
			facts, err = storeFacts(ctx)
		} else {
			facts, err = NewFacts(filepath.Join(store.Root, "facts.json"), false)
		}
		if err != nil {
			return nil, err
		}
		for _, pkg := range missing {
			clear(checked)
			if _, err := InstallPackage(ctx, pkg, lockfile, facts, store, target, InstallPackageOptions{
				Progress: opts.Progress, PauseEvent: opts.PauseEvent, DownloadProgress: opts.DownloadProgress, LockHeld: op != nil,
			}); err != nil {
				return nil, err
			}
		}
	}
	env, err := envFor(ctx, []string{name}, opts.BaseEnv)
	if err != nil {
		return nil, err
	}
	return &Runner{Name: name, Env: env}, nil
}

// Comma separated package names
func packageNames(list []Package) string {
	names := make([]string, len(list))
	for i, p := range list {
		names[i] = p.Base().Name
	}
	return strings.Join(names, ", ")
}

// Composed env of installed packages only, never installing and skipping unknown or missing ones
func envFor(ctx context.Context, names []string, base map[string]string) (map[string]string, error) {
	lockfile := currentLockfile()
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	var diffs []EnvDiff
	for _, name := range names {
		chain, err := Walk([]string{name})
		var unknown *UnknownPackageError
		if errors.As(err, &unknown) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, p := range chain {
			if p.Base().Internal {
				continue
			}
			loc, err := InstalledLocation(ctx, p, lockfile, target, LocationOptions{})
			if err != nil {
				return nil, err
			}
			if loc != nil {
				diffs = append(diffs, loc.Facts.EnvFor(p.Base().Name, loc.Store.Root))
			}
		}
	}
	return ComposeEnv(diffs, base), nil
}

// Composed env of already installed packages over base, raising only when the store itself cannot be read
func EnvFor(names []string, base map[string]string) map[string]string {
	env, err := envFor(context.Background(), names, base)
	if err != nil {
		panic(err)
	}
	return env
}

// Strings of a decoded JSON iterable as Python iterates it, a string yielding its characters
func pyIterStrings(v any) ([]string, error) {
	switch t := v.(type) {
	case []string:
		return append([]string{}, t...), nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("expected string or bytes-like object, got '%s'", pyrepr.TypeName(item))}
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		out := []string{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		return sortedKeys(t), nil
	}
	return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))}
}

// DriftReport maps each unhealthy package to its reason in the order the problems were found
type DriftReport struct {
	Names   []string
	Reasons map[string]string
}

// Records a problem, a repeated name keeping its first position as a dict assignment does
func (d *DriftReport) set(name, reason string) {
	if _, ok := d.Reasons[name]; !ok {
		d.Names = append(d.Names, name)
	}
	d.Reasons[name] = reason
}

// Exception classes a drift venv probe propagates instead of reporting as the venv's reason
var driftPropagates = map[string]bool{"TypeError": true, "KeyError": true, "AttributeError": true}

// Cheap stamp comparisons of installed state against the lockfile, never installing or fetching
func Drift(ctx context.Context, includeVenv bool) (*DriftReport, error) {
	report := &DriftReport{Reasons: map[string]string{}}
	factsPath, err := FactsPath(ctx)
	if err != nil {
		return nil, err
	}
	if !isRegularFile(factsPath) && !isRegularFile(RuntimeFactsPath(ctx)) {
		return report, nil
	}
	lockfile := currentLockfile()
	facts, err := storeFacts(ctx)
	if err != nil {
		return nil, err
	}
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	for _, name := range lockfile.Names() {
		p, err := GetPackage(name)
		if err != nil {
			continue
		}
		if p.Base().Optional || p.Base().Internal || p.MissingReason(target) != "" {
			continue
		}
		loc, err := InstalledLocation(ctx, p, lockfile, target, LocationOptions{})
		if err != nil {
			return nil, err
		}
		if loc == nil {
			report.set(name, "not installed or outdated")
		}
	}
	_, venvErr := GetPackage("venv")
	recordedVenv := facts.packages["venv"] != nil
	if includeVenv && venvErr == nil && (isRegularFile(RuntimeFactsPath(ctx)) || recordedVenv) {
		current, err := venvIsCurrent(ctx, nil, nil, "")
		switch {
		case err != nil && driftPropagates[errorClass(err)]:
			return nil, err
		case err != nil:
			report.set("venv", err.Error())
		case !current:
			report.set("venv", "out of sync with uv.lock")
		}
	}
	return report, nil
}

// Human readable startup diagnostics, one name and reason per problem
func Check(ctx context.Context, includeVenv bool) ([]string, error) {
	report, err := Drift(ctx, includeVenv)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(report.Names))
	for _, name := range report.Names {
		out = append(out, name+": "+report.Reasons[name])
	}
	return out, nil
}

// Names with each package moved ahead of its dependencies, unrelated packages keeping their order
func dependentsFirst(names []string) ([]string, error) {
	order := append([]string{}, names...)
	for _, name := range names {
		chain, err := Walk([]string{name})
		var unknown *UnknownPackageError
		if errors.As(err, &unknown) {
			continue
		}
		if err != nil {
			return nil, err
		}
		lowest := -1
		for _, dep := range chain {
			if dep.Base().Name == name {
				continue
			}
			if i := slices.Index(order, dep.Base().Name); i >= 0 && (lowest < 0 || i < lowest) {
				lowest = i
			}
		}
		if lowest >= 0 && lowest < slices.Index(order, name) {
			order = slices.Delete(order, slices.Index(order, name), slices.Index(order, name)+1)
			order = slices.Insert(order, lowest, name)
		}
	}
	return order, nil
}

// Deduped PATH dirs of installed store packages whose whole chain is installed, dependents first
func StorePathDirs(ctx context.Context) ([]string, error) {
	lockfile := currentLockfile()
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	locations := map[string]*Location{}
	located := func(p Package) (*Location, error) {
		name := p.Base().Name
		if loc, ok := locations[name]; ok {
			return loc, nil
		}
		loc, err := InstalledLocation(ctx, p, lockfile, target, LocationOptions{})
		if err != nil {
			return nil, err
		}
		locations[name] = loc
		return loc, nil
	}
	order, err := dependentsFirst(lockfile.Names())
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, name := range order {
		p, err := GetPackage(name)
		if err != nil {
			continue
		}
		chain, err := Walk([]string{name})
		var unknown *UnknownPackageError
		if errors.As(err, &unknown) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if p.Base().Internal || !onPath(p) || p.MissingReason(target) != "" {
			continue
		}
		loc, err := located(p)
		if err != nil {
			return nil, err
		}
		if loc == nil {
			continue
		}
		complete := true
		for _, dep := range chain {
			if dep.MissingReason(target) != "" {
				continue
			}
			depLoc, err := located(dep)
			if err != nil {
				return nil, err
			}
			if depLoc == nil {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		env := loc.Facts.EnvFor(name, loc.Store.Root)
		var pathDirs []string
		if value, ok := env["PATH"]; ok && truthyEnv(value) {
			pathDirs = envDirs(value)
		}
		for _, directory := range pathDirs {
			if directory != "" && !slices.Contains(dirs, Native(directory)) {
				dirs = append(dirs, Native(directory))
			}
		}
	}
	return dirs, nil
}

// Python truthiness of a resolved env value
func truthyEnv(value any) bool {
	if list, ok := envList(value); ok {
		return len(list) > 0
	}
	return pyrepr.Str(value) != ""
}

// Store verdict activation reports, the engine check unless a caller counts it
var activationCheck = Check

// Puts the installed store's tool dirs at the front of the process PATH and returns the check problems
func Activate(ctx context.Context, allowIncomplete bool) ([]string, error) {
	problems, err := activationCheck(ctx, !allowIncomplete)
	if err != nil {
		return nil, err
	}
	path := os.Getenv("PATH")
	first, err := StoreFirstPath(ctx, path)
	if err != nil {
		return nil, err
	}
	if first != path {
		if err := os.Setenv("PATH", first); err != nil {
			return nil, err
		}
	}
	return problems, nil
}

// PATH with the installed store's tool dirs moved to the front for children that prepend after activation
func StoreFirstPath(ctx context.Context, path string) (string, error) {
	dirs, err := StorePathDirs(ctx)
	if err != nil || len(dirs) == 0 {
		return path, err
	}
	store := map[string]bool{}
	for _, d := range dirs {
		store[strings.ToLower(d)] = true
	}
	parts := append([]string{}, dirs...)
	for _, p := range strings.Split(path, string(os.PathListSeparator)) {
		if p != "" && !store[strings.ToLower(p)] {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, string(os.PathListSeparator)), nil
}
