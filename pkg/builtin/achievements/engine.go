package achievements

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	pluginName              = "hermes-achievements"
	snapshotTTLSeconds      = 120
	snapshotFile            = "scan_snapshot.json"
	checkpointFile          = "scan_checkpoint.json"
	checkpointSchemaVersion = 2
	defaultProgressEvery    = 250
)

var sessionMaxMetrics = [][2]string{
	{"max_tool_calls_in_session", "tool_call_count"},
	{"max_distinct_tools_in_session", "distinct_tool_count"},
	{"max_messages_in_session", "message_count"},
	{"max_terminal_calls_in_session", "terminal_calls"},
	{"max_file_tool_calls_in_session", "file_tool_calls"},
	{"max_web_calls_in_session", "web_calls"},
	{"max_web_browser_calls_in_session", "web_browser_calls"},
	{"max_files_touched_in_session", "files_touched_count"},
}

var sessionSumMetrics = [][2]string{
	{"total_errors", "error_count"},
	{"total_tool_calls", "tool_call_count"},
	{"total_terminal_calls", "terminal_calls"},
	{"total_web_calls", "web_calls"},
	{"total_web_extract_calls", "web_extract_calls"},
	{"total_patch_calls", "patch_calls"},
	{"total_file_reads_searches", "file_reads_searches"},
	{"total_delegate_calls", "delegate_calls"},
	{"total_process_calls", "process_calls"},
	{"total_cron_calls", "cron_calls"},
	{"browser_calls", "browser_calls"},
	{"image_vision_calls", "image_vision_calls"},
	{"tts_calls", "tts_calls"},
}

var sessionEventKeys = []string{
	"traceback_events", "log_read_events", "port_conflict_events", "permission_denied_events", "install_error_events", "install_success_events", "restart_after_error_events", "env_var_error_events", "yaml_error_events", "docker_conflict_events", "frontend_activity_events", "css_activity_events", "git_events", "tiny_patch_after_errors_events", "skill_events", "skill_manage_events", "memory_events", "memory_write_events", "context_events", "gateway_events", "plugin_events", "rollback_events", "docs_activity_events", "model_events", "openrouter_events", "codex_events", "claude_events", "gemini_events", "local_model_events", "toolset_events", "config_events", "git_history_events", "test_events", "screenshot_events", "release_events", "cache_events",
}

type sessionStore interface {
	ListSessionsRich(ctx context.Context, o state.ListSessionsRichOptions) ([]*pyjson.Object, error)
	GetMessages(ctx context.Context, sessionID string, o state.GetMessagesOptions) ([]*pyjson.Object, error)
	Close() error
}

type progressFunc func(partial []*pyjson.Object, scanned, total int)

type engine struct {
	ctx           context.Context
	catalog       []*pyjson.Object
	now           func() time.Time
	openDB        func(ctx context.Context) (sessionStore, error)
	dataDir       func() (string, error)
	runScan       func(publishPartial bool)
	compute       func(scan *pyjson.Object, isPartial bool) (*pyjson.Object, error)
	progressEvery int

	scanMu  sync.Mutex
	mu      sync.Mutex
	cache   *pyjson.Object
	cacheAt int64
	status  *pyjson.Object

	bgMu   sync.Mutex
	bgDone chan struct{}
}

// Engine bound to the profile home of ctx with a clean cache and idle scan status
func newEngine(ctx context.Context) *engine {
	e := &engine{ctx: ctx, catalog: catalog(), now: time.Now, openDB: openReadOnlySessionDB, progressEvery: defaultProgressEvery}
	e.status = obj("state", "idle", "started_at", nil, "finished_at", nil, "last_error", nil, "last_duration_ms", nil, "run_count", int64(0))
	e.dataDir = e.defaultDataDir
	e.runScan = e.runScanAndUpdateCache
	e.compute = e.computeFromScan
	return e
}

// Session store of the active home opened read only, never as a second writer
func openReadOnlySessionDB(ctx context.Context) (sessionStore, error) {
	db, err := state.NewSessionDB(ctx, home.StatePath(home.Dir(ctx)), true)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// Durable data root under plugin-data, created locally when plugin storage refuses the name
func (e *engine) defaultDataDir() (string, error) {
	if dir, err := plugins.PluginStorageDir(e.ctx, pluginName); err == nil {
		return dir, nil
	}
	root := filepath.Join(home.Dir(e.ctx), "plugin-data", pluginName)
	return root, os.MkdirAll(root, 0o777)
}

// Data file path, migrating a legacy install tree copy on first use
func (e *engine) dataFile(name string) (string, error) {
	dir, err := e.dataDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		legacy := filepath.Join(home.Dir(e.ctx), "plugins", pluginName, name)
		if _, err := os.Stat(legacy); err == nil {
			if text, err := readText(legacy); err == nil {
				_ = writeText(path, text)
			}
		}
	}
	return path, nil
}

// Parsed data file, nil when missing or unreadable
func (e *engine) readJSON(name string) (any, error) {
	path, err := e.dataFile(name)
	if err != nil {
		return nil, err
	}
	text, err := readText(path)
	if err != nil {
		return nil, nil
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil
	}
	return v, nil
}

// Writes a data file as sorted indented JSON with sets rendered as lists
func (e *engine) writeJSON(name string, data any) error {
	path, err := e.dataFile(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	text, err := pyjson.Dumps(jsonSafe(data), pyjson.Indent(2), pyjson.SortKeys(true))
	if err != nil {
		return err
	}
	return writeText(path, text)
}

// Persisted unlock state, an empty unlock map when absent
func (e *engine) loadState() (any, error) {
	data, err := e.readJSON("state.json")
	if err != nil || data != nil {
		return data, err
	}
	return obj("unlocks", pyjson.NewObject()), nil
}

// Persists the unlock state
func (e *engine) saveState(st any) error { return e.writeJSON("state.json", st) }

// Scan checkpoint with schema, timestamp and session defaults, a fresh schema 2 checkpoint when unusable
func (e *engine) loadCheckpoint() (*pyjson.Object, error) {
	data, err := e.readJSON(checkpointFile)
	if err != nil {
		return nil, err
	}
	if o, ok := data.(*pyjson.Object); ok {
		for _, kv := range []struct {
			k string
			v any
		}{{"schema_version", int64(1)}, {"generated_at", int64(0)}, {"sessions", pyjson.NewObject()}} {
			if _, ok := o.Get(kv.k); !ok {
				o.Set(kv.k, kv.v)
			}
		}
		if _, ok := get(o, "sessions").(*pyjson.Object); ok {
			return o, nil
		}
	}
	return obj("schema_version", int64(checkpointSchemaVersion), "generated_at", int64(0), "sessions", pyjson.NewObject()), nil
}

// Fingerprint deciding whether a checkpointed session needs a rescan
func sessionFingerprint(meta *pyjson.Object) *pyjson.Object {
	return obj("last_active", get(meta, "last_active"), "started_at", get(meta, "started_at"), "model", get(meta, "model"),
		"title", or(get(meta, "title"), get(meta, "preview"), "Untitled"))
}

// Whether the in memory snapshot is inside its TTL, called with mu held
func (e *engine) cacheFreshLocked(now int64) bool {
	return e.cache != nil && now-e.cacheAt <= snapshotTTLSeconds
}

// Whether a snapshot is missing a timestamp or older than the TTL
func isSnapshotStale(snapshot any, now int64) (bool, error) {
	var ts int64
	if o, ok := snapshot.(*pyjson.Object); ok {
		n, err := intOr0(get(o, "generated_at"))
		if err != nil {
			return false, err
		}
		ts = n
	}
	return ts <= 0 || now-ts > snapshotTTLSeconds, nil
}

// Scan status merged with the snapshot age and staleness
func (e *engine) scanStatusPayload(now int64) (*pyjson.Object, error) {
	if now == 0 {
		now = e.now().Unix()
	}
	e.mu.Lock()
	snap := e.cache
	out := e.status.Copy()
	e.mu.Unlock()
	var generated int64
	if snap != nil {
		n, err := intOr0(get(snap, "generated_at"))
		if err != nil {
			return nil, err
		}
		generated = n
	}
	var genAt, age any
	if generated != 0 {
		genAt, age = generated, now-generated
	}
	var snapArg any
	if snap != nil {
		snapArg = snap
	}
	stale, err := isSnapshotStale(snapArg, now)
	if err != nil {
		return nil, err
	}
	out.Set("ttl_seconds", int64(snapshotTTLSeconds))
	out.Set("snapshot_generated_at", genAt)
	out.Set("snapshot_age_seconds", age)
	out.Set("snapshot_stale", stale)
	return out, nil
}

// Scan metadata with the progress counters when a scan reports them
func scanMeta(mode string, total, rescanned, reused int, scannedSoFar, expectedTotal *int) *pyjson.Object {
	meta := obj("mode", mode, "sessions_total", int64(total), "sessions_rescanned", int64(rescanned), "sessions_reused", int64(reused))
	if scannedSoFar != nil {
		meta.Set("sessions_scanned_so_far", int64(*scannedSoFar))
		if expectedTotal != nil {
			meta.Set("sessions_expected_total", int64(*expectedTotal))
		} else {
			meta.Set("sessions_expected_total", nil)
		}
	}
	return meta
}

// Per session stats of the whole history, reusing checkpointed stats whose fingerprint still matches
func (e *engine) scanSessions(limit int, progress progressFunc, progressEvery int) (out *pyjson.Object, err error) {
	db, err := e.openDB(e.ctx)
	if err != nil {
		return obj("sessions", []any{}, "aggregate", pyjson.NewObject(), "error", fmt.Sprintf("Could not open SessionDB: %v", err), "scan_meta", scanMeta("failed", 0, 0, 0, nil, nil)), nil
	}
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			out, err = nil, cerr
		}
	}()
	previous, err := e.loadCheckpoint()
	if err != nil {
		return nil, err
	}
	previousSessions := get(previous, "sessions").(*pyjson.Object)
	version, err := intOr0(get(previous, "schema_version"))
	if err != nil {
		return nil, err
	}
	current := version == checkpointSchemaVersion
	reused, rescanned := 0, 0
	dbLimit := -1
	if limit > 0 {
		dbLimit = limit
	}
	noTips := false
	metas, err := db.ListSessionsRich(e.ctx, state.ListSessionsRichOptions{Limit: &dbLimit, IncludeChildren: true, ProjectCompressionTips: &noTips})
	if err != nil {
		return nil, err
	}
	total := len(metas)
	var sessions []*pyjson.Object
	checkpointSessions := pyjson.NewObject()
	for i, meta := range metas {
		idx := i + 1
		sidAny := get(meta, "id")
		if !truthy(sidAny) {
			continue
		}
		sid := pyrepr.Str(sidAny)
		fp := sessionFingerprint(meta)
		cached, _ := get(previousSessions, sid).(*pyjson.Object)
		if cached == nil {
			cached = pyjson.NewObject()
		}
		title := or(get(meta, "title"), get(meta, "preview"))
		var stats *pyjson.Object
		if cs, ok := get(cached, "stats").(*pyjson.Object); ok && current && pyjson.Equal(get(cached, "fingerprint"), fp) {
			stats = cs.Copy()
			reused++
		} else {
			msgs, err := db.GetMessages(e.ctx, sid, state.GetMessagesOptions{IncludeCompacted: true})
			if err != nil {
				return nil, err
			}
			t, _ := or(title, "Untitled").(string)
			if stats, err = analyzeMessages(sid, t, msgs); err != nil {
				return nil, err
			}
			rescanned++
		}
		stats.Set("session_id", sid)
		stats.Set("title", or(title, get(stats, "title"), "Untitled"))
		stats.Set("started_at", get(meta, "started_at"))
		stats.Set("last_active", get(meta, "last_active"))
		stats.Set("source", get(meta, "source"))
		if m := get(meta, "model"); truthy(m) {
			model := pyrepr.Str(m)
			names, present := stats.Get("model_names")
			if !present {
				names = stringSet{}
				stats.Set("model_names", names)
			}
			switch t := names.(type) {
			case stringSet:
				t[model] = struct{}{}
			case []any:
				found := false
				for _, x := range t {
					if x == model {
						found = true
						break
					}
				}
				if !found {
					stats.Set("model_names", append(t, model))
				}
			default:
				stats.Set("model_names", newSet(model))
			}
		}
		sessions = append(sessions, stats)
		checkpointSessions.Set(sid, obj("fingerprint", fp, "stats", jsonSafe(stats)))
		if progress != nil && progressEvery > 0 && idx%progressEvery == 0 && idx < total {
			progress(append([]*pyjson.Object(nil), sessions...), idx, total)
		}
	}
	if err := e.writeJSON(checkpointFile, obj("schema_version", int64(checkpointSchemaVersion), "generated_at", e.now().Unix(), "sessions", checkpointSessions)); err != nil {
		return nil, err
	}
	agg, err := aggregateStats(sessions)
	if err != nil {
		return nil, err
	}
	mode := "full"
	if reused > 0 {
		mode = "incremental"
	}
	n := len(sessions)
	return obj("sessions", toAny(sessions), "aggregate", agg, "scan_meta", scanMeta(mode, n, rescanned, reused, &n, &total)), nil
}

// Session list as a generic list
func toAny(sessions []*pyjson.Object) []any {
	out := make([]any, len(sessions))
	for i, s := range sessions {
		out[i] = s
	}
	return out
}

// Session dicts of a scan session list
func sessionsOf(v any) []*pyjson.Object {
	var out []*pyjson.Object
	items, _ := v.([]any)
	for _, it := range items {
		if o, ok := it.(*pyjson.Object); ok {
			out = append(out, o)
		}
	}
	return out
}

// Lifetime sums, per session bests, model and provider counts and weekend and night session counts
func aggregateStats(sessions []*pyjson.Object) (*pyjson.Object, error) {
	agg := obj("session_count", int64(len(sessions)))
	keys := []string{}
	for _, kv := range sessionMaxMetrics {
		keys = append(keys, kv[0])
	}
	for _, kv := range sessionSumMetrics {
		keys = append(keys, kv[0])
	}
	keys = append(keys, "distinct_model_count", "distinct_provider_count", "local_model_chat_sessions", "weekend_sessions", "night_sessions")
	keys = append(keys, sessionEventKeys...)
	for _, k := range keys {
		agg.Set(k, int64(0))
	}
	add := func(key string, delta int64) { agg.Set(key, agg.Vals[key].(int64)+delta) }
	models, providers := stringSet{}, stringSet{}
	for _, s := range sessions {
		for _, kv := range sessionMaxMetrics {
			v, err := intOf(getOr(s, kv[1], int64(0)))
			if err != nil {
				return nil, err
			}
			agg.Set(kv[0], max(agg.Vals[kv[0]].(int64), v))
		}
		for _, kv := range sessionSumMetrics {
			v, err := intOf(getOr(s, kv[1], int64(0)))
			if err != nil {
				return nil, err
			}
			add(kv[0], v)
		}
		for _, k := range sessionEventKeys {
			v, err := intOf(getOr(s, k, int64(0)))
			if err != nil {
				return nil, err
			}
			add(k, v)
		}
		raw := get(s, "model_names")
		var sessionModels []any
		if truthy(raw) {
			items, err := iterate(raw)
			if err != nil {
				return nil, err
			}
			sessionModels = items
		}
		local := false
		for _, m := range sessionModels {
			name := pyrepr.Str(m)
			models[name] = struct{}{}
			if p, ok := modelProvider(name).(string); ok && p != "" {
				providers[p] = struct{}{}
			}
			local = local || isLocalModelName(name)
		}
		if local {
			add("local_model_chat_sessions", 1)
		}
		if started := get(s, "started_at"); truthy(started) {
			if f, err := pyint.Float(started); err == nil && !math.IsNaN(f) && math.Abs(f) < 1e17 {
				sec := math.Floor(f)
				lt := time.Unix(int64(sec), int64((f-sec)*1e9)).Local()
				if wd := lt.Weekday(); wd == time.Saturday || wd == time.Sunday {
					add("weekend_sessions", 1)
				}
				if lt.Hour() < 6 || lt.Hour() >= 23 {
					add("night_sessions", 1)
				}
			}
		}
	}
	distinct := int64(0)
	for m := range models {
		if m != "" && m != "None" {
			distinct++
		}
	}
	agg.Set("distinct_model_count", distinct)
	agg.Set("distinct_provider_count", int64(len(providers)))
	return agg, nil
}

// Best session evidence for a per session metric definition, nil for lifetime and multi condition badges
func evidenceFor(def *pyjson.Object, sessions []*pyjson.Object) (any, error) {
	metric, _ := get(def, "threshold_metric").(string)
	key := ""
	for _, kv := range sessionMaxMetrics {
		if kv[0] == metric {
			key = kv[1]
		}
	}
	if len(sessions) == 0 || key == "" {
		return nil, nil
	}
	best := sessions[0]
	bestV, err := pyint.Float(getOr(best, key, int64(0)))
	if err != nil {
		return nil, err
	}
	for _, s := range sessions[1:] {
		v, err := pyint.Float(getOr(s, key, int64(0)))
		if err != nil {
			return nil, err
		}
		if v > bestV {
			best, bestV = s, v
		}
	}
	return obj("session_id", get(best, "session_id"), "title", get(best, "title"), "value", getOr(best, key, int64(0))), nil
}

// Wire payload shared by finished, partial and pending snapshots
func snapshot(evaluated []*pyjson.Object, scan *pyjson.Object, now int64) *pyjson.Object {
	var unlocked, discovered, secret int64
	for _, a := range evaluated {
		if truthy(get(a, "unlocked")) {
			unlocked++
		}
		switch get(a, "state") {
		case "discovered":
			discovered++
		case "secret":
			secret++
		}
	}
	return obj("achievements", toAny(evaluated), "sessions", getOr(scan, "sessions", []any{}), "aggregate", getOr(scan, "aggregate", pyjson.NewObject()),
		"scan_meta", getOr(scan, "scan_meta", pyjson.NewObject()), "error", get(scan, "error"), "unlocked_count", unlocked,
		"discovered_count", discovered, "secret_count", secret, "total_count", int64(len(evaluated)), "generated_at", now)
}

// Evaluates every definition against a scan with sticky persisted unlocks, recording new unlocks unless partial
func (e *engine) computeFromScan(scan *pyjson.Object, isPartial bool) (*pyjson.Object, error) {
	aggregate, _ := getOr(scan, "aggregate", pyjson.NewObject()).(*pyjson.Object)
	stAny, err := e.loadState()
	if err != nil {
		return nil, err
	}
	st, ok := stAny.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'setdefault'", pyrepr.TypeName(stAny))
	}
	if _, ok := st.Get("unlocks"); !ok {
		st.Set("unlocks", pyjson.NewObject())
	}
	unlocks, ok := get(st, "unlocks").(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object does not support item assignment", pyrepr.TypeName(get(st, "unlocks")))
	}
	sessions := sessionsOf(getOr(scan, "sessions", []any{}))
	now := e.now().Unix()
	evaluated := make([]*pyjson.Object, 0, len(e.catalog))
	for _, def := range e.catalog {
		res, err := evaluateDefinition(def, aggregate)
		if err != nil {
			return nil, err
		}
		id := pyrepr.Str(get(def, "id"))
		_, have := unlocks.Get(id)
		if !isPartial && truthy(get(res, "unlocked")) && !have {
			ev, err := evidenceFor(def, sessions)
			if err != nil {
				return nil, err
			}
			unlocks.Set(id, obj("unlocked_at", now, "first_tier", get(res, "tier"), "evidence", ev))
			have = true
		}
		rec, _ := get(unlocks, id).(*pyjson.Object)
		if have && !truthy(get(res, "unlocked")) {
			res.Set("unlocked", true)
			res.Set("discovered", true)
			res.Set("state", "unlocked")
			res.Set("tier", or(get(res, "tier"), get(rec, "first_tier")))
		}
		item := merge(def, res)
		if truthy(get(res, "unlocked")) {
			item.Set("unlocked_at", get(rec, "unlocked_at"))
			ev := get(rec, "evidence")
			if !truthy(ev) {
				if ev, err = evidenceFor(def, sessions); err != nil {
					return nil, err
				}
			}
			item.Set("evidence", ev)
		}
		shown, err := displayAchievement(item)
		if err != nil {
			return nil, err
		}
		evaluated = append(evaluated, shown)
	}
	if !isPartial {
		if err := e.saveState(st); err != nil {
			return nil, err
		}
	}
	return snapshot(evaluated, scan, now), nil
}

// Full history scan evaluated and persisted as a finished snapshot
func (e *engine) computeAll(progress progressFunc, progressEvery int) (*pyjson.Object, error) {
	scan, err := e.scanSessions(0, progress, progressEvery)
	if err != nil {
		return nil, err
	}
	return e.compute(scan, false)
}

// Complete pending payload served while the first scan runs
func (e *engine) buildPendingSnapshot(now int64) (*pyjson.Object, error) {
	evaluated := make([]*pyjson.Object, 0, len(e.catalog))
	for _, d := range e.catalog {
		first := pyjson.NewObject()
		if tiers, ok := get(d, "tiers").([]any); ok && len(tiers) > 0 {
			first, _ = tiers[0].(*pyjson.Object)
		}
		st := "discovered"
		if truthy(get(d, "secret")) {
			st = "secret"
		}
		item := merge(d, obj("unlocked", false, "discovered", false, "state", st, "progress", int64(0), "progress_pct", int64(0),
			"next_tier", get(first, "name"), "next_threshold", getOr(first, "threshold", int64(1)), "tier", nil))
		shown, err := displayAchievement(item)
		if err != nil {
			return nil, err
		}
		evaluated = append(evaluated, shown)
	}
	return snapshot(evaluated, obj("scan_meta", scanMeta("pending", 0, 0, 0, nil, nil), "error", nil), now), nil
}

// Stores a JSON safe copy of a snapshot as the in memory cache stamped at at
func (e *engine) setCache(snap *pyjson.Object, at int64) {
	safe := jsonSafe(snap).(*pyjson.Object)
	e.mu.Lock()
	e.cache, e.cacheAt = safe, at
	e.mu.Unlock()
}

// Runs one scan under the scan lock, publishing partial snapshots when asked and recording the scan status
func (e *engine) runScanAndUpdateCache(publishPartial bool) {
	e.scanMu.Lock()
	defer e.scanMu.Unlock()
	started := e.now().Unix()
	e.mu.Lock()
	e.status.Set("state", "running")
	e.status.Set("started_at", started)
	e.status.Set("last_error", nil)
	e.mu.Unlock()
	var publish progressFunc
	if publishPartial {
		publish = func(partial []*pyjson.Object, scanned, total int) {
			agg, err := aggregateStats(partial)
			if err != nil {
				return
			}
			partialScan := obj("sessions", toAny(partial), "aggregate", agg, "scan_meta", scanMeta("in_progress", scanned, 0, 0, &scanned, &total))
			snap, err := e.compute(partialScan, true)
			if err != nil {
				return
			}
			e.setCache(snap, 0)
		}
	}
	err := func() error {
		computed, err := e.computeAll(publish, e.progressEvery)
		if err != nil {
			return err
		}
		safe := jsonSafe(computed).(*pyjson.Object)
		at, err := intOf(or(get(safe, "generated_at"), e.now().Unix()))
		if err != nil {
			return err
		}
		e.setCache(safe, at)
		e.mu.Lock()
		cached := e.cache
		e.mu.Unlock()
		return e.writeJSON(snapshotFile, cached)
	}()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		e.status.Set("state", "idle")
	} else {
		e.status.Set("state", "failed")
		e.status.Set("last_error", err.Error())
	}
	finished := e.now().Unix()
	runs, _ := intOf(getOr(e.status, "run_count", int64(0)))
	e.status.Set("finished_at", finished)
	e.status.Set("last_duration_ms", (finished-started)*1000)
	e.status.Set("run_count", runs+1)
}

// Starts a background scan unless one is already running
func (e *engine) startBackgroundScan() {
	e.bgMu.Lock()
	defer e.bgMu.Unlock()
	if e.bgDone != nil {
		select {
		case <-e.bgDone:
		default:
			return
		}
	}
	done := make(chan struct{})
	e.bgDone = done
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Exception in thread hermes-achievements-scan", "error", r)
			}
		}()
		e.runScan(true)
	}()
}

// Current payload, a fresh cache as is, a stale or pending one while a background scan runs, a synchronous scan when forced
func (e *engine) evaluateAll(force bool) (*pyjson.Object, error) {
	now := e.now().Unix()
	e.mu.Lock()
	if !force && e.cacheFreshLocked(now) {
		c := e.cache
		e.mu.Unlock()
		if c.Len() == 0 {
			return pyjson.NewObject(), nil
		}
		return c, nil
	}
	missing := e.cache == nil
	e.mu.Unlock()
	if missing {
		persisted, err := e.readJSON(snapshotFile)
		if err != nil {
			return nil, err
		}
		if p, ok := persisted.(*pyjson.Object); ok {
			at, err := intOr0(get(p, "generated_at"))
			if err != nil {
				return nil, err
			}
			if at == 0 {
				at = now
			}
			e.mu.Lock()
			e.cache, e.cacheAt = p, at
			e.mu.Unlock()
		}
	}
	if force {
		e.runScan(false)
	} else {
		e.mu.Lock()
		fresh := e.cacheFreshLocked(now)
		e.mu.Unlock()
		if !fresh {
			e.startBackgroundScan()
		}
	}
	e.mu.Lock()
	c := e.cache
	e.mu.Unlock()
	if c != nil {
		return c, nil
	}
	return e.buildPendingSnapshot(now)
}
