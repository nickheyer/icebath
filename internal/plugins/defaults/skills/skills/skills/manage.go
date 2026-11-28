package skills

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Skills the sources a filter selects offer for a query, without repeats, the most trusted first, at most limit
func (h *Hub) Search(ctx context.Context, scope *skills.Scope, query, filter string, limit int) ([]Meta, error) {
	r := h.run(scope, nil)
	sources, err := r.sources()
	if err != nil {
		return nil, err
	}
	return r.unified(ctx, sources, query, filter, limit), nil
}

// One page of every skill the sources a filter selects offer; done hears of each source as it answers. A screen asks the sources for
// fewer skills than the command line
func (h *Hub) Browse(ctx context.Context, scope *skills.Scope, page, size int, filter string, screen bool, done func(source string, n int)) (Page, error) {
	return h.run(scope, nil).browse(ctx, page, size, filter, screen, done)
}

// Identifiers the hub installed in the scope's skills folder, with the catalog card of each ClawHub skill
func (h *Hub) InstalledIDs(scope *skills.Scope) map[string]bool {
	out := map[string]bool{}
	lock, err := skills.ReadLock(scope.Dir())
	if err != nil {
		return out
	}
	for _, entry := range lock.Installed {
		if entry == nil || entry.Identifier == "" {
			continue
		}
		out[entry.Identifier] = true
		if card, ok := clawhubCard(entry.Identifier); ok && entry.Source == "clawhub" {
			out[card] = true
		}
	}
	return out
}

// What a skill of the hub is and the start of its SKILL.md, without installing it; nil when no source has it
func (h *Hub) Inspect(ctx context.Context, scope *skills.Scope, identifier string, say Say) (*Meta, string, error) {
	r := h.run(scope, say)
	sources, err := r.sources()
	if err != nil {
		return nil, "", err
	}
	if identifier = r.full(ctx, identifier, sources); identifier == "" {
		return nil, "", nil
	}
	meta, bundle, _ := r.resolve(ctx, identifier, sources)
	if meta == nil {
		r.fail("No source has %s. Search for it with icebath skills search <query>.", identifier)
		return nil, "", nil
	}
	return meta, preview(bundle), nil
}

// First fifty lines of a bundle's SKILL.md with a count of the rest, empty when it has none
func preview(b *Bundle) string {
	if b == nil {
		return ""
	}
	data, ok := b.file("SKILL.md")
	if !ok {
		return ""
	}
	lines := strings.Split(strings.ToValidUTF8(string(data), "\uFFFD"), "\n")
	more := ""
	if len(lines) > 50 {
		more = fmt.Sprintf("\n\n... (%d more lines)", len(lines)-50)
		lines = lines[:50]
	}
	return strings.Join(lines, "\n") + more
}

// How an installed hub skill stands against its source
type Check struct {
	Name, Identifier, Source string
	// up_to_date, update_available, unavailable, orphaned or invalid_install
	Status string
	// Digest of the installed copy and of the source's now
	Current, Latest string
}

// Names of a lock's skills in order
func names(lock *skills.Lock) []string {
	return slices.Sorted(maps.Keys(lock.Installed))
}

// Checks the installed hub skills, or the one named, against the source each came from only
func (h *Hub) Check(ctx context.Context, scope *skills.Scope, name string) ([]Check, error) {
	checks, err := h.run(scope, nil).check(ctx, name)
	return checks, err
}

func (r *run) check(ctx context.Context, name string) ([]Check, error) {
	lock, err := skills.ReadLock(r.scope.Dir())
	if err != nil {
		return nil, err
	}
	sources, err := r.sources()
	if err != nil {
		return nil, err
	}
	var out []Check
	for _, entryName := range names(lock) {
		if name != "" && entryName != name {
			continue
		}
		entry := lock.Installed[entryName]
		if entry == nil {
			continue
		}
		row := Check{Name: entryName, Identifier: entry.Identifier, Source: entry.Source}
		dir, err := r.installDir(entry.InstallPath, cmp.Or(entryName, "skill"))
		switch {
		case err != nil:
			row.Status = "invalid_install"
		case !fsx.IsDir(dir):
			row.Status = "orphaned"
		}
		if row.Status != "" {
			out = append(out, row)
			continue
		}
		alias := map[string]string{"skills.sh": "skills-sh"}[row.Source]
		matching := slices.DeleteFunc(slices.Clone(sources), func(s Source) bool { return s.ID() != row.Source && s.ID() != alias })
		recorded := ""
		if v, ok := entry.Metadata["source_revision"]; ok && v != nil {
			recorded = fmt.Sprint(v)
		}
		if recorded != "" && slices.ContainsFunc(matching, func(s Source) bool { return s.Revision(ctx, row.Identifier) == recorded }) {
			row.Status, row.Current, row.Latest = "up_to_date", entry.ContentHash, entry.ContentHash
			out = append(out, row)
			continue
		}
		var bundle *Bundle
		for _, src := range matching {
			if bundle = src.Fetch(ctx, row.Identifier); bundle != nil {
				break
			}
		}
		if bundle == nil {
			row.Status = "unavailable"
			out = append(out, row)
			continue
		}
		row.Current, row.Latest = entry.ContentHash, bundleHash(bundle)
		row.Status = "update_available"
		if row.Current == row.Latest {
			row.Status = "up_to_date"
		}
		out = append(out, row)
	}
	return out, nil
}

// Whether the installed copy no longer is what was installed
func (r *run) edited(entry *skills.Installed) bool {
	dir := filepath.Join(r.scope.Dir(), filepath.FromSlash(entry.InstallPath))
	return entry.ContentHash != "" && fsx.IsDir(dir) && skills.ContentHash(dir) != entry.ContentHash
}

// Updates the installed hub skills that have a newer version, or the one named, skipping those the user changed unless forced; false
// when one could not be updated
func (h *Hub) Update(ctx context.Context, scope *skills.Scope, name string, force bool, say Say) (bool, error) {
	r := h.run(scope, say)
	checks, err := r.check(ctx, name)
	if err != nil {
		return false, err
	}
	updates := slices.DeleteFunc(checks, func(c Check) bool { return c.Status != "update_available" })
	if len(updates) == 0 {
		r.line(Dim, "✓ Every skill from the hub is up to date.")
		return true, nil
	}
	lock, err := skills.ReadLock(scope.Dir())
	if err != nil {
		return false, err
	}
	var skipped, failed []string
	for _, c := range updates {
		category := ""
		if entry := lock.Installed[c.Name]; entry != nil {
			if parent := path.Dir(path.Clean(entry.InstallPath)); parent != "." {
				category = parent
			}
			if !force && r.edited(entry) {
				skipped = append(skipped, c.Name)
				r.line(Warn, "⚠ Skipped %s, because you edited it and an update would replace your changes.", c.Name)
				continue
			}
		}
		r.line(Heading, "Updating %s…", c.Name)
		outcome, _, err := h.Stage(ctx, scope, Install{Identifier: c.Identifier, Category: category, Force: true, Source: c.Source}, say)
		if err != nil {
			return false, err
		}
		if outcome == Failed {
			failed = append(failed, c.Name)
		}
	}
	if updated := len(updates) - len(skipped) - len(failed); updated > 0 {
		r.line(Good, "✓ Updated %s.", strs.Count(updated, "skill"))
	}
	if len(skipped) > 0 {
		slices.Sort(skipped)
		r.line(Dim, "Kept your edits in %s: %s.", strs.Count(len(skipped), "skill"), strings.Join(skipped, ", "))
		r.line(Dim, "To replace them with the update, run icebath skills update <name> --force.")
	}
	if len(failed) > 0 {
		slices.Sort(failed)
		r.line(Bad, "✗ These were not updated, for the reasons above: %s.", strings.Join(failed, ", "))
		return false, nil
	}
	return true, nil
}

// Scans the installed hub skills again, or the one named; deep adds the scan of their Python for code loaded by computed names
func (h *Hub) Audit(ctx context.Context, scope *skills.Scope, name string, deep bool, say Say) (bool, error) {
	r := h.run(scope, say)
	lock, err := skills.ReadLock(scope.Dir())
	if err != nil {
		return false, err
	}
	if len(lock.Installed) == 0 {
		r.line(Dim, "You have no skills from the hub to scan. Install one with icebath skills install <identifier>.")
		return true, nil
	}
	targets := slices.DeleteFunc(names(lock), func(n string) bool { return name != "" && n != name || lock.Installed[n] == nil })
	if len(targets) == 0 {
		r.fail("%s is not a skill from the hub, so audit does not scan it.", name)
		return false, nil
	}
	r.line(Heading, "Scanning %s for risks…", strs.Count(len(targets), "skill"))
	for _, n := range targets {
		entry := lock.Installed[n]
		dir := filepath.Join(scope.Dir(), filepath.FromSlash(entry.InstallPath))
		if !fsx.Exists(dir) {
			r.line(Warn, "⚠ The folder of %s is missing: %s", n, entry.InstallPath)
			continue
		}
		result, _, err := r.scan(ctx, dir, cmp.Or(entry.Identifier, entry.Source), "", "")
		if err != nil {
			r.fail("%s could not be scanned: %s", n, strings.TrimSuffix(err.Error(), ".")+".")
			continue
		}
		r.line(Plain, report(result))
		if deep {
			r.line(Plain, deepReport(deepScan(dir), n))
		}
		r.line(Plain, "")
	}
	return true, nil
}

// Removes a skill the hub installed and its record; false with why when it was not installed from the hub or its path is unsafe
func (h *Hub) Uninstall(ctx context.Context, scope *skills.Scope, name string) (bool, string, error) {
	r := h.run(scope, nil)
	lock, err := skills.ReadLock(scope.Dir())
	if err != nil {
		return false, "", err
	}
	entry := lock.Installed[name]
	if entry == nil {
		return false, fmt.Sprintf("%s is not a skill from the hub, so there is nothing to uninstall. It may be a built-in skill.", name), nil
	}
	dir, err := r.installDir(entry.InstallPath, name)
	if err != nil {
		return false, fmt.Sprintf("%s was not uninstalled: %s", name, strings.TrimSuffix(err.Error(), ".")+"."), nil
	}
	if fsx.Exists(dir) {
		if err := os.RemoveAll(dir); err != nil {
			return false, "", err
		}
	}
	err = skills.UpdateLock(ctx, scope.Dir(), func(lock *skills.Lock) (bool, error) {
		delete(lock.Installed, name)
		return true, nil
	})
	if err != nil {
		return false, "", err
	}
	r.audit("UNINSTALL", name, entry.Source, entry.Trust, "n/a", "user_request")
	h.registry.Forget()
	return true, fmt.Sprintf("Uninstalled %s from %s.", name, entry.InstallPath), nil
}

// A GitHub repository the user added to those the hub searches
type Tap struct {
	Repo string
	Path string
}

// The user's taps, in the order added
func (h *Hub) Taps(scope *skills.Scope) ([]Tap, error) {
	taps, err := h.run(scope, nil).taps()
	var out []Tap
	for _, t := range taps {
		out = append(out, Tap{Repo: t.Repo, Path: t.Path})
	}
	return out, err
}

// Adds a repository to the taps, or removes it; false when that changed nothing
func (h *Hub) Tap(ctx context.Context, scope *skills.Scope, repo string, remove bool) (bool, error) {
	r := h.run(scope, nil)
	if err := r.ensure(); err != nil {
		return false, err
	}
	if remove {
		return r.removeTap(ctx, repo)
	}
	return r.addTap(ctx, repo, "skills/")
}

// Trusts a project with its own skills, or stops trusting it, saying what it did; what it did, and false when it could not
func (h *Hub) Trust(scope *skills.Scope, path string, trust bool, say Say) (skills.Trust, bool) {
	r := h.run(scope, say)
	result, err := scope.SetTrusted(path, trust)
	if err != nil {
		r.line(Plain, err.Error())
		return result, false
	}
	switch {
	case !trust && !result.Changed:
		r.line(Plain, "You had not trusted %s, so nothing changed.", result.Root)
		return result, true
	case !trust:
		r.line(Good, "✓ You no longer trust %s.", result.Root)
		r.line(Plain, "icebath no longer loads the skills this project keeps in its own folder.")
		return result, true
	case result.Changed:
		r.line(Good, "✓ You trust %s now.", result.Root)
	default:
		r.line(Plain, "You trust %s already.", result.Root)
	}
	if result.Skills > 0 {
		r.line(Plain, "%s of the project load in sessions started inside it, before skills of the profile with the same name.", strs.Count(result.Skills, "skill"))
	} else {
		r.line(Plain, "The project has no skills yet. Add them under %s.", strings.Join(skills.ProjectSkillDirs(), " or "))
	}
	return result, true
}

// One skill of a snapshot
type SnapshotSkill struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	Identifier string `json:"identifier"`
	Category   string `json:"category"`
}

// The hub's skills and taps of a profile, to install again elsewhere
type Snapshot struct {
	Version    string          `json:"icebath_version"`
	ExportedAt string          `json:"exported_at"`
	Skills     []SnapshotSkill `json:"skills"`
	Taps       []SnapshotTap   `json:"taps"`
}

type SnapshotTap struct {
	Repo string `json:"repo"`
	Path string `json:"path,omitempty"`
}

// The hub's skills and taps of the scope's profile as a snapshot
func (h *Hub) Export(scope *skills.Scope) (Snapshot, error) {
	r := h.run(scope, nil)
	lock, err := skills.ReadLock(scope.Dir())
	if err != nil {
		return Snapshot{}, err
	}
	taps, err := r.taps()
	if err != nil {
		return Snapshot{}, err
	}
	out := Snapshot{Version: provider.Version(), ExportedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00"), Skills: []SnapshotSkill{},
		Taps: []SnapshotTap{}}
	for _, n := range names(lock) {
		entry := lock.Installed[n]
		if entry == nil {
			continue
		}
		category := ""
		if strings.Contains(entry.InstallPath, "/") {
			category = path.Dir(path.Clean(entry.InstallPath))
		}
		out.Skills = append(out.Skills, SnapshotSkill{Name: n, Source: entry.Source, Identifier: entry.Identifier, Category: category})
	}
	for _, t := range taps {
		out.Taps = append(out.Taps, SnapshotTap{Repo: t.Repo, Path: t.Path})
	}
	return out, nil
}

// A snapshot read from its JSON, an error naming what is wrong with it
func ReadSnapshot(data []byte) (Snapshot, error) {
	var out Snapshot
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\uFEFF")), &out); err != nil {
		return Snapshot{}, err
	}
	return out, nil
}
