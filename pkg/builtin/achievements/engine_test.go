package achievements

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

type fakeSessionDB struct {
	count        int
	lastLimit    int
	lastChildren bool
	base         int64
	listCalls    atomic.Int64
}

func (f *fakeSessionDB) ListSessionsRich(_ context.Context, o state.ListSessionsRichOptions) ([]*pyjson.Object, error) {
	f.lastLimit, f.lastChildren = *o.Limit, o.IncludeChildren
	f.listCalls.Add(1)
	n := f.count
	if *o.Limit != -1 {
		n = min(n, *o.Limit)
	}
	if f.base == 0 {
		f.base = time.Now().Unix()
	}
	now := f.base
	out := make([]*pyjson.Object, n)
	for i := range out {
		start := now - int64(f.count-i)*60
		out[i] = obj("id", fmt.Sprintf("sess-%d", i), "title", fmt.Sprintf("Session %d", i), "preview", fmt.Sprintf("preview %d", i),
			"started_at", start, "last_active", start+30, "source", "cli", "model", "test-model")
	}
	return out, nil
}

func (f *fakeSessionDB) GetMessages(_ context.Context, sid string, o state.GetMessagesOptions) ([]*pyjson.Object, error) {
	return []*pyjson.Object{
		obj("role", "user", "content", "ask "+sid),
		obj("role", "assistant", "tool_calls", []any{obj("function", obj("name", "terminal"))}),
		obj("role", "tool", "tool_name", "terminal", "content", "ok"),
	}, nil
}

func (f *fakeSessionDB) Close() error { return nil }

// Engine over a temp home whose session store is the given fake
func testEngine(t *testing.T, db sessionStore) *engine {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	e := newEngine(context.Background())
	e.openDB = func(context.Context) (sessionStore, error) { return db, nil }
	return e
}

// Waits for the running background scan to finish
func (e *engine) waitBackground(t *testing.T) {
	t.Helper()
	e.bgMu.Lock()
	done := e.bgDone
	e.bgMu.Unlock()
	if done == nil {
		t.Fatal("no background scan")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("background scan still running")
	}
}

func tiersOf(pairs ...any) []any {
	out := []any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, obj("name", pairs[i], "threshold", int64(pairs[i+1].(int))))
	}
	return out
}

func findDef(t *testing.T, id string) *pyjson.Object {
	for _, d := range catalog() {
		if get(d, "id") == id {
			return d
		}
	}
	t.Fatalf("no %s", id)
	return nil
}

func mustEval(t *testing.T) func(*pyjson.Object, error) *pyjson.Object {
	return func(r *pyjson.Object, err error) *pyjson.Object {
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
}

func TestToolCallStatsDetectToolNamesAndErrors(t *testing.T) {
	stats := mustEval(t)(analyzeMessages("s1", "Fix dev server", []*pyjson.Object{
		obj("role", "assistant", "tool_calls", []any{obj("function", obj("name", "terminal"))}),
		obj("role", "tool", "tool_name", "terminal", "content", "Error: port 3000 already in use"),
		obj("role", "assistant", "tool_calls", []any{obj("function", obj("name", "web_search"))}),
	}))
	names := get(stats, "tool_names").(stringSet)
	if get(stats, "tool_call_count") != int64(2) || len(names) != 2 || get(stats, "error_count") != int64(1) || get(stats, "port_conflict") != true {
		t.Fatal(stats.Keys, get(stats, "tool_call_count"), names, get(stats, "error_count"))
	}
	if _, ok := names["web_search"]; !ok {
		t.Fatal(names)
	}
}

func TestTieredAchievementReachesHighestMatchingTier(t *testing.T) {
	def := obj("id", "let_him_cook", "threshold_metric", "max_tool_calls_in_session", "tiers", tiersOf("Copper", 10, "Silver", 25, "Gold", 50))
	r := mustEval(t)(evaluateTiered(def, obj("max_tool_calls_in_session", int64(28))))
	if get(r, "unlocked") != true || get(r, "tier") != "Silver" || get(r, "progress") != int64(28) || get(r, "next_tier") != "Gold" {
		t.Fatal(r.Keys, r.Vals)
	}
	if get(r, "progress_pct") != int64(12) {
		t.Fatal(get(r, "progress_pct"))
	}
}

func TestTieredAchievementCanBeDiscoveredWithoutUnlocking(t *testing.T) {
	def := obj("id", "terminal_goblin", "threshold_metric", "total_terminal_calls", "tiers", tiersOf("Copper", 50))
	r := mustEval(t)(evaluateTiered(def, obj("total_terminal_calls", int64(12))))
	if get(r, "unlocked") != false || get(r, "discovered") != true || get(r, "state") != "discovered" || get(r, "progress") != int64(12) || get(r, "next_threshold") != int64(50) {
		t.Fatal(r.Vals)
	}
}

func TestFinishedRescanKeepsPersistedUnlockWhenLiveMetricShrinks(t *testing.T) {
	e := testEngine(t, &fakeSessionDB{})
	dir := t.TempDir()
	e.dataDir = func() (string, error) { return dir, nil }
	e.catalog = []*pyjson.Object{obj("id", "durable_unlock", "name", "Durable Unlock", "threshold_metric", "total_terminal_calls", "tiers", tiersOf("Copper", 40))}
	scan := func(n int) *pyjson.Object {
		return obj("aggregate", obj("total_terminal_calls", int64(n)), "sessions", []any{})
	}
	unlocked := mustEval(t)(e.computeFromScan(scan(40), false))
	rescanned := mustEval(t)(e.computeFromScan(scan(39), false))
	partial := mustEval(t)(e.computeFromScan(scan(39), true))
	st, _ := e.loadState()
	persisted := get(st.(*pyjson.Object), "unlocks").(*pyjson.Object)
	first := func(s *pyjson.Object) *pyjson.Object { return get(s, "achievements").([]any)[0].(*pyjson.Object) }
	if get(first(unlocked), "unlocked") != true || get(first(rescanned), "unlocked") != true || get(first(rescanned), "state") != "unlocked" {
		t.Fatal(first(rescanned).Vals)
	}
	if get(first(rescanned), "tier") != "Copper" || get(first(partial), "unlocked") != true || get(partial, "unlocked_count") != int64(1) {
		t.Fatal(first(partial).Vals)
	}
	if strings.Join(persisted.Keys, ",") != "durable_unlock" {
		t.Fatal(persisted.Keys)
	}
}

func TestSecretAchievementStaysHiddenWithoutProgress(t *testing.T) {
	def := obj("id", "permission_denied_any_percent", "name", "Permission Denied Any%", "secret", true,
		"requirements", []any{obj("metric", "permission_denied_events", "gte", int64(3))})
	r := mustEval(t)(evaluateRequirements(def, obj("permission_denied_events", int64(0))))
	d := mustEval(t)(displayAchievement(merge(def, r)))
	if get(r, "state") != "secret" || get(d, "name") != "???" || strings.Contains(get(d, "description").(string), "Permission") {
		t.Fatal(d.Vals)
	}
	if get(d, "icon") != "secret" || !strings.HasPrefix(get(d, "criteria").(string), "Secret: exact requirement hidden") {
		t.Fatal(d.Vals)
	}
}

func TestMultiConditionUnlockRequiresAllRequirements(t *testing.T) {
	def := obj("id", "full_send", "requirements", []any{
		obj("metric", "max_terminal_calls_in_session", "gte", int64(10)),
		obj("metric", "max_file_tool_calls_in_session", "gte", int64(5)),
		obj("metric", "max_web_calls_in_session", "gte", int64(2)),
	})
	partial := mustEval(t)(evaluateRequirements(def, obj("max_terminal_calls_in_session", int64(12), "max_file_tool_calls_in_session", int64(2), "max_web_calls_in_session", int64(0))))
	complete := mustEval(t)(evaluateRequirements(def, obj("max_terminal_calls_in_session", int64(12), "max_file_tool_calls_in_session", int64(6), "max_web_calls_in_session", int64(2))))
	if get(partial, "state") != "discovered" || get(partial, "unlocked") != false || get(partial, "progress_pct").(int64) >= 100 {
		t.Fatal(partial.Vals)
	}
	if get(partial, "progress") != int64(46) || get(complete, "state") != "unlocked" || get(complete, "unlocked") != true || get(complete, "progress_pct") != int64(100) {
		t.Fatal(partial.Vals, complete.Vals)
	}
	crit := mustCriteria(t, def)
	if crit != "Requirement: terminal calls in one session \u2265 10; file/search/patch calls in one session \u2265 5; max web calls in session \u2265 2." {
		t.Fatal(crit)
	}
}

func mustCriteria(t *testing.T, def *pyjson.Object) string {
	t.Helper()
	s, err := criteriaFor(def)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCatalogHas60PlusUniqueAchievements(t *testing.T) {
	seen := map[any]bool{}
	for _, d := range catalog() {
		if seen[get(d, "id")] {
			t.Fatal("duplicate", get(d, "id"))
		}
		seen[get(d, "id")] = true
	}
	if len(seen) < 60 {
		t.Fatal(len(seen))
	}
	cook := findDef(t, "let_him_cook")
	if get(cook, "kind") != "best_session" || mustCriteria(t, cook) != "Requirement: tool calls in one session. Tier ladder: Copper 200, Silver 500, Gold 1200, Diamond 3000, Olympian 8000." {
		t.Fatal(mustCriteria(t, cook))
	}
	if get(findDef(t, "autonomous_avalanche"), "kind") != "lifetime" || get(findDef(t, "full_send"), "kind") != "multi_condition" {
		t.Fatal("kinds")
	}
}

func TestModelProviderMetricsAreAggregated(t *testing.T) {
	agg := mustEval(t)(aggregateStats([]*pyjson.Object{
		obj("model_names", newSet("openai/gpt-5", "anthropic/claude-sonnet-4")),
		obj("model_names", newSet("google/gemini-pro", "mistral/large")),
		obj("model_names", newSet("qwen/qwen3")),
	}))
	if get(agg, "distinct_model_count") != int64(5) || get(agg, "distinct_provider_count") != int64(5) {
		t.Fatal(agg.Vals)
	}
	r := mustEval(t)(evaluateDefinition(findDef(t, "five_model_flight"), agg))
	if get(r, "state") != "unlocked" || get(r, "tier") != "Copper" {
		t.Fatal(r.Vals)
	}
	if agg.Keys[0] != "session_count" || agg.Keys[1] != "max_tool_calls_in_session" || agg.Keys[len(agg.Keys)-1] != "cache_events" {
		t.Fatal(agg.Keys)
	}
}

func TestRemovedNoisyAchievementsAreNotInCatalog(t *testing.T) {
	for _, d := range catalog() {
		switch get(d, "id") {
		case "fallback_pilot", "browser_sleuth", "release_ritualist":
			t.Fatal(get(d, "id"))
		}
	}
}

func TestOpenWeightsPilgrimCountsOnlyLocalModelMetadata(t *testing.T) {
	mentions := mustEval(t)(aggregateStats([]*pyjson.Object{obj("model_names", newSet("openai/gpt-5"), "local_model_events", int64(999))}))
	local := mustEval(t)(aggregateStats([]*pyjson.Object{obj("model_names", newSet("openai/gpt-5")), obj("model_names", newSet("ollama/llama3"))}))
	def := findDef(t, "open_weights_pilgrim")
	if get(mentions, "local_model_chat_sessions") != int64(0) || get(mustEval(t)(evaluateDefinition(def, mentions)), "state") != "discovered" {
		t.Fatal(mentions.Vals)
	}
	if get(local, "local_model_chat_sessions") != int64(1) || get(mustEval(t)(evaluateDefinition(def, local)), "state") != "unlocked" {
		t.Fatal(local.Vals)
	}
}

func TestConfigSurgeonIgnoresGenericConfigMentions(t *testing.T) {
	s1 := mustEval(t)(analyzeMessages("s1", "Config talk", []*pyjson.Object{obj("content", "config config configuration not configured")}))
	s2 := mustEval(t)(analyzeMessages("s2", "Real config", []*pyjson.Object{obj("content", "edited config.yaml, manifest.json, and .env.local")}))
	if get(s1, "config_events") != int64(0) || get(s2, "config_events").(int64) < 3 {
		t.Fatal(get(s1, "config_events"), get(s2, "config_events"))
	}
}

func TestDashboardCardHoverDoesNotMoveClickTarget(t *testing.T) {
	css, err := dashboardFS.ReadFile("dashboard/dist/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(css), "\n") {
		if strings.HasPrefix(line, ".ha-card:hover") {
			if strings.Contains(line, "transform:") || !strings.Contains(line, "border-color: var(--ha-tier)") || !strings.Contains(line, "box-shadow:") {
				t.Fatal(line)
			}
			return
		}
	}
	t.Fatal("no hover rule")
}

func TestModelProviderAndLocalNames(t *testing.T) {
	cases := map[string]any{"": nil, "None": nil, "openai/gpt-5": "openai", "gemini-2.5-pro": "google", "claude-3": "claude", "my-model:7b": "my", "GPT-4o": "gpt"}
	for in, want := range cases {
		if got := modelProvider(in); got != want {
			t.Fatalf("%q -> %v want %v", in, got, want)
		}
	}
	if !isLocalModelName("ollama/llama3") || isLocalModelName("none") || isLocalModelName("openai/gpt-5") {
		t.Fatal("local markers")
	}
}

func TestScanSessionsDefaultScansAllHistoryNotFirst200(t *testing.T) {
	db := &fakeSessionDB{count: 500}
	e := testEngine(t, db)
	res, err := e.scanSessions(0, nil, defaultProgressEvery)
	if err != nil {
		t.Fatal(err)
	}
	if db.lastLimit != -1 || !db.lastChildren {
		t.Fatal(db.lastLimit, db.lastChildren)
	}
	meta := get(res, "scan_meta").(*pyjson.Object)
	if len(get(res, "sessions").([]any)) != 500 || get(meta, "sessions_total") != int64(500) || get(meta, "mode") != "full" {
		t.Fatal(meta.Vals)
	}
	again, err := e.scanSessions(0, nil, defaultProgressEvery)
	if err != nil {
		t.Fatal(err)
	}
	meta = get(again, "scan_meta").(*pyjson.Object)
	if get(meta, "mode") != "incremental" || get(meta, "sessions_reused") != int64(500) || get(get(again, "aggregate").(*pyjson.Object), "total_terminal_calls") != int64(500) {
		t.Fatal(meta.Vals)
	}
}

func TestEvaluateAllFirstRunReturnsPendingAndStartsBackgroundScan(t *testing.T) {
	e := testEngine(t, &fakeSessionDB{count: 50})
	started, release := make(chan struct{}), make(chan struct{})
	orig := e.runScan
	e.runScan = func(p bool) {
		close(started)
		<-release
		orig(p)
	}
	t0 := time.Now()
	res, err := e.evaluateAll(false)
	if err != nil || time.Since(t0) > time.Second {
		t.Fatal(err, time.Since(t0))
	}
	meta := get(res, "scan_meta").(*pyjson.Object)
	if get(meta, "mode") != "pending" || get(res, "unlocked_count") != int64(0) || get(res, "total_count").(int64) < 60 {
		t.Fatal(meta.Vals)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background scan did not start")
	}
	close(release)
	e.waitBackground(t)
	second, err := e.evaluateAll(false)
	if err != nil {
		t.Fatal(err)
	}
	meta = get(second, "scan_meta").(*pyjson.Object)
	if get(meta, "mode") == "pending" || get(meta, "sessions_total") != int64(50) {
		t.Fatal(meta.Vals)
	}
}

func TestStartBackgroundScanIsIdempotentWhileRunning(t *testing.T) {
	e := testEngine(t, &fakeSessionDB{count: 5})
	release := make(chan struct{})
	var runs atomic.Int64
	orig := e.runScan
	e.runScan = func(p bool) {
		runs.Add(1)
		<-release
		orig(p)
	}
	e.startBackgroundScan()
	e.bgMu.Lock()
	first := e.bgDone
	e.bgMu.Unlock()
	e.startBackgroundScan()
	e.startBackgroundScan()
	e.bgMu.Lock()
	same := e.bgDone == first
	e.bgMu.Unlock()
	close(release)
	e.waitBackground(t)
	if !same || runs.Load() != 1 {
		t.Fatal(same, runs.Load())
	}
}

func TestBackgroundScanPublishesPartialSnapshots(t *testing.T) {
	e := testEngine(t, &fakeSessionDB{count: 750})
	var mu sync.Mutex
	var partials []*pyjson.Object
	orig := e.compute
	e.compute = func(scan *pyjson.Object, isPartial bool) (*pyjson.Object, error) {
		r, err := orig(scan, isPartial)
		if isPartial && err == nil {
			mu.Lock()
			partials = append(partials, r)
			mu.Unlock()
		}
		return r, err
	}
	e.runScanAndUpdateCache(true)
	if len(partials) < 2 {
		t.Fatal(len(partials))
	}
	var counts []int
	for _, p := range partials {
		meta := get(p, "scan_meta").(*pyjson.Object)
		counts = append(counts, int(get(meta, "sessions_total").(int64)))
		if get(meta, "sessions_expected_total") != int64(750) {
			t.Fatal(meta.Vals)
		}
	}
	for i := 1; i < len(counts); i++ {
		if counts[i] < counts[i-1] {
			t.Fatal(counts)
		}
	}
	if counts[0] >= 750 || counts[len(counts)-1] >= 750 {
		t.Fatal(counts)
	}
	meta := get(e.cache, "scan_meta").(*pyjson.Object)
	if get(meta, "mode") == "in_progress" || get(meta, "sessions_total") != int64(750) || get(e.status, "state") != "idle" || get(e.status, "run_count") != int64(1) {
		t.Fatal(meta.Vals, e.status.Vals)
	}
}

func TestPartialSnapshotsDoNotPersistUnlockTimestamps(t *testing.T) {
	e := testEngine(t, &fakeSessionDB{count: 10})
	if err := e.saveState(obj("unlocks", pyjson.NewObject())); err != nil {
		t.Fatal(err)
	}
	scan := obj("sessions", []any{obj("session_id", "x", "tool_call_count", int64(99999), "tool_names", stringSet{})},
		"aggregate", obj("max_tool_calls_in_session", int64(99999), "total_tool_calls", int64(99999)), "scan_meta", obj("mode", "in_progress"))
	res := mustEval(t)(e.computeFromScan(scan, true))
	anyUnlocked := false
	for _, a := range get(res, "achievements").([]any) {
		anyUnlocked = anyUnlocked || get(a.(*pyjson.Object), "unlocked") == true
	}
	st, _ := e.loadState()
	if !anyUnlocked || get(st.(*pyjson.Object), "unlocks").(*pyjson.Object).Len() != 0 {
		t.Fatal(anyUnlocked, st)
	}
}

func TestScanFailureIsRecordedInScanStatus(t *testing.T) {
	e := testEngine(t, &fakeSessionDB{})
	e.openDB = func(context.Context) (sessionStore, error) { return nil, fmt.Errorf("locked") }
	res, err := e.scanSessions(0, nil, defaultProgressEvery)
	if err != nil || get(res, "error") != "Could not open SessionDB: locked" || get(get(res, "scan_meta").(*pyjson.Object), "mode") != "failed" {
		t.Fatal(err, res)
	}
	dir := t.TempDir()
	e.dataDir = func() (string, error) { return dir, nil }
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("[1]"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.runScanAndUpdateCache(false)
	if get(e.status, "state") != "failed" || get(e.status, "last_error") != "'list' object has no attribute 'setdefault'" {
		t.Fatal(e.status.Vals)
	}
}

func TestLegacyStateMigratesFromInstallTree(t *testing.T) {
	e := testEngine(t, &fakeSessionDB{})
	h := os.Getenv("ICEBATH_HOME")
	legacy := filepath.Join(h, "plugins", pluginName)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "state.json"), []byte("\xef\xbb\xbf{\"unlocks\": {\"a\": {}}}"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := e.loadState()
	if err != nil || get(get(st.(*pyjson.Object), "unlocks").(*pyjson.Object), "a") == nil {
		t.Fatal(st, err)
	}
	b, err := os.ReadFile(filepath.Join(h, "plugin-data", pluginName, "state.json"))
	if err != nil || strings.HasPrefix(string(b), "\xef\xbb\xbf") {
		t.Fatal(string(b), err)
	}
}

// Decodes a JSON response body into generic values
func decode(t *testing.T, rec *httptest.ResponseRecorder) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(rec.Body.String(), err)
	}
	return v
}

func TestRoutesServeEveryEndpoint(t *testing.T) {
	e := testEngine(t, &fakeSessionDB{count: 3})
	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		e.ServeHTTP(rec, req)
		return rec
	}
	rec := do("POST", "/rescan")
	body := decode(t, rec).(map[string]any)
	if rec.Code != 200 || body["ok"] != true || body["total_count"].(float64) < 60 || !strings.HasPrefix(rec.Body.String(), `{"ok":true,"achievements":[`) {
		t.Fatal(rec.Code, rec.Body.String()[:80])
	}
	rec = do("GET", "/achievements")
	body = decode(t, rec).(map[string]any)
	meta := body["scan_meta"].(map[string]any)
	if body["is_stale"] != false || meta["status"].(map[string]any)["run_count"] != float64(1) || meta["mode"] != "full" {
		t.Fatal(body["is_stale"], meta)
	}
	if _, ok := body["sessions"]; ok {
		t.Fatal("sessions leaked into /achievements")
	}
	rec = do("GET", "/scan-status")
	status := decode(t, rec).(map[string]any)
	if status["state"] != "idle" || status["ttl_seconds"] != float64(120) || status["snapshot_stale"] != false {
		t.Fatal(status)
	}
	if !strings.HasPrefix(rec.Body.String(), `{"state":"idle","started_at":`) {
		t.Fatal(rec.Body.String())
	}
	if rec = do("GET", "/recent-unlocks"); rec.Code != 200 || decode(t, rec) == nil {
		t.Fatal(rec.Body.String())
	}
	rec = do("GET", "/sessions/sess-1/badges")
	badges := decode(t, rec).(map[string]any)
	if badges["session_id"] != "sess-1" {
		t.Fatal(badges)
	}
	rec = do("GET", "/sessions/missing/badges")
	if rec.Body.String() != `{"session_id":"missing","badges":[]}` {
		t.Fatal(rec.Body.String())
	}
	if rec = do("GET", "/rescan"); rec.Code != 405 || rec.Header().Get("Allow") != "POST" || rec.Body.String() != `{"detail":"Method Not Allowed"}` {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec = do("GET", "/nope"); rec.Code != 404 || rec.Body.String() != `{"detail":"Not Found"}` {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec = do("GET", "/achievements/?x=1"); rec.Code != 307 || rec.Header().Get("Location") != "http://example.com/achievements?x=1" {
		t.Fatal(rec.Code, rec.Header())
	}
	if rec = do("POST", "/reset-state"); rec.Body.String() != `{"ok":true}` {
		t.Fatal(rec.Body.String())
	}
	st, _ := e.loadState()
	if get(st.(*pyjson.Object), "unlocks").(*pyjson.Object).Len() != 0 || e.cache != nil || get(e.status, "run_count") != int64(1) {
		t.Fatal(st, e.status.Vals)
	}
	dir, _ := e.dataDir()
	for _, f := range []string{snapshotFile, checkpointFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Fatal(f, err)
		}
	}
	rec = httptest.NewRecorder()
	http.StripPrefix("/api/plugins/hermes-achievements", Handler(context.Background())).ServeHTTP(rec, httptest.NewRequest("GET", "/api/plugins/hermes-achievements/scan-status", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
}

func TestStaleSnapshotIsServedWhileBackgroundRescanRuns(t *testing.T) {
	e := testEngine(t, &fakeSessionDB{count: 2})
	old := obj("achievements", []any{}, "generated_at", time.Now().Unix()-1000, "scan_meta", obj("mode", "full"))
	if err := e.writeJSON(snapshotFile, old); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	orig := e.runScan
	e.runScan = func(p bool) { <-release; orig(p) }
	res, err := e.evaluateAll(false)
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := isSnapshotStale(res, time.Now().Unix())
	if !stale || get(get(res, "scan_meta").(*pyjson.Object), "mode") != "full" {
		t.Fatal(res.Vals)
	}
	close(release)
	e.waitBackground(t)
}
