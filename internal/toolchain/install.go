package toolchain

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Phase of an install a progress report describes
type Stage string

// Waiting on another installer, downloading, unpacking and verifying
const (
	StageWait     Stage = "wait"
	StageDownload Stage = "download"
	StageUnpack   Stage = "unpack"
	StageVerify   Stage = "verify"
)

// One report, Total zero while a size is unknown, Label naming one archive of several or the awaited lock
type Progress struct {
	Name   string
	Stage  Stage
	Done   int64
	Total  int64
	Label  string
	Ranges map[string][]Range
}

// How Ensure may act
type EnsureOptions struct {
	// Deliberate install command, exempt from the lazy install policy, rehashing and repairing an existing install
	Explicit bool
	// Receives progress reports in order from the installing goroutine
	Progress           func(Progress)
	ConfirmHostPackage func(ctx context.Context, p HostPackage) bool
}

// Usable install of a catalog entry
type Installation struct {
	Name    string
	Version string
	// Directory holding the install
	Dir string
	// First entry point for this machine, empty when the entry has none
	Binary string
	// Every entry point for this machine, more than one when the entry ships variants
	Binaries []string
	// False for an install of an earlier pin kept because the caller allowed it
	Current bool
}

// Install of an entry found in one store root
type location struct {
	root    string
	facts   *facts
	fact    fact
	current bool
}

// Digests the lock pins for name on the host, nil when it pins none
func (s *Store) identity(name string, host hostTarget) []string {
	artifacts := s.lock.artifacts(name, host)
	if len(artifacts) == 0 {
		return nil
	}
	shas := make([]string, len(artifacts))
	for i, a := range artifacts {
		shas[i] = a.SHA256
	}
	return shas
}

// Reports an install whose files and recorded digest still hold
func (s *Store) entryVerified(ctx context.Context, r *recipe, row fact, root string, host hostTarget) bool {
	dir := filepath.Join(root, row.Entry)
	return s.verifyEntry(ctx, r, dir, host.target, host.runs()) == "" && digestMatches(dir, row.Digest)
}

// Finds the install of r among the store roots, the current pin first, an earlier pin when allowed
func (s *Store) locate(ctx context.Context, r *recipe, host hostTarget, verify, allowOutdated bool) (*location, error) {
	roots, err := s.searchRoots(ctx)
	if err != nil {
		return nil, err
	}
	version, _ := s.lock.version(r.name)
	identity := s.identity(r.name, host)
	var fallback *location
	for _, root := range roots {
		f := loadFacts(filepath.Join(root, factsName))
		row, ok := f.get(r.name)
		if !ok || !f.installed(r.name, "", nil, host.target, root) {
			continue
		}
		dir := filepath.Join(root, row.Entry)
		if _, pattern := r.binaries(dir, host.target); pattern != "" {
			binary := r.binary(dir, host.target)
			if binary == "" {
				continue
			}
			if r.kind == Program && !healExecBit(binary) {
				continue
			}
		}
		if verify && !s.entryVerified(ctx, r, row, root, host) {
			continue
		}
		if f.installed(r.name, version, identity, host.target, root) {
			return &location{root: root, facts: f, fact: row, current: true}, nil
		}
		if allowOutdated && row.Target == string(host.target) && len(row.Artifacts) > 0 && row.Digest != "" {
			fallback = &location{root: root, facts: f, fact: row}
		}
	}
	return fallback, nil
}

// Describes a located install to a caller
func (s *Store) installation(r *recipe, loc *location, host hostTarget) *Installation {
	dir := filepath.Join(loc.root, loc.fact.Entry)
	binaries, _ := r.binaries(dir, host.target)
	inst := &Installation{Name: r.name, Version: loc.fact.Version, Dir: dir, Binaries: binaries, Current: loc.current}
	if len(binaries) > 0 {
		inst.Binary = binaries[0]
	}
	return inst
}

// Reads the install of name without installing or changing anything, ErrNotInstalled when there is none
func (s *Store) Installed(ctx context.Context, name string, allowOutdated bool) (*Installation, error) {
	r, err := s.recipe(name)
	if err != nil {
		return nil, err
	}
	host, err := s.hostTarget(ctx)
	if err != nil {
		return nil, err
	}
	if r.inHome() {
		return s.homeInstalled(ctx, r, host)
	}
	loc, err := s.locate(ctx, r, host, false, allowOutdated)
	if err != nil {
		return nil, err
	}
	if loc == nil {
		return nil, notInstalled(name)
	}
	return s.installation(r, loc, host), nil
}

// Reports whether installs on first use are allowed, the bridge variable first and config after it
func (s *Store) lazyAllowed(ctx context.Context) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvDisableLazy))) {
	case "1", "true", "yes":
		return false
	}
	allowed, err := s.lazy(ctx)
	return err == nil && allowed
}

// Makes the install of name match its pin and returns it, installing when the policy allows
func (s *Store) Ensure(ctx context.Context, name string, o EnsureOptions) (*Installation, error) {
	r, err := s.recipe(name)
	if err != nil {
		return nil, err
	}
	host, err := s.hostTarget(ctx)
	if err != nil {
		return nil, err
	}
	report := func(p Progress) {
		if o.Progress != nil {
			p.Name = name
			o.Progress(p)
		}
	}
	if r.inHome() {
		return s.homeEnsure(ctx, r, host, report)
	}
	loc, err := s.locate(ctx, r, host, o.Explicit, false)
	if err != nil {
		return nil, err
	}
	if loc != nil {
		if err := s.refreshFoundEnv(ctx, r, loc, host); err != nil {
			return nil, err
		}
		return s.installation(r, loc, host), nil
	}
	if !o.Explicit && !s.lazyAllowed(ctx) {
		return nil, &InstallError{
			Name:   name,
			Cause:  "not installed and lazy installs are disabled: " + name,
			Remedy: "enable security.allow_lazy_installs or run `icebath pm install`",
			Err:    ErrLazyInstallsDisabled,
		}
	}
	s.installBeforeLock(ctx, o.ConfirmHostPackage)
	_, writable, err := s.roots(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.install(ctx, r, host, writable, report); err != nil {
		return nil, err
	}
	loc, err = s.locate(ctx, r, host, false, false)
	if err != nil {
		return nil, err
	}
	if loc == nil {
		return nil, &InstallError{Name: name, Cause: "install published no usable entry"}
	}
	return s.installation(r, loc, host), nil
}

// Rerecords the environment of a found install in the writable store when the recipe now derives another
func (s *Store) refreshFoundEnv(ctx context.Context, r *recipe, loc *location, host hostTarget) error {
	_, writable, err := s.roots(ctx)
	if err != nil {
		return err
	}
	dir := filepath.Join(loc.root, loc.fact.Entry)
	want := r.environment(dir, host.target)
	if loc.root != writable || equalEnv(loc.facts.env(r.name, loc.root), want) {
		return nil
	}
	l, err := s.lockRoot(ctx, loc.root, nil)
	if err != nil {
		return err
	}
	defer l.Release()
	if err := loc.facts.reload(); err != nil {
		return err
	}
	row, ok := loc.facts.get(r.name)
	if !ok || row.Entry != loc.fact.Entry {
		return nil
	}
	row.Env = templatize(want, loc.root)
	return loc.facts.record(ctx, r.name, row)
}

// Reports two environments with the same keys and values
func equalEnv(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, values := range a {
		other, ok := b[key]
		if !ok || !equalStrings(values, other) {
			return false
		}
	}
	return true
}

// Why the lock holds no artifact the host can use, naming the C library release a pinned row needs
func (s *Store) noArtifact(r *recipe, host hostTarget) error {
	if floor, ok := s.lock.glibcFloor(r.name, host.target); ok {
		return &UnavailableError{Name: r.name, Target: host.target, Reason: fmt.Sprintf(
			"the pinned build needs the GNU C library %d.%d or newer and this system has %s", floor[0], floor[1], describeLibC(host))}
	}
	return &InstallError{Name: r.name, Cause: fmt.Sprintf("no artifact for %s in the lock", host.target), Remedy: "none"}
}

// C library release of the host in words
func describeLibC(host hostTarget) string {
	if !host.hasLib {
		return "none this build can read"
	}
	return fmt.Sprintf("%d.%d", host.glibc[0], host.glibc[1])
}

// Realizes the pin of r under the writer lock of root and commits its receipt
func (s *Store) install(ctx context.Context, r *recipe, host hostTarget, root string, report func(Progress)) error {
	_, err := s.realize(ctx, r, host, root, report, realizeMode{})
	return err
}

type realizeMode struct {
	stage    bool
	from     *location
	lockHeld bool
}

func (s *Store) realize(ctx context.Context, r *recipe, host hostTarget, root string, report func(Progress), m realizeMode) (string, error) {
	version, _ := s.lock.version(r.name)
	if reason := r.missing(host.target); reason != "" {
		return "", &UnavailableError{Name: r.name, Target: host.target, Reason: reason}
	}
	entryName := r.directory(version, host.target)
	entry := filepath.Join(root, entryName)
	artifacts := s.lock.artifacts(r.name, host)
	if !m.lockHeld {
		l, err := s.lockRoot(ctx, root, report)
		if err != nil {
			return "", err
		}
		defer l.Release()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var f *facts
	var previous fact
	var hadPrevious bool
	previousName := previousPrefix + entryName
	if m.stage {
		previousName = previousPrefix + stagePrefix + entryName
	} else {
		f = &facts{path: filepath.Join(root, factsName)}
		if err := f.reload(); err != nil {
			return "", err
		}
		previous, hadPrevious = f.get(r.name)
	}
	previousEntry := filepath.Join(root, previousName)
	if err := s.settlePrevious(ctx, r, root, entry, previousEntry, previous, hadPrevious, host); err != nil {
		return "", installFailure(r.name, err)
	}
	reclaimSetAside(ctx, root)
	removeStaleScratch(root)
	if partials, err := s.partialsRoot(ctx); err == nil {
		collectPartials(partials, partialGrace)
	}
	identity := s.identity(r.name, host)
	pin, err := stagePin(host.target, identity)
	if err != nil {
		return "", installFailure(r.name, err)
	}
	if m.from == nil {
		if m.stage && s.stageCurrent(ctx, r, entry, pin, host) {
			s.removeDownloads(ctx, root, artifacts)
			return entry, nil
		}
		if !m.stage && hadPrevious && f.installed(r.name, version, identity, host.target, root) && s.entryVerified(ctx, r, previous, root, host) {
			if want := r.environment(entry, host.target); !equalEnv(f.env(r.name, root), want) {
				previous.Env = templatize(want, root)
				if err := f.record(ctx, r.name, previous); err != nil {
					return "", installFailure(r.name, err)
				}
			}
			s.removeDownloads(ctx, root, artifacts)
			return entry, nil
		}
	}
	if len(artifacts) == 0 {
		return "", s.noArtifact(r, host)
	}
	scratch, err := os.MkdirTemp(root, stagingPrefix)
	if err != nil {
		return "", installFailure(r.name, err)
	}
	defer fileutil.RemoveTree(scratch, true)
	st := &staging{store: s, recipe: r, target: host.target, version: version, tree: filepath.Join(scratch, "tree"), scratch: scratch, artifacts: artifacts}
	if m.from != nil {
		if err := s.copyVerifiedSource(ctx, r, m.from, version, host, st.tree); err != nil {
			return "", err
		}
	} else if err := s.prepare(ctx, st, root, report); err != nil {
		return "", installFailure(r.name, err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	report(Progress{Stage: StageVerify})
	reason, remedy := s.verifyEntry(ctx, r, st.tree, host.target, host.runs()), ""
	if reason != "" {
		reason, remedy = s.repairStaged(ctx, r, st.tree, host, reason)
	}
	if reason != "" {
		return "", &InstallError{Name: r.name, Cause: "staged entry failed verification: " + reason, Remedy: remedy}
	}
	if m.stage {
		if err := os.WriteFile(filepath.Join(st.tree, stagePinName), []byte(pin), 0o666); err != nil {
			return "", installFailure(r.name, err)
		}
	}
	commit := func() error {
		if m.stage {
			return nil
		}
		digest, err := treeDigest(entry, true)
		if err != nil {
			return err
		}
		return f.record(ctx, r.name, fact{
			Entry: entryName, Version: version, Env: templatize(r.environment(entry, host.target), root),
			Target: string(host.target), Artifacts: identity, Digest: digest,
		})
	}
	if err := s.publishEntry(ctx, r, root, st.tree, entry, previousEntry, host, commit); err != nil {
		return "", installFailure(r.name, err)
	}
	s.removeDownloads(ctx, root, artifacts)
	if hadPrevious {
		if previous.Entry != entryName {
			discardEntry(ctx, root, previous.Entry)
		}
		old := "?"
		if len(previous.Artifacts) > 0 {
			old = previous.Artifacts[0][:min(12, len(previous.Artifacts[0]))]
		}
		slog.Info(fmt.Sprintf("repair: %s re-realized %s/%s -> %s/%s", r.name, previous.Version, old, version, artifacts[0].SHA256[:12]))
	}
	return entry, nil
}

// Label of archive index among count for a progress report, empty for a lone archive
func archiveLabel(index, count int) string {
	if count < 2 {
		return ""
	}
	return fmt.Sprintf("%d/%d", index+1, count)
}

// Downloads and verifies every pinned archive, then unpacks and stages the tree
func (s *Store) prepare(ctx context.Context, st *staging, root string, report func(Progress)) error {
	archives, err := s.fetch(ctx, root, st.scratch, st.artifacts, func(done, total int64, active string, ranges map[string][]Range) {
		index := 0
		for i, a := range st.artifacts {
			if a.URL == active {
				index = i
				break
			}
		}
		report(Progress{Stage: StageDownload, Done: done, Total: total, Label: archiveLabel(index, len(st.artifacts)), Ranges: ranges})
	})
	if err != nil {
		return err
	}
	r := st.recipe
	for index, archive := range archives {
		if err := ctx.Err(); err != nil {
			return err
		}
		report(Progress{Stage: StageUnpack, Label: archiveLabel(index, len(archives))})
		dest := st.tree
		if index > 0 {
			dest = filepath.Join(st.scratch, fmt.Sprintf("extra-%d", index))
		}
		merge := index > 0
		if r.unpack != nil {
			merge, err = r.unpack(ctx, st, index, archive, dest)
		} else {
			err = extract(ctx, r.name, archive, dest)
		}
		if err != nil {
			return err
		}
		if merge {
			if err := mergeTree(ctx, dest, st.tree); err != nil {
				return err
			}
		}
	}
	packaged := strings.HasSuffix(strings.ToLower(st.artifacts[0].fileName()), ".deb")
	if r.flatten && !(st.target.bionic() && packaged) {
		if err := flattenSingleDir(ctx, st.tree); err != nil {
			return err
		}
	}
	if r.stage != nil {
		if err := r.stage(ctx, st); err != nil {
			return err
		}
	}
	return finishPlatform(ctx, st.tree, r.binaryCandidates(st.tree, st.target))
}

// Entry points of a staged tree worth a platform signature check
func (r *recipe) binaryCandidates(dir string, target Target) []string {
	found, _ := r.binaries(dir, target)
	return found
}

// Fetches the pinned artifacts into the download cache of root and returns the verified archive paths in order
func (s *Store) fetch(ctx context.Context, root, scratch string, artifacts []Artifact, tick downloadTick) ([]string, error) {
	partials, err := s.partialsRoot(ctx)
	if err != nil {
		return nil, err
	}
	sources := make([]Source, len(artifacts))
	urls := map[string]string{}
	for i, a := range artifacts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cache := filepath.Join(root, fetchPrefix+a.SHA256)
		dest := filepath.Join(scratch, fetchPrefix+a.SHA256, a.fileName())
		if entries, err := os.ReadDir(cache); err == nil && len(entries) == 1 && entries[0].Type().IsRegular() {
			dest = filepath.Join(cache, entries[0].Name())
		} else if _, statErr := os.Lstat(cache); statErr == nil {
			if err := removeAll(cache); err != nil {
				return nil, err
			}
		}
		sources[i] = s.pinnedSource(a, dest)
		urls[dest] = a.URL
	}
	runErr := s.downloader(partials, 0).run(ctx, sources, func(done, total int64, active string, ranges map[string][]Range) {
		if tick == nil {
			return
		}
		byURL := make(map[string][]Range, len(ranges))
		for dest, rows := range ranges {
			byURL[urls[dest]] = rows
		}
		tick(done, total, urls[active], byURL)
	})
	paths := make([]string, len(sources))
	for i, src := range sources {
		cache := filepath.Join(root, fetchPrefix+src.SHA256)
		if rel, err := filepath.Rel(scratch, src.Dest); err == nil && !strings.HasPrefix(rel, "..") {
			if fi, err := os.Stat(src.Dest); err == nil && fi.Mode().IsRegular() {
				if err := publish(ctx, filepath.Dir(src.Dest), cache); err != nil && runErr == nil {
					runErr = err
				}
			}
		}
		paths[i] = filepath.Join(cache, filepath.Base(src.Dest))
	}
	if runErr != nil {
		return nil, runErr
	}
	return paths, nil
}

// Releases the cached archives of a published install
func (s *Store) removeDownloads(ctx context.Context, root string, artifacts []Artifact) {
	for _, a := range artifacts {
		discardEntry(ctx, root, fetchPrefix+a.SHA256)
	}
}

// Finishes or undoes a publication an interrupted install left behind
func (s *Store) settlePrevious(ctx context.Context, r *recipe, root, entry, previousEntry string, previous fact, had bool, host hostTarget) error {
	if _, err := os.Lstat(previousEntry); err != nil {
		return nil
	}
	if had && previous.Entry == filepath.Base(entry) && s.entryVerified(ctx, r, previous, root, host) {
		discardEntry(ctx, root, filepath.Base(previousEntry))
		return nil
	}
	return restorePrevious(ctx, root, entry, previousEntry)
}

// Puts the earlier directory back, keeping both recoverable until the rename lands
func restorePrevious(ctx context.Context, root, entry, previous string) error {
	aside, err := randomName()
	if err != nil {
		return err
	}
	displaced := filepath.Join(root, displacedPrefix+aside)
	_, statErr := os.Lstat(entry)
	had := statErr == nil
	if had {
		if err := renameHeld(ctx, entry, displaced); err != nil {
			return err
		}
	}
	if err := renameHeld(ctx, previous, entry); err != nil {
		if had {
			return errors.Join(err, renameHeld(ctx, displaced, entry))
		}
		return err
	}
	if had {
		discardEntry(ctx, root, filepath.Base(displaced))
	}
	return nil
}

// Swaps the staged tree in, verifies it where it now lives, commits its receipt and rolls back on any failure
func (s *Store) publishEntry(ctx context.Context, r *recipe, root, staged, entry, previousEntry string, host hostTarget, commit func() error) error {
	keptLive := false
	if _, err := os.Lstat(entry); err == nil {
		if renameErr := renameHeld(ctx, entry, previousEntry); renameErr != nil {
			if s.verifyEntry(ctx, r, entry, host.target, host.runs()) != "" {
				return renameErr
			}
			live, errA := treeDigest(entry, true)
			fresh, errB := treeDigest(staged, true)
			if errA != nil || errB != nil || live != fresh {
				return renameErr
			}
			slog.Warn(fmt.Sprintf("%s entry is in use but already matches the staged bytes; keeping it live", r.name))
			keptLive = true
		}
	}
	err := func() error {
		if !keptLive {
			if err := publish(ctx, staged, entry); err != nil {
				return err
			}
		}
		if reason := s.verifyEntry(ctx, r, entry, host.target, host.runs()); reason != "" {
			return &InstallError{Name: r.name, Cause: "published entry failed verification: " + reason}
		}
		return commit()
	}()
	_, statErr := os.Lstat(previousEntry)
	hasPrevious := statErr == nil || !errors.Is(statErr, fs.ErrNotExist)
	if err != nil {
		if hasPrevious {
			if restoreErr := restorePrevious(ctx, root, entry, previousEntry); restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
		}
		return err
	}
	if hasPrevious {
		discardEntry(ctx, root, filepath.Base(previousEntry))
	}
	return nil
}
