package skills

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Marker file that limits sync to the essential skills on an opted out profile
const NoBundledSkillsMarker = ".no-bundled-skills"

// Seams for the packaged tree copy and the guarded removal so tests can make either fail
var syncSteps = struct {
	copyTree func(t skillTree, dir, dest string, dirsExistOK, keepExisting bool) error
	rmtree   func(s *Scope, p string) error
}{copyTree: skillTree.copyDir, rmtree: (*Scope).rmtreeWritable}

// Writer sync progress lines go to, stdout in a running process
var syncOut io.Writer = os.Stdout

// Generated runtime state directories that never count as skill content
var runtimeCacheDirs = map[string]bool{"__pycache__": true, ".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true}

// A packaged skills tree, a directory named by an env override or the copy embedded in the binary
type skillTree struct {
	fsys fs.FS
	root string
	tag  string
}

// Packaged tree from the env override, else the embedded copy
func packagedTree(envVar, tag string, embedded func() fs.FS) skillTree {
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return skillTree{fsys: os.DirFS(v), root: v}
	}
	return skillTree{fsys: embedded(), tag: tag}
}

// Bundled skills source tree
func bundledTree() skillTree { return packagedTree("ICEBATH_BUNDLED_SKILLS", "bundled", BundledFS) }

// Official optional skills source tree
func optionalTree() skillTree { return packagedTree("ICEBATH_OPTIONAL_SKILLS", "optional", OptionalFS) }

// Reports whether the tree root exists
func (t skillTree) exists() bool {
	if t.root == "" {
		return true
	}
	_, err := os.Stat(t.root)
	return err == nil
}

// Path of a tree entry for messages and results
func (t skillTree) display(rel string) string {
	if t.root == "" {
		return t.tag + "/" + rel
	}
	return filepath.Join(t.root, filepath.FromSlash(rel))
}

// Every entry path under dir in an fs tree without following symlinked directories
func walkEntries(fsys fs.FS, dir string) []string {
	var out []string
	fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != dir {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// Sorts slash paths the way pathlib orders them, component by component
func sortByParts(paths []string) {
	sort.SliceStable(paths, func(i, j int) bool {
		return slices.Compare(strings.Split(paths[i], "/"), strings.Split(paths[j], "/")) < 0
	})
}

// Slash path of p relative to dir inside an fs tree
func relTo(dir, p string) string {
	if dir == "." {
		return p
	}
	return strings.TrimPrefix(p, dir+"/")
}

// Reports whether a following stat of the fs entry is a directory
func fsIsDir(fsys fs.FS, p string) bool {
	st, err := fs.Stat(fsys, p)
	return err == nil && st.IsDir()
}

// Reports whether a following stat of the fs entry is a regular file
func fsIsFile(fsys fs.FS, p string) bool {
	st, err := fs.Stat(fsys, p)
	return err == nil && st.Mode().IsRegular()
}

// Reports whether a skill relative path is disposable Python or tool runtime state
func isRuntimeCacheFS(fsys fs.FS, p, skillDir string) bool {
	parts := strings.Split(relTo(skillDir, p), "/")
	for _, part := range parts[:len(parts)-1] {
		if runtimeCacheDirs[part] {
			return true
		}
	}
	name := path.Base(p)
	if runtimeCacheDirs[name] && fsIsDir(fsys, p) {
		return true
	}
	suffix := pypath.PathSuffix(name)
	return (suffix == ".pyc" || suffix == ".pyo") && fsIsFile(fsys, strings.TrimSuffix(p, suffix)+".py")
}

// MD5 of package paths and content in a tree directory, runtime caches excluded unless asked
func dirHashFS(fsys fs.FS, dir string, includeRuntimeCache bool) string {
	h := md5.New()
	entries := walkEntries(fsys, dir)
	sortByParts(entries)
	for _, p := range entries {
		if (includeRuntimeCache || !isRuntimeCacheFS(fsys, p, dir)) && fsIsFile(fsys, p) {
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				break
			}
			h.Write([]byte(filepath.FromSlash(relTo(dir, p))))
			h.Write(data)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MD5 of a directory's package paths and content excluding generated runtime state
func DirHash(directory string) string { return dirHashFS(os.DirFS(directory), ".", false) }

// Reports whether a directory still matches its origin hash, clean or with legacy runtime caches
func matchesOriginHash(directory, originHash, userHash string) bool {
	if originHash == "" {
		return false
	}
	current := userHash
	if current == "" {
		current = DirHash(directory)
	}
	return current == originHash || dirHashFS(os.DirFS(directory), ".", true) == originHash
}

// Package files of a tree directory as sorted slash paths, runtime caches excluded
func skillFileListFS(fsys fs.FS, dir string) []string {
	entries := walkEntries(fsys, dir)
	sortByParts(entries)
	out := []string{}
	for _, p := range entries {
		if !isRuntimeCacheFS(fsys, p, dir) && fsIsFile(fsys, p) {
			out = append(out, relTo(dir, p))
		}
	}
	return out
}

// Excluded or support package check for a tree relative path
func (t skillTree) excluded(rel string) bool {
	if t.root != "" {
		return isExcludedSkillPath(filepath.FromSlash(rel), t.root)
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		if ExcludedSkillDirs[part] {
			return true
		}
	}
	for idx := 1; idx < len(parts)-1; idx++ {
		if SkillSupportDirs[parts[idx]] && fsIsFile(t.fsys, strings.Join(parts[:idx], "/")+"/SKILL.md") {
			return true
		}
	}
	return false
}

// Frontmatter name of a SKILL.md inside a tree, the fallback when unreadable or unnamed
func (t skillTree) skillName(rel, fallback string) string {
	data, err := fs.ReadFile(t.fsys, rel)
	if err != nil {
		return fallback
	}
	return skillNameFromText(decodeText(data), fallback)
}

// Collects shutil.copytree errors and renders them like shutil.Error
type copyTreeError struct{ errs [][3]string }

func (e *copyTreeError) Error() string {
	items := make([]string, len(e.errs))
	for i, x := range e.errs {
		items[i] = "(" + pyrepr.Repr(x[0]) + ", " + pyrepr.Repr(x[1]) + ", " + pyrepr.Repr(x[2]) + ")"
	}
	return "[" + strings.Join(items, ", ") + "]"
}

// Copies a tree directory like shutil.copytree with the runtime cache filter, keeping existing files when asked
func (t skillTree) copyDir(dir, dest string, dirsExistOK, keepExisting bool) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return err
	}
	cerr := &copyTreeError{}
	if err := t.copyInto(dir, dir, dest, dirsExistOK, keepExisting, cerr); err != nil {
		return err
	}
	if len(cerr.errs) > 0 {
		return cerr
	}
	return nil
}

// One copytree level, files through copy2 semantics and directory stat applied after children
func (t skillTree) copyInto(top, dir, dest string, dirsExistOK, keepExisting bool, cerr *copyTreeError) error {
	entries, err := fs.ReadDir(t.fsys, dir)
	if err != nil {
		return err
	}
	if dirsExistOK {
		err = os.MkdirAll(dest, 0o777)
	} else {
		err = os.Mkdir(dest, 0o777)
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := path.Join(dir, e.Name())
		if isRuntimeCacheFS(t.fsys, src, dir) {
			continue
		}
		target := filepath.Join(dest, e.Name())
		if fsIsDir(t.fsys, src) {
			if err := t.copyInto(top, src, target, dirsExistOK, keepExisting, cerr); err != nil {
				var nested *copyTreeError
				if errors.As(err, &nested) {
					cerr.errs = append(cerr.errs, nested.errs...)
				} else {
					cerr.errs = append(cerr.errs, [3]string{t.display(src), target, pyfs.OSErrorText(err)})
				}
			}
			continue
		}
		if keepExisting && lexists(target) {
			continue
		}
		if err := t.copyFile(src, target); err != nil {
			cerr.errs = append(cerr.errs, [3]string{t.display(src), target, pyfs.OSErrorText(err)})
		}
	}
	if err := t.applyStat(dir, dest); err != nil {
		cerr.errs = append(cerr.errs, [3]string{t.display(dir), dest, pyfs.OSErrorText(err)})
	}
	return nil
}

// Permission bits of a tree entry, embedded scripts with an interpreter line being executable
func (t skillTree) mode(p string, isDir bool) (fs.FileInfo, os.FileMode) {
	st, err := fs.Stat(t.fsys, p)
	if t.root != "" && err == nil {
		return st, st.Mode().Perm()
	}
	if isDir || t.startsWithShebang(p) {
		return nil, 0o755
	}
	return nil, 0o644
}

// Whether an embedded file opens with an interpreter line
func (t skillTree) startsWithShebang(p string) bool {
	f, err := t.fsys.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var head [2]byte
	n, _ := io.ReadFull(f, head[:])
	return n == 2 && head[0] == '#' && head[1] == '!'
}

// Copies one file with its permission bits and modification time
func (t skillTree) copyFile(src, target string) error {
	data, err := fs.ReadFile(t.fsys, src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(target, data, 0o666); err != nil {
		return err
	}
	return t.applyStat(src, target)
}

// Copies permission bits and times of a tree entry onto a destination path
func (t skillTree) applyStat(src, target string) error {
	st, perm := t.mode(src, fsIsDir(t.fsys, src))
	if err := os.Chmod(target, perm); err != nil {
		return err
	}
	if st != nil {
		return os.Chtimes(target, st.ModTime(), st.ModTime())
	}
	return nil
}

// Text of a file decoded as utf-8-sig, the UnicodeDecodeError text on invalid bytes
func readUTF8Sig(p string) (string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	data = []byte(strings.TrimPrefix(string(data), "\xEF\xBB\xBF"))
	for pos := 0; pos < len(data); {
		r, n := utf8.DecodeRune(data[pos:])
		if r == utf8.RuneError && n <= 1 {
			return "", pystr.UTF8DecodeError(data, pos)
		}
		pos += n
	}
	return string(data), nil
}

// Path of the bundled manifest in the scope skills dir
func (s *Scope) manifestFile() string { return filepath.Join(s.SkillsDir(), ".bundled_manifest") }

// Manifest of skill name to origin hash, plain v1 names mapping to an empty hash
func (s *Scope) readManifest() (map[string]string, error) {
	text, err := readUTF8Sig(s.manifestFile())
	if err != nil {
		var dec *pystr.UnicodeDecodeError
		if errors.As(err, &dec) {
			return nil, err
		}
		return map[string]string{}, nil
	}
	out := map[string]string{}
	for _, line := range pystr.Splitlines(text, false) {
		line = pystr.Strip(line)
		if line == "" {
			continue
		}
		if name, hash, ok := strings.Cut(line, ":"); ok {
			out[pystr.Strip(name)] = pystr.Strip(hash)
		} else {
			out[line] = ""
		}
	}
	return out, nil
}

// Writes the manifest atomically in v2 form keeping an existing file mode, failures only logged
func (s *Scope) writeManifest(ctx context.Context, entries map[string]string) {
	mf := s.manifestFile()
	if err := os.MkdirAll(filepath.Dir(mf), 0o755); err != nil {
		slog.Default().Debug("Failed to write skills manifest", "path", mf, "err", err)
		return
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n + ":" + entries[n] + "\n")
	}
	if err := pyfs.AtomicWriteTextWith(home.WithOverride(ctx, s.Home), mf, b.String(), pyfs.TextWrite{Prefix: ".bundled_manifest_", PreserveMode: true}); err != nil {
		slog.Default().Debug("Failed to write skills manifest", "path", mf, "err", err)
	}
}

// Curator suppressed built in names, the raw file read when the usage reader fails
func (s *Scope) syncSuppressedNames() map[string]bool {
	if names, err := s.ReadSuppressedNames(); err == nil {
		return names
	}
	names := map[string]bool{}
	text, err := readUTF8Sig(filepath.Join(s.SkillsDir(), ".curator_suppressed"))
	if err != nil {
		return names
	}
	for _, line := range pystr.Splitlines(text, false) {
		if line = pystr.Strip(line); line != "" && !strings.HasPrefix(line, "#") {
			names[line] = true
		}
	}
	return names
}

// One discovered packaged skill with its tree relative directory
type bundledSkill struct{ name, dir string }

// Packaged skills of a tree, exclusions judged relative to the tree root
func discoverBundledSkills(t skillTree) []bundledSkill {
	if !t.exists() {
		return nil
	}
	var out []bundledSkill
	for _, p := range walkEntries(t.fsys, ".") {
		if path.Base(p) != "SKILL.md" || t.excluded(p) {
			continue
		}
		dir := path.Dir(p)
		out = append(out, bundledSkill{t.skillName(p, path.Base(dir)), dir})
	}
	return out
}

// Destination of a packaged skill dir in the scope skills tree keeping its category path
func (s *Scope) relativeDest(dir string) string {
	return filepath.Join(s.SkillsDir(), filepath.FromSlash(dir))
}

// Non excluded SKILL.md paths under an OS directory, sorted by parts when asked
func iterSkillMDs(root string, sorted bool) []string {
	if !pathExists(root) {
		return nil
	}
	var out []string
	entries := walkEntries(os.DirFS(root), ".")
	if sorted {
		sortByParts(entries)
	}
	for _, p := range entries {
		full := filepath.Join(root, filepath.FromSlash(p))
		if path.Base(p) == "SKILL.md" && !isExcludedSkillPath(full, "") {
			out = append(out, full)
		}
	}
	return out
}

// Directory and frontmatter names of every skill external_dirs provide
func (s *Scope) buildExternalSkillIndex() map[string]bool {
	externalDirsCacheClear()
	names := map[string]bool{}
	for _, ext := range s.ExternalSkillsDirs() {
		for _, md := range iterSkillMDs(ext, false) {
			names[filepath.Base(filepath.Dir(md))] = true
			names[readSkillName(md, "")] = true
		}
	}
	delete(names, "")
	return names
}

// Mutable accumulator of one sync run
type syncState struct {
	manifest           map[string]string
	quiet              bool
	skipped            int
	copied             []string
	updated            []string
	userModified       []string
	suppressed         []string
	relocated          []string
	shadowedByExternal []string
	activeIndex        map[string][]string
	hubPaths           map[string]bool
}

func (st *syncState) say(msg string) {
	if !st.quiet {
		fmt.Fprintln(syncOut, msg)
	}
}

// Slash path of a skills tree entry relative to the skills dir, false outside it
func (s *Scope) relSkillsPosix(p string) (string, bool) {
	rel, err := filepath.Rel(s.SkillsDir(), p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// Moves a stale byte identical copy of a renamed bundled skill to its new path, the old rel path on success
func (s *Scope) recoverRenamedSkill(st *syncState, name, dest string) (string, error) {
	originHash := st.manifest[name]
	if originHash == "" {
		return "", nil
	}
	if st.activeIndex == nil {
		st.activeIndex = map[string][]string{}
		for _, md := range iterSkillMDs(s.SkillsDir(), false) {
			n := readSkillName(md, filepath.Base(filepath.Dir(md)))
			st.activeIndex[n] = append(st.activeIndex[n], filepath.Dir(md))
		}
		paths, err := s.readHubInstallPaths()
		if err != nil {
			return "", err
		}
		st.hubPaths = paths
	}
	destRel, _ := s.relSkillsPosix(dest)
	for _, candidate := range st.activeIndex[name] {
		if candidate == dest || !isDir(candidate) {
			continue
		}
		rel, ok := s.relSkillsPosix(candidate)
		if !ok || st.hubPaths[rel] {
			continue
		}
		if !matchesOriginHash(candidate, originHash, "") {
			st.say(fmt.Sprintf("  ⚠ %s: upstream moved this skill to %s, but your modified copy at %s was kept — it will not receive updates. Run `icebath skills reset %s --restore` to move to the new location.", name, destRel, rel, name))
			continue
		}
		if err := moveDir(candidate, dest); err != nil {
			slog.Default().Warn("Could not relocate renamed skill", "from", candidate, "to", dest, "err", err)
			return "", nil
		}
		slog.Default().Info("Relocated renamed bundled skill", "from", candidate, "to", dest)
		st.say(fmt.Sprintf("  → %s (moved %s → %s)", name, rel, destRel))
		return rel, nil
	}
	return "", nil
}

// Moves a directory creating the destination parent like shutil.move
func moveDir(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return err
	}
	_, err := pyfs.Move(src, dest)
	return err
}

// Path with its final suffix replaced by .bak like Path.with_suffix
func bakPath(dest string) string {
	name := filepath.Base(dest)
	return filepath.Join(filepath.Dir(dest), strings.TrimSuffix(name, pypath.PathSuffix(name))+".bak")
}

// Moves an orphaned .bak copy back when an interrupted update left dest missing
func recoverOrphanBackup(dest string) {
	orphan := bakPath(dest)
	if pathExists(orphan) && !pathExists(dest) {
		if err := moveDir(orphan, dest); err != nil {
			slog.Default().Warn("Could not recover orphaned skill backup", "path", orphan, "err", err)
		} else {
			slog.Default().Info("Recovered orphaned skill backup", "path", orphan)
		}
	}
}

// Reports whether a path exists following symlinks
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Defers a skill external_dirs provide, removing a byte identical stale local shadow
func (s *Scope) deferToExternal(st *syncState, name, dest, bundledHash string) error {
	st.shadowedByExternal = append(st.shadowedByExternal, name)
	st.skipped++
	st.say(fmt.Sprintf("  ⇢ %s (deferred to external_dirs, not written to local tree)", name))
	if pathExists(dest) && DirHash(dest) == bundledHash {
		if err := syncSteps.rmtree(s, dest); err != nil {
			return err
		}
		st.say("  ✓ removed stale shadow of " + name)
		delete(st.manifest, name)
	}
	return nil
}

// Seeds a skill never offered before, never overwriting a same named user skill
func (s *Scope) installNewSkill(st *syncState, t skillTree, name, src, dest, bundledHash string) {
	if pathExists(dest) {
		st.skipped++
		if DirHash(dest) == bundledHash {
			st.manifest[name] = bundledHash
		} else {
			st.say(fmt.Sprintf("  ⚠ %s: bundled version shipped but you already have a local skill by this name — yours was kept. Run `icebath skills reset %s` to replace it with the bundled version.", name, name))
		}
		return
	}
	if err := syncSteps.copyTree(t, src, dest, false, false); err != nil {
		st.say(fmt.Sprintf("  ! Failed to copy %s: %s", name, pyfs.OSErrorText(err)))
		return
	}
	st.copied = append(st.copied, name)
	st.manifest[name] = bundledHash
	st.say("  + " + name)
}

// Replaces dest with a fresh copy through a .bak sibling restored on failure
func (s *Scope) replaceSkillDir(t skillTree, src, dest string) error {
	backup := bakPath(dest)
	if pathExists(backup) {
		if err := syncSteps.rmtree(s, backup); err != nil {
			return err
		}
	}
	if _, err := pyfs.Move(dest, backup); err != nil {
		return err
	}
	if err := syncSteps.copyTree(t, src, dest, false, false); err != nil {
		if pathExists(backup) {
			if pathExists(dest) {
				if rerr := syncSteps.rmtree(s, dest); rerr != nil {
					if !isOSErr(rerr) {
						return rerr
					}
					slog.Default().Warn("Could not clear partial copy during restore", "path", dest, "err", rerr)
				}
			}
			if !pathExists(dest) {
				if _, merr := pyfs.Move(backup, dest); merr != nil {
					return merr
				}
			}
		}
		return err
	}
	if err := syncSteps.rmtree(s, backup); err != nil {
		if !isOSErr(err) {
			return err
		}
		slog.Default().Debug("Could not remove backup", "path", backup, "err", err)
	}
	return nil
}

// Updates a tracked on disk skill when bundled changed and the user copy is pristine
func (s *Scope) updateExistingSkill(st *syncState, t skillTree, name, src, dest, bundledHash string) error {
	originHash := st.manifest[name]
	if originHash != "" && bundledHash == originHash {
		st.skipped++
		return nil
	}
	userHash := DirHash(dest)
	if originHash == "" {
		st.manifest[name] = userHash
		st.skipped++
		return nil
	}
	if !matchesOriginHash(dest, originHash, userHash) {
		st.userModified = append(st.userModified, name)
		st.say(fmt.Sprintf("  ~ %s (user-modified, skipping)", name))
		return nil
	}
	if err := s.replaceSkillDir(t, src, dest); err != nil {
		if !isOSErr(err) {
			return err
		}
		st.say(fmt.Sprintf("  ! Failed to update %s: %s", name, pyfs.OSErrorText(err)))
		return nil
	}
	st.manifest[name] = bundledHash
	st.updated = append(st.updated, name)
	st.say(fmt.Sprintf("  ↑ %s (updated)", name))
	return nil
}

// OSError check that also counts the shutil.Error raised by a failed tree copy
func isOSErr(err error) bool {
	var ce *copyTreeError
	return errors.As(err, &ce) || config.IsOSError(err)
}

// Copies category DESCRIPTION.md files not already present, limited to onlyDirs when given
func (s *Scope) seedCategoryDescriptions(t skillTree, onlyDirs map[string]bool) {
	for _, p := range walkEntries(t.fsys, ".") {
		if path.Base(p) != "DESCRIPTION.md" {
			continue
		}
		dest := s.relativeDest(p)
		if (onlyDirs != nil && !onlyDirs[filepath.Dir(dest)]) || pathExists(dest) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o777); err == nil {
			err = t.copyFile(p, dest)
			if err != nil {
				slog.Default().Debug("Could not copy category description", "path", p, "err", err)
			}
		} else {
			slog.Default().Debug("Could not copy category description", "path", p, "err", err)
		}
	}
}

// Syncs bundled skills into the home of ctx and returns the per category result
func SyncSkills(ctx context.Context, quiet bool) (map[string]any, error) {
	return NewScope(home.Dir(ctx), nil).SyncSkills(ctx, quiet)
}

// Syncs bundled skills into the scope skills tree through the manifest, essentials only when opted out
func (s *Scope) SyncSkills(ctx context.Context, quiet bool) (map[string]any, error) {
	essentialOnly := pathExists(filepath.Join(s.Home, NoBundledSkillsMarker))
	if essentialOnly && !quiet {
		fmt.Fprintln(syncOut, "  (profile opted out of bundled skills via .no-bundled-skills — seeding essential skills only)")
	}
	t := bundledTree()
	if !t.exists() {
		return map[string]any{"copied": []string{}, "updated": []string{}, "skipped": 0, "user_modified": []string{}, "cleaned": []string{},
			"suppressed": []string{}, "total_bundled": 0, "optional_provenance_backfilled": []string{}}, nil
	}
	if err := os.MkdirAll(s.SkillsDir(), 0o777); err != nil {
		return nil, err
	}
	bundled := discoverBundledSkills(t)
	if essentialOnly {
		bundled = slices.DeleteFunc(bundled, func(b bundledSkill) bool { return !EssentialSkills[b.name] })
	}
	suppressed := s.syncSuppressedNames()
	external := s.buildExternalSkillIndex()
	manifest, err := s.readManifest()
	if err != nil {
		return nil, err
	}
	st := &syncState{manifest: manifest, quiet: quiet, copied: []string{}, updated: []string{}, userModified: []string{},
		suppressed: []string{}, relocated: []string{}, shadowedByExternal: []string{}}
	for _, b := range bundled {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if suppressed[b.name] && !EssentialSkills[b.name] {
			st.suppressed = append(st.suppressed, b.name)
			continue
		}
		dest := s.relativeDest(b.dir)
		bundledHash := dirHashFS(t.fsys, b.dir, false)
		recoverOrphanBackup(dest)
		if _, tracked := st.manifest[b.name]; !pathExists(dest) && tracked {
			rel, err := s.recoverRenamedSkill(st, b.name, dest)
			if err != nil {
				return nil, err
			}
			if rel != "" {
				st.relocated = append(st.relocated, b.name)
			}
		}
		_, tracked := st.manifest[b.name]
		switch {
		case external[b.name]:
			if err := s.deferToExternal(st, b.name, dest, bundledHash); err != nil {
				return nil, err
			}
		case !tracked:
			s.installNewSkill(st, t, b.name, b.dir, dest, bundledHash)
		case pathExists(dest):
			if err := s.updateExistingSkill(st, t, b.name, b.dir, dest, bundledHash); err != nil {
				return nil, err
			}
		default:
			st.skipped++
		}
	}
	cleaned := []string{}
	if !essentialOnly {
		bundledNames := map[string]bool{}
		for _, b := range bundled {
			bundledNames[b.name] = true
		}
		var removed []string
		for n := range st.manifest {
			if !bundledNames[n] {
				removed = append(removed, n)
			}
		}
		sort.Strings(removed)
		present := map[string]bool{}
		if len(removed) > 0 {
			for _, md := range iterSkillMDs(s.SkillsDir(), false) {
				present[readSkillName(md, filepath.Base(filepath.Dir(md)))] = true
			}
			archive := filepath.Join(s.SkillsDir(), ".archive")
			if entries, err := os.ReadDir(archive); err == nil {
				for _, e := range entries {
					if isDir(filepath.Join(archive, e.Name())) {
						present[e.Name()] = true
					}
				}
			}
		}
		for _, n := range removed {
			if !present[n] {
				cleaned = append(cleaned, n)
				delete(st.manifest, n)
			}
		}
	}
	var only map[string]bool
	if essentialOnly {
		only = map[string]bool{}
		for _, b := range bundled {
			only[filepath.Dir(s.relativeDest(b.dir))] = true
		}
	}
	s.seedCategoryDescriptions(t, only)
	s.writeManifest(home.WithOverride(ctx, s.Home), st.manifest)
	backfilled, err := s.backfillOptionalProvenance(home.WithOverride(ctx, s.Home), quiet)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"copied": st.copied, "updated": st.updated, "skipped": st.skipped, "user_modified": st.userModified,
		"cleaned": cleaned, "suppressed": st.suppressed, "relocated": st.relocated, "total_bundled": len(bundled),
		"optional_provenance_backfilled": backfilled, "shadowed_by_external": st.shadowedByExternal,
		"skipped_opt_out": essentialOnly,
	}, nil
}

// Removes a tree strictly under the skills root, making read only entries writable on failure
func (s *Scope) rmtreeWritable(p string) error {
	target := resolvePath(p)
	root := resolvePath(s.SkillsDir())
	if target == root || !isRelativeTo(target, root) {
		return fmt.Errorf("refusing to rmtree %s: not strictly under %s (scope guard — see #48200)", pathRepr(target), pathRepr(root))
	}
	return pyfs.RmTree(p, func(op pyfs.RmTreeOp, fpath string, _ error) error {
		os.Chmod(filepath.Dir(fpath), 0o700)
		os.Chmod(fpath, 0o700)
		return op(fpath)
	})
}

// Normalized relative slash install path, an error for traversal, absolute or empty paths
func safeRelInstallPath(p, base string) (string, error) {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return "", err
	}
	posix := filepath.ToSlash(rel)
	var parts []string
	for _, part := range strings.Split(posix, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if strings.HasPrefix(posix, "/") || len(parts) == 0 || slices.Contains(parts, "..") {
		return "", fmt.Errorf("Unsafe optional skill path: %s", posix)
	}
	return strings.Join(parts, "/"), nil
}

// Parsed hub lock of the scope, nil when missing or not JSON
func (s *Scope) loadHubLock() (any, error) {
	text, err := readUTF8Sig(filepath.Join(s.SkillsDir(), ".hub", "lock.json"))
	if err != nil {
		var dec *pystr.UnicodeDecodeError
		if errors.As(err, &dec) {
			return nil, err
		}
		return nil, nil
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil
	}
	return data, nil
}

// Dict entries of a hub lock installed table
func hubLockInstalledEntries(data any) ([]*pyjson.Object, error) {
	if data == nil {
		return nil, nil
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		if pyTruthyJSON(data) {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyjsonTypeName(data))
		}
		return nil, nil
	}
	installed, _ := obj.Get("installed")
	if installed == nil || !pyTruthyJSON(installed) {
		return nil, nil
	}
	table, ok := installed.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'values'", pyjsonTypeName(installed))
	}
	var out []*pyjson.Object
	for _, k := range table.Keys {
		if e, ok := table.Vals[k].(*pyjson.Object); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// Python truthiness of a decoded JSON value
func pyTruthyJSON(v any) bool {
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
	case float64:
		return x != 0
	}
	return pyrepr.Str(v) != "0"
}

// Python type name of a decoded JSON value
func pyjsonTypeName(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	case string:
		return "str"
	case bool:
		return "bool"
	case float64:
		return "float"
	case *pyjson.Object:
		return "dict"
	}
	return "int"
}

// Hub lock install paths as slash strings stripped of surrounding slashes
func (s *Scope) readHubInstallPaths() (map[string]bool, error) {
	data, err := s.loadHubLock()
	if err != nil {
		return nil, err
	}
	entries, err := hubLockInstalledEntries(data)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range entries {
		if v, ok := e.Get("install_path"); ok && pyTruthyJSON(v) {
			out[strings.Trim(pyrepr.Str(v), "/")] = true
		}
	}
	return out, nil
}

// One safe official optional skill with its tree relative SKILL.md, dir and install path
type optionalSkill struct{ md, dir, installPath string }

// Every safe official optional skill of a tree in sorted order
func iterOptionalSkills(t skillTree, rootRelative bool) []optionalSkill {
	var mds []string
	for _, p := range walkEntries(t.fsys, ".") {
		if path.Base(p) == "SKILL.md" {
			mds = append(mds, p)
		}
	}
	sortByParts(mds)
	var out []optionalSkill
	for _, md := range mds {
		if rootRelative || t.root == "" {
			if t.excluded(md) {
				continue
			}
		} else if isExcludedSkillPath(filepath.Join(t.root, filepath.FromSlash(md)), "") {
			continue
		}
		dir := path.Dir(md)
		install, err := safeRelInstallPath(filepath.FromSlash(dir), ".")
		if err != nil {
			slog.Default().Debug("Skipping optional skill with unsafe path", "dir", dir, "err", err)
			continue
		}
		out = append(out, optionalSkill{md, dir, install})
	}
	return out
}

// Optional skill index value of folder name, install path and tree dir
type optionalEntry struct{ folder, installPath, dir string }

// Official optional skills keyed by folder name and frontmatter name
func optionalSkillIndex(t skillTree) map[string]optionalEntry {
	index := map[string]optionalEntry{}
	if !t.exists() {
		return index
	}
	for _, o := range iterOptionalSkills(t, true) {
		folder := path.Base(o.dir)
		v := optionalEntry{folder, o.installPath, o.dir}
		index[t.skillName(o.md, folder)] = v
		index[folder] = v
	}
	return index
}

// Moves an existing skill directory into a restore backup keeping its relative path
func (s *Scope) moveToRestoreBackup(p, backupRoot string) (string, error) {
	rel, err := filepath.Rel(s.SkillsDir(), p)
	if err != nil {
		return "", err
	}
	target := filepath.Join(backupRoot, rel)
	for suffix := 1; lexists(target); suffix++ {
		target = filepath.Join(backupRoot, filepath.Dir(rel), fmt.Sprintf("%s-%d", filepath.Base(rel), suffix))
	}
	if err := moveDir(p, target); err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// Restores one or all official optional skills from source, backing up conflicting active copies when restoring
func (s *Scope) RestoreOfficialOptionalSkill(ctx context.Context, name string, restore bool) (map[string]any, error) {
	t := optionalTree()
	index := optionalSkillIndex(t)
	var targets []optionalEntry
	switch {
	case len(index) > 0 && (name == "all" || name == "*"):
		seen := map[optionalEntry]bool{}
		for _, v := range index {
			if !seen[v] {
				seen[v] = true
				targets = append(targets, v)
			}
		}
		sort.SliceStable(targets, func(i, j int) bool { return targets[i].installPath < targets[j].installPath })
	default:
		if v, ok := index[name]; ok {
			targets = []optionalEntry{v}
		} else {
			msg := "No official optional skills directory found."
			if len(index) > 0 {
				msg = "Official optional skill not found: " + name
			}
			return map[string]any{"ok": false, "message": msg, "restored": []string{}, "backfilled": []string{}, "backed_up": []string{}}, nil
		}
	}
	restored, backedUp := []string{}, []string{}
	backupRoot := filepath.Join(s.SkillsDir(), ".restore-backups", "official-optional-"+time.Now().UTC().Format("20060102-150405"))
	if restore {
		for _, tg := range targets {
			dest := filepath.Join(s.SkillsDir(), filepath.FromSlash(tg.installPath))
			canonicalOK := pathExists(dest) && DirHash(dest) == dirHashFS(t.fsys, tg.dir, false)
			names := map[string]bool{tg.folder: true, t.skillName(tg.dir+"/SKILL.md", tg.folder): true}
			for _, md := range iterSkillMDs(s.SkillsDir(), true) {
				match := filepath.Dir(md)
				if match != dest && pathExists(match) && (filepath.Base(match) == tg.folder || names[readSkillName(md, filepath.Base(match))]) {
					rel, err := s.moveToRestoreBackup(match, backupRoot)
					if err != nil {
						return nil, err
					}
					backedUp = append(backedUp, rel)
				}
			}
			if pathExists(dest) && !canonicalOK {
				rel, err := s.moveToRestoreBackup(dest, backupRoot)
				if err != nil {
					return nil, err
				}
				backedUp = append(backedUp, rel)
			}
			if !pathExists(dest) {
				if err := syncSteps.copyTree(t, tg.dir, dest, false, false); err != nil {
					return nil, err
				}
				restored = append(restored, tg.folder)
			}
		}
	}
	backfilled, err := s.backfillOptionalProvenance(home.WithOverride(ctx, s.Home), true)
	if err != nil {
		return nil, err
	}
	backupDir := ""
	if len(backedUp) > 0 {
		backupDir = backupRoot
	}
	return map[string]any{"ok": true, "message": "Official optional skill repair complete.", "restored": restored,
		"backfilled": backfilled, "backed_up": backedUp, "backup_dir": backupDir}, nil
}

// Installed skill dirs by directory name, skipping anything resolving outside the skills tree
func (s *Scope) indexInstalledSkillDirsByName() map[string][]string {
	index := map[string][]string{}
	root := resolvePath(s.SkillsDir())
	for _, md := range iterSkillMDs(s.SkillsDir(), false) {
		dir := filepath.Dir(md)
		if isRelativeTo(resolvePath(dir), root) {
			index[filepath.Base(dir)] = append(index[filepath.Base(dir)], dir)
		}
	}
	return index
}

// Unique same named installed dir and its install path, false when none or ambiguous
func (s *Scope) relocatedDest(srcName string, index map[string][]string) (string, string, bool) {
	candidates := index[srcName]
	if len(candidates) != 1 {
		return "", "", false
	}
	install, err := safeRelInstallPath(candidates[0], s.SkillsDir())
	if err != nil {
		slog.Default().Debug("Skipping relocated optional skill", "dir", candidates[0], "err", err)
		return "", "", false
	}
	return candidates[0], install, true
}

// Marks byte identical present official optional skills as hub installed in the lock
func (s *Scope) backfillOptionalProvenance(ctx context.Context, quiet bool) ([]string, error) {
	t := optionalTree()
	if !t.exists() {
		return []string{}, nil
	}
	raw, err := s.loadHubLock()
	if err != nil {
		return nil, err
	}
	if raw == nil {
		obj := pyjson.NewObject()
		obj.Set("version", 1)
		obj.Set("installed", pyjson.NewObject())
		raw = obj
	}
	data, ok := raw.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'setdefault'", pyjsonTypeName(raw))
	}
	if _, present := data.Get("installed"); !present {
		data.Set("installed", pyjson.NewObject())
	}
	instAny, _ := data.Get("installed")
	installed, ok := instAny.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("argument of type '%s' is not iterable", pyjsonTypeName(instAny))
	}
	entries, err := hubLockInstalledEntries(data)
	if err != nil {
		return nil, err
	}
	existing := map[string]bool{}
	for _, e := range entries {
		if v, ok := e.Get("install_path"); ok {
			if p, isStr := v.(string); isStr {
				existing[p] = true
			}
		}
	}
	backfilled := []string{}
	var dirIndex map[string][]string
	for _, o := range iterOptionalSkills(t, false) {
		lockName := path.Base(o.dir)
		if _, has := installed.Get(lockName); has || existing[o.installPath] {
			continue
		}
		install := o.installPath
		dest := filepath.Join(s.SkillsDir(), filepath.FromSlash(install))
		if !isDir(dest) {
			if dirIndex == nil {
				dirIndex = s.indexInstalledSkillDirsByName()
			}
			d, ip, found := s.relocatedDest(lockName, dirIndex)
			if !found {
				continue
			}
			dest, install = d, ip
		}
		if existing[install] || DirHash(dest) != dirHashFS(t.fsys, o.dir, false) {
			continue
		}
		ts := nowISO()
		files := []any{}
		for _, f := range skillFileListFS(os.DirFS(dest), ".") {
			files = append(files, f)
		}
		meta := pyjson.NewObject()
		meta.Set("backfilled_from", "optional-skills")
		entry := pyjson.NewObject()
		for _, kv := range [][2]any{{"source", "official"}, {"identifier", "official/" + install}, {"trust_level", "builtin"},
			{"scan_verdict", "backfilled"}, {"content_hash", ContentHash(dest)}, {"install_path", install}, {"files", files},
			{"metadata", meta}, {"installed_at", ts}, {"updated_at", ts}} {
			entry.Set(kv[0].(string), kv[1])
		}
		installed.Set(lockName, entry)
		existing[install] = true
		backfilled = append(backfilled, lockName)
		if !quiet {
			fmt.Fprintf(syncOut, "  = %s (official optional provenance backfilled)\n", lockName)
		}
	}
	if len(backfilled) > 0 {
		text, err := pyjson.Dumps(data, pyjson.Indent(2), pyjson.EnsureASCII(false))
		if err != nil {
			return nil, err
		}
		lock := filepath.Join(s.SkillsDir(), ".hub", "lock.json")
		if err := os.MkdirAll(filepath.Dir(lock), 0o777); err != nil {
			return nil, err
		}
		if err := pyfs.AtomicWriteText(home.WithOverride(ctx, s.Home), lock, text+"\n", ".lock_"); err != nil {
			return nil, err
		}
	}
	return backfilled, nil
}

// Manifest, bundled tree and bundled skills by name shared by the bundled ops
func (s *Scope) bundledState() (map[string]string, skillTree, map[string]string, error) {
	t := bundledTree()
	manifest, err := s.readManifest()
	if err != nil {
		return nil, t, nil, err
	}
	byName := map[string]string{}
	for _, b := range discoverBundledSkills(t) {
		byName[b.name] = b.dir
	}
	return manifest, t, byName, nil
}

// Clears a bundled skill's manifest entry, deleting the user copy first when restoring, then syncs
func (s *Scope) ResetBundledSkill(ctx context.Context, name string, restore bool) (map[string]any, error) {
	manifest, _, byName, err := s.bundledState()
	if err != nil {
		return nil, err
	}
	_, inManifest := manifest[name]
	src, isBundled := byName[name]
	fail := func(action, message string) (map[string]any, error) {
		return map[string]any{"ok": false, "action": action, "message": message, "synced": nil}, nil
	}
	if !inManifest && !isBundled {
		return fail("not_in_manifest", fmt.Sprintf("'%s' is not a tracked bundled skill. Nothing to reset. (Hub-installed skills use `icebath skills uninstall`.)", name))
	}
	deletedUserCopy := false
	if restore {
		if !isBundled {
			return fail("bundled_missing", fmt.Sprintf("'%s' has no bundled source — manifest entry preserved but cannot restore from bundled (skill was removed upstream).", name))
		}
		dest := s.relativeDest(src)
		if deletedUserCopy = pathExists(dest); deletedUserCopy {
			if err := syncSteps.rmtree(s, dest); err != nil {
				if !config.IsOSError(err) {
					return nil, err
				}
				return fail("not_reset", fmt.Sprintf("Could not delete user copy at %s: %s. Manifest entry preserved — nothing was changed.", dest, pyfs.OSErrorText(err)))
			}
		}
	}
	if inManifest {
		delete(manifest, name)
		s.writeManifest(home.WithOverride(ctx, s.Home), manifest)
	}
	synced, err := s.SyncSkills(ctx, true)
	if err != nil {
		return nil, err
	}
	action, message := "manifest_cleared", fmt.Sprintf("Cleared manifest entry for '%s'. Future `icebath update` runs will re-baseline against your current copy and accept upstream changes.", name)
	if restore {
		action = "restored"
		message = fmt.Sprintf("Restored '%s' (no prior user copy, re-copied from bundled).", name)
		if deletedUserCopy {
			message = fmt.Sprintf("Restored '%s' from bundled source.", name)
		}
	}
	return map[string]any{"ok": true, "action": action, "message": message, "synced": synced}, nil
}

// Repo prefixes whose install identifiers name this project's own bundled skills
var ownRepoPrefixes = []string{"nousresearch/hermes-agent/skills/", "skills-sh/nousresearch/hermes-agent/"}

// Bundled skill name an install identifier means, empty when it names none
func (s *Scope) BundledSkillForInstall(identifier string) (string, error) {
	ident := strings.Trim(pystr.Strip(identifier), "/")
	lowered := strings.ToLower(ident)
	if strings.Contains(ident, "/") && !slices.ContainsFunc(ownRepoPrefixes, func(p string) bool { return strings.HasPrefix(lowered, p) }) {
		return "", nil
	}
	name := ident[strings.LastIndex(ident, "/")+1:]
	_, _, byName, err := s.bundledState()
	if err != nil {
		return "", err
	}
	if _, ok := byName[name]; ok {
		return name, nil
	}
	return "", nil
}

// Makes a bundled skill active, leaving any present copy or hub shadow alone and restoring otherwise
func (s *Scope) EnsureBundledSkill(ctx context.Context, name string) (map[string]any, error) {
	manifest, t, byName, err := s.bundledState()
	if err != nil {
		return nil, err
	}
	src, ok := byName[name]
	if !ok {
		return nil, fmt.Errorf("KeyError: %s", pyrepr.Repr(name))
	}
	dest := s.relativeDest(src)
	if s.IsHubInstalled(name) {
		return map[string]any{"ok": true, "action": "hub_shadowed", "path": nil, "message": fmt.Sprintf("'%s' is a built-in skill, but a skill installed from the hub uses the same name and shadows it. To get the built-in back: `icebath skills uninstall %s`, then `icebath skills install %s`.", name, name, name)}, nil
	}
	skillMD := filepath.Join(dest, "SKILL.md")
	var active any
	for _, md := range iterSkillMDs(s.SkillsDir(), false) {
		if readSkillName(md, filepath.Base(filepath.Dir(md))) == name {
			active = filepath.Dir(md)
			break
		}
	}
	if active == nil && fsIsFile(os.DirFS(dest), "SKILL.md") && readSkillName(skillMD, name) == name {
		active = dest
	}
	if active != nil || s.buildExternalSkillIndex()[name] {
		return map[string]any{"ok": true, "action": "present", "path": active, "message": ""}, nil
	}
	notRestored := func(err error) (map[string]any, error) {
		return map[string]any{"ok": false, "action": "not_restored", "path": dest,
			"message": fmt.Sprintf("Could not copy the built-in skill '%s' to %s: %s", name, dest, pyfs.OSErrorText(err))}, nil
	}
	rel, _ := s.relSkillsPosix(dest)
	if _, err := resolveLockInstallPath(s, rel, filepath.Base(dest)); err != nil {
		return notRestored(err)
	}
	if lexists(skillMD) {
		return notRestored(errors.New("the directory holds a different skill's SKILL.md"))
	}
	if pathExists(dest) {
		for _, p := range walkEntries(os.DirFS(dest), ".") {
			full := filepath.Join(dest, filepath.FromSlash(p))
			if st, err := os.Lstat(full); err == nil && (st.Mode()&os.ModeSymlink != 0 || pm.IsJunction(full)) {
				return notRestored(errors.New("the leftover directory contains a symlink or junction"))
			}
		}
	}
	if err := syncSteps.copyTree(t, src, dest, true, true); err != nil {
		return notRestored(err)
	}
	manifest[name] = dirHashFS(t.fsys, src, false)
	s.writeManifest(home.WithOverride(ctx, s.Home), manifest)
	if err := s.toggleSuppressedName(home.WithOverride(ctx, s.Home), name, false); err != nil {
		return nil, err
	}
	s.SetState(ctx, name, StateActive)
	return map[string]any{"ok": true, "action": "restored", "path": dest, "message": ""}, nil
}

// Bundled skills the user edited, sorted by name with dest and bundled source
func (s *Scope) ListUserModifiedBundledSkills() ([]map[string]any, error) {
	manifest, err := s.readManifest()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	if len(manifest) == 0 {
		return out, nil
	}
	t := bundledTree()
	for _, b := range discoverBundledSkills(t) {
		origin := manifest[b.name]
		dest := s.relativeDest(b.dir)
		if origin != "" && pathExists(dest) && !matchesOriginHash(dest, origin, "") {
			out = append(out, map[string]any{"name": b.name, "dest": dest, "bundled_src": t.display(b.dir)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out, nil
}

// Raw bytes and text of a diff side, text absent for binary and both absent when unreadable
func readForDiff(read func() ([]byte, error)) ([]byte, *string) {
	data, err := read()
	if err != nil {
		return nil, nil
	}
	if strings.Contains(string(data), "\x00") || !utf8.Valid(data) {
		return data, nil
	}
	text := string(data)
	return data, &text
}

// Diffs the user copy of a bundled skill against stock per file
func (s *Scope) DiffBundledSkill(name string) (map[string]any, error) {
	_, t, byName, err := s.bundledState()
	if err != nil {
		return nil, err
	}
	fail := func(found bool, message string) (map[string]any, error) {
		return map[string]any{"ok": false, "name": name, "found": found, "modified": false, "diffs": []map[string]any{}, "message": message}, nil
	}
	src, ok := byName[name]
	if !ok {
		return fail(false, fmt.Sprintf("'%s' is not a tracked bundled skill (no stock version to diff against). Hub-installed skills use `icebath skills inspect`.", name))
	}
	dest := s.relativeDest(src)
	if !pathExists(dest) {
		return fail(true, fmt.Sprintf("No local copy of '%s' found at %s.", name, dest))
	}
	userFiles, stockFiles := map[string]bool{}, map[string]bool{}
	for _, f := range skillFileListFS(os.DirFS(dest), ".") {
		userFiles[f] = true
	}
	for _, f := range skillFileListFS(t.fsys, src) {
		stockFiles[f] = true
	}
	var all []string
	for f := range userFiles {
		all = append(all, f)
	}
	for f := range stockFiles {
		if !userFiles[f] {
			all = append(all, f)
		}
	}
	sort.Strings(all)
	diffs := []map[string]any{}
	for _, rel := range all {
		if !stockFiles[rel] {
			diffs = append(diffs, map[string]any{"path": rel, "status": "added", "diff": "+ only in your copy: " + rel})
			continue
		}
		if !userFiles[rel] {
			diffs = append(diffs, map[string]any{"path": rel, "status": "removed", "diff": "- only in stock: " + rel})
			continue
		}
		userBytes, userText := readForDiff(func() ([]byte, error) { return os.ReadFile(filepath.Join(dest, filepath.FromSlash(rel))) })
		stockBytes, stockText := readForDiff(func() ([]byte, error) { return fs.ReadFile(t.fsys, src+"/"+rel) })
		if userText == nil || stockText == nil {
			if string(userBytes) != string(stockBytes) || (userBytes == nil) != (stockBytes == nil) {
				diffs = append(diffs, map[string]any{"path": rel, "status": "binary", "diff": "<binary file differs>"})
			}
		} else if *userText != *stockText {
			lines := tools.UnifiedDiff(pystr.Splitlines(*stockText, true), pystr.Splitlines(*userText, true), "stock/"+rel, "yours/"+rel, 3)
			diffs = append(diffs, map[string]any{"path": rel, "status": "modified", "diff": strings.Join(lines, "")})
		}
	}
	message := fmt.Sprintf("'%s' matches the stock version.", name)
	if len(diffs) > 0 {
		message = fmt.Sprintf("'%s' differs from the stock version in %d file(s).", name, len(diffs))
	}
	return map[string]any{"ok": true, "name": name, "found": true, "modified": len(diffs) > 0, "diffs": diffs, "message": message}, nil
}

// Opt out marker messages keyed by enabled and changed
var optOutMessages = map[[2]bool]string{
	{true, true}:   "Opted out of bundled skills. Future install / update / sync runs will not seed bundled skills into this profile.",
	{true, false}:  "Already opted out — marker was already present.",
	{false, true}:  "Opted back in. The next `icebath update` (or `icebath skills opt-in --sync`) will re-seed bundled skills.",
	{false, false}: "Not opted out — no marker to remove.",
}

// Writes or removes the bundled skills opt out marker of the scope home
func (s *Scope) SetBundledSkillsOptOut(enabled bool) map[string]any {
	marker := filepath.Join(s.Home, NoBundledSkillsMarker)
	existed := pathExists(marker)
	var err error
	if enabled {
		if err = os.MkdirAll(s.Home, 0o777); err == nil {
			err = os.WriteFile(marker, []byte("This profile opted out of bundled-skill seeding (`icebath skills opt-out`).\nDelete this file to re-enable sync on the next `icebath update`.\n"), 0o666)
		}
	} else if existed {
		err = os.Remove(marker)
	}
	if err != nil {
		return map[string]any{"ok": false, "changed": false, "marker": marker, "message": fmt.Sprintf("Could not update opt-out marker at %s: %s", marker, pyfs.OSErrorText(err))}
	}
	changed := enabled != existed
	return map[string]any{"ok": true, "changed": changed, "marker": marker, "message": optOutMessages[[2]bool{enabled, changed}]}
}

// Deletes manifest tracked bundled skills still byte identical to their origin hash
func (s *Scope) RemovePristineBundledSkills(ctx context.Context, dryRun bool) (map[string]any, error) {
	manifest, _, byName, err := s.bundledState()
	if err != nil {
		return nil, err
	}
	removed, skipped := []string{}, []map[string]any{}
	names := make([]string, 0, len(manifest))
	for n := range manifest {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		src, ok := byName[name]
		if !ok {
			skipped = append(skipped, map[string]any{"name": name, "reason": "no bundled source (removed upstream)"})
			continue
		}
		dest := s.relativeDest(src)
		if !pathExists(dest) {
			if !dryRun {
				delete(manifest, name)
			}
			continue
		}
		if !matchesOriginHash(dest, manifest[name], "") {
			skipped = append(skipped, map[string]any{"name": name, "reason": "user-modified (kept)"})
			continue
		}
		if !dryRun {
			if err := syncSteps.rmtree(s, dest); err != nil {
				if !config.IsOSError(err) {
					return nil, err
				}
				skipped = append(skipped, map[string]any{"name": name, "reason": "delete failed: " + pyfs.OSErrorText(err)})
				continue
			}
			delete(manifest, name)
		}
		removed = append(removed, name)
	}
	if !dryRun && len(removed) > 0 {
		s.writeManifest(home.WithOverride(ctx, s.Home), manifest)
	}
	verb := "Removed"
	if dryRun {
		verb = "Would remove"
	}
	return map[string]any{"ok": true, "removed": removed, "skipped": skipped, "dry_run": dryRun,
		"message": fmt.Sprintf("%s %d pristine bundled skill(s); kept %d.", verb, len(removed), len(skipped))}, nil
}

// Repr of a pathlib path on this host
func pathRepr(p string) string {
	if filepath.Separator == '\\' {
		return "WindowsPath(" + pyrepr.StrRepr(p) + ")"
	}
	return "PosixPath(" + pyrepr.StrRepr(p) + ")"
}

// Script entry that syncs the home of ctx loudly and prints the new, updated and unchanged summary line
func SyncSkillsMain(ctx context.Context) error {
	fmt.Fprintln(syncOut, "Syncing bundled skills into ~/.icebath/skills/ ...")
	result, err := SyncSkills(ctx, false)
	if err != nil {
		return err
	}
	parts := []string{fmt.Sprintf("%d new", len(result["copied"].([]string))), fmt.Sprintf("%d updated", len(result["updated"].([]string))),
		fmt.Sprintf("%d unchanged", result["skipped"])}
	if names := result["user_modified"].([]string); len(names) > 0 {
		shown := strings.Join(names[:min(5, len(names))], ", ")
		if len(names) > 5 {
			shown += fmt.Sprintf(", +%d more", len(names)-5)
		}
		parts = append(parts, fmt.Sprintf("%d user-modified (kept): %s", len(names), shown))
	}
	if cleaned := result["cleaned"].([]string); len(cleaned) > 0 {
		parts = append(parts, fmt.Sprintf("%d cleaned from manifest", len(cleaned)))
	}
	if backfilled, _ := result["optional_provenance_backfilled"].([]string); len(backfilled) > 0 {
		parts = append(parts, fmt.Sprintf("%d official optional backfilled", len(backfilled)))
	}
	fmt.Fprintf(syncOut, "\nDone: %s. %d total bundled.\n", strings.Join(parts, ", "), result["total_bundled"])
	return nil
}
