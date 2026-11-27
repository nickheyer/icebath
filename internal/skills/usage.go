package skills

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Lifecycle states of a skill
const (
	StateActive   = "active"
	StateStale    = "stale"
	StateArchived = "archived"
)

// Who made a skill, as its record says
const (
	ByAgent     = "agent"
	ByLearn     = "learn"
	ByInstalled = "installed"
)

// What is known of one skill's use: how often it was loaded, viewed and changed, its lifecycle state and the user's holds on it
type Record struct {
	CreatedBy                 string `json:"created_by,omitempty"`
	UseCount                  int64  `json:"use_count"`
	ViewCount                 int64  `json:"view_count"`
	LastUsedAt                string `json:"last_used_at,omitempty"`
	LastViewedAt              string `json:"last_viewed_at,omitempty"`
	PatchCount                int64  `json:"patch_count"`
	PatchGeneration           int64  `json:"patch_generation"`
	LastReusedPatchGeneration int64  `json:"last_reused_patch_generation"`
	LastPatchedAt             string `json:"last_patched_at,omitempty"`
	CreatedAt                 string `json:"created_at,omitempty"`
	State                     string `json:"state"`
	// Kept from automatic maintenance; skill_manage may not delete it
	Pinned     bool   `json:"pinned"`
	ArchivedAt string `json:"archived_at,omitempty"`
	// When the curator first saw it, which starts its inactivity clock
	FirstSeenAt string `json:"first_seen_at,omitempty"`
	// The user's content lock: the agent may not change it at all
	Locked bool `json:"locked,omitempty"`
	// Older spelling of CreatedBy agent
	AgentCreated bool `json:"agent_created,omitempty"`
}

func fresh() *Record {
	return &Record{CreatedAt: now(), State: StateActive}
}

// Latest time it was used, viewed or changed, none when never
func (r *Record) Activity() string {
	var latest time.Time
	out := ""
	for _, stamp := range []string{r.LastUsedAt, r.LastViewedAt, r.LastPatchedAt} {
		if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil && (out == "" || t.After(latest)) {
			latest, out = t, stamp
		}
	}
	return out
}

// Times it was used, viewed or changed
func (r *Record) Count() int64 {
	return r.UseCount + r.ViewCount + r.PatchCount
}

// Whether the curator manages it
func (r *Record) Managed() bool {
	return r != nil && (r.CreatedBy == ByAgent || r.AgentCreated)
}

func (s *Scope) usageFile() string { return filepath.Join(s.Dir(), ".usage.json") }

// Folder archived skills are moved to
func (s *Scope) ArchiveDir() string { return filepath.Join(s.Dir(), ".archive") }

type heldKey struct{}

// Holds an exclusive lock on the path across processes, again for a context already holding it; the returned context holds it
func hold(ctx context.Context, path string) (context.Context, func(), error) {
	key := filepath.Clean(path)
	held, _ := ctx.Value(heldKey{}).(map[string]bool)
	if held[key] {
		return ctx, func() {}, nil
	}
	release, err := fsx.Lock(ctx, key, -1)
	if err != nil {
		return ctx, nil, err
	}
	next := map[string]bool{key: true}
	for k := range held {
		next[k] = true
	}
	return context.WithValue(ctx, heldKey{}, next), release, nil
}

// Every record of the profile's skills, by name; none when there is no file
func (s *Scope) Usage() (map[string]*Record, error) {
	out := map[string]*Record{}
	data, err := os.ReadFile(s.usageFile())
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(data), "\uFEFF")), &raw); err != nil {
		return nil, fmt.Errorf("The skill usage file %s can't be read: %w", s.usageFile(), err)
	}
	for name, body := range raw {
		record := &Record{}
		if json.Unmarshal(body, record) == nil {
			out[name] = record
		}
	}
	return out, nil
}

// Record of a skill, a fresh one when it has none
func (s *Scope) Record(name string) (*Record, error) {
	usage, err := s.Usage()
	if err != nil {
		return nil, err
	}
	if record := usage[name]; record != nil {
		return record, nil
	}
	return fresh(), nil
}

// Changes the records under their lock and writes them when change says so
func (s *Scope) update(ctx context.Context, change func(usage map[string]*Record) (bool, error)) error {
	_, release, err := hold(ctx, s.usageFile()+".lock")
	if err != nil {
		return err
	}
	defer release()
	usage, err := s.Usage()
	if err != nil {
		return err
	}
	dirty, err := change(usage)
	if err != nil || !dirty {
		return err
	}
	data, err := json.MarshalIndent(usage, "", "  ")
	if err != nil {
		return err
	}
	return fsx.Replace(s.usageFile(), data, 0o644)
}

// Changes one skill's record, made fresh when it has none; eligible limits it to skills the curator may manage
func (s *Scope) mutate(ctx context.Context, name string, eligible bool, change func(r *Record)) error {
	if name == "" {
		return nil
	}
	if eligible {
		ok, err := s.Eligible(name, "")
		if err != nil || !ok {
			return err
		}
	}
	return s.update(ctx, func(usage map[string]*Record) (bool, error) {
		if usage[name] == nil {
			usage[name] = fresh()
		}
		change(usage[name])
		return true, nil
	})
}

// Counts a view of a skill
func (s *Scope) Viewed(ctx context.Context, name string) error {
	return s.mutate(ctx, name, false, func(r *Record) {
		r.ViewCount++
		r.LastViewedAt = now()
	})
}

// Tells the on_skill_lifecycle hooks of a change of the skill's state its record took: what the change was, where the skill comes from,
// the turn it happened in, and for a load its count and whether it was a reuse, after a change of the skill or not
func (s *Scope) lifecycle(ctx context.Context, name, action string, uses, reused, afterPatch any) {
	h := s.registry.shared().hooks
	if name == "" || !h.Has(s.Config, hooks.SkillLifecycle) {
		return
	}
	provenance, err := s.origin(name)
	if err != nil {
		slog.Debug(fmt.Sprintf("skill_usage lifecycle hook failed for %s/%s", name, action), "error", err)
		return
	}
	turn := hooks.TurnOf(ctx)
	h.Fire(ctx, s.Config, hooks.Skill{Action: action, Name: name, Provenance: provenance, Task: turn.Task, Session: cmp.Or(turn.Session, s.Session),
		Uses: uses, Reused: reused, Patched: afterPatch})
}

// Where a skill comes from as the lifecycle hooks tell it: installed for one the hub, the bundle or a plugin brought, agent_created for
// one the agent made, external for one of an external folder, local for another of the profile's own, unknown otherwise
func (s *Scope) origin(name string) (string, error) {
	bundled, err := s.Bundled(name)
	if err != nil {
		return "", err
	}
	if s.HubInstalled(name) || bundled {
		return "installed", nil
	}
	if strings.Contains(name, ":") {
		if _, found := s.Find(name); found {
			return "installed", nil
		}
	}
	usage, err := s.Usage()
	if err != nil {
		return "", err
	}
	record := usage[name]
	switch {
	case record != nil && record.CreatedBy == ByInstalled:
		return "installed", nil
	case record.Managed():
		return "agent_created", nil
	case s.external(name) != "":
		return "external", nil
	case s.local(name) != "" || record != nil:
		return "local", nil
	}
	return "unknown", nil
}

// Counts a use of a skill, keeping which patch generation was last reused
func (s *Scope) Used(ctx context.Context, name string) error {
	var uses int64
	afterPatch := false
	err := s.mutate(ctx, name, false, func(r *Record) {
		uses = r.UseCount
		r.LastReusedPatchGeneration = min(r.LastReusedPatchGeneration, r.PatchGeneration)
		if afterPatch = r.UseCount > 0 && r.PatchGeneration > r.LastReusedPatchGeneration; afterPatch {
			r.LastReusedPatchGeneration = r.PatchGeneration
		}
		r.UseCount++
		r.LastUsedAt = now()
	})
	if err == nil {
		s.lifecycle(ctx, name, "loaded", uses+1, uses > 0, afterPatch)
	}
	return err
}

// Counts a change of a skill and starts a new patch generation; the hooks hear of a patch as patched and of any other change as edited
func (s *Scope) patched(ctx context.Context, name, action string) error {
	err := s.mutate(ctx, name, false, func(r *Record) {
		r.PatchCount++
		r.LastPatchedAt = now()
		r.PatchGeneration++
	})
	if err == nil {
		event := "edited"
		if action == "patch" {
			event = "patched"
		}
		s.lifecycle(ctx, name, event, nil, nil, nil)
	}
	return err
}

// Starts a new record for a skill just made, by the agent or by /learn
func (s *Scope) Created(ctx context.Context, name string, agent bool) error {
	err := s.mutate(ctx, name, false, func(r *Record) {
		*r = *fresh()
		r.CreatedBy = ByLearn
		if agent {
			r.CreatedBy = ByAgent
		}
	})
	if err == nil {
		s.lifecycle(ctx, name, "created", nil, nil, nil)
	}
	return err
}

// Records a skill the hub installed as active
func (s *Scope) Installed(ctx context.Context, name string) error {
	err := s.mutate(ctx, name, false, func(r *Record) {
		r.CreatedBy, r.State, r.ArchivedAt = ByInstalled, StateActive, ""
	})
	if err == nil {
		s.lifecycle(ctx, name, "installed", nil, nil, nil)
	}
	return err
}

// Starts the inactivity clock of a skill the curator may manage the first time it is seen
func (s *Scope) Seen(ctx context.Context, name string) error {
	if ok, err := s.Eligible(name, ""); err != nil || !ok {
		return err
	}
	return s.update(ctx, func(usage map[string]*Record) (bool, error) {
		if usage[name] != nil {
			return false, nil
		}
		usage[name] = fresh()
		usage[name].FirstSeenAt = now()
		return true, nil
	})
}

// Starts a skill's inactivity clock again now, a stale one made active
func (s *Scope) Reanchor(ctx context.Context, name string) error {
	return s.mutate(ctx, name, false, func(r *Record) {
		r.CreatedAt, r.FirstSeenAt = now(), now()
		if r.State == StateStale {
			r.State = StateActive
		}
	})
}

// Hands a skill to the curator
func (s *Scope) Adopt(ctx context.Context, name string) error {
	return s.mutate(ctx, name, true, func(r *Record) { r.CreatedBy = ByAgent })
}

// Sets the lifecycle state of a skill the curator may manage
func (s *Scope) SetState(ctx context.Context, name, state string) error {
	if state != StateActive && state != StateStale && state != StateArchived {
		return fmt.Errorf("%q is not a skill state.", state)
	}
	previous, changed := "", false
	err := s.mutate(ctx, name, true, func(r *Record) {
		if r.State == state {
			return
		}
		previous, changed = r.State, true
		r.State = state
		switch state {
		case StateArchived:
			r.ArchivedAt = now()
		case StateActive:
			r.ArchivedAt = ""
		}
	})
	// The hooks hear of a skill archived, gone stale, or brought back from the archive
	action := map[string]string{StateArchived: "archived", StateStale: "stale"}[state]
	if state == StateActive && previous == StateArchived {
		action = "restored"
	}
	if err == nil && changed && action != "" {
		s.lifecycle(ctx, name, action, nil, nil, nil)
	}
	return err
}

// Pins or unpins a skill the curator may manage
func (s *Scope) SetPinned(ctx context.Context, name string, pinned bool) error {
	return s.mutate(ctx, name, true, func(r *Record) { r.Pinned = pinned })
}

// Locks a skill's content from the agent, or lifts the lock
func (s *Scope) SetLocked(ctx context.Context, name string, locked bool) error {
	return s.mutate(ctx, name, false, func(r *Record) { r.Locked = locked })
}

// Whether the user locked the skill's content; an error when the records can't be read, so a write guard fails closed
func (s *Scope) IsLocked(name string) (bool, error) {
	usage, err := s.Usage()
	if err != nil {
		return false, err
	}
	return usage[name] != nil && usage[name].Locked, nil
}

// Skills whose content the user locked, sorted
func (s *Scope) LockedNames() ([]string, error) {
	usage, err := s.Usage()
	if err != nil {
		return nil, err
	}
	var out []string
	for name, r := range usage {
		if r.Locked {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Drops a skill's record
func (s *Scope) drop(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	return s.update(ctx, func(usage map[string]*Record) (bool, error) {
		_, ok := usage[name]
		delete(usage, name)
		return ok, nil
	})
}

// Lines of a small file of names, without blanks or comments
func lines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(strings.TrimPrefix(string(data), "\uFEFF"), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, nil
}

func (s *Scope) suppressedFile() string { return filepath.Join(s.Dir(), ".curator_suppressed") }

// Built in skills the curator archived, which syncing leaves archived
func (s *Scope) Suppressed() (map[string]bool, error) {
	names, err := lines(s.suppressedFile())
	out := map[string]bool{}
	for _, name := range names {
		out[name] = true
	}
	return out, err
}

// Adds a name to the curator's suppressed list or takes it off
func (s *Scope) suppress(name string, on bool) error {
	names, err := s.Suppressed()
	if err != nil || name == "" || names[name] == on {
		return err
	}
	names[name] = on
	list := sorted(names)
	content := strings.Join(list, "\n")
	if len(list) > 0 {
		content += "\n"
	}
	return fsx.Replace(s.suppressedFile(), []byte(content), 0o644)
}

// Names of the built in skills: those the seeded sources put in the profile, and those the curator archived
func (s *Scope) bundledNames() (map[string]bool, error) {
	out, err := s.Suppressed()
	if err != nil {
		return nil, err
	}
	manifest, err := s.manifest()
	if err != nil {
		return nil, err
	}
	for name := range manifest {
		out[name] = true
	}
	return out, nil
}

// Whether the skill is built in
func (s *Scope) Bundled(name string) (bool, error) {
	names, err := s.bundledNames()
	return names[name], err
}

// Folder of the skill of the name in the profile's own folder, never in an extra one
func (s *Scope) local(name string) string {
	if !fsx.Exists(s.Dir()) {
		return ""
	}
	for _, skillMD := range skillFiles(s.Dir()) {
		if !s.ExternalPath(skillMD) && readName(skillMD, filepath.Base(filepath.Dir(skillMD))) == name {
			return filepath.Dir(skillMD)
		}
	}
	return ""
}

// Folder of the skill of the name in an extra folder
func (s *Scope) external(name string) string {
	dirs := s.Dirs()
	for _, base := range dirs[min(1, len(dirs)):] {
		for _, skillMD := range skillFiles(base) {
			if readName(skillMD, filepath.Base(filepath.Dir(skillMD))) == name {
				return filepath.Dir(skillMD)
			}
		}
	}
	return ""
}

// Whether the curator may manage the skill: the agent's own yes, a built in one only with curator.prune_builtins, hub and extra
// folder ones never
func (s *Scope) Eligible(name, path string) (bool, error) {
	if path != "" && s.ExternalPath(path) || s.HubInstalled(name) {
		return false, nil
	}
	bundled, err := s.Bundled(name)
	if err != nil {
		return false, err
	}
	if bundled {
		return s.Config.GetBool(PruneBuiltins), nil
	}
	if dir := s.local(name); dir != "" {
		return !s.ExternalPath(dir), nil
	}
	return s.external(name) == "", nil
}

// Setting of the curator that lets it archive built in skills; the curator's own section registers it
const PruneBuiltins = "curator.prune_builtins"

// Where a skill comes from: hub, bundled, external or agent
func (s *Scope) Provenance(name string) (string, error) {
	if s.HubInstalled(name) {
		return "hub", nil
	}
	bundled, err := s.Bundled(name)
	if err != nil {
		return "", err
	}
	switch {
	case bundled:
		return "bundled", nil
	case s.local(name) == "" && s.external(name) != "":
		return "external", nil
	}
	return "agent", nil
}

// Moves a skill the curator may manage into the archive, under its name or with the time when that is taken
func (s *Scope) Archive(ctx context.Context, name string) (string, error) {
	dir := s.local(name)
	if dir == "" && s.external(name) != "" {
		return "", fmt.Errorf("skill '%s' lives in skills.external_dirs; external skills are read-only to the curator", name)
	}
	eligible, err := s.Eligible(name, dir)
	if err != nil {
		return "", err
	}
	if !eligible {
		if s.HubInstalled(name) {
			return "", fmt.Errorf("skill '%s' is hub-installed; never archive", name)
		}
		return "", fmt.Errorf("skill '%s' is a bundled built-in; enable curator.prune_builtins to allow pruning it", name)
	}
	if dir == "" {
		return "", fmt.Errorf("skill '%s' not found", name)
	}
	dest := filepath.Join(s.ArchiveDir(), name)
	if err := os.MkdirAll(s.ArchiveDir(), 0o755); err != nil {
		return "", fmt.Errorf("failed to create archive dir: %s", pathError(err))
	}
	if fsx.Exists(dest) {
		dest = filepath.Join(s.ArchiveDir(), filepath.Base(dir)+"-"+time.Now().UTC().Format("20060102150405"))
	}
	return dest, s.relocate(ctx, dir, dest, name, "archive")
}

// Moves an archived skill back into the profile's folder, never over a hub or built in skill of the name
func (s *Scope) Restore(ctx context.Context, name string) (string, error) {
	if s.HubInstalled(name) {
		return "", fmt.Errorf("skill '%s' is now hub-installed; restore would shadow the upstream version", name)
	}
	bundled, err := s.Bundled(name)
	if err != nil {
		return "", err
	}
	if bundled && !s.Config.GetBool(PruneBuiltins) {
		return "", fmt.Errorf("skill '%s' is now bundled; restore would shadow the upstream version", name)
	}
	if !fsx.Exists(s.ArchiveDir()) {
		return "", errors.New("no archive directory")
	}
	var dirs []string
	filepath.WalkDir(s.ArchiveDir(), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != s.ArchiveDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	var found []string
	for _, dir := range dirs {
		if filepath.Base(dir) == name {
			found = append(found, dir)
		}
	}
	if len(found) == 0 {
		for _, dir := range dirs {
			if stamp, ok := strings.CutPrefix(filepath.Base(dir), name+"-"); ok && len(stamp) == 14 && strings.Trim(stamp, "0123456789") == "" {
				found = append(found, dir)
			}
		}
		slices.Sort(found)
		slices.Reverse(found)
	}
	if len(found) == 0 {
		for _, dir := range dirs {
			if skillMD := filepath.Join(dir, "SKILL.md"); fsx.IsFile(skillMD) && readName(skillMD, filepath.Base(dir)) == name {
				found = append(found, dir)
			}
		}
	}
	if len(found) == 0 {
		return "", fmt.Errorf("skill '%s' not found in archive", name)
	}
	dest := filepath.Join(s.Dir(), name)
	if fsx.Exists(dest) {
		return "", errors.New("destination already exists: " + dest)
	}
	return dest, s.relocate(ctx, found[0], dest, name, "restore")
}

// Moves a skill folder for an archive or a restore, logged in the change log, with the suppressed list and its state following
func (s *Scope) relocate(ctx context.Context, src, dest, name, action string) error {
	archiving := action == "archive"
	before := s.capture(ctx, src, archiving, name)
	if err := move(src, dest); err != nil {
		return fmt.Errorf("failed to %s: %s", action, pathError(err))
	}
	bundled := false
	if archiving {
		stamp := time.Now()
		os.Chtimes(dest, stamp, stamp)
		var err error
		if bundled, err = s.Bundled(name); err != nil {
			return err
		}
	}
	if !archiving || bundled {
		if err := s.suppress(name, archiving); err != nil {
			return err
		}
	}
	state := StateActive
	if archiving {
		state = StateArchived
	}
	if err := s.SetState(ctx, name, state); err != nil {
		return err
	}
	s.log(ctx, action, name, change{before: before, captured: true, after: dest})
	return nil
}

// Renames a folder, copying it across file systems
func move(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	if err := copyTree(os.DirFS(src), ".", dest, false, false); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// Names of the archived skills
func (s *Scope) Archived() ([]string, error) {
	entries, err := os.ReadDir(s.ArchiveDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if fsx.IsDir(filepath.Join(s.ArchiveDir(), entry.Name())) {
			out = append(out, entry.Name())
		}
	}
	return out, nil
}

// Folders of the skills the user locked, by name, resolved: the file and terminal tools refuse changes under them with LockRefusal
func (s *Scope) LockedDirs() (map[string]string, error) {
	names, err := s.LockedNames()
	if err != nil || len(names) == 0 {
		return nil, err
	}
	out := map[string]string{}
	for _, base := range s.Dirs() {
		for _, skillMD := range skillFiles(base) {
			name := filepath.Base(filepath.Dir(skillMD))
			if _, seen := out[name]; !seen && slices.Contains(names, name) {
				out[name] = resolve(filepath.Dir(skillMD))
			}
		}
	}
	return out, nil
}
