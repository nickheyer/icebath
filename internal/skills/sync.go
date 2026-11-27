package skills

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tools/diff"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// File in a profile's folder that keeps its skills folder to the essential skills of the seeded sources
const optOutMarker = ".no-bundled-skills"

// One skill of a seeded or offered source: its name and its folder in the source's tree
type packaged struct {
	name string
	dir  string
	tree fs.FS
	// Where the tree is, for messages
	source Source
}

// Path of a tree's folder for messages
func (p packaged) display(rel string) string {
	if p.source.Dir != "" {
		return filepath.Join(p.source.Dir, filepath.FromSlash(rel))
	}
	return p.source.Name + "/" + rel
}

// Whether a path of a tree has an excluded folder or lies in a support folder of a skill of the tree
func treeExcluded(tree fs.FS, rel string) bool {
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		if excluded[part] {
			return true
		}
	}
	for i := 1; i < len(parts)-1; i++ {
		if slices.Contains(supportDirs, parts[i]) && treeFile(tree, strings.Join(parts[:i], "/")+"/SKILL.md") {
			return true
		}
	}
	return false
}

func treeFile(tree fs.FS, p string) bool {
	info, err := fs.Stat(tree, p)
	return err == nil && info.Mode().IsRegular()
}

// Every path of a tree under a folder, in path order part by part
func treePaths(tree fs.FS, dir string) []string {
	var out []string
	fs.WalkDir(tree, dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != dir {
			out = append(out, p)
		}
		return nil
	})
	slices.SortStableFunc(out, func(a, b string) int { return slices.Compare(strings.Split(a, "/"), strings.Split(b, "/")) })
	return out
}

// Skills of the sources of a kind, the first of a name winning
func (s *Scope) shipped(kind Kind) []packaged {
	var out []packaged
	for _, source := range s.registry.kind(s.Config, kind) {
		for _, p := range treePaths(source.Files, ".") {
			if path.Base(p) != "SKILL.md" || treeExcluded(source.Files, p) {
				continue
			}
			dir := path.Dir(p)
			name := path.Base(dir)
			if data, err := fs.ReadFile(source.Files, p); err == nil {
				name = nameIn(decodeText(data), name)
			}
			if !slices.ContainsFunc(out, func(o packaged) bool { return o.name == name }) {
				out = append(out, packaged{name: name, dir: dir, tree: source.Files, source: source})
			}
		}
	}
	return out
}

// Digest of a folder of a tree: its files' paths and bytes, runtime state left out unless asked for
func treeHash(tree fs.FS, dir string, runtime bool) string {
	h := md5.New()
	for _, p := range treePaths(tree, dir) {
		if !runtime && runtimeCache(tree, p, dir) || !treeFile(tree, p) {
			continue
		}
		data, err := fs.ReadFile(tree, p)
		if err != nil {
			break
		}
		rel := p
		if dir != "." {
			rel = strings.TrimPrefix(p, dir+"/")
		}
		h.Write([]byte(rel))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Digest of a skill folder on disk
func dirHash(dir string) string {
	return treeHash(os.DirFS(dir), ".", false)
}

// Whether a folder is still as it was copied: the same digest, or the same with the runtime state an older copy counted
func pristine(dir, origin string) bool {
	return origin != "" && (dirHash(dir) == origin || treeHash(os.DirFS(dir), ".", true) == origin)
}

// Files of a tree's folder by path within it, runtime state left out
func treeFiles(tree fs.FS, dir string) []string {
	out := []string{}
	for _, p := range treePaths(tree, dir) {
		if !runtimeCache(tree, p, dir) && treeFile(tree, p) {
			rel := p
			if dir != "." {
				rel = strings.TrimPrefix(p, dir+"/")
			}
			out = append(out, rel)
		}
	}
	return out
}

func (s *Scope) manifestPath() string { return filepath.Join(s.Dir(), ".bundled_manifest") }

// The seeded skills the profile's folder tracks, by name, with the digest of each as it was copied; empty for an old entry without one
func (s *Scope) manifest() (map[string]string, error) {
	out := map[string]string{}
	entries, err := lines(s.manifestPath())
	if err != nil {
		return nil, err
	}
	for _, line := range entries {
		if name, digest, ok := strings.Cut(line, ":"); ok {
			out[strings.TrimSpace(name)] = strings.TrimSpace(digest)
		} else {
			out[line] = ""
		}
	}
	return out, nil
}

func (s *Scope) writeManifest(entries map[string]string) error {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		b.WriteString(name + ":" + entries[name] + "\n")
	}
	return fsx.Replace(s.manifestPath(), []byte(b.String()), 0o644)
}

// What a sync did
type Synced struct {
	// Copied for the first time
	Copied []string
	// Replaced by a newer copy, the user's copy being untouched
	Updated []string
	// Unchanged, or left alone
	Skipped int
	// Changed by the user, so kept as they are
	Modified []string
	// No longer shipped, so no longer tracked
	Cleaned []string
	// Archived by the curator, so not copied again
	Suppressed []string
	// Moved to where they are shipped now
	Relocated []string
	// Provided by an extra skill folder, so not copied
	Deferred []string
	// Official optional skills found installed by hand, recorded as installed from the hub
	Backfilled []string
	// Skills shipped
	Total int
	// Whether the profile opted out, so only the essential skills were synced
	OptedOut bool
	// What happened to each skill, as the user reads it
	Lines []string
}

func (st *Synced) say(format string, args ...any) {
	st.Lines = append(st.Lines, fmt.Sprintf(format, args...))
}

// Names of the skills the extra folders hold, by folder and declared name
func (s *Scope) externalIndex() map[string]bool {
	out := map[string]bool{}
	for _, dir := range s.External() {
		for _, skillMD := range skillFiles(dir) {
			out[filepath.Base(filepath.Dir(skillMD))] = true
			out[readName(skillMD, "")] = true
		}
	}
	delete(out, "")
	return out
}

// Syncs the seeded skills of the profile once per process, before the profile's skills are first used
func (s *Scope) Seed(ctx context.Context) error {
	st := s.registry.shared()
	st.mu.Lock()
	done := st.synced[s.Home()]
	st.mu.Unlock()
	if done {
		return nil
	}
	if _, err := s.Sync(ctx); err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.synced == nil {
		st.synced = map[string]bool{}
	}
	st.synced[s.Home()] = true
	return nil
}

// Brings the seeded skills into the profile's skills folder: new ones copied, unchanged copies updated, the user's changes and deletions
// kept, skills an extra folder provides left to it; only the essential ones when the profile opted out
func (s *Scope) Sync(ctx context.Context) (*Synced, error) {
	result := &Synced{OptedOut: fsx.Exists(filepath.Join(s.Home(), optOutMarker))}
	if len(s.registry.kind(s.Config, Seeded)) == 0 {
		return result, nil
	}
	if result.OptedOut {
		result.say("  This profile does not take built-in skills, because %s is in its skills folder, so only the essential ones are copied.", optOutMarker)
	}
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		return nil, err
	}
	_, release, err := hold(ctx, filepath.Join(s.Dir(), ".locks", "sync.lock"))
	if err != nil {
		return nil, err
	}
	defer release()
	shipped := s.shipped(Seeded)
	if result.OptedOut {
		shipped = slices.DeleteFunc(shipped, func(p packaged) bool { return !Essential[p.name] })
	}
	result.Total = len(shipped)
	suppressed, err := s.Suppressed()
	if err != nil {
		return nil, err
	}
	external := s.externalIndex()
	manifest, err := s.manifest()
	if err != nil {
		return nil, err
	}
	var index map[string][]string
	var hubPaths map[string]bool
	for _, p := range shipped {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if suppressed[p.name] && !Essential[p.name] {
			result.Suppressed = append(result.Suppressed, p.name)
			continue
		}
		dest := filepath.Join(s.Dir(), filepath.FromSlash(p.dir))
		digest := treeHash(p.tree, p.dir, false)
		recoverBackup(dest)
		if _, tracked := manifest[p.name]; tracked && !fsx.Exists(dest) {
			if index == nil {
				index, hubPaths = s.installedByName(), s.hubPaths()
			}
			if s.follow(result, manifest, index, hubPaths, p.name, dest) {
				result.Relocated = append(result.Relocated, p.name)
			}
		}
		_, tracked := manifest[p.name]
		switch {
		case external[p.name]:
			if err := s.deferTo(result, manifest, p.name, dest, digest); err != nil {
				return nil, err
			}
		case !tracked:
			s.install(result, manifest, p, dest, digest)
		case fsx.Exists(dest):
			s.refresh(result, manifest, p, dest, digest)
		default:
			result.Skipped++
		}
	}
	if !result.OptedOut {
		s.clean(result, manifest, shipped)
	}
	s.describeCategories(shipped, result.OptedOut)
	if err := s.writeManifest(manifest); err != nil {
		return nil, err
	}
	backfilled, err := s.backfill(result)
	if err != nil {
		return nil, err
	}
	result.Backfilled = backfilled
	s.registry.Forget()
	return result, nil
}

// Skill folders of the profile's own folder by declared name
func (s *Scope) installedByName() map[string][]string {
	out := map[string][]string{}
	for _, skillMD := range skillFiles(s.Dir()) {
		dir := filepath.Dir(skillMD)
		name := readName(skillMD, filepath.Base(dir))
		out[name] = append(out[name], dir)
	}
	return out
}

// Install paths of the hub's lock, without slashes at their ends
func (s *Scope) hubPaths() map[string]bool {
	out := map[string]bool{}
	lock, err := ReadLock(s.Dir())
	if err != nil {
		return out
	}
	for _, entry := range lock.Installed {
		if entry != nil && entry.InstallPath != "" {
			out[strings.Trim(entry.InstallPath, "/")] = true
		}
	}
	return out
}

// Path of a folder of the skills folder, with slashes, false outside it
func (s *Scope) rel(p string) (string, bool) {
	rel, err := filepath.Rel(s.Dir(), p)
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// Moves an unchanged copy of a seeded skill that moved in its source to where it is now, true when it did
func (s *Scope) follow(result *Synced, manifest map[string]string, index map[string][]string, hubPaths map[string]bool, name, dest string) bool {
	origin := manifest[name]
	if origin == "" {
		return false
	}
	destRel, _ := s.rel(dest)
	for _, dir := range index[name] {
		rel, ok := s.rel(dir)
		if dir == dest || !fsx.IsDir(dir) || !ok || hubPaths[rel] {
			continue
		}
		if !pristine(dir, origin) {
			result.say("  ⚠ %s moved to %s, but your edited copy in %s stays where it is and takes no updates. Run icebath skills reset %s --restore to move to the new place.", name, destRel, rel, name)
			continue
		}
		if move(dir, dest) != nil {
			return false
		}
		result.say("  Moved %s from %s to %s.", name, rel, destRel)
		return true
	}
	return false
}

// Path with its last extension replaced by .bak
func backupOf(dest string) string {
	return withExt(dest, ".bak")
}

// Puts back a copy an interrupted update left aside
func recoverBackup(dest string) {
	if orphan := backupOf(dest); fsx.Exists(orphan) && !fsx.Exists(dest) {
		move(orphan, dest)
	}
}

// Leaves a skill an extra folder provides to it, removing an unchanged copy that would hide it
func (s *Scope) deferTo(result *Synced, manifest map[string]string, name, dest, digest string) error {
	result.Deferred = append(result.Deferred, name)
	result.Skipped++
	result.say("  Left %s to the extra skill folder that has it, so it was not copied here.", name)
	if fsx.Exists(dest) && dirHash(dest) == digest {
		if err := s.removeTree(dest); err != nil {
			return err
		}
		result.say("  ✓ Removed an old copy of %s that hid the one in the extra folder.", name)
		delete(manifest, name)
	}
	return nil
}

// Copies a skill never offered before, never over a skill of the user of the same name
func (s *Scope) install(result *Synced, manifest map[string]string, p packaged, dest, digest string) {
	if fsx.Exists(dest) {
		result.Skipped++
		if dirHash(dest) == digest {
			manifest[p.name] = digest
		} else {
			result.say("  ⚠ %s is a new built-in skill, but you have a skill of that name already, so yours stays. Run icebath skills reset %s --restore to take the built-in one instead.", p.name, p.name)
		}
		return
	}
	if err := copyTree(p.tree, p.dir, dest, false, false); err != nil {
		result.say("  ✗ %s could not be copied: %s.", p.name, pathError(err))
		return
	}
	result.Copied = append(result.Copied, p.name)
	manifest[p.name] = digest
	result.say("  Copied %s.", p.name)
}

// Replaces a tracked copy with the shipped one when that changed and the copy did not
func (s *Scope) refresh(result *Synced, manifest map[string]string, p packaged, dest, digest string) {
	origin := manifest[p.name]
	if origin != "" && digest == origin {
		result.Skipped++
		return
	}
	if origin == "" {
		manifest[p.name] = dirHash(dest)
		result.Skipped++
		return
	}
	if !pristine(dest, origin) {
		result.Modified = append(result.Modified, p.name)
		result.say("  Left %s as it is, because you edited it.", p.name)
		return
	}
	if err := s.replace(p, dest); err != nil {
		result.say("  ✗ %s could not be updated: %s.", p.name, pathError(err))
		return
	}
	manifest[p.name] = digest
	result.Updated = append(result.Updated, p.name)
	result.say("  Updated %s.", p.name)
}

// Replaces a folder with a fresh copy, the old one kept aside until the copy is whole and put back when it fails
func (s *Scope) replace(p packaged, dest string) error {
	aside := backupOf(dest)
	if fsx.Exists(aside) {
		if err := s.removeTree(aside); err != nil {
			return err
		}
	}
	if err := os.Rename(dest, aside); err != nil {
		return err
	}
	if err := copyTree(p.tree, p.dir, dest, false, false); err != nil {
		if fsx.Exists(dest) {
			s.removeTree(dest)
		}
		if !fsx.Exists(dest) {
			return errors.Join(err, os.Rename(aside, dest))
		}
		return err
	}
	return s.removeTree(aside)
}

// Stops tracking skills no longer shipped that are gone from the profile
func (s *Scope) clean(result *Synced, manifest map[string]string, shipped []packaged) {
	var gone []string
	for name := range manifest {
		if !slices.ContainsFunc(shipped, func(p packaged) bool { return p.name == name }) {
			gone = append(gone, name)
		}
	}
	if len(gone) == 0 {
		return
	}
	slices.Sort(gone)
	present := map[string]bool{}
	for name := range s.installedByName() {
		present[name] = true
	}
	if entries, err := os.ReadDir(s.ArchiveDir()); err == nil {
		for _, entry := range entries {
			if fsx.IsDir(filepath.Join(s.ArchiveDir(), entry.Name())) {
				present[entry.Name()] = true
			}
		}
	}
	for _, name := range gone {
		if !present[name] {
			result.Cleaned = append(result.Cleaned, name)
			delete(manifest, name)
		}
	}
}

// Copies the category descriptions of the seeded sources that are missing, only those of the essential skills' categories when opted out
func (s *Scope) describeCategories(shipped []packaged, essential bool) {
	var only map[string]bool
	if essential {
		only = map[string]bool{}
		for _, p := range shipped {
			only[path.Dir(p.dir)] = true
		}
	}
	for _, source := range s.registry.kind(s.Config, Seeded) {
		for _, p := range treePaths(source.Files, ".") {
			if path.Base(p) != "DESCRIPTION.md" || only != nil && !only[path.Dir(p)] {
				continue
			}
			dest := filepath.Join(s.Dir(), filepath.FromSlash(p))
			if fsx.Exists(dest) {
				continue
			}
			if os.MkdirAll(filepath.Dir(dest), 0o755) == nil {
				copyFile(source.Files, p, dest)
			}
		}
	}
}

// Removes a tree strictly inside the skills folder, making read only entries writable when that is what stops it
func (s *Scope) removeTree(p string) error {
	target, root := resolve(p), resolve(s.Dir())
	if target == root || !within(target, root) {
		return fmt.Errorf("icebath does not remove %s, because it is not inside %s.", target, root)
	}
	if err := os.RemoveAll(p); err == nil {
		return nil
	}
	filepath.WalkDir(p, func(entry string, d fs.DirEntry, err error) error {
		os.Chmod(entry, 0o700)
		return nil
	})
	return os.RemoveAll(p)
}

// An offered skill: its folder in the source's tree and its path in a skills folder
type offered struct {
	packaged
	installPath string
}

// Skills of the offered sources in path order, those whose path is unsafe left out
func (s *Scope) offers() []offered {
	var out []offered
	for _, source := range s.registry.kind(s.Config, Offered) {
		for _, p := range treePaths(source.Files, ".") {
			if path.Base(p) != "SKILL.md" || treeExcluded(source.Files, p) {
				continue
			}
			dir := path.Dir(p)
			if dir == "." || strings.HasPrefix(dir, "/") || traversal(dir) {
				continue
			}
			name := path.Base(dir)
			if data, err := fs.ReadFile(source.Files, p); err == nil {
				name = nameIn(decodeText(data), name)
			}
			out = append(out, offered{packaged: packaged{name: name, dir: dir, tree: source.Files, source: source}, installPath: dir})
		}
	}
	return out
}

// Official optional skills that were put in the profile by hand, recorded in the hub's lock as installed from the official source
func (s *Scope) backfill(result *Synced) ([]string, error) {
	all := s.offers()
	if len(all) == 0 {
		return []string{}, nil
	}
	lock, err := ReadLock(s.Dir())
	if err != nil && result != nil {
		result.say("  ✗ %s The optional skills were not checked.", strings.TrimSuffix(err.Error(), ".")+".")
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	recorded := map[string]bool{}
	for _, entry := range lock.Installed {
		if entry != nil {
			recorded[entry.InstallPath] = true
		}
	}
	backfilled := []string{}
	var byFolder map[string][]string
	for _, o := range all {
		lockName := path.Base(o.dir)
		if lock.Installed[lockName] != nil || recorded[o.installPath] {
			continue
		}
		install := o.installPath
		dest := filepath.Join(s.Dir(), filepath.FromSlash(install))
		if !fsx.IsDir(dest) {
			if byFolder == nil {
				byFolder = s.foldersByName()
			}
			found := byFolder[lockName]
			if len(found) != 1 {
				continue
			}
			rel, ok := s.rel(found[0])
			if !ok {
				continue
			}
			dest, install = found[0], rel
		}
		if recorded[install] || dirHash(dest) != treeHash(o.tree, o.dir, false) {
			continue
		}
		stamp := now()
		lock.Installed[lockName] = &Installed{
			Source: "official", Identifier: "official/" + install, Trust: "builtin", Verdict: "backfilled", ContentHash: ContentHash(dest),
			InstallPath: install, Files: treeFiles(os.DirFS(dest), "."), Metadata: map[string]any{"backfilled_from": "optional-skills"},
			InstalledAt: stamp, UpdatedAt: stamp,
		}
		recorded[install] = true
		backfilled = append(backfilled, lockName)
		if result != nil {
			result.say("  Recorded that %s came from the optional skills.", lockName)
		}
	}
	if len(backfilled) > 0 {
		if err := WriteLock(s.Dir(), lock); err != nil {
			return nil, err
		}
	}
	return backfilled, nil
}

// Skill folders of the profile's own folder inside it, by folder name
func (s *Scope) foldersByName() map[string][]string {
	out := map[string][]string{}
	root := resolve(s.Dir())
	for _, skillMD := range skillFiles(s.Dir()) {
		if dir := filepath.Dir(skillMD); within(resolve(dir), root) {
			out[filepath.Base(dir)] = append(out[filepath.Base(dir)], dir)
		}
	}
	return out
}

// What a repair of official optional skills did
type Repaired struct {
	Restored   []string
	Backfilled []string
	// Copies moved aside to put the official one in place, by their path in the skills folder
	BackedUp []string
	// Folder they were moved to, none when nothing was
	BackupDir string
}

// Records official optional skills found in the profile as installed from the hub, and with restore puts the official copy of one, or
// of all, in place, moving aside any copy in the way
func (s *Scope) Repair(name string, restore bool) (*Repaired, error) {
	all := s.offers()
	var targets []offered
	switch {
	case len(all) > 0 && (name == "all" || name == "*"):
		targets = all
	default:
		for _, o := range all {
			if o.name == name || path.Base(o.dir) == name {
				targets = []offered{o}
				break
			}
		}
		if len(targets) == 0 {
			if len(all) == 0 {
				return nil, errors.New("The folder of optional skills is missing. Set Source folder in /settings under Skills › Optional skills.")
			}
			return nil, errors.New("There is no optional skill named " + name + ".")
		}
	}
	result := &Repaired{Restored: []string{}, BackedUp: []string{}}
	backupRoot := filepath.Join(s.Dir(), ".restore-backups", "official-optional-"+time.Now().UTC().Format("20060102-150405"))
	if restore {
		for _, o := range targets {
			dest := filepath.Join(s.Dir(), filepath.FromSlash(o.installPath))
			canonical := fsx.Exists(dest) && dirHash(dest) == treeHash(o.tree, o.dir, false)
			folder := path.Base(o.dir)
			for _, skillMD := range skillFiles(s.Dir()) {
				dir := filepath.Dir(skillMD)
				if dir != dest && fsx.Exists(dir) && (filepath.Base(dir) == folder || readName(skillMD, filepath.Base(dir)) == o.name) {
					rel, err := s.moveAside(dir, backupRoot)
					if err != nil {
						return nil, err
					}
					result.BackedUp = append(result.BackedUp, rel)
				}
			}
			if fsx.Exists(dest) && !canonical {
				rel, err := s.moveAside(dest, backupRoot)
				if err != nil {
					return nil, err
				}
				result.BackedUp = append(result.BackedUp, rel)
			}
			if !fsx.Exists(dest) {
				if err := copyTree(o.tree, o.dir, dest, false, false); err != nil {
					return nil, err
				}
				result.Restored = append(result.Restored, folder)
			}
		}
	}
	backfilled, err := s.backfill(nil)
	if err != nil {
		return nil, err
	}
	result.Backfilled = backfilled
	if len(result.BackedUp) > 0 {
		result.BackupDir = backupRoot
	}
	s.registry.Forget()
	return result, nil
}

// Moves a skill folder into the backup folder under its path in the skills folder, numbered when that is taken
func (s *Scope) moveAside(dir, backupRoot string) (string, error) {
	rel, err := filepath.Rel(s.Dir(), dir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(backupRoot, rel)
	for n := 1; fsx.Lexists(target); n++ {
		target = filepath.Join(backupRoot, filepath.Dir(rel), fmt.Sprintf("%s-%d", filepath.Base(rel), n))
	}
	return filepath.ToSlash(rel), move(dir, target)
}

// The seeded skill of the name
func (s *Scope) seeded(name string) (packaged, bool) {
	for _, p := range s.shipped(Seeded) {
		if p.name == name {
			return p, true
		}
	}
	return packaged{}, false
}

// Stops tracking a built in skill so it follows its source again from the profile's copy; with restore the copy is replaced by the
// source's first. The sync after it
func (s *Scope) Reset(ctx context.Context, name string, restore bool) (string, *Synced, error) {
	manifest, err := s.manifest()
	if err != nil {
		return "", nil, err
	}
	_, tracked := manifest[name]
	p, shipped := s.seeded(name)
	if !tracked && !shipped {
		return "", nil, fmt.Errorf("%s is not a built-in skill, so there is nothing to reset. Remove a skill from the hub with icebath skills uninstall %s.", name, name)
	}
	deleted := false
	if restore {
		if !shipped {
			return "", nil, fmt.Errorf("%s is no longer a built-in skill, so there is no built-in version to restore. Its record was kept.", name)
		}
		dest := filepath.Join(s.Dir(), filepath.FromSlash(p.dir))
		if deleted = fsx.Exists(dest); deleted {
			if err := s.removeTree(dest); err != nil {
				return "", nil, fmt.Errorf("Your copy in %s could not be deleted: %s. Nothing changed.", dest, pathError(err))
			}
		}
	}
	if tracked {
		delete(manifest, name)
		if err := s.writeManifest(manifest); err != nil {
			return "", nil, err
		}
	}
	synced, err := s.Sync(ctx)
	if err != nil {
		return "", nil, err
	}
	switch {
	case !restore:
		return fmt.Sprintf("✓ %s takes updates again, starting from your copy as it is now.", name), synced, nil
	case deleted:
		return fmt.Sprintf("✓ Replaced your copy of %s with the built-in version.", name), synced, nil
	}
	return fmt.Sprintf("✓ Copied the built-in version of %s, since you had no copy of it.", name), synced, nil
}

// Built in skill an install identifier names: a bare name, or a path of this project's own bundled skills; none otherwise
func (s *Scope) BundledFor(identifier string) string {
	ident := strings.Trim(strings.TrimSpace(identifier), "/")
	lowered := strings.ToLower(ident)
	if strings.Contains(ident, "/") && !strings.HasPrefix(lowered, "nickheyer/icebath/pkg/skills/bundled/") && !strings.HasPrefix(lowered, "skills-sh/nickheyer/icebath/") {
		return ""
	}
	name := ident[strings.LastIndex(ident, "/")+1:]
	if _, ok := s.seeded(name); ok {
		return name
	}
	return ""
}

// What making a built in skill active did: restored it, found it present, or found the hub's skill of its name in its place
type Ensured struct {
	Action string
	// Where the skill is, none when an extra folder provides it or the hub's shadows it
	Path string
	// What the user should know, none when nothing
	Message string
}

// Makes a built in skill active: a copy present, or one an extra folder provides, is left as it is; one the hub shadows is reported;
// else it is copied in again and taken off the curator's suppressed list
func (s *Scope) Ensure(ctx context.Context, name string) (Ensured, error) {
	p, ok := s.seeded(name)
	if !ok {
		return Ensured{}, fmt.Errorf("There is no built-in skill named %s.", name)
	}
	if s.HubInstalled(name) {
		return Ensured{Action: "hub_shadowed", Message: fmt.Sprintf("%s is a built-in skill, but a skill from the hub has the same name and hides it. To get the built-in one back, run icebath skills uninstall %s, then icebath skills install %s.", name, name, name)}, nil
	}
	dest := filepath.Join(s.Dir(), filepath.FromSlash(p.dir))
	skillMD := filepath.Join(dest, "SKILL.md")
	if dirs := s.installedByName()[name]; len(dirs) > 0 {
		return Ensured{Action: "present", Path: dirs[0]}, nil
	}
	if s.externalIndex()[name] {
		return Ensured{Action: "present"}, nil
	}
	refuse := func(reason string) (Ensured, error) {
		return Ensured{}, fmt.Errorf("The built-in skill %s could not be copied to %s: %s.", name, dest, strings.TrimSuffix(reason, "."))
	}
	for at := dest; within(at, s.Dir()) && at != filepath.Clean(s.Dir()); at = filepath.Dir(at) {
		if redirect(at) {
			return refuse("the way to it passes through a link")
		}
	}
	if fsx.Lexists(skillMD) {
		return refuse("the folder holds the SKILL.md of another skill")
	}
	if fsx.Exists(dest) {
		linked := false
		filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
			linked = linked || err == nil && redirect(p)
			return nil
		})
		if linked {
			return refuse("the folder left there holds a link")
		}
	}
	if err := copyTree(p.tree, p.dir, dest, true, true); err != nil {
		return refuse(pathError(err))
	}
	manifest, err := s.manifest()
	if err != nil {
		return Ensured{}, err
	}
	manifest[name] = treeHash(p.tree, p.dir, false)
	if err := s.writeManifest(manifest); err != nil {
		return Ensured{}, err
	}
	if err := s.suppress(name, false); err != nil {
		return Ensured{}, err
	}
	if err := s.SetState(ctx, name, StateActive); err != nil {
		return Ensured{}, err
	}
	s.registry.Forget()
	return Ensured{Action: "restored", Path: dest}, nil
}

// Built in skills the user changed, sorted by name
func (s *Scope) Modified() ([]string, error) {
	manifest, err := s.manifest()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, p := range s.shipped(Seeded) {
		dest := filepath.Join(s.Dir(), filepath.FromSlash(p.dir))
		if origin := manifest[p.name]; origin != "" && fsx.Exists(dest) && !pristine(dest, origin) {
			out = append(out, p.name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// One file of the difference between the user's copy of a built in skill and the shipped one
type FileDiff struct {
	Path string
	// added, removed, modified or binary
	Status string
	// The unified diff of a modified file
	Diff string
}

// How the profile's copy of a built in skill differs from the shipped one, file by file
func (s *Scope) Diff(name string) ([]FileDiff, error) {
	p, ok := s.seeded(name)
	if !ok {
		return nil, fmt.Errorf("%s is not a built-in skill, so there is no built-in version to compare it with. See a skill from the hub with icebath skills inspect %s.", name, name)
	}
	dest := filepath.Join(s.Dir(), filepath.FromSlash(p.dir))
	if !fsx.Exists(dest) {
		return nil, fmt.Errorf("You have no copy of %s in %s.", name, dest)
	}
	yours, stock := treeFiles(os.DirFS(dest), "."), treeFiles(p.tree, p.dir)
	all := slices.Clone(yours)
	for _, f := range stock {
		if !slices.Contains(all, f) {
			all = append(all, f)
		}
	}
	slices.Sort(all)
	out := []FileDiff{}
	for _, rel := range all {
		switch {
		case !slices.Contains(stock, rel):
			out = append(out, FileDiff{Path: rel, Status: "added"})
			continue
		case !slices.Contains(yours, rel):
			out = append(out, FileDiff{Path: rel, Status: "removed"})
			continue
		}
		mine, errMine := os.ReadFile(filepath.Join(dest, filepath.FromSlash(rel)))
		theirs, errTheirs := fs.ReadFile(p.tree, p.dir+"/"+rel)
		textual := errMine == nil && errTheirs == nil && utf8.Valid(mine) && utf8.Valid(theirs) && !strings.ContainsRune(string(mine), 0) && !strings.ContainsRune(string(theirs), 0)
		switch {
		case !textual && (string(mine) != string(theirs) || (errMine == nil) != (errTheirs == nil)):
			out = append(out, FileDiff{Path: rel, Status: "binary"})
		case textual && string(mine) != string(theirs):
			out = append(out, FileDiff{Path: rel, Status: "modified", Diff: diff.Unified(textLines(string(theirs)), textLines(string(mine)), "stock/"+rel, "yours/"+rel, 3)})
		}
	}
	return out, nil
}

// Lines of a text, each with its own ending
func textLines(text string) []string {
	var out []string
	for line := range strings.Lines(text) {
		out = append(out, line)
	}
	return out
}

// Opts the profile out of the seeded skills, or back in: what changed, as the user reads it
func (s *Scope) OptOut(on bool) (string, error) {
	marker := filepath.Join(s.Home(), optOutMarker)
	had := fsx.Exists(marker)
	var err error
	switch {
	case on && !had:
		err = os.WriteFile(marker, []byte("This profile opted out of bundled-skill seeding (`icebath skills opt-out`).\nDelete this file to re-enable sync.\n"), 0o644)
	case !on && had:
		err = os.Remove(marker)
	}
	if err != nil {
		return "", fmt.Errorf("The marker file %s could not be changed: %s.", marker, pathError(err))
	}
	switch {
	case on && !had:
		return "✓ icebath no longer copies built-in skills into this profile.", nil
	case on:
		return "icebath already does not copy built-in skills into this profile.", nil
	case had:
		return "✓ icebath copies built-in skills into this profile again, the next time it starts or when you run icebath skills opt-in --sync.", nil
	}
	return "icebath already copies built-in skills into this profile.", nil
}

// One built in skill a removal kept, and why
type Kept struct {
	Name   string
	Reason string
}

// Deletes the built in skills still as they were copied, the user's changed ones kept; dry only says which
func (s *Scope) RemovePristine(dry bool) ([]string, []Kept, error) {
	manifest, err := s.manifest()
	if err != nil {
		return nil, nil, err
	}
	removed, kept := []string{}, []Kept{}
	for _, name := range slices.Sorted(maps.Keys(manifest)) {
		p, ok := s.seeded(name)
		if !ok {
			kept = append(kept, Kept{name, "no bundled source (removed upstream)"})
			continue
		}
		dest := filepath.Join(s.Dir(), filepath.FromSlash(p.dir))
		if !fsx.Exists(dest) {
			if !dry {
				delete(manifest, name)
			}
			continue
		}
		if !pristine(dest, manifest[name]) {
			kept = append(kept, Kept{name, "user-modified (kept)"})
			continue
		}
		if !dry {
			if err := s.removeTree(dest); err != nil {
				kept = append(kept, Kept{name, "delete failed: " + pathError(err)})
				continue
			}
			delete(manifest, name)
		}
		removed = append(removed, name)
	}
	if !dry && len(removed) > 0 {
		if err := s.writeManifest(manifest); err != nil {
			return nil, nil, err
		}
		s.registry.Forget()
	}
	return removed, kept, nil
}

// Names of the built in skills the profile's folder tracks
func (s *Scope) Builtin() (map[string]bool, error) {
	manifest, err := s.manifest()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for name := range manifest {
		out[name] = true
	}
	return out, nil
}

// File whose presence opts the profile out of the seeded skills
func (s *Scope) OptOutMarker() string {
	return filepath.Join(s.Home(), optOutMarker)
}
