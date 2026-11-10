package skills

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	StateActive   = "active"
	StateStale    = "stale"
	StateArchived = "archived"
)

var validStates = map[string]bool{StateActive: true, StateStale: true, StateArchived: true}

// Load bearing built-in names the curator never archives or consolidates
var ProtectedBuiltinSkills = map[string]bool{}

// Reports whether a skill name is a protected built-in
func IsProtectedBuiltin(name string) bool { return ProtectedBuiltinSkills[name] }

func (s *Scope) usageFile() string  { return filepath.Join(s.SkillsDir(), ".usage.json") }
func (s *Scope) archiveDir() string { return filepath.Join(s.SkillsDir(), ".archive") }

// Lifecycle observers and plugin hooks the top level wiring installs from the lifecycle port
type LifecycleDispatch struct {
	HasHook    func(ctx context.Context, hookName string) bool
	InvokeHook func(ctx context.Context, hookName string, kwargs map[string]any) []any
}

var (
	lifecycleMu       sync.RWMutex
	lifecycleDispatch *LifecycleDispatch
)

// Installs the lifecycle dispatch skill telemetry emits through
func RegisterLifecycleDispatch(d LifecycleDispatch) {
	lifecycleMu.Lock()
	lifecycleDispatch = &d
	lifecycleMu.Unlock()
}

type heldSkillLocksKey struct{}

// Exclusive cross process lock on a path, reentrant for callers holding the returned context
func SkillFileLock(ctx context.Context, lockPath string) (context.Context, func(), error) {
	key := filepath.Clean(lockPath)
	held, _ := ctx.Value(heldSkillLocksKey{}).(map[string]bool)
	if held[key] {
		return ctx, func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(key), 0o755); err != nil {
		return ctx, nil, err
	}
	f, err := openSkillLockFile(key)
	if err != nil {
		return ctx, nil, err
	}
	unlock, err := lockSkillFile(f)
	if err != nil {
		f.Close()
		return ctx, nil, err
	}
	next := make(map[string]bool, len(held)+1)
	for k := range held {
		next[k] = true
	}
	next[key] = true
	return context.WithValue(ctx, heldSkillLocksKey{}, next), func() { unlock(); f.Close() }, nil
}

// Stripped non-empty lines of a small metadata file, an error only for undecodable bytes
func (s *Scope) readMetaLines(path, failLog string) ([]string, error) {
	if !lexistsFollow(path) {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		s.logger().Debug(fmt.Sprintf(failLog, pyOSErrorText(err)))
		return nil, nil
	}
	text, err := pyjson.DecodeBytes(data)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range pystr.Splitlines(strings.TrimPrefix(text, "\xEF\xBB\xBF"), false) {
		if line = pystr.Strip(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func nowISO() string { return pydatetime.ISOFormatUTC(time.Now()) }

// Parsed timestamp of a record value, naive values read as UTC, false when absent or invalid
func parseISOTimestamp(v any) (time.Time, bool) {
	if !pyrepr.Truthy(v) {
		return time.Time{}, false
	}
	t, err := pydatetime.FromISOFormat(pyrepr.Str(v))
	return t, err == nil
}

// Newest use, view or patch timestamp text of a record, nil when none parses
func LatestActivityAt(rec *pyjson.Object) any {
	var best time.Time
	var raw any
	for _, k := range []string{"last_used_at", "last_viewed_at", "last_patched_at"} {
		v, _ := rec.Get(k)
		if t, ok := parseISOTimestamp(v); ok && (raw == nil || t.After(best)) {
			best, raw = t, pyrepr.Str(v)
		}
	}
	return raw
}

// Python int of a counter value with falsy and unconvertible values as zero, overflow as an error
func intOrZero(v any) (*big.Int, error) {
	if !pyrepr.Truthy(v) {
		return big.NewInt(0), nil
	}
	n, err := pyint.Of(v)
	if err != nil {
		if e, ok := err.(*pyint.Error); ok && e.Kind == "OverflowError" {
			return nil, err
		}
		return big.NewInt(0), nil
	}
	return n, nil
}

// Counter value clamped at zero, booleans reading as zero
func nonNegativeInt(v any) (*big.Int, error) {
	if _, ok := v.(bool); ok {
		return big.NewInt(0), nil
	}
	n, err := intOrZero(v)
	if err != nil {
		return nil, err
	}
	if n.Sign() < 0 {
		return big.NewInt(0), nil
	}
	return n, nil
}

// Total use, view and patch events of a record
func ActivityCount(rec *pyjson.Object) (*big.Int, error) {
	total := big.NewInt(0)
	for _, k := range []string{"use_count", "view_count", "patch_count"} {
		v, _ := rec.Get(k)
		n, err := intOrZero(v)
		if err != nil {
			return nil, err
		}
		total.Add(total, n)
	}
	return total, nil
}

// Bundled manifest names plus the curator suppression list
func (s *Scope) readBundledNames() (map[string]bool, error) {
	lines, err := s.readMetaLines(filepath.Join(s.SkillsDir(), ".bundled_manifest"), "Failed to read bundled manifest: %s")
	if err != nil {
		return nil, err
	}
	names, err := s.ReadSuppressedNames()
	if err != nil {
		return nil, err
	}
	for _, line := range lines {
		if n := pystr.Strip(strings.SplitN(line, ":", 2)[0]); n != "" {
			names[n] = true
		}
	}
	return names, nil
}

// Hub lock names plus the frontmatter name of each in tree install path
func (s *Scope) readHubInstalledNames() map[string]bool {
	skillsDir := s.SkillsDir()
	lockPath := filepath.Join(skillsDir, ".hub", "lock.json")
	names := map[string]bool{}
	if !lexistsFollow(lockPath) {
		return names
	}
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		s.logger().Debug("Failed to read hub lock file: " + pyOSErrorText(err))
		return names
	}
	data, err := pyjson.Loads(decodeText(raw))
	if err != nil {
		s.logger().Debug("Failed to read hub lock file: " + err.Error())
		return names
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return names
	}
	instRaw, _ := obj.Get("installed")
	if !pyrepr.Truthy(instRaw) {
		return names
	}
	installed, ok := instRaw.(*pyjson.Object)
	if !ok {
		return map[string]bool{}
	}
	for _, k := range installed.Keys {
		names[k] = true
	}
	root := resolvePath(skillsDir)
	for _, k := range installed.Keys {
		entry, ok := installed.Vals[k].(*pyjson.Object)
		if !ok {
			continue
		}
		ip, _ := entry.Get("install_path")
		p, ok := ip.(string)
		if !ok || pystr.Strip(p) == "" {
			continue
		}
		joined := p
		if !filepath.IsAbs(p) {
			joined = filepath.Join(skillsDir, p)
		}
		resolved := resolvePath(joined)
		if !isRelativeTo(resolved, root) {
			continue
		}
		if md := filepath.Join(resolved, "SKILL.md"); lexistsFollow(md) {
			names[readSkillName(md, filepath.Base(resolved))] = true
		}
	}
	return names
}

// Configured curator.prune_builtins, false by default
func (s *Scope) pruneBuiltinsEnabled() bool {
	cur := config.AsMap(s.tree()["curator"])
	if cur == nil {
		return false
	}
	return pyrepr.Truthy(cur["prune_builtins"])
}

// Built-ins the curator pruned, which the re-seeder leaves archived
func (s *Scope) ReadSuppressedNames() (map[string]bool, error) {
	lines, err := s.readMetaLines(filepath.Join(s.SkillsDir(), ".curator_suppressed"), "Failed to read curator suppression list: %s")
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, l := range lines {
		if !strings.HasPrefix(l, "#") {
			names[l] = true
		}
	}
	return names, nil
}

// Adds or drops a name in the suppression list, unchanged lists untouched
func (s *Scope) toggleSuppressedName(ctx context.Context, name string, add bool) error {
	if name == "" {
		return nil
	}
	names, err := s.ReadSuppressedNames()
	if err != nil {
		return err
	}
	if names[name] == add {
		return nil
	}
	if add {
		names[name] = true
	} else {
		delete(names, name)
	}
	sorted := sortedKeys(names)
	content := strings.Join(sorted, "\n")
	if len(sorted) > 0 {
		content += "\n"
	}
	if err := pyfs.AtomicWriteText(home.WithOverride(ctx, s.Home), filepath.Join(s.SkillsDir(), ".curator_suppressed"), content, ".curator_suppressed_"); err != nil {
		s.logger().Debug("Failed to write curator suppression list: " + err.Error())
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type namedSkillMD struct{ name, path string }

// Frontmatter names and SKILL.md paths under base minus excluded dirs, external mounts too when local only
func (s *Scope) iterSkillMDs(base string, localOnly bool) []namedSkillMD {
	var out []namedSkillMD
	for _, md := range rglob(base, "SKILL.md") {
		if isExcludedSkillPath(md, "") || (localOnly && s.isExternalSkillPath(md)) {
			continue
		}
		out = append(out, namedSkillMD{readSkillName(md, filepath.Base(filepath.Dir(md))), md})
	}
	return out
}

type skillKeepFunc func(name, skillMD string, bundled map[string]bool, usage *pyjson.Object) (bool, error)

// Sorted local skill names passing keep, hub and protected names excluded
func (s *Scope) scanLocalSkills(keep skillKeepFunc) ([]string, error) {
	base := s.SkillsDir()
	if !lexistsFollow(base) {
		return []string{}, nil
	}
	hub := s.readHubInstalledNames()
	bundled, err := s.readBundledNames()
	if err != nil {
		return nil, err
	}
	usage, err := s.LoadUsage()
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for _, e := range s.iterSkillMDs(base, true) {
		if hub[e.name] || IsProtectedBuiltin(e.name) {
			continue
		}
		ok, err := keep(e.name, e.path, bundled, usage)
		if err != nil {
			return nil, err
		}
		if ok {
			found[e.name] = true
		}
	}
	return sortedKeys(found), nil
}

func usageRecord(usage *pyjson.Object, name string) any {
	v, _ := usage.Get(name)
	return v
}

// Curator manageable skills, agent records plus bundled built-ins when pruning is enabled, never hub
func (s *Scope) ListAgentCreatedSkillNames() ([]string, error) {
	prune := s.pruneBuiltinsEnabled()
	return s.scanLocalSkills(func(name, _ string, bundled map[string]bool, usage *pyjson.Object) (bool, error) {
		if bundled[name] {
			return prune, nil
		}
		return isCuratorManagedRecord(usageRecord(usage, name)), nil
	})
}

// Directory names under the archive
func (s *Scope) ListArchivedSkillNames() ([]string, error) {
	root := s.archiveDir()
	if !lexistsFollow(root) {
		return []string{}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, e := range entries {
		if isDir(filepath.Join(root, e.Name())) {
			names[e.Name()] = true
		}
	}
	return sortedKeys(names), nil
}

// Neither bundled nor hub installed and not only present in an external dir
func (s *Scope) IsAgentCreated(name string) (bool, error) {
	bundled, err := s.IsBundled(name)
	if err != nil || bundled || s.IsHubInstalled(name) {
		return false, err
	}
	return s.findSkillDir(name) != "" || s.findExternalSkillDir(name) == "", nil
}

func (s *Scope) IsHubInstalled(name string) bool { return s.readHubInstalledNames()[name] }

// Reports whether a name is a bundled built-in or a pruned one
func (s *Scope) IsBundled(name string) (bool, error) {
	names, err := s.readBundledNames()
	return names[name], err
}

func externalReadOnlyMessage(name string) string {
	return fmt.Sprintf("skill '%s' lives in skills.external_dirs; external skills are read-only to the curator", name)
}

// Agent created yes, bundled only with prune_builtins, hub external and protected never
func (s *Scope) IsCurationEligible(name, skillPath string) (bool, error) {
	if (skillPath != "" && s.isExternalSkillPath(skillPath)) || IsProtectedBuiltin(name) || s.IsHubInstalled(name) {
		return false, nil
	}
	bundled, err := s.IsBundled(name)
	if err != nil {
		return false, err
	}
	if bundled {
		return s.pruneBuiltinsEnabled(), nil
	}
	if local := s.findSkillDir(name); local != "" {
		return !s.isExternalSkillPath(local), nil
	}
	return s.findExternalSkillDir(name) == "", nil
}

// Reports whether a usage record opted into curator management
func isCuratorManagedRecord(rec any) bool {
	obj, ok := rec.(*pyjson.Object)
	if !ok {
		return false
	}
	createdBy, _ := obj.Get("created_by")
	agentCreated, _ := obj.Get("agent_created")
	b, isBool := agentCreated.(bool)
	return createdBy == "agent" || (isBool && b)
}

func (s *Scope) IsCuratorManaged(name string) (bool, error) {
	usage, err := s.LoadUsage()
	if err != nil {
		return false, err
	}
	return isCuratorManagedRecord(usageRecord(usage, name)), nil
}

// Curation eligible skills without the curator management marker
func (s *Scope) ListUnmanagedSkillNames() ([]string, error) {
	return s.scanLocalSkills(func(name, md string, bundled map[string]bool, usage *pyjson.Object) (bool, error) {
		if bundled[name] || isCuratorManagedRecord(usageRecord(usage, name)) {
			return false, nil
		}
		return s.IsCurationEligible(name, md)
	})
}

// Report rows for unmanaged skills with has_provenance_key and has_record
func (s *Scope) UnmanagedReport() ([]*pyjson.Object, error) {
	usage, err := s.LoadUsage()
	if err != nil {
		return nil, err
	}
	names, err := s.ListUnmanagedSkillNames()
	if err != nil {
		return nil, err
	}
	rows := []*pyjson.Object{}
	for _, n := range names {
		raw, has := usage.Get(n)
		hasKey := false
		if obj, ok := raw.(*pyjson.Object); ok {
			_, hasKey = obj.Get("created_by")
		}
		row, err := reportRow(n, raw, "has_provenance_key", hasKey, "has_record", has)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Hands a local skill to curator management, refusing hub, external, bundled and protected skills
func (s *Scope) AdoptSkill(ctx context.Context, name string) (bool, string, error) {
	if name == "" {
		return false, "no skill name given", nil
	}
	if IsProtectedBuiltin(name) {
		return false, fmt.Sprintf("'%s' is a protected built-in; the curator never manages it", name), nil
	}
	if s.IsHubInstalled(name) {
		return false, fmt.Sprintf("'%s' is hub-installed; its upstream owns it", name), nil
	}
	if bundled, err := s.IsBundled(name); err != nil || bundled {
		return false, fmt.Sprintf("'%s' is a bundled built-in — it is governed by curator.prune_builtins, not by adoption", name), err
	}
	dir := s.findSkillDir(name)
	if dir == "" {
		if s.findExternalSkillDir(name) != "" {
			return false, fmt.Sprintf("'%s' lives in skills.external_dirs and is read-only to the curator", name), nil
		}
		return false, fmt.Sprintf("skill '%s' not found", name), nil
	}
	if s.isExternalSkillPath(dir) {
		return false, externalReadOnlyMessage(name), nil
	}
	if managed, err := s.IsCuratorManaged(name); err != nil || managed {
		return managed, fmt.Sprintf("'%s' is already curator-managed", name), err
	}
	s.MarkAgentCreated(ctx, name)
	if managed, err := s.IsCuratorManaged(name); err != nil || managed {
		return managed, fmt.Sprintf("adopted '%s' into curator management", name), err
	}
	return false, fmt.Sprintf("could not mark '%s' as curator-managed", name), nil
}

// Fresh usage record with every default key in upstream order
func emptyRecord() *pyjson.Object {
	o := pyjson.NewObject()
	for _, kv := range []struct {
		k string
		v any
	}{{"created_by", nil}, {"use_count", int64(0)}, {"view_count", int64(0)}, {"last_used_at", nil}, {"last_viewed_at", nil},
		{"patch_count", int64(0)}, {"patch_generation", int64(0)}, {"last_reused_patch_generation", int64(0)}, {"last_patched_at", nil},
		{"created_at", nowISO()}, {"state", StateActive}, {"pinned", false}, {"archived_at", nil}, {"first_seen_at", nil}} {
		o.Set(kv.k, kv.v)
	}
	return o
}

// Record copy with every missing default key appended, a fresh record when not an object
func backfilled(rec any) *pyjson.Object {
	obj, ok := rec.(*pyjson.Object)
	if !ok {
		return emptyRecord()
	}
	out := pyjson.NewObject()
	for _, k := range obj.Keys {
		out.Set(k, obj.Vals[k])
	}
	def := emptyRecord()
	for _, k := range def.Keys {
		if _, has := obj.Get(k); !has {
			out.Set(k, def.Vals[k])
		}
	}
	return out
}

// Report row of name, backfilled record, extra pairs, then last_activity_at and activity_count
func reportRow(name string, raw any, extra ...any) (*pyjson.Object, error) {
	row := pyjson.NewObject()
	row.Set("name", name)
	rec := backfilled(raw)
	for _, k := range rec.Keys {
		row.Set(k, rec.Vals[k])
	}
	for i := 0; i+1 < len(extra); i += 2 {
		row.Set(extra[i].(string), extra[i+1])
	}
	count, err := ActivityCount(row)
	if err != nil {
		return nil, err
	}
	row.Set("last_activity_at", LatestActivityAt(row))
	row.Set("activity_count", pyint.Value(count))
	return row, nil
}

// Whole usage map with non object values dropped, empty on a missing or corrupt file
func (s *Scope) LoadUsage() (*pyjson.Object, error) {
	path := s.usageFile()
	out := pyjson.NewObject()
	if !lexistsFollow(path) {
		return out, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		s.logger().Debug(fmt.Sprintf("Failed to read %s: %s", path, pyOSErrorText(err)))
		return out, nil
	}
	text, err := pyjson.DecodeBytes(raw)
	if err != nil {
		return nil, err
	}
	data, err := pyjson.Loads(strings.TrimPrefix(text, "\xEF\xBB\xBF"))
	if err != nil {
		s.logger().Debug(fmt.Sprintf("Failed to read %s: %s", path, err.Error()))
		return out, nil
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return out, nil
	}
	for _, k := range obj.Keys {
		if _, isObj := obj.Vals[k].(*pyjson.Object); isObj {
			out.Set(k, obj.Vals[k])
		}
	}
	return out, nil
}

// Writes the usage map atomically with sorted keys, true when it committed
func (s *Scope) SaveUsage(ctx context.Context, data *pyjson.Object) bool {
	path := s.usageFile()
	text, err := pyjson.Dumps(data, pyjson.Indent(2), pyjson.SortKeys(true), pyjson.EnsureASCII(false))
	if err == nil {
		err = pyfs.AtomicWriteText(home.WithOverride(ctx, s.Home), path, text, ".usage_")
	}
	if err != nil {
		s.logger().Debug(fmt.Sprintf("Failed to write %s: %s", path, err.Error()))
		return false
	}
	return true
}

// Backfilled record for a skill, fresh defaults when missing
func (s *Scope) GetRecord(name string) (*pyjson.Object, error) {
	usage, err := s.LoadUsage()
	if err != nil {
		return nil, err
	}
	return backfilled(usageRecord(usage, name)), nil
}

type usageOp func(data *pyjson.Object) (any, bool, error)

// Runs op under the usage lock saving when dirty, false when the guard failed, the save missed or anything erred
func (s *Scope) lockedUpdate(ctx context.Context, name string, op usageOp, failLog string, guard func() (bool, error)) (any, bool) {
	fail := func(err error) (any, bool) {
		s.logger().Debug(fmt.Sprintf(failLog, name, err.Error()))
		return nil, false
	}
	if guard != nil {
		ok, err := guard()
		if err != nil {
			return fail(err)
		}
		if !ok {
			return nil, false
		}
	}
	_, release, err := SkillFileLock(ctx, s.usageFile()+".lock")
	if err != nil {
		return fail(err)
	}
	defer release()
	data, err := s.LoadUsage()
	if err != nil {
		return fail(err)
	}
	result, dirty, err := op(data)
	if err != nil {
		return fail(err)
	}
	if dirty && !s.SaveUsage(home.WithOverride(ctx, s.Home), data) {
		return nil, false
	}
	return result, true
}

// Seeds a baseline record for a curation eligible skill so its inactivity clock starts at first sight
func (s *Scope) SeedRecordIfMissing(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	eligible, err := s.IsCurationEligible(name, "")
	if err != nil || !eligible {
		return err
	}
	s.lockedUpdate(ctx, name, func(data *pyjson.Object) (any, bool, error) {
		if _, has := data.Get(name); has {
			return nil, false, nil
		}
		rec := emptyRecord()
		rec.Set("first_seen_at", nowISO())
		data.Set(name, rec)
		return nil, true, nil
	}, "skill_usage.seed_record_if_missing(%s) failed: %s", nil)
	return nil
}

// Starts the inactivity clock now once and reactivates a record wrongly marked stale
func (s *Scope) ReanchorClock(ctx context.Context, name string) {
	s.mutate(ctx, name, func(rec *pyjson.Object) (any, error) {
		now := nowISO()
		rec.Set("created_at", now)
		rec.Set("first_seen_at", now)
		if st, _ := rec.Get("state"); st == StateStale {
			rec.Set("state", StateActive)
		}
		return nil, nil
	}, false)
}

// Applies a mutator to the record under the lock and saves, false when nothing landed
func (s *Scope) mutate(ctx context.Context, name string, mutator func(rec *pyjson.Object) (any, error), requireEligible bool) (any, bool) {
	if name == "" {
		return nil, false
	}
	var guard func() (bool, error)
	if requireEligible {
		guard = func() (bool, error) { return s.IsCurationEligible(name, "") }
	}
	return s.lockedUpdate(ctx, name, func(data *pyjson.Object) (any, bool, error) {
		raw, has := data.Get(name)
		rec, ok := raw.(*pyjson.Object)
		if !has || !ok {
			rec = emptyRecord()
			data.Set(name, rec)
		}
		result, err := mutator(rec)
		return result, true, err
	}, "skill_usage._mutate(%s) failed: %s", guard)
}

// Curation gated single field write, true only when it landed
func (s *Scope) setField(ctx context.Context, name, key string, value any) bool {
	_, ok := s.mutate(ctx, name, func(rec *pyjson.Object) (any, error) {
		rec.Set(key, value)
		return true, nil
	}, true)
	return ok
}

// Increments a counter and stamps its timestamp
func bumpCounter(rec *pyjson.Object, countKey, tsKey string) error {
	v, _ := rec.Get(countKey)
	n, err := nonNegativeInt(v)
	if err != nil {
		return err
	}
	rec.Set(countKey, pyint.Value(n.Add(n, big.NewInt(1))))
	rec.Set(tsKey, nowISO())
	return nil
}

// Bounded provenance label for shared skill metrics
func (s *Scope) TelemetryProvenance(ctx context.Context, name string, record map[string]any) (string, error) {
	bundled, err := s.IsBundled(name)
	if err != nil {
		return "", err
	}
	if s.IsHubInstalled(name) || bundled {
		return "installed", nil
	}
	if strings.Contains(name, ":") {
		if host, err := s.pluginHost(ctx); err == nil {
			if _, found := host.FindPluginSkill(name); found {
				return "installed", nil
			}
		}
	}
	if record != nil {
		switch record["created_by"] {
		case "installed":
			return "installed", nil
		case "agent":
			return "agent_created", nil
		}
	}
	if s.findExternalSkillDir(name) != "" {
		return "external", nil
	}
	if s.findSkillDir(name) != "" || record != nil {
		return "local", nil
	}
	return "unknown", nil
}

// Emits an on_skill_lifecycle hook after an authoritative state change
func (s *Scope) emitSkillLifecycle(ctx context.Context, name, action string, record map[string]any, taskID, sessionID string) {
	err := func() error {
		lifecycleMu.RLock()
		d := lifecycleDispatch
		lifecycleMu.RUnlock()
		if d == nil || d.HasHook == nil || d.InvokeHook == nil {
			return fmt.Errorf("lifecycle dispatch is not registered")
		}
		if !d.HasHook(ctx, "on_skill_lifecycle") {
			return nil
		}
		prov, err := s.TelemetryProvenance(ctx, name, record)
		if err != nil {
			return err
		}
		d.InvokeHook(ctx, "on_skill_lifecycle", map[string]any{"action": action, "skill_name": name, "provenance": prov,
			"task_id": taskID, "session_id": sessionID, "use_count": record["use_count"], "reused": record["reused"],
			"reuse_after_patch": record["reuse_after_patch"]})
		return nil
	}()
	if err != nil {
		s.logger().Debug(fmt.Sprintf("skill_usage lifecycle hook failed for %s/%s", name, action), "error", err)
	}
}

// Mutates then emits the action with the mutator facts, only when the write landed
func (s *Scope) mutateAndEmit(ctx context.Context, name, action string, mutator func(rec *pyjson.Object) (any, error), taskID, sessionID string) {
	if facts, ok := s.mutate(ctx, name, mutator, false); ok {
		if m, isMap := facts.(map[string]any); isMap {
			s.emitSkillLifecycle(ctx, name, action, m, taskID, sessionID)
		}
	}
}

// Counts a skill view
func (s *Scope) BumpView(ctx context.Context, name string) {
	s.mutate(ctx, name, func(rec *pyjson.Object) (any, error) {
		return nil, bumpCounter(rec, "view_count", "last_viewed_at")
	}, false)
}

// Counts a skill use and derives reuse and post patch reuse facts atomically
func (s *Scope) BumpUse(ctx context.Context, name, taskID, sessionID string) {
	s.mutateAndEmit(ctx, name, "loaded", func(rec *pyjson.Object) (any, error) {
		get := func(k string) (*big.Int, error) {
			v, _ := rec.Get(k)
			return nonNegativeInt(v)
		}
		uses, err := get("use_count")
		if err != nil {
			return nil, err
		}
		gen, err := get("patch_generation")
		if err != nil {
			return nil, err
		}
		lastReused, err := get("last_reused_patch_generation")
		if err != nil {
			return nil, err
		}
		if lastReused.Cmp(gen) > 0 {
			lastReused = gen
		}
		reuseAfterPatch := uses.Sign() > 0 && gen.Cmp(lastReused) > 0
		newUses := new(big.Int).Add(uses, big.NewInt(1))
		rec.Set("use_count", pyint.Value(newUses))
		rec.Set("last_used_at", nowISO())
		rec.Set("patch_generation", pyint.Value(gen))
		if reuseAfterPatch {
			rec.Set("last_reused_patch_generation", pyint.Value(gen))
		} else {
			rec.Set("last_reused_patch_generation", pyint.Value(lastReused))
		}
		createdBy, _ := rec.Get("created_by")
		return map[string]any{"created_by": createdBy, "use_count": pyint.Value(newUses), "reused": uses.Sign() > 0,
			"reuse_after_patch": reuseAfterPatch}, nil
	}, taskID, sessionID)
}

// Counts a skill_manage patch or edit and advances the patch generation
func (s *Scope) BumpPatch(ctx context.Context, name, action, taskID, sessionID string) {
	event := "edited"
	if action == "patch" {
		event = "patched"
	}
	s.mutateAndEmit(ctx, name, event, func(rec *pyjson.Object) (any, error) {
		if err := bumpCounter(rec, "patch_count", "last_patched_at"); err != nil {
			return nil, err
		}
		v, _ := rec.Get("patch_generation")
		gen, err := nonNegativeInt(v)
		if err != nil {
			return nil, err
		}
		rec.Set("patch_generation", pyint.Value(gen.Add(gen, big.NewInt(1))))
		createdBy, _ := rec.Get("created_by")
		return map[string]any{"created_by": createdBy}, nil
	}, taskID, sessionID)
}

// Resets the record for a new logical skill stamped agent or learn and emits created
func (s *Scope) RecordCreated(ctx context.Context, name string, agentCreated bool, taskID, sessionID string) {
	s.mutateAndEmit(ctx, name, "created", func(rec *pyjson.Object) (any, error) {
		createdBy := "learn"
		if agentCreated {
			createdBy = "agent"
		}
		fresh := emptyRecord()
		rec.Keys, rec.Vals = fresh.Keys, fresh.Vals
		rec.Set("created_by", createdBy)
		return map[string]any{"created_by": createdBy}, nil
	}, taskID, sessionID)
}

// Records a successful hub install as an active installed skill
func (s *Scope) RecordInstalled(ctx context.Context, name string) {
	s.mutateAndEmit(ctx, name, "installed", func(rec *pyjson.Object) (any, error) {
		rec.Set("created_by", "installed")
		rec.Set("state", StateActive)
		rec.Set("archived_at", nil)
		return map[string]any{"created_by": "installed"}, nil
	}, "", "")
}

// Opts a skill into curator management
func (s *Scope) MarkAgentCreated(ctx context.Context, name string) {
	s.setField(ctx, name, "created_by", "agent")
}

// Sets a lifecycle state on a manageable skill and emits archived, stale or restored on a real change
func (s *Scope) SetState(ctx context.Context, name, state string) {
	if !validStates[state] {
		s.logger().Debug(fmt.Sprintf("set_state: invalid state %s for %s", pyrepr.Repr(state), name))
		return
	}
	facts, ok := s.mutate(ctx, name, func(rec *pyjson.Object) (any, error) {
		previous, _ := rec.Get("state")
		if previous != state {
			rec.Set("state", state)
			if state != StateStale {
				if state == StateArchived {
					rec.Set("archived_at", nowISO())
				} else {
					rec.Set("archived_at", nil)
				}
			}
		}
		createdBy, _ := rec.Get("created_by")
		return map[string]any{"changed": previous != state, "created_by": createdBy, "previous_state": previous}, nil
	}, true)
	m, isMap := facts.(map[string]any)
	if !ok || !isMap || m["changed"] != true {
		return
	}
	action := map[string]string{StateArchived: "archived", StateStale: "stale"}[state]
	if state == StateActive && m["previous_state"] == StateArchived {
		action = "restored"
	}
	if action != "" {
		s.emitSkillLifecycle(ctx, name, action, m, "", "")
	}
}

// Pins or unpins a manageable skill, false when the write did not land
func (s *Scope) SetPinned(ctx context.Context, name string, pinned bool) bool {
	return s.setField(ctx, name, "pinned", pinned)
}

// Records the user content lock on any skill, true when the write landed
func (s *Scope) SetLocked(ctx context.Context, name string, locked bool) bool {
	_, ok := s.mutate(ctx, name, func(rec *pyjson.Object) (any, error) {
		rec.Set("locked", locked)
		return true, nil
	}, false)
	return ok
}

// Locked flag of a skill, an error on a corrupt sidecar so the write guard fails closed
func (s *Scope) IsLocked(name string) (bool, error) {
	path := s.usageFile()
	if !lexistsFollow(path) {
		return false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	text, err := pyjson.DecodeBytes(raw)
	if err != nil {
		return false, err
	}
	data, err := pyjson.Loads(strings.TrimPrefix(text, "\xEF\xBB\xBF"))
	if err != nil {
		return false, err
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return false, nil
	}
	rec, ok := usageRecord(obj, name).(*pyjson.Object)
	if !ok {
		return false, nil
	}
	v, _ := rec.Get("locked")
	return pyrepr.Truthy(v), nil
}

// Names carrying a truthy locked flag, empty on a corrupt sidecar
func (s *Scope) LockedSkillNames() ([]string, error) {
	usage, err := s.LoadUsage()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, k := range usage.Keys {
		v, _ := usage.Vals[k].(*pyjson.Object).Get("locked")
		if pyrepr.Truthy(v) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Drops a skill's usage record
func (s *Scope) Forget(ctx context.Context, name string) {
	if name == "" {
		return
	}
	s.lockedUpdate(ctx, name, func(data *pyjson.Object) (any, bool, error) {
		return nil, data.Delete(name), nil
	}, "skill_usage.forget(%s) failed: %s", nil)
}

// Moves src to dest inside a ledger entry then applies suppression and state side effects
func (s *Scope) relocate(ctx context.Context, src, dest, name, action string, completePackage bool, skill string) (bool, string, error) {
	before := s.LedgerCaptureBefore(ctx, src, completePackage, skill)
	if err := os.Rename(src, dest); err != nil {
		if _, err := pyfs.Move(src, dest); err != nil {
			return false, fmt.Sprintf("failed to %s: %s", action, pyOSErrorText(err)), nil
		}
	}
	archiving := action == "archive"
	if archiving {
		now := time.Now()
		os.Chtimes(dest, now, now)
	}
	bundled := false
	if archiving {
		var err error
		if bundled, err = s.IsBundled(name); err != nil {
			return false, "", err
		}
	}
	if !archiving || bundled {
		if err := s.toggleSuppressedName(home.WithOverride(ctx, s.Home), name, archiving); err != nil {
			return false, "", err
		}
	}
	state := StateActive
	if archiving {
		state = StateArchived
	}
	s.SetState(ctx, name, state)
	if before == nil {
		before = []map[string]string{}
	}
	s.LedgerRecordMutation(ctx, action, name, LedgerMutation{Before: before, BeforeCaptured: true, AfterRoot: dest})
	return true, fmt.Sprintf("%sd to %s", action, dest), nil
}

// Moves a curation eligible skill into the flat archive under its name, timestamped on collision
func (s *Scope) ArchiveSkill(ctx context.Context, name string) (bool, string, error) {
	dir := s.findSkillDir(name)
	if dir == "" && s.findExternalSkillDir(name) != "" {
		return false, externalReadOnlyMessage(name), nil
	}
	eligible, err := s.IsCurationEligible(name, dir)
	if err != nil {
		return false, "", err
	}
	if !eligible {
		if IsProtectedBuiltin(name) {
			return false, fmt.Sprintf("skill '%s' is a protected built-in; it backs load-bearing UX and is never archived or consolidated", name), nil
		}
		if s.IsHubInstalled(name) {
			return false, fmt.Sprintf("skill '%s' is hub-installed; never archive", name), nil
		}
		return false, fmt.Sprintf("skill '%s' is a bundled built-in; enable curator.prune_builtins to allow pruning it", name), nil
	}
	if dir == "" {
		return false, fmt.Sprintf("skill '%s' not found", name), nil
	}
	if s.isExternalSkillPath(dir) {
		return false, externalReadOnlyMessage(name), nil
	}
	dest := filepath.Join(s.archiveDir(), name)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, "failed to create archive dir: " + pyOSErrorText(err), nil
	}
	if lexistsFollow(dest) {
		dest = filepath.Join(filepath.Dir(dest), filepath.Base(dir)+"-"+time.Now().UTC().Format("20060102150405"))
	}
	return s.relocate(ctx, dir, dest, name, "archive", true, name)
}

// Moves an archived skill back to the flat layout, refusing hub and bundled collisions
func (s *Scope) RestoreSkill(ctx context.Context, name string) (bool, string, error) {
	if s.IsHubInstalled(name) {
		return false, fmt.Sprintf("skill '%s' is now hub-installed; restore would shadow the upstream version", name), nil
	}
	bundled, err := s.IsBundled(name)
	if err != nil {
		return false, "", err
	}
	if bundled && !s.pruneBuiltinsEnabled() {
		return false, fmt.Sprintf("skill '%s' is now bundled; restore would shadow the upstream version", name), nil
	}
	root := s.archiveDir()
	if !lexistsFollow(root) {
		return false, "no archive directory", nil
	}
	var dirs []string
	for _, p := range rglob(root, "*") {
		if isDir(p) {
			dirs = append(dirs, p)
		}
	}
	var cands []string
	for _, p := range dirs {
		if filepath.Base(p) == name {
			cands = append(cands, p)
		}
	}
	if len(cands) == 0 {
		prefix := name + "-"
		for _, p := range dirs {
			b := filepath.Base(p)
			if rest, ok := strings.CutPrefix(b, prefix); ok && len(b)-len(prefix) == 14 && pystr.All(rest, pystr.IsDigit) {
				cands = append(cands, p)
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(cands)))
	}
	if len(cands) == 0 {
		for _, p := range dirs {
			md := filepath.Join(p, "SKILL.md")
			if isFile(md) && readSkillName(md, filepath.Base(p)) == name {
				cands = append(cands, p)
			}
		}
	}
	if len(cands) == 0 {
		return false, fmt.Sprintf("skill '%s' not found in archive", name), nil
	}
	dest := filepath.Join(s.SkillsDir(), name)
	if lexistsFollow(dest) {
		return false, "destination already exists: " + dest, nil
	}
	return s.relocate(ctx, cands[0], dest, name, "restore", false, "")
}

// Directory of the first SKILL.md whose frontmatter name matches
func matchSkillDir(skillMDs []string, name string) string {
	for _, p := range skillMDs {
		if readSkillName(p, filepath.Base(filepath.Dir(p))) == name {
			return filepath.Dir(p)
		}
	}
	return ""
}

// Local skill dir by frontmatter name, external mounts skipped
func (s *Scope) findSkillDir(name string) string {
	base := s.SkillsDir()
	if !lexistsFollow(base) {
		return ""
	}
	var mds []string
	for _, p := range iterSkillIndexFiles(base, "SKILL.md") {
		if !s.isExternalSkillPath(p) {
			mds = append(mds, p)
		}
	}
	return matchSkillDir(mds, name)
}

// Skill dir under the configured external dirs by frontmatter name
func (s *Scope) findExternalSkillDir(name string) string {
	dirs := s.AllSkillsDirs()
	for _, base := range dirs[min(1, len(dirs)):] {
		if !lexistsFollow(base) {
			continue
		}
		var mds []string
		for _, p := range rglob(base, "SKILL.md") {
			if !isExcludedSkillPath(p, "") {
				mds = append(mds, p)
			}
		}
		if found := matchSkillDir(mds, name); found != "" {
			return found
		}
	}
	return ""
}

// Rows for every curator managed skill plus pinned eligible local skills, with provenance and _persisted
func (s *Scope) CuratedReport() ([]*pyjson.Object, error) {
	data, err := s.LoadUsage()
	if err != nil {
		return nil, err
	}
	list, err := s.ListAgentCreatedSkillNames()
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, n := range list {
		names[n] = true
	}
	for _, n := range data.Keys {
		pinned, _ := data.Vals[n].(*pyjson.Object).Get("pinned")
		if !pyrepr.Truthy(pinned) {
			continue
		}
		eligible, err := s.IsCurationEligible(n, "")
		if err != nil {
			return nil, err
		}
		if eligible && s.findSkillDir(n) != "" {
			names[n] = true
		}
	}
	return s.reportRows(data, sortedKeys(names), true)
}

// Report rows with _persisted and provenance in the requested order
func (s *Scope) reportRows(data *pyjson.Object, names []string, persistedFirst bool) ([]*pyjson.Object, error) {
	rows := []*pyjson.Object{}
	for _, n := range names {
		prov, err := s.Provenance(n)
		if err != nil {
			return nil, err
		}
		raw, persisted := data.Get(n)
		extra := []any{"provenance", prov, "_persisted", persisted}
		if persistedFirst {
			extra = []any{"_persisted", persisted, "provenance", prov}
		}
		row, err := reportRow(n, raw, extra...)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Reports whether a skill lives only under the external dirs
func (s *Scope) IsExternalOnly(name string) bool {
	return s.findSkillDir(name) == "" && s.findExternalSkillDir(name) != ""
}

// Frontmatter names of every skill under the configured external dirs
func (s *Scope) ExternalSkillNames() map[string]bool {
	names := map[string]bool{}
	dirs := s.AllSkillsDirs()
	for _, base := range dirs[min(1, len(dirs)):] {
		if !lexistsFollow(base) {
			continue
		}
		for _, md := range rglob(base, "SKILL.md") {
			if !isExcludedSkillPath(md, "") {
				names[readSkillName(md, filepath.Base(filepath.Dir(md)))] = true
			}
		}
	}
	return names
}

// Origin label hub, bundled, external or agent
func (s *Scope) Provenance(name string) (string, error) {
	if s.IsHubInstalled(name) {
		return "hub", nil
	}
	bundled, err := s.IsBundled(name)
	if err != nil {
		return "", err
	}
	if bundled {
		return "bundled", nil
	}
	if s.IsExternalOnly(name) {
		return "external", nil
	}
	return "agent", nil
}

// Usage rows for every skill on disk, built-ins and hub included
func (s *Scope) UsageReport() ([]*pyjson.Object, error) {
	base := s.SkillsDir()
	if !lexistsFollow(base) {
		return []*pyjson.Object{}, nil
	}
	data, err := s.LoadUsage()
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, e := range s.iterSkillMDs(base, false) {
		names[e.name] = true
	}
	return s.reportRows(data, sortedKeys(names), false)
}
