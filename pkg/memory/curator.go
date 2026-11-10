package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	curatorDefaultIntervalHours    = 24 * 7
	curatorDefaultMinIdleHours     = 2.0
	curatorDefaultStaleAfterDays   = 14
	curatorDefaultArchiveAfterDays = 30
	curatorDefaultConsolidate      = false
	curatorClaimStaleSeconds       = 3600.0
)

// CuratorCronRewrite is one cron job whose skill references a curator pass rewrote
type CuratorCronRewrite struct {
	JobID   any
	JobName any
	Before  []string
	After   []string
	Mapped  *pyjson.Object
	Dropped []string
}

// CuratorCronRewriteReport is the cron rewrite_skill_refs result
type CuratorCronRewriteReport struct {
	Rewrites    []CuratorCronRewrite
	JobsUpdated int
	JobsScanned int
}

// CuratorReviewRequest carries every AIAgent argument the curator review fork is built with
type CuratorReviewRequest struct {
	Prompt              string
	Model               string
	Provider            string
	Route               *provider.Route
	RequestOverrides    map[string]any
	ReasoningConfig     map[string]any
	EnabledToolsets     []string
	MaxIterations       int
	Platform            string
	WriteOrigin         string
	QuietMode           bool
	SkipContextFiles    bool
	SkipMemory          bool
	MemoryNudgeInterval int
	SkillNudgeInterval  int
	ACPCommand          string
	ACPArgs             []string
	CredentialPool      *provider.Pool
}

// CuratorReviewResult is what the review fork returned and the session messages it recorded
type CuratorReviewResult struct {
	Conversation any
	Messages     []any
}

// CuratorHooks is the cron and agent access the curator reaches, installed by pkg/cron and pkg/agent in init
var CuratorHooks struct {
	ReferencedSkillNames func(ctx context.Context, home string) (map[string]bool, error)
	RewriteSkillRefs     func(ctx context.Context, home string, consolidated *pyjson.Object, pruned []string) (CuratorCronRewriteReport, error)
	UpdateJobs           func(ctx context.Context, home string, update func(jobs []any) (bool, error)) error
	RunReview            func(ctx context.Context, req CuratorReviewRequest) (*CuratorReviewResult, error)
}

var errCronNotLinked = errors.New("No module named 'cron'")

func curatorLog() *slog.Logger { return slog.Default().With("logger", "agent.curator") }

// Skills scope and tool env bound to the home the context resolves to
func curatorScope(ctx context.Context) (*skills.Scope, *tools.Env, string) {
	h := home.Dir(ctx)
	env := &tools.Env{Home: h}
	return skills.ScopeFromEnv(env), env, h
}

func curatorStateFile(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "skills", ".curator_state")
}

// Curator scheduler state merged over the defaults, underscore keys kept, a decode failure yields the defaults
func LoadCuratorState(ctx context.Context) (*pyjson.Object, error) {
	base := pyjson.NewObject()
	for _, k := range []string{"last_run_at", "last_run_duration_seconds", "last_run_summary", "last_run_summary_shown_at", "last_report_path"} {
		base.Set(k, nil)
	}
	base.Set("paused", false)
	base.Set("run_count", 0)
	raw, err := os.ReadFile(curatorStateFile(ctx))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			curatorLog().Debug(fmt.Sprintf("Failed to read curator state: %v", err))
		}
		return base, nil
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("'utf-8' codec can't decode curator state %s", curatorStateFile(ctx))
	}
	data, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
	if err != nil {
		curatorLog().Debug(fmt.Sprintf("Failed to read curator state: %v", err))
		return base, nil
	}
	if obj, ok := data.(*pyjson.Object); ok {
		for _, k := range obj.Keys {
			if _, known := base.Get(k); known || strings.HasPrefix(k, "_") {
				base.Set(k, obj.Vals[k])
			}
		}
	}
	return base, nil
}

// Writes the curator state sorted and indented, logging instead of failing
func SaveCuratorState(ctx context.Context, data *pyjson.Object) {
	if err := config.AtomicJSONWriteWith(ctx, curatorStateFile(ctx), data, pyjson.Indent(2), pyjson.SortKeys(true)); err != nil {
		curatorLog().Debug(fmt.Sprintf("Failed to save curator state: %v", err))
	}
}

// Persists the paused flag on top of the current state
func SetCuratorPaused(ctx context.Context, paused bool) error {
	st, err := LoadCuratorState(ctx)
	if err != nil {
		return err
	}
	st.Set("paused", paused)
	SaveCuratorState(ctx, st)
	return nil
}

// Whether the persisted state marks the curator paused
func CuratorIsPaused(ctx context.Context) (bool, error) {
	st, err := LoadCuratorState(ctx)
	if err != nil {
		return false, err
	}
	v, _ := st.Get("paused")
	return pyrepr.Truthy(v), nil
}

func curatorAsMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case config.Tree:
		return map[string]any(m), true
	}
	return nil, false
}

// Nested dict lookup returning an empty map when any level is missing or not a mapping
func curatorSubdict(node any, keys ...string) map[string]any {
	for _, k := range keys {
		m, ok := curatorAsMap(node)
		if !ok {
			node = nil
			break
		}
		node = m[k]
	}
	if m, ok := curatorAsMap(node); ok {
		return m
	}
	return map[string]any{}
}

// Nested section of the readonly config, empty when the config cannot load
func curatorConfigSection(ctx context.Context, label string, log *slog.Logger, path ...string) map[string]any {
	h := home.Dir(ctx)
	cfg, err := config.LoadReadonly(ctx, h, configLookup(&tools.Env{Home: h}))
	if err != nil {
		log.Debug(fmt.Sprintf("Failed to load config for %s: %v", label, err))
		return map[string]any{}
	}
	return curatorSubdict(map[string]any(cfg), path...)
}

// curator section of the config, replaced by tests
var curatorConfig = func(ctx context.Context) map[string]any {
	return curatorConfigSection(ctx, "curator", curatorLog(), "curator")
}

func curatorConfigGet(ctx context.Context, key string, def any) any {
	cfg := curatorConfig(ctx)
	if v, ok := cfg[key]; ok {
		return v
	}
	return def
}

func curatorBigToInt(n *big.Int) int {
	if !n.IsInt64() {
		if n.Sign() < 0 {
			return math.MinInt
		}
		return math.MaxInt
	}
	return int(n.Int64())
}

// Curator enabled flag, on unless the config says otherwise
func CuratorIsEnabled(ctx context.Context) bool {
	return pyrepr.Truthy(curatorConfigGet(ctx, "enabled", true))
}

// Hours between automatic passes, floored at one
func CuratorIntervalHours(ctx context.Context) int {
	return curatorBoundedCount(ctx, "interval_hours", curatorDefaultIntervalHours)
}

// Idle hours a session needs before an automatic pass, the default when the value is not a number
func CuratorMinIdleHours(ctx context.Context) float64 {
	f, err := pyint.Float(curatorConfigGet(ctx, "min_idle_hours", curatorDefaultMinIdleHours))
	if err != nil {
		return curatorDefaultMinIdleHours
	}
	return f
}

var curatorWarned = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

// Day or hour count from config, the default with one warning per bad value when below one
func curatorBoundedCount(ctx context.Context, key string, def int) int {
	value := def
	if n, err := pyint.Of(curatorConfigGet(ctx, key, def)); err == nil {
		value = curatorBigToInt(n)
	}
	if value < 1 {
		mark := fmt.Sprintf("%s\x00%d", key, value)
		curatorWarned.Lock()
		first := !curatorWarned.seen[mark]
		curatorWarned.seen[mark] = true
		curatorWarned.Unlock()
		if first {
			curatorLog().Warn(fmt.Sprintf("curator.%s must be >= 1 (got %d); using the default of %d", key, value, def))
		}
		return def
	}
	return value
}

// Days of inactivity before a skill turns stale
func CuratorStaleAfterDays(ctx context.Context) int {
	return curatorBoundedCount(ctx, "stale_after_days", curatorDefaultStaleAfterDays)
}

// Days of inactivity before a skill is archived
func CuratorArchiveAfterDays(ctx context.Context) int {
	return curatorBoundedCount(ctx, "archive_after_days", curatorDefaultArchiveAfterDays)
}

// Whether the LLM consolidation pass runs, off by default
func CuratorConsolidate(ctx context.Context) bool {
	return pyrepr.Truthy(curatorConfigGet(ctx, "consolidate", curatorDefaultConsolidate))
}

// datetime.fromisoformat of a truthy string, false for anything unparseable
func curatorParseISO(ts any) (time.Time, bool, bool) {
	if !pyrepr.Truthy(ts) {
		return time.Time{}, false, false
	}
	s, ok := ts.(string)
	if !ok {
		return time.Time{}, false, false
	}
	t, aware, err := pydatetime.ParseISOFormat(s)
	if err != nil {
		return time.Time{}, false, false
	}
	return t, aware, true
}

// Aware instant of an ISO value, naive values read as UTC
func curatorParseUTC(ts any) (time.Time, bool) {
	t, aware, ok := curatorParseISO(ts)
	if !ok {
		return time.Time{}, false
	}
	if !aware {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	}
	return t, true
}

// Enabled, unpaused and one interval past the last run, the first sighting seeds the clock and defers
func CuratorShouldRunNow(ctx context.Context, now time.Time) (bool, error) {
	if !CuratorIsEnabled(ctx) {
		return false, nil
	}
	if paused, err := CuratorIsPaused(ctx); err != nil || paused {
		return false, err
	}
	st, err := LoadCuratorState(ctx)
	if err != nil {
		return false, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	lastRaw, _ := st.Get("last_run_at")
	last, ok := curatorParseUTC(lastRaw)
	if !ok {
		st.Set("last_run_at", pydatetime.ISOFormatUTC(now))
		st.Set("last_run_summary", "deferred first run — curator seeded, will run after one interval; use `icebath curator run --dry-run` to preview now")
		SaveCuratorState(ctx, st)
		return false, nil
	}
	return now.Sub(last) >= time.Duration(CuratorIntervalHours(ctx))*time.Hour, nil
}

// Skill names any cron job references, empty when the cron store cannot be read
func curatorCronReferencedSkills(ctx context.Context) map[string]bool {
	var refs map[string]bool
	err := errCronNotLinked
	if CuratorHooks.ReferencedSkillNames != nil {
		refs, err = CuratorHooks.ReferencedSkillNames(ctx, home.Dir(ctx))
	}
	if err != nil {
		curatorLog().Debug(fmt.Sprintf("Curator could not read cron skill references: %v", err))
		return map[string]bool{}
	}
	return refs
}

func curatorGet(d any, key string) (any, bool) {
	switch m := d.(type) {
	case *pyjson.Object:
		if m == nil {
			return nil, false
		}
		return m.Get(key)
	case map[string]any:
		v, ok := m[key]
		return v, ok
	case config.Tree:
		v, ok := m[key]
		return v, ok
	}
	return nil, false
}

func curatorGetOr(d any, key string, def any) any {
	if v, ok := curatorGet(d, key); ok {
		return v
	}
	return def
}

func curatorIsDict(d any) bool {
	switch m := d.(type) {
	case *pyjson.Object:
		return m != nil
	case map[string]any, config.Tree:
		return true
	}
	return false
}

// int(value or 0) with the Python int conversion errors
func curatorIntOrZero(v any) (int, error) {
	if !pyrepr.Truthy(v) {
		return 0, nil
	}
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	return curatorBigToInt(n), nil
}

func curatorCounter(keys ...string) *pyjson.Object {
	o := pyjson.NewObject()
	for _, k := range keys {
		o.Set(k, 0)
	}
	return o
}

func curatorBump(o *pyjson.Object, key string) {
	v, _ := o.Get(key)
	n, _ := v.(int)
	o.Set(key, n+1)
}

func curatorCount(o any, key string) int {
	n, _ := curatorIntOrZero(curatorGetOr(o, key, 0))
	return n
}

// Moves every curator managed skill between active, stale and archived by its latest activity
func ApplyCuratorTransitions(ctx context.Context, now time.Time) (*pyjson.Object, error) {
	scope, env, _ := curatorScope(ctx)
	if now.IsZero() {
		now = time.Now().UTC()
	}
	staleCutoff := now.Add(-time.Duration(CuratorStaleAfterDays(ctx)) * 24 * time.Hour)
	archiveCutoff := now.Add(-time.Duration(CuratorArchiveAfterDays(ctx)) * 24 * time.Hour)
	protected := curatorCronReferencedSkills(ctx)
	counts := curatorCounter("marked_stale", "archived", "reactivated", "checked", "seeded")
	set := func(name, state, key string) {
		scope.SetState(ctx, name, state)
		curatorBump(counts, key)
	}
	locked, err := scope.LockedSkillNames()
	if err != nil {
		return nil, err
	}
	rows, err := scope.CuratedReport()
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		curatorBump(counts, "checked")
		name := pyStr(curatorGetOr(row, "name", nil))
		if pyrepr.Truthy(curatorGetOr(row, "pinned", nil)) || protected[name] || (len(locked) > 0 && tools.IsLockedSkill(env, name, locked)) {
			continue
		}
		if !pyrepr.Truthy(curatorGetOr(row, "_persisted", true)) {
			if err := scope.SeedRecordIfMissing(ctx, name); err != nil {
				return nil, err
			}
			curatorBump(counts, "seeded")
			continue
		}
		useCount, err := curatorIntOrZero(curatorGetOr(row, "use_count", 0))
		if err != nil {
			return nil, err
		}
		_, _, hasActivity := curatorParseISO(curatorGetOr(row, "last_activity_at", nil))
		if curatorGetOr(row, "provenance", nil) == "bundled" && useCount == 0 && !hasActivity && !pyrepr.Truthy(curatorGetOr(row, "first_seen_at", nil)) {
			scope.ReanchorClock(ctx, name)
			curatorBump(counts, "seeded")
			continue
		}
		anchor, ok := curatorParseUTC(curatorGetOr(row, "last_activity_at", nil))
		if !ok {
			if anchor, ok = curatorParseUTC(curatorGetOr(row, "created_at", nil)); !ok {
				anchor = now
			}
		}
		current := curatorGetOr(row, "state", skills.StateActive)
		if useCount == 0 && anchor.After(staleCutoff) {
			if current == skills.StateStale {
				set(name, skills.StateActive, "reactivated")
			}
			continue
		}
		switch {
		case !anchor.After(archiveCutoff) && current != skills.StateArchived:
			archived, _, err := scope.ArchiveSkill(skills.WithLedgerActor(ctx, "curator"), name)
			if err != nil {
				return nil, err
			}
			if archived {
				curatorBump(counts, "archived")
			}
		case !anchor.After(staleCutoff) && current == skills.StateActive:
			set(name, skills.StateStale, "marked_stale")
		case anchor.After(staleCutoff) && current == skills.StateStale:
			set(name, skills.StateActive, "reactivated")
		}
	}
	return counts, nil
}

// logs/curator under the home, created on the way
func curatorReportsRoot(ctx context.Context) string {
	root := filepath.Join(home.Dir(ctx), "logs", "curator")
	if err := os.MkdirAll(root, 0o777); err != nil {
		curatorLog().Debug(fmt.Sprintf("Curator reports dir create failed: %v", err))
	}
	return root
}

// Whether needle equals a whole filename stem or directory name of path, hyphens and underscores alike
func curatorNeedleInPathComponent(needle, path string) bool {
	norm := strings.ReplaceAll(needle, "-", "_")
	for _, part := range strings.Split(strings.ReplaceAll(path, "\\", "/"), "/") {
		if part == "" {
			continue
		}
		stem := part
		if i := strings.LastIndex(part, "."); i >= 0 {
			stem = part[:i]
		}
		if strings.ReplaceAll(stem, "-", "_") == norm {
			return true
		}
	}
	return false
}

// Parsed skill_manage arguments of a tool call, a raw holder for malformed JSON when asked
func curatorSkillManageArgs(tc any, rawFallback bool) (any, bool) {
	if !curatorIsDict(tc) || curatorGetOr(tc, "name", nil) != "skill_manage" {
		return nil, false
	}
	raw := curatorGetOr(tc, "arguments", nil)
	if !pyrepr.Truthy(raw) {
		raw = ""
	}
	s, isStr := raw.(string)
	if !isStr {
		return raw, curatorIsDict(raw)
	}
	args, err := pyjson.Loads(s)
	if err != nil {
		if rawFallback {
			o := pyjson.NewObject()
			o.Set("_raw", s)
			return o, true
		}
		return nil, false
	}
	return args, curatorIsDict(args)
}

// First argument value that references one of the needles by path component or word boundary
func curatorFindReference(args any, needles []string) (string, bool) {
	for _, key := range []string{"file_path", "file_content", "content", "new_string", "_raw"} {
		hay, ok := curatorGetOr(args, key, nil).(string)
		if !ok {
			continue
		}
		for _, n := range needles {
			if n == "" {
				continue
			}
			if key == "file_path" {
				if curatorNeedleInPathComponent(n, hay) {
					return hay, true
				}
				continue
			}
			if curatorWordMatch(n, hay) {
				return hay, true
			}
		}
	}
	return "", false
}

func curatorRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func curatorEntry(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func curatorSetOf(names ...[]string) map[string]bool {
	out := map[string]bool{}
	for _, list := range names {
		for _, n := range list {
			out[n] = true
		}
	}
	return out
}

// Splits removed skills into consolidated and pruned by skill_manage calls on surviving skills that reference them
func curatorClassifyRemoved(removed, added []string, afterNames map[string]bool, toolCalls []any) ([]*pyjson.Object, []*pyjson.Object) {
	var consolidated, pruned []*pyjson.Object
	var parsed []any
	for _, tc := range toolCalls {
		if a, ok := curatorSkillManageArgs(tc, true); ok {
			parsed = append(parsed, a)
		}
	}
	destinations := map[string]bool{}
	for n := range afterNames {
		destinations[n] = true
	}
	for _, n := range added {
		destinations[n] = true
	}
	for _, name := range removed {
		if name == "" {
			continue
		}
		needles := []string{name, strings.ReplaceAll(name, "-", "_"), strings.ReplaceAll(name, "_", "-")}
		found := false
		for _, args := range parsed {
			target, ok := curatorGetOr(args, "name", nil).(string)
			if !ok || target == "" || target == name || !destinations[target] {
				continue
			}
			if hay, ok := curatorFindReference(args, needles); ok {
				evidence := fmt.Sprintf("skill_manage action=%s on '%s' referenced '%s' in %s", pyStr(curatorGetOr(args, "action", "?")), target, name, curatorRunes(hay, 80))
				consolidated = append(consolidated, curatorEntry("name", name, "into", target, "evidence", evidence))
				found = true
				break
			}
		}
		if !found {
			pruned = append(pruned, curatorEntry("name", name))
		}
	}
	return consolidated, pruned
}

// Consolidations and prunings of the fenced yaml block in the final response, empty lists when absent or malformed
func curatorParseStructuredSummary(final any) (cons, prunes []*pyjson.Object) {
	text, ok := final.(string)
	if !ok {
		return nil, nil
	}
	body, ok := curatorYAMLFence(text)
	if !ok {
		return nil, nil
	}
	data, err := config.FastSafeLoad([]byte(body))
	if err != nil {
		return nil, nil
	}
	if _, isMap := curatorAsMap(data); !isMap {
		return nil, nil
	}
	entries := func(key string, fields ...string) []*pyjson.Object {
		raw := curatorGetOr(data, key, nil)
		list, isList := raw.([]any)
		if !isList {
			return nil
		}
		var out []*pyjson.Object
		for _, e := range list {
			if _, isMap := curatorAsMap(e); !isMap {
				continue
			}
			cleaned := pyjson.NewObject()
			keep := true
			for _, f := range append(append([]string{}, fields...), "reason") {
				v, isStr := curatorGetOr(e, f, nil).(string)
				if isStr {
					v = pyStrip(v)
				}
				cleaned.Set(f, v)
				if f != "reason" && v == "" {
					keep = false
				}
			}
			if keep {
				out = append(out, cleaned)
			}
		}
		return out
	}
	return entries("consolidations", "from", "into"), entries("prunings", "name")
}

// Absorption targets the model declared on skill_manage delete calls, an empty target meaning prune
func curatorAbsorbedDeclarations(toolCalls []any) map[string]string {
	out := map[string]string{}
	for _, tc := range toolCalls {
		args, ok := curatorSkillManageArgs(tc, false)
		if !ok || curatorGetOr(args, "action", nil) != "delete" {
			continue
		}
		name, okName := curatorGetOr(args, "name", nil).(string)
		target, okTarget := curatorGetOr(args, "absorbed_into", nil).(string)
		if okName && pyStrip(name) != "" && okTarget {
			out[pyStrip(name)] = pyStrip(target)
		}
	}
	return out
}

func curatorStrField(o *pyjson.Object, key string) string {
	if o == nil {
		return ""
	}
	v, _ := o.Get(key)
	s, _ := v.(string)
	return s
}

// Merges the declared targets, the model block and the tool call audit so every removal lands in one bucket
func curatorReconcile(removed []string, heurCons []*pyjson.Object, modelCons, modelPrunes []*pyjson.Object, destinations map[string]bool, declared map[string]string) ([]*pyjson.Object, []*pyjson.Object) {
	index := func(list []*pyjson.Object, key string) map[string]*pyjson.Object {
		out := map[string]*pyjson.Object{}
		for _, e := range list {
			out[curatorStrField(e, key)] = e
		}
		return out
	}
	hcBy, mcBy, mpBy := index(heurCons, "name"), index(modelCons, "from"), index(modelPrunes, "name")
	var consolidated, pruned []*pyjson.Object
	for _, name := range removed {
		mc, mp, hc := mcBy[name], mpBy[name], hcBy[name]
		dec, hasDec := declared[name]
		mcReason := curatorStrField(mc, "reason")
		cons := func(into, source, reason string, extra ...any) {
			consolidated = append(consolidated, curatorEntry(append([]any{"name", name, "into", into, "source", source, "reason", reason}, extra...)...))
		}
		prune := func(source, reason string) {
			pruned = append(pruned, curatorEntry("name", name, "source", source, "reason", reason))
		}
		var hcEvidence []any
		if hc != nil && curatorStrField(hc, "evidence") != "" {
			hcEvidence = []any{"evidence", curatorStrField(hc, "evidence")}
		}
		switch {
		case hasDec && dec != "" && destinations[dec]:
			cons(dec, "absorbed_into (model-declared at delete)", mcReason, hcEvidence...)
		case hasDec && dec == "":
			prune("absorbed_into=\"\" (model-declared prune)", curatorStrField(mp, "reason"))
		case mc != nil && destinations[curatorStrField(mc, "into")]:
			source := "model"
			if hc != nil {
				source += "+audit"
			}
			cons(curatorStrField(mc, "into"), source, mcReason, hcEvidence...)
		case mc != nil && hc != nil:
			cons(curatorStrField(hc, "into"), "tool-call audit (model named missing umbrella)", "", "evidence", curatorStrField(hc, "evidence"), "model_claimed_into", curatorStrField(mc, "into"))
		case mc != nil:
			prune("fallback (model named missing umbrella, no tool-call evidence)", "")
		case hc != nil:
			cons(curatorStrField(hc, "into"), "tool-call audit (model omitted from structured block)", "", "evidence", curatorStrField(hc, "evidence"))
		case mp != nil:
			prune("model", curatorStrField(mp, "reason"))
		default:
			prune("no-evidence fallback", "")
		}
	}
	return consolidated, pruned
}

type curatorRunDiff struct {
	afterNames   map[string]bool
	removed      []string
	added        []string
	consolidated []*pyjson.Object
	pruned       []*pyjson.Object
}

func curatorSortedDiff(a, b map[string]bool) []string {
	out := []string{}
	for n := range a {
		if !b[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Diffs the before and after skill sets and classifies every removal
func curatorDiffAndClassify(before, after map[string]bool, toolCalls []any, final any) curatorRunDiff {
	removed, added := curatorSortedDiff(before, after), curatorSortedDiff(after, before)
	heur, _ := curatorClassifyRemoved(removed, added, after, toolCalls)
	mc, mp := curatorParseStructuredSummary(final)
	destinations := curatorSetOf(added)
	for n := range after {
		destinations[n] = true
	}
	cons, pruned := curatorReconcile(removed, heur, mc, mp, destinations, curatorAbsorbedDeclarations(toolCalls))
	return curatorRunDiff{after, removed, added, cons, pruned}
}

func curatorByName(report []*pyjson.Object) map[string]*pyjson.Object {
	out := map[string]*pyjson.Object{}
	for _, r := range report {
		if r != nil {
			out[pyStr(curatorGetOr(r, "name", nil))] = r
		}
	}
	return out
}

func curatorNames(by map[string]*pyjson.Object) map[string]bool {
	out := map[string]bool{}
	for n := range by {
		out[n] = true
	}
	return out
}

// Where did my skills go lines appended to the run summary, empty when nothing was archived
func curatorRenameSummary(beforeNames map[string]bool, afterReport []*pyjson.Object, toolCalls []any, final any) string {
	afterNames := curatorNames(curatorByName(afterReport))
	if len(curatorSortedDiff(beforeNames, afterNames)) == 0 {
		return ""
	}
	diff := curatorDiffAndClassify(beforeNames, afterNames, toolCalls, final)
	const show = 10
	total := len(diff.consolidated) + len(diff.pruned)
	var entries []string
	for _, e := range diff.consolidated {
		entries = append(entries, fmt.Sprintf("  • %s → %s", pyStr(curatorGetOr(e, "name", "?")), pyStr(curatorGetOr(e, "into", "?"))))
	}
	for _, e := range diff.pruned {
		entries = append(entries, fmt.Sprintf("  • %s — pruned (stale)", pyStr(curatorGetOr(e, "name", "?"))))
	}
	lines := []string{fmt.Sprintf("archived %d skill(s):", total)}
	lines = append(lines, entries[:min(show, len(entries))]...)
	if total > show {
		lines = append(lines, fmt.Sprintf("  … and %d more", total-show))
	}
	lines = append(lines, "full report: icebath curator status")
	umbrellas := map[string]bool{}
	for _, e := range diff.consolidated {
		if into := curatorStrField(e, "into"); into != "" {
			umbrellas[into] = true
		}
	}
	if len(umbrellas) > 0 {
		sorted := curatorSortedDiff(umbrellas, nil)
		lines = append(lines, "keep an umbrella stable: icebath curator pin "+sorted[0])
	}
	return strings.Join(lines, "\n")
}

func curatorEmptyRewrites() *pyjson.Object {
	return curatorEntry("rewrites", []any{}, "jobs_updated", 0, "jobs_scanned", 0)
}

// Points cron jobs at the umbrella of a consolidated skill and drops pruned ones, never failing the run
func curatorRewriteCronRefs(ctx context.Context, consolidated, pruned []*pyjson.Object) *pyjson.Object {
	cmap := pyjson.NewObject()
	for _, e := range consolidated {
		if n, into := curatorGetOr(e, "name", nil), curatorGetOr(e, "into", nil); pyrepr.Truthy(n) && pyrepr.Truthy(into) {
			cmap.Set(pyStr(n), into)
		}
	}
	var names []string
	for _, e := range pruned {
		if n := curatorGetOr(e, "name", nil); pyrepr.Truthy(n) {
			names = append(names, pyStr(n))
		}
	}
	if cmap.Len() == 0 && len(names) == 0 {
		return curatorEmptyRewrites()
	}
	err := errCronNotLinked
	var rep CuratorCronRewriteReport
	if CuratorHooks.RewriteSkillRefs != nil {
		rep, err = CuratorHooks.RewriteSkillRefs(ctx, home.Dir(ctx), cmap, names)
	}
	if err != nil {
		curatorLog().Debug(fmt.Sprintf("Curator cron skill rewrite failed: %v", err))
		out := curatorEmptyRewrites()
		out.Set("error", err.Error())
		return out
	}
	rewrites := make([]any, 0, len(rep.Rewrites))
	for _, r := range rep.Rewrites {
		mapped := r.Mapped
		if mapped == nil {
			mapped = pyjson.NewObject()
		}
		rewrites = append(rewrites, curatorEntry("job_id", r.JobID, "job_name", r.JobName, "before", curatorStrList(r.Before),
			"after", curatorStrList(r.After), "mapped", mapped, "dropped", curatorStrList(r.Dropped)))
	}
	return curatorEntry("rewrites", rewrites, "jobs_updated", rep.JobsUpdated, "jobs_scanned", rep.JobsScanned)
}

func curatorStrList(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func curatorObjList(in []*pyjson.Object) []any {
	out := make([]any, len(in))
	for i, o := range in {
		out[i] = o
	}
	return out
}

// Writes the rendered text or the JSON dump of a payload, logging any failure
func curatorWriteFile(path, label string, render any) {
	var text string
	switch r := render.(type) {
	case func() string:
		text = r()
	default:
		s, err := pyjson.Dumps(r, pyjson.Indent(2), pyjson.EnsureASCII(false))
		if err != nil {
			curatorLog().Debug(fmt.Sprintf("Curator %s write failed: %v", label, err))
			return
		}
		text = s + "\n"
	}
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
		curatorLog().Debug(fmt.Sprintf("Curator %s write failed: %v", label, err))
	}
}

type curatorRunReportInput struct {
	startedAt    time.Time
	elapsed      float64
	autoCounts   *pyjson.Object
	beforeReport []*pyjson.Object
	beforeNames  map[string]bool
	afterReport  []*pyjson.Object
	llmMeta      *pyjson.Object
}

func curatorToolCalls(meta *pyjson.Object) []any {
	v, _ := meta.Get("tool_calls")
	list, _ := v.([]any)
	return list
}

// Writes run.json and REPORT.md under a fresh logs/curator stamp directory, empty when it cannot be created
func curatorWriteRunReport(ctx context.Context, in curatorRunReportInput) string {
	root, stamp := curatorReportsRoot(ctx), in.startedAt.UTC().Format("20060102-150405")
	runDir, suffix := filepath.Join(root, stamp), 1
	for curatorExists(runDir) {
		suffix++
		runDir = filepath.Join(root, fmt.Sprintf("%s-%d", stamp, suffix))
	}
	if err := os.MkdirAll(filepath.Dir(runDir), 0o777); err == nil {
		err = os.Mkdir(runDir, 0o777)
		if err != nil {
			curatorLog().Debug(fmt.Sprintf("Curator run dir create failed: %v", err))
			return ""
		}
	} else {
		curatorLog().Debug(fmt.Sprintf("Curator run dir create failed: %v", err))
		return ""
	}
	toolCalls := curatorToolCalls(in.llmMeta)
	afterBy, beforeBy := curatorByName(in.afterReport), curatorByName(in.beforeReport)
	final := curatorGetOr(in.llmMeta, "final", "")
	if !pyrepr.Truthy(final) {
		final = ""
	}
	diff := curatorDiffAndClassify(in.beforeNames, curatorNames(afterBy), toolCalls, final)
	var both []string
	for n := range diff.afterNames {
		if in.beforeNames[n] {
			both = append(both, n)
		}
	}
	sort.Strings(both)
	transitions := []any{}
	for _, n := range both {
		b, a := curatorGetOr(beforeBy[n], "state", nil), curatorGetOr(afterBy[n], "state", nil)
		if pyrepr.Truthy(b) && pyrepr.Truthy(a) && b != a {
			transitions = append(transitions, curatorEntry("name", n, "from", b, "to", a))
		}
	}
	tcCounts := pyjson.NewObject()
	total := 0
	for _, tc := range toolCalls {
		key := pyStr(curatorGetOr(tc, "name", "unknown"))
		v, _ := tcCounts.Get(key)
		n, _ := v.(int)
		tcCounts.Set(key, n+1)
		total++
	}
	cronRewrites := curatorRewriteCronRefs(ctx, diff.consolidated, diff.pruned)
	jobsUpdated := curatorCount(cronRewrites, "jobs_updated")
	prunedNames := []any{}
	for _, p := range diff.pruned {
		prunedNames = append(prunedNames, curatorGetOr(p, "name", nil))
	}
	counts := curatorEntry("before", len(in.beforeNames), "after", len(diff.afterNames), "delta", len(diff.afterNames)-len(in.beforeNames),
		"archived_this_run", len(diff.removed), "added_this_run", len(diff.added), "consolidated_this_run", len(diff.consolidated),
		"pruned_this_run", len(diff.pruned), "state_transitions", len(transitions), "cron_jobs_rewritten", jobsUpdated, "tool_calls_total", total)
	payload := curatorEntry("started_at", pydatetime.ISOFormatUTC(in.startedAt), "duration_seconds", pyint.Round(in.elapsed, 2),
		"model", curatorGetOr(in.llmMeta, "model", ""), "provider", curatorGetOr(in.llmMeta, "provider", ""), "auto_transitions", in.autoCounts,
		"counts", counts, "tool_call_counts", tcCounts, "archived", curatorStrList(diff.removed), "consolidated", curatorObjList(diff.consolidated),
		"pruned", curatorObjList(diff.pruned), "pruned_names", prunedNames, "added", curatorStrList(diff.added), "state_transitions", transitions,
		"cron_rewrites", cronRewrites, "llm_final", curatorGetOr(in.llmMeta, "final", ""), "llm_summary", curatorGetOr(in.llmMeta, "summary", ""),
		"llm_error", curatorGetOr(in.llmMeta, "error", nil), "tool_calls", curatorGetOr(in.llmMeta, "tool_calls", []any{}))
	curatorWriteFile(filepath.Join(runDir, "run.json"), "run.json", payload)
	curatorWriteFile(filepath.Join(runDir, "REPORT.md"), "REPORT.md", func() string { return curatorRenderReport(payload) })
	if jobsUpdated > 0 {
		curatorWriteFile(filepath.Join(runDir, "cron_rewrites.json"), "cron_rewrites.json", cronRewrites)
	}
	return runDir
}

func curatorExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func curatorReasonSuffix(entry any) string {
	reason := curatorGetOr(entry, "reason", nil)
	if !pyrepr.Truthy(reason) {
		return ""
	}
	if r := pyStrip(pyStr(reason)); r != "" {
		return " — " + r
	}
	return ""
}

func curatorConsolidatedLines(entry any) []string {
	line := fmt.Sprintf("- `%s` → merged into `%s`", pyStr(curatorGetOr(entry, "name", "?")), pyStr(curatorGetOr(entry, "into", "?"))) + curatorReasonSuffix(entry)
	if source, _ := curatorGetOr(entry, "source", "").(string); source != "" && strings.HasPrefix(source, "tool-call audit") {
		line += fmt.Sprintf("  _(detected via %s)_", source)
	}
	out := []string{line}
	if claimed := curatorGetOr(entry, "model_claimed_into", nil); pyrepr.Truthy(claimed) {
		out = append(out, fmt.Sprintf("  ⚠ The curator's summary named `%s` as the umbrella but that skill doesn't exist post-run; showing the tool-call audit's finding instead.", pyStr(claimed)))
	}
	return out
}

func curatorPrunedLines(entry any) []string {
	if curatorIsDict(entry) {
		return []string{fmt.Sprintf("- `%s`", pyStr(curatorGetOr(entry, "name", "?"))) + curatorReasonSuffix(entry)}
	}
	return []string{fmt.Sprintf("- `%s`", pyStr(entry))}
}

func curatorJoinList(v any) string {
	list, _ := v.([]any)
	parts := make([]string, len(list))
	for i, s := range list {
		parts[i] = pyStr(s)
	}
	return strings.Join(parts, ", ")
}

func curatorCronRewriteLines(entry any) []string {
	jobName := curatorGetOr(entry, "job_name", nil)
	if !pyrepr.Truthy(jobName) {
		if jobName = curatorGetOr(entry, "job_id", nil); !pyrepr.Truthy(jobName) {
			jobName = "?"
		}
	}
	after := curatorJoinList(curatorGetOr(entry, "after", nil))
	if after == "" {
		after = "(none)"
	}
	out := []string{fmt.Sprintf("- `%s`: `%s` → `%s`", pyStr(jobName), curatorJoinList(curatorGetOr(entry, "before", nil)), after)}
	if mapped, ok := curatorGetOr(entry, "mapped", nil).(*pyjson.Object); ok {
		for _, k := range mapped.Keys {
			out = append(out, fmt.Sprintf("    - `%s` → `%s` (consolidated)", k, pyStr(mapped.Vals[k])))
		}
	}
	if dropped, ok := curatorGetOr(entry, "dropped", nil).([]any); ok {
		for _, n := range dropped {
			out = append(out, fmt.Sprintf("    - `%s` dropped (pruned)", pyStr(n)))
		}
	}
	return out
}

type curatorReportSection struct {
	key, title, intro string
	render            func(any) []string
	show              int
	hint              string
}

var curatorReportSections = []curatorReportSection{
	{"consolidated", "Consolidated into umbrella skills", curatorIntroConsolidated, curatorConsolidatedLines, 50, "see `run.json`"},
	{"pruned", "Pruned — archived for staleness", curatorIntroPruned, curatorPrunedLines, 50, "see `run.json`"},
	{"added", "New skills this run", curatorIntroAdded, func(n any) []string { return []string{fmt.Sprintf("- `%s`", pyStr(n))} }, -1, ""},
	{"state_transitions", "State transitions", "", func(t any) []string {
		return []string{fmt.Sprintf("- `%s`: %s → %s", pyStr(curatorGetOr(t, "name", nil)), pyStr(curatorGetOr(t, "from", nil)), pyStr(curatorGetOr(t, "to", nil)))}
	}, -1, ""},
	{"cron_rewrites", "Cron job skill references rewritten", curatorIntroCronRewrites, curatorCronRewriteLines, 25, "see `cron_rewrites.json`"},
}

func curatorSignedInt(n int) string {
	if n >= 0 {
		return fmt.Sprintf("+%d", n)
	}
	return fmt.Sprintf("%d", n)
}

// Human readable REPORT.md of a run payload
func curatorRenderReport(p *pyjson.Object) string {
	durV := curatorGetOr(p, "duration_seconds", 0)
	dur := 0
	if f, err := pyint.Float(durV); err == nil && pyrepr.Truthy(durV) {
		dur = int(f)
	}
	mins, secs := dur/60, dur%60
	label := fmt.Sprintf("%ds", secs)
	if mins != 0 {
		label = fmt.Sprintf("%dm %ds", mins, secs)
	}
	counts, auto, tc := curatorGetOr(p, "counts", nil), curatorGetOr(p, "auto_transitions", nil), curatorGetOr(p, "tool_call_counts", nil)
	errV := curatorGetOr(p, "llm_error", nil)
	orUnresolved := func(key string) string {
		if v := curatorGetOr(p, key, nil); pyrepr.Truthy(v) {
			return pyStr(v)
		}
		return "(not resolved)"
	}
	var byName []string
	if o, ok := tc.(*pyjson.Object); ok {
		keys := append([]string{}, o.Keys...)
		sort.Strings(keys)
		for _, k := range keys {
			byName = append(byName, fmt.Sprintf("%s=%s", k, pyStr(o.Vals[k])))
		}
	}
	tcLabel := strings.Join(byName, ", ")
	if tcLabel == "" {
		tcLabel = "none"
	}
	lines := []string{
		fmt.Sprintf("# Curator run — %s\n", pyStr(curatorGetOr(p, "started_at", ""))),
		fmt.Sprintf("Model: `%s` via `%s`  ·  Duration: %s  ·  Agent-created skills: %d → %d (%s)\n", orUnresolved("model"), orUnresolved("provider"),
			label, curatorCount(counts, "before"), curatorCount(counts, "after"), curatorSignedInt(curatorCount(counts, "delta"))),
	}
	if pyrepr.Truthy(errV) {
		lines = append(lines, fmt.Sprintf("> ⚠ LLM pass error: `%s`\n", pyStr(errV)))
	}
	lines = append(lines, "## Auto-transitions (pure, no LLM)\n", fmt.Sprintf("- checked: %d", curatorCount(auto, "checked")),
		fmt.Sprintf("- marked stale: %d", curatorCount(auto, "marked_stale")),
		fmt.Sprintf("- archived (no LLM, pure time-based staleness): %d", curatorCount(auto, "archived")),
		fmt.Sprintf("- reactivated: %d", curatorCount(auto, "reactivated")), "",
		"## LLM consolidation pass\n",
		fmt.Sprintf("- tool calls: **%d** (by name: %s)", curatorCount(counts, "tool_calls_total"), tcLabel),
		fmt.Sprintf("- consolidated into umbrellas: **%d**", curatorCount(counts, "consolidated_this_run")),
		fmt.Sprintf("- pruned (archived for staleness): **%d**", curatorCount(counts, "pruned_this_run")),
		fmt.Sprintf("- new skills this run: **%d**", curatorCount(counts, "added_this_run")),
		fmt.Sprintf("- state transitions (active ↔ stale ↔ archived): **%d**", curatorCount(counts, "state_transitions")), "")
	for _, sec := range curatorReportSections {
		raw := curatorGetOr(p, sec.key, nil)
		if sec.key == "cron_rewrites" {
			raw = curatorGetOr(raw, "rewrites", nil)
		}
		items, _ := raw.([]any)
		if len(items) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("### %s (%d)\n", sec.title, len(items)))
		if sec.intro != "" {
			lines = append(lines, sec.intro)
		}
		shown := items
		if sec.show >= 0 && len(items) > sec.show {
			shown = items[:sec.show]
		}
		for _, e := range shown {
			lines = append(lines, sec.render(e)...)
		}
		if sec.show >= 0 && len(items) > sec.show {
			lines = append(lines, fmt.Sprintf("- … and %d more (%s)", len(items)-sec.show, sec.hint))
		}
		lines = append(lines, "")
	}
	finalV := curatorGetOr(p, "llm_final", nil)
	final := ""
	if pyrepr.Truthy(finalV) {
		final = pyStrip(pyStr(finalV))
	}
	if final != "" {
		lines = append(lines, "## LLM final summary\n", final, "")
	} else if summary := curatorGetOr(p, "llm_summary", nil); !pyrepr.Truthy(errV) && pyrepr.Truthy(summary) {
		lines = append(lines, "## LLM summary\n", pyStr(summary), "")
	}
	lines = append(lines, "## Recovery\n", "- Restore an archived skill: `icebath curator restore <name>`",
		"- All archives live under `~/.icebath/skills/.archive/` and are recoverable by `mv`",
		"- See `run.json` in this directory for the full machine-readable record.", "")
	return strings.Join(lines, "\n")
}

// Consolidation candidates for the review prompt, bundled and disabled skills left out
func curatorRenderCandidateList(ctx context.Context) (string, error) {
	scope, _, _ := curatorScope(ctx)
	disabled := scope.DisabledSkillNames("")
	report, err := scope.CuratedReport()
	if err != nil {
		return "", err
	}
	var rows []*pyjson.Object
	for _, r := range report {
		name := pyStr(curatorGetOr(r, "name", nil))
		bundled, err := scope.IsBundled(name)
		if err != nil {
			return "", err
		}
		if !bundled && !disabled[name] {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return "No agent-created skills to review.", nil
	}
	cron := curatorCronReferencedSkills(ctx)
	yesNo := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	lines := []string{fmt.Sprintf("Agent-created skills (%d):\n", len(rows))}
	for _, r := range rows {
		name := pyStr(curatorGetOr(r, "name", nil))
		last := curatorGetOr(r, "last_activity_at", nil)
		if !pyrepr.Truthy(last) {
			last = "never"
		}
		lines = append(lines, fmt.Sprintf("- %s  provenance=%s  state=%s  pinned=%s  cron=%s  activity=%s  use=%s  view=%s  patches=%s  last_activity=%s",
			name, pyStr(curatorGetOr(r, "provenance", "agent")), pyStr(curatorGetOr(r, "state", nil)), yesNo(pyrepr.Truthy(curatorGetOr(r, "pinned", nil))),
			yesNo(cron[name]), pyStr(curatorGetOr(r, "activity_count", 0)), pyStr(curatorGetOr(r, "use_count", 0)), pyStr(curatorGetOr(r, "view_count", 0)),
			pyStr(curatorGetOr(r, "patch_count", 0)), pyStr(last)))
	}
	return strings.Join(lines, "\n"), nil
}

// Result of an LLM pass that did not run
func curatorLLMMeta(summary string, errText any) *pyjson.Object {
	return curatorEntry("final", "", "summary", summary, "model", "", "provider", "", "tool_calls", []any{}, "error", errText)
}

func curatorNotify(onSummary func(string), message string) {
	if onSummary == nil {
		return
	}
	defer func() { _ = recover() }()
	onSummary(message)
}

func curatorSafeReport(ctx context.Context) []*pyjson.Object {
	scope, _, _ := curatorScope(ctx)
	rows, err := scope.CuratedReport()
	if err != nil {
		return nil
	}
	return rows
}

// LLM half of a run, forking unless there are no candidates, then appending the rename map
func curatorConsolidationPass(ctx context.Context, prefix, autoSummary string, dryRun bool, beforeNames map[string]bool) (string, *pyjson.Object) {
	var final string
	var meta *pyjson.Object
	list, err := curatorRenderCandidateList(ctx)
	switch {
	case err != nil:
		curatorLog().Debug(fmt.Sprintf("Curator LLM pass failed: %v", err))
		final = fmt.Sprintf("%s%s; llm: error (%v)", prefix, autoSummary, err)
		meta = curatorLLMMeta(fmt.Sprintf("error (%v)", err), err.Error())
	case strings.Contains(list, "No agent-created skills"):
		final = prefix + autoSummary + "; llm: skipped (no candidates)"
		meta = curatorLLMMeta("skipped (no candidates)", nil)
	default:
		prompt := CuratorReviewPrompt + "\n\n" + list
		if dryRun {
			prompt = CuratorDryRunBanner + "\n\n" + prompt
		}
		meta = curatorLLMReview(ctx, prompt)
		final = fmt.Sprintf("%s%s; llm: %s", prefix, autoSummary, pyStr(curatorGetOr(meta, "summary", "no change")))
	}
	scope, _, _ := curatorScope(ctx)
	after, err := scope.CuratedReport()
	if err != nil {
		curatorLog().Debug(fmt.Sprintf("Curator rename summary build failed: %v", err))
		return final, meta
	}
	finalText := curatorGetOr(meta, "final", "")
	if !pyrepr.Truthy(finalText) {
		finalText = ""
	}
	if lines := curatorRenameSummary(beforeNames, after, curatorToolCalls(meta), finalText); lines != "" {
		final += "\n" + lines
	}
	return final, meta
}

// Live curator-review background passes, joinable like the named Python daemon threads
var curatorReviewThreads sync.WaitGroup

// CuratorRunOptions are the run_curator_review keyword arguments
type CuratorRunOptions struct {
	OnSummary   func(string)
	Synchronous bool
	DryRun      bool
	Consolidate *bool
	Trigger     string
}

// One curator pass, deterministic transitions then the optional LLM fork, recording state and a report
func RunCuratorReview(ctx context.Context, opts CuratorRunOptions) (*pyjson.Object, error) {
	consolidate := CuratorConsolidate(ctx)
	if opts.Consolidate != nil {
		consolidate = *opts.Consolidate
	}
	trigger := opts.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	start := time.Now().UTC()
	h := home.Dir(ctx)
	var counts *pyjson.Object
	if opts.DryRun {
		counts = curatorEntry("checked", len(curatorSafeReport(ctx)), "marked_stale", 0, "archived", 0, "reactivated", 0)
	} else {
		if consolidate {
			snap, ok, err := curatorSnapshotSkills(ctx, "pre-curator-run", nil)
			if err != nil {
				curatorLog().Debug(fmt.Sprintf("Curator pre-run snapshot failed: %v", err))
			} else if ok {
				curatorNotify(opts.OnSummary, fmt.Sprintf("curator: snapshot created (%s)", filepath.Base(snap)))
			}
		} else {
			PruneOldSnapshots(ctx)
		}
		var err error
		if counts, err = ApplyCuratorTransitions(ctx, start); err != nil {
			return nil, err
		}
	}
	var parts []string
	for _, kl := range [][2]string{{"marked_stale", "marked stale"}, {"archived", "archived"}, {"reactivated", "reactivated"}} {
		if n := curatorCount(counts, kl[0]); n != 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, kl[1]))
		}
	}
	autoSummary := strings.Join(parts, ", ")
	if autoSummary == "" {
		autoSummary = "no changes"
	}
	prefix := "auto: "
	if opts.DryRun {
		prefix = "dry-run auto: "
	}
	st, err := LoadCuratorState(ctx)
	if err != nil {
		return nil, err
	}
	st.Set("last_run_summary", prefix+autoSummary)
	if !opts.DryRun {
		runCount, err := pyint.Of(curatorGetOr(st, "run_count", 0))
		if err != nil {
			return nil, err
		}
		st.Set("last_run_at", pydatetime.ISOFormatUTC(start))
		st.Set("run_count", pyint.Native(new(big.Int).Add(runCount, big.NewInt(1))))
	}
	SaveCuratorState(ctx, st)
	llmPass := func(ctx context.Context) {
		beforeReport := curatorSafeReport(ctx)
		beforeNames := curatorNames(curatorByName(beforeReport))
		var final string
		var meta *pyjson.Object
		if consolidate {
			final, meta = curatorConsolidationPass(ctx, prefix, autoSummary, opts.DryRun, beforeNames)
		} else {
			final = prefix + autoSummary + "; llm: skipped (consolidation off)"
			meta = curatorLLMMeta("skipped (consolidation off)", nil)
		}
		elapsed := time.Since(start).Seconds()
		st2, err := LoadCuratorState(ctx)
		if err != nil {
			curatorLog().Debug(fmt.Sprintf("Curator report write failed: %v", err))
			return
		}
		st2.Set("last_run_duration_seconds", elapsed)
		st2.Set("last_run_summary", final)
		afterReport := curatorSafeReport(ctx)
		if dir := curatorWriteRunReport(ctx, curatorRunReportInput{start, elapsed, counts, beforeReport, beforeNames, afterReport, meta}); dir != "" {
			st2.Set("last_report_path", dir)
		}
		SaveCuratorState(ctx, st2)
		curatorRecordRunMetric(ctx, trigger, opts.DryRun, counts, beforeReport, afterReport, meta, h)
		curatorNotify(opts.OnSummary, "curator: "+final)
	}
	if opts.Synchronous {
		llmPass(ctx)
	} else {
		curatorReviewThreads.Add(1)
		go func() {
			defer curatorReviewThreads.Done()
			llmPass(context.WithoutCancel(ctx))
		}()
	}
	return curatorEntry("started_at", pydatetime.ISOFormatUTC(start), "auto_transitions", counts, "summary_so_far", autoSummary), nil
}

// Emits the curator run metric with the change counts computed only when telemetry is on
func curatorRecordRunMetric(ctx context.Context, trigger string, dryRun bool, counts *pyjson.Object, before, after []*pyjson.Object, meta *pyjson.Object, h string) {
	whatChanged := func() map[string]int {
		beforeBy, afterBy := curatorByName(before), curatorByName(after)
		final := curatorGetOr(meta, "final", nil)
		if !pyrepr.Truthy(final) {
			final = ""
		}
		diff := curatorDiffAndClassify(curatorNames(beforeBy), curatorNames(afterBy), curatorToolCalls(meta), final)
		patched := 0
		for n := range diff.afterNames {
			if b, ok := beforeBy[n]; ok && curatorCount(afterBy[n], "patch_count") > curatorCount(b, "patch_count") {
				patched++
			}
		}
		return map[string]int{"archived": curatorCount(counts, "archived") + len(diff.removed), "created": len(diff.added),
			"merged": len(diff.consolidated), "patched": patched}
	}
	outcome := "success"
	if dryRun {
		outcome = "skipped"
	} else if pyrepr.Truthy(curatorGetOr(meta, "error", nil)) {
		outcome = "failed"
	}
	curatorRecordRun(ctx, trigger, outcome, whatChanged, h)
}

type curatorReviewBinding struct {
	provider, model  string
	apiKey, baseURL  string
	requestOverrides map[string]any
}

// Copies the resolver overrides and layers a non empty slot extra_body over theirs
func curatorMergeRequestOverrides(runtime map[string]any, slotExtra any) map[string]any {
	merged := map[string]any{}
	for k, v := range runtime {
		merged[k] = v
	}
	if extra, ok := curatorAsMap(slotExtra); ok && len(extra) > 0 {
		body := map[string]any{}
		if prev, ok := curatorAsMap(merged["extra_body"]); ok {
			for k, v := range prev {
				body[k] = v
			}
		}
		for k, v := range extra {
			body[k] = v
		}
		merged["extra_body"] = body
	}
	return merged
}

func curatorStripOrEmpty(v any) (string, error) {
	if !pyrepr.Truthy(v) {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(v))
	}
	return pyStrip(s), nil
}

// Provider and model for the review fork from auxiliary.curator, the legacy curator.auxiliary, then the main model
func curatorResolveReviewRuntime(cfg any) (curatorReviewBinding, error) {
	slot := func(provider, model string, s map[string]any) curatorReviewBinding {
		b := curatorReviewBinding{provider: provider, model: model}
		if v, ok := s["api_key"]; ok && v != nil {
			b.apiKey = pyStrip(pyStr(v))
		}
		if v, ok := s["base_url"]; ok && v != nil {
			b.baseURL = pyStrip(pyStr(v))
		}
		b.requestOverrides = curatorMergeRequestOverrides(nil, s["extra_body"])
		return b
	}
	task := curatorSubdict(cfg, "auxiliary", "curator")
	tp, err := curatorStripOrEmpty(task["provider"])
	if err != nil {
		return curatorReviewBinding{}, err
	}
	tm, err := curatorStripOrEmpty(task["model"])
	if err != nil {
		return curatorReviewBinding{}, err
	}
	if tp != "" && tp != "auto" && tm != "" {
		return slot(tp, tm, task), nil
	}
	legacy := curatorSubdict(cfg, "curator", "auxiliary")
	if pyrepr.Truthy(legacy["provider"]) && pyrepr.Truthy(legacy["model"]) {
		curatorLog().Info("curator: using deprecated curator.auxiliary.{provider,model} config — please migrate to auxiliary.curator.{provider,model}")
		return slot(pyStr(legacy["provider"]), pyStr(legacy["model"]), legacy), nil
	}
	main := curatorSubdict(cfg, "model")
	provider, model := main["provider"], main["default"]
	if !pyrepr.Truthy(provider) {
		provider = "auto"
	}
	if !pyrepr.Truthy(model) {
		if model = main["model"]; !pyrepr.Truthy(model) {
			model = ""
		}
	}
	ps, okP := provider.(string)
	ms, okM := model.(string)
	if !okP || !okM {
		return curatorReviewBinding{}, fmt.Errorf("model.provider and model.default must be strings")
	}
	return curatorReviewBinding{provider: ps, model: ms, requestOverrides: map[string]any{}}, nil
}

// Runtime of the review fork, model, provider and request overrides, logging and falling back to the main model on failure
func curatorResolveReviewProvider(ctx context.Context) (*provider.Runtime, string, string, map[string]any) {
	h := home.Dir(ctx)
	overrides, prov, model := map[string]any{}, "", ""
	bindingProvider := "None"
	cfg, err := config.LoadReadonly(ctx, h, configLookup(&tools.Env{Home: h}))
	var rt *provider.Runtime
	if err == nil {
		var b curatorReviewBinding
		if b, err = curatorResolveReviewRuntime(map[string]any(cfg)); err == nil {
			bindingProvider, model = b.provider, b.model
			if rt, err = curatorResolveRuntime(ctx, h, b); err == nil {
				prov = rt.Provider
				if prov == "" {
					prov = b.provider
				}
				overrides = curatorMergeRequestOverrides(rt.RequestOverrides, b.requestOverrides["extra_body"])
				if m := pyStrip(rt.Model); m != "" {
					model = m
				}
			} else {
				rt = nil
			}
		}
	}
	if err != nil {
		curatorLog().Warn(fmt.Sprintf("curator: auxiliary.curator.provider '%s' (model '%s') could not be resolved: %v — the review runs on the main model instead", bindingProvider, model, err))
	}
	return rt, model, prov, overrides
}

// Forks the review agent on the prompt and collects its final response and tool calls
func curatorRunLLMReview(ctx context.Context, prompt string) *pyjson.Object {
	meta := curatorLLMMeta("", nil)
	if CuratorHooks.RunReview == nil {
		msg := "AIAgent import failed: No module named 'run_agent'"
		meta.Set("error", msg)
		meta.Set("summary", msg)
		return meta
	}
	rt, model, prov, overrides := curatorResolveReviewProvider(ctx)
	meta.Set("model", model)
	meta.Set("provider", prov)
	h := home.Dir(ctx)
	cfg, err := config.LoadReadonly(ctx, h, configLookup(&tools.Env{Home: h}))
	var res *CuratorReviewResult
	if err == nil {
		req := CuratorReviewRequest{
			Prompt: prompt, Model: model, Provider: prov, RequestOverrides: overrides,
			ReasoningConfig: config.ResolveReasoningConfig(map[string]any(cfg), model), EnabledToolsets: []string{"skills"},
			MaxIterations: 9999, Platform: "curator", WriteOrigin: backgroundReviewOrigin, QuietMode: true, SkipContextFiles: true, SkipMemory: true,
		}
		if rt != nil {
			route := rt.Route()
			req.Route = &route
			req.CredentialPool = rt.CredentialPool
			if rt.Command != "" {
				req.ACPCommand, req.ACPArgs = rt.Command, append([]string{}, rt.Args...)
			}
		}
		res, err = CuratorHooks.RunReview(ctx, req)
	}
	if err != nil {
		msg := "error: " + err.Error()
		meta.Set("error", msg)
		meta.Set("summary", msg)
		return meta
	}
	final := ""
	if curatorIsDict(res.Conversation) {
		if v := curatorGetOr(res.Conversation, "final_response", nil); pyrepr.Truthy(v) {
			final = pyStrip(pyStr(v))
		}
	}
	meta.Set("final", final)
	switch {
	case utf8.RuneCountInString(final) > 240:
		meta.Set("summary", curatorRunes(final, 240)+"…")
	case final == "":
		meta.Set("summary", "no change")
	default:
		meta.Set("summary", final)
	}
	calls := []any{}
	for _, msg := range res.Messages {
		if !curatorIsDict(msg) {
			continue
		}
		tcs, _ := curatorGetOr(msg, "tool_calls", nil).([]any)
		for _, tc := range tcs {
			if !curatorIsDict(tc) {
				continue
			}
			fn := curatorGetOr(tc, "function", nil)
			if !pyrepr.Truthy(fn) {
				fn = map[string]any{}
			}
			name := curatorGetOr(fn, "name", nil)
			if !pyrepr.Truthy(name) {
				name = ""
			}
			args := curatorGetOr(fn, "arguments", nil)
			if !pyrepr.Truthy(args) {
				args = ""
			}
			if s, ok := args.(string); ok && utf8.RuneCountInString(s) > 400 {
				args = curatorRunes(s, 400) + "…"
			}
			calls = append(calls, curatorEntry("name", name, "arguments", args))
		}
	}
	meta.Set("tool_calls", calls)
	return meta
}

func curatorRunClaimPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "skills", ".locks", "curator-run")
}

// Takes the per home automatic pass claim, replacing a claim older than an hour
func curatorClaimRun(ctx context.Context) bool {
	lock := curatorRunClaimPath(ctx)
	if err := os.MkdirAll(filepath.Dir(lock), 0o777); err != nil {
		return true
	}
	fh, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o777)
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return false
		}
		if info, serr := os.Stat(lock); serr == nil && float64(time.Now().UnixNano()-info.ModTime().UnixNano())/1e9 > curatorClaimStaleSeconds {
			if os.Remove(lock) == nil {
				return curatorClaimRun(ctx)
			}
		}
		return false
	}
	defer fh.Close()
	fmt.Fprintf(fh, "%d", os.Getpid())
	return true
}

func curatorReleaseRunClaim(ctx context.Context) { _ = os.Remove(curatorRunClaimPath(ctx)) }

// Starts a scheduled pass when every gate passes, nil when it did not run or failed
func MaybeRunCurator(ctx context.Context, idleForSeconds *float64, onSummary func(string)) *pyjson.Object {
	run, err := CuratorShouldRunNow(ctx, time.Time{})
	if err != nil {
		curatorLog().Debug(fmt.Sprintf("maybe_run_curator failed: %v", err))
		return nil
	}
	if !run || (idleForSeconds != nil && *idleForSeconds < CuratorMinIdleHours(ctx)*3600.0) {
		return nil
	}
	if !curatorClaimRun(ctx) {
		return nil
	}
	defer curatorReleaseRunClaim(ctx)
	res, err := RunCuratorReview(ctx, CuratorRunOptions{OnSummary: onSummary, Trigger: "scheduled"})
	if err != nil {
		curatorLog().Debug(fmt.Sprintf("maybe_run_curator failed: %v", err))
		return nil
	}
	return res
}

func registerCurator() {}

// Runtime the review fork runs on, the curator binding standing in as the explicit request, replaced by tests
var curatorResolveRuntime = func(ctx context.Context, h string, b curatorReviewBinding) (*provider.Runtime, error) {
	return provider.ResolveRuntimeProvider(ctx, h, b.provider, b.model, b.baseURL, b.apiKey)
}

// Python re.search of the needle between word boundaries
func curatorWordMatch(needle, hay string) bool {
	re, err := pyre.Compile(`\b` + fnmatch.ReEscape(needle) + `\b`)
	if err != nil {
		return false
	}
	ok, err := re.MatchString(hay)
	return err == nil && ok
}

var curatorFenceRE = sync.OnceValue(func() *regexp2.Regexp {
	re, err := pyre.Compile("(?si)```ya?ml\\s*\\n(.*?)\\n```")
	if err != nil {
		panic(err)
	}
	return re
})

// Body of the first fenced yaml block in the text
func curatorYAMLFence(text string) (string, bool) {
	m, err := curatorFenceRE().FindStringMatch(text)
	if err != nil || m == nil {
		return "", false
	}
	return m.GroupByNumber(1).String(), true
}

// Review fork entry the consolidation pass calls and the run metric sink, replaced by tests
var (
	curatorLLMReview = curatorRunLLMReview
	curatorRecordRun = observability.RecordCuratorRun
)
