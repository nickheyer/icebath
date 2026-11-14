package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Installs the profile store doubles the session_search tests swap, a custom home and no known profiles
func installProfileStoreDoubles() {
	ProfileStoreHooks.ActiveName = func(context.Context) string { return "custom" }
	ProfileStoreHooks.Normalize = func(name string) (string, error) { return strings.ToLower(strings.TrimSpace(name)), nil }
	ProfileStoreHooks.Validate = func(string) error { return nil }
	ProfileStoreHooks.Exists = func(string) bool { return false }
	ProfileStoreHooks.Dir = func(name string) (string, error) { return "", fmt.Errorf("profile '%s' does not exist", name) }
}

// Points the profile doubles at one home like the hermes _patch_profiles monkeypatches
func patchProfileStore(t *testing.T, dir string, exists bool) {
	t.Helper()
	prevN, prevV, prevE, prevD := ProfileStoreHooks.Normalize, ProfileStoreHooks.Validate, ProfileStoreHooks.Exists, ProfileStoreHooks.Dir
	ProfileStoreHooks.Normalize = func(name string) (string, error) { return name, nil }
	ProfileStoreHooks.Validate = func(string) error { return nil }
	ProfileStoreHooks.Exists = func(string) bool { return exists }
	ProfileStoreHooks.Dir = func(string) (string, error) { return dir, nil }
	t.Cleanup(func() {
		ProfileStoreHooks.Normalize, ProfileStoreHooks.Validate, ProfileStoreHooks.Exists, ProfileStoreHooks.Dir = prevN, prevV, prevE, prevD
	})
}

func newSessionSearchDB(t *testing.T, dir string) *state.DB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(dir, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func ssCreate(t *testing.T, db *state.DB, n state.NewSession) {
	t.Helper()
	if err := db.CreateSession(n); err != nil {
		t.Fatal(err)
	}
}

func ssAppend(t *testing.T, db *state.DB, sid, role, content string) int64 {
	t.Helper()
	id, err := db.AppendMessage(context.Background(), sid, map[string]any{"role": role, "content": content}, state.AppendGuards{})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ssExec(t *testing.T, db *state.DB, query string, args ...any) {
	t.Helper()
	err := db.ExecuteWrite(context.Background(), func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(context.Background(), query, args...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func ssEnd(t *testing.T, db *state.DB, sid, reason string) {
	t.Helper()
	if err := db.EndSession(sid, reason); err != nil {
		t.Fatal(err)
	}
}

func ssCompact(t *testing.T, db *state.DB, sid string, compacted []map[string]any) {
	t.Helper()
	if _, err := db.ArchiveAndCompact(context.Background(), sid, compacted, state.ArchiveCompactOptions{}); err != nil {
		t.Fatal(err)
	}
}

func ssSearch(t *testing.T, a SessionSearchArgs) map[string]any {
	t.Helper()
	return ssDecode(t, SessionSearch(context.Background(), a))
}

func ssDecode(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	return out
}

func ssResults(result map[string]any) []map[string]any {
	rows, _ := result["results"].([]any)
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.(map[string]any))
	}
	return out
}

func ssMessages(entry map[string]any, key string) []map[string]any {
	rows, _ := entry[key].([]any)
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.(map[string]any))
	}
	return out
}

func ssSessionIDs(result map[string]any) []string {
	var out []string
	for _, r := range ssResults(result) {
		out = append(out, r["session_id"].(string))
	}
	return out
}

func ssCount(result map[string]any) int { return int(result["count"].(float64)) }

func ssContent(m map[string]any) string {
	s, _ := m["content"].(string)
	return s
}

// Three sessions about a modpack so FTS5 has hits to dedupe
func seedModpackSessions(t *testing.T, db *state.DB) {
	t.Helper()
	now := time.Now().Unix()
	ssCreate(t, db, state.NewSession{ID: "s_oldest", Source: "cli"})
	ssExec(t, db, "UPDATE sessions SET started_at = ?, title = ? WHERE id = ?", now-30000, "Building the Modpack", "s_oldest")
	ssAppend(t, db, "s_oldest", "user", "Let's build a Minecraft modpack")
	ssAppend(t, db, "s_oldest", "assistant", "Great. Let me scaffold the modpack repo.")
	ssAppend(t, db, "s_oldest", "user", "Use NeoForge 1.21.1")
	ssAppend(t, db, "s_oldest", "assistant", "Done. Modpack repo created with NeoForge 1.21.1.")
	ssAppend(t, db, "s_oldest", "assistant", "Tier-0 mods installed; modpack smoke test passes.")
	ssCreate(t, db, state.NewSession{ID: "s_middle", Source: "cli"})
	ssExec(t, db, "UPDATE sessions SET started_at = ?, title = ? WHERE id = ?", now-15000, "Modpack Quest Coverage", "s_middle")
	ssAppend(t, db, "s_middle", "user", "Deep-dive every modpack reference quest guide")
	ssAppend(t, db, "s_middle", "assistant", "Surveying ATM10 questbook for modpack inspiration.")
	ssAppend(t, db, "s_middle", "user", "Update the modpack version too")
	ssAppend(t, db, "s_middle", "assistant", "Modpack version bumped 0.4 → 0.8.5; quest coverage page added.")
	ssCreate(t, db, state.NewSession{ID: "s_newest", Source: "cli"})
	ssExec(t, db, "UPDATE sessions SET started_at = ?, title = ? WHERE id = ?", now-1000, "Modpack Mob Spawn Fix", "s_newest")
	ssAppend(t, db, "s_newest", "user", "Fix the modpack mob spawning")
	ssAppend(t, db, "s_newest", "assistant", "Investigating elite mob gating in the modpack KubeJS.")
	ssAppend(t, db, "s_newest", "assistant", "Shipped commit b850442. Modpack alternator nerfed too.")
}

// Store double whose unexpected reads fail the test, the hermes _DB classes with only one method
type sessionSearchFakeStore struct {
	t      *testing.T
	closed int
}

func (f *sessionSearchFakeStore) unexpected(name string) {
	f.t.Helper()
	f.t.Fatalf("unexpected %s call on the fake store", name)
}

func (f *sessionSearchFakeStore) GetSession(context.Context, string) (*pyjson.Object, error) {
	f.unexpected("GetSession")
	return nil, nil
}
func (f *sessionSearchFakeStore) GetMessages(context.Context, string, state.GetMessagesOptions) ([]*pyjson.Object, error) {
	f.unexpected("GetMessages")
	return nil, nil
}
func (f *sessionSearchFakeStore) SearchMessages(context.Context, string, state.SearchMessagesOptions) ([]*pyjson.Object, error) {
	f.unexpected("SearchMessages")
	return nil, nil
}
func (f *sessionSearchFakeStore) GetAnchoredView(context.Context, string, int64, state.AnchoredViewOptions) (*pyjson.Object, error) {
	f.unexpected("GetAnchoredView")
	return nil, nil
}
func (f *sessionSearchFakeStore) GetMessagesAround(context.Context, string, int64, int) (*pyjson.Object, error) {
	f.unexpected("GetMessagesAround")
	return nil, nil
}
func (f *sessionSearchFakeStore) ResolveSessionByTitle(context.Context, string) (string, bool, error) {
	f.unexpected("ResolveSessionByTitle")
	return "", false, nil
}
func (f *sessionSearchFakeStore) FTSRebuildStatus(context.Context) (*pyjson.Object, error) {
	f.unexpected("FTSRebuildStatus")
	return nil, nil
}
func (f *sessionSearchFakeStore) ReadCtx(context.Context, func(conn state.SQLiteConn) error) error {
	f.unexpected("ReadCtx")
	return nil
}
func (f *sessionSearchFakeStore) Close() error {
	f.closed++
	return nil
}

// Fake store with the bounded browse listing recording its options
type boundedFakeStore struct {
	sessionSearchFakeStore
	boundedOpts *state.RecentSessionsBoundedOptions
}

func (f *boundedFakeStore) ListRecentSessionsBounded(_ context.Context, o state.RecentSessionsBoundedOptions) ([]*pyjson.Object, error) {
	f.boundedOpts = &o
	return nil, nil
}

func TestFormatTimestampFormatsUnixAndPassesThroughTheRest(t *testing.T) {
	if got := formatTimestamp(int64(1700000000)); !strings.Contains(got, "2023") {
		t.Fatalf("formatted %q", got)
	}
	if got := formatTimestamp(nil); got != "unknown" {
		t.Fatalf("None gave %q", got)
	}
	if got := formatTimestamp("not-a-number-string"); got != "not-a-number-string" {
		t.Fatalf("passthrough gave %q", got)
	}
}

func TestBrowseShapeBrowseUsesBoundedRecentPath(t *testing.T) {
	db := &boundedFakeStore{sessionSearchFakeStore: sessionSearchFakeStore{t: t}}
	result := ssSearch(t, SessionSearchArgs{DB: db})
	if result["success"] != true {
		t.Fatalf("result %v", result)
	}
	if db.boundedOpts == nil {
		t.Fatal("bounded listing not called")
	}
}

func TestBrowseShapeBrowseFailsClosedWithoutBoundedDatabaseCapability(t *testing.T) {
	result := ssSearch(t, SessionSearchArgs{DB: &sessionSearchFakeStore{t: t}})
	if result["success"] != false {
		t.Fatalf("result %v", result)
	}
}

func TestBrowseShapeLazyDatabaseIsReleasedAfterSearch(t *testing.T) {
	db := &boundedFakeStore{sessionSearchFakeStore: sessionSearchFakeStore{t: t}}
	released := 0
	prevOpen, prevRelease := sessionSearchOpenDefaultFn, sessionSearchReleaseFn
	sessionSearchOpenDefaultFn = func(context.Context) (SessionSearchStore, error) { return db, nil }
	sessionSearchReleaseFn = func(SessionSearchStore) error { released++; return nil }
	t.Cleanup(func() { sessionSearchOpenDefaultFn, sessionSearchReleaseFn = prevOpen, prevRelease })
	result := ssSearch(t, SessionSearchArgs{})
	if result["success"] != true || released != 1 {
		t.Fatalf("result %v released %d", result, released)
	}
}

func TestBrowseShapeCrossProfileDatabaseIsClosedButSharedDatabaseIsNot(t *testing.T) {
	shared := &boundedFakeStore{sessionSearchFakeStore: sessionSearchFakeStore{t: t}}
	profile := &boundedFakeStore{sessionSearchFakeStore: sessionSearchFakeStore{t: t}}
	prev := resolveProfileDBFn
	resolveProfileDBFn = func(context.Context, any) (SessionSearchStore, error) { return profile, nil }
	t.Cleanup(func() { resolveProfileDBFn = prev })
	result := ssSearch(t, SessionSearchArgs{DB: shared, Profile: "work"})
	if result["success"] != true || profile.closed != 1 || shared.closed != 0 {
		t.Fatalf("result %v profile closed %d shared closed %d", result, profile.closed, shared.closed)
	}
}

func TestBrowseShapeNoArgsReturnsRecentSessions(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	result := ssSearch(t, SessionSearchArgs{DB: db})
	if result["success"] != true || result["mode"] != "browse" || ssCount(result) < 3 {
		t.Fatalf("result %v", result)
	}
}

func TestBrowseShapeBrowseExcludesCurrentSession(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	result := ssSearch(t, SessionSearchArgs{DB: db, CurrentSessionID: "s_newest"})
	if containsString(ssSessionIDs(result), "s_newest") {
		t.Fatalf("current session listed: %v", ssSessionIDs(result))
	}
}

func TestDiscoveryShapeFullDetailReturnsBookendsAndWindowForEveryHit(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	result := ssSearch(t, SessionSearchArgs{Query: "modpack", Limit: 3, Detail: "full", DB: db})
	if result["success"] != true || result["mode"] != "discover" || result["detail"] != "full" || ssCount(result) < 1 {
		t.Fatalf("result %v", result)
	}
	for _, hit := range ssResults(result) {
		if hit["detail"] != "full" {
			t.Fatalf("hit detail %v", hit["detail"])
		}
		for _, key := range []string{"bookend_start", "messages", "bookend_end", "match_message_id", "snippet", "messages_before", "messages_after"} {
			if _, ok := hit[key]; !ok {
				t.Fatalf("hit lacks %s: %v", key, hit)
			}
		}
	}
}

func TestDiscoveryShapeDefaultDiscoveryKeepsTopFullAndCompactsLowerHits(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	result := ssSearch(t, SessionSearchArgs{Query: "modpack", Limit: 3, DB: db})
	hits := ssResults(result)
	if result["success"] != true || result["detail"] != "adaptive" || len(hits) != 3 {
		t.Fatalf("result %v", result)
	}
	top, lower := hits[0], hits[1:]
	if top["detail"] != "full" || top["bookend_start"] == nil || len(ssMessages(top, "messages")) <= 1 || top["bookend_end"] == nil {
		t.Fatalf("top %v", top)
	}
	for _, hit := range lower {
		msgs := ssMessages(hit, "messages")
		if hit["detail"] != "compact" || len(ssMessages(hit, "bookend_start")) != 0 || len(msgs) != 1 ||
			msgs[0]["id"] != hit["match_message_id"] || msgs[0]["anchor"] != true || len(ssMessages(hit, "bookend_end")) != 0 {
			t.Fatalf("lower hit %v", hit)
		}
	}
}

func TestDiscoveryShapeAdaptiveDetailPreservesRankingAndReducesPayload(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	now := time.Now().Unix()
	for sessionIndex := 0; sessionIndex < 3; sessionIndex++ {
		sid := fmt.Sprintf("payload_%d", sessionIndex)
		ssCreate(t, db, state.NewSession{ID: sid, Source: "cli"})
		ssExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", now-int64(sessionIndex), sid)
		for i := 0; i < 8; i++ {
			role := "assistant"
			if i%2 == 0 {
				role = "user"
			}
			ssAppend(t, db, sid, role, fmt.Sprintf("opening %d-%d ", sessionIndex, i)+strings.Repeat("o", 2500))
		}
		ssAppend(t, db, sid, "user", fmt.Sprintf("payloadneedle anchor %d ", sessionIndex)+strings.Repeat("a", 3500))
		for i := 0; i < 8; i++ {
			role := "user"
			if i%2 == 0 {
				role = "assistant"
			}
			ssAppend(t, db, sid, role, fmt.Sprintf("closing %d-%d ", sessionIndex, i)+strings.Repeat("c", 2500))
		}
	}
	adaptiveJSON := SessionSearch(context.Background(), SessionSearchArgs{Query: "payloadneedle", Limit: 3, DB: db})
	fullJSON := SessionSearch(context.Background(), SessionSearchArgs{Query: "payloadneedle", Limit: 3, Detail: "full", DB: db})
	adaptive, full := ssDecode(t, adaptiveJSON), ssDecode(t, fullJSON)
	if strings.Join(ssSessionIDs(adaptive), ",") != strings.Join(ssSessionIDs(full), ",") {
		t.Fatalf("ranking differs %v vs %v", ssSessionIDs(adaptive), ssSessionIDs(full))
	}
	for i, hit := range ssResults(adaptive) {
		if hit["match_message_id"] != ssResults(full)[i]["match_message_id"] {
			t.Fatalf("anchor differs at %d", i)
		}
	}
	if float64(len(adaptiveJSON)) >= float64(len(fullJSON))*0.6 {
		t.Fatalf("adaptive %d bytes, full %d bytes", len(adaptiveJSON), len(fullJSON))
	}
}

func TestDiscoveryShapeCurrentSessionFilteredOut(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	result := ssSearch(t, SessionSearchArgs{Query: "modpack", DB: db, CurrentSessionID: "s_newest"})
	if containsString(ssSessionIDs(result), "s_newest") {
		t.Fatalf("current session surfaced: %v", ssSessionIDs(result))
	}
}

func TestDiscoverySortSortNewestOrdersByRecency(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	first := ssResults(ssSearch(t, SessionSearchArgs{Query: "modpack", Limit: 3, Sort: "newest", DB: db}))[0]
	title, _ := first["title"].(string)
	if first["session_id"] != "s_newest" && !strings.Contains(title, "Newest") {
		t.Fatalf("first %v", first)
	}
}

func TestDiscoverySortSortOldestOrdersByAge(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	first := ssResults(ssSearch(t, SessionSearchArgs{Query: "modpack", Limit: 3, Sort: "oldest", DB: db}))[0]
	if first["session_id"] != "s_oldest" {
		t.Fatalf("first %v", first)
	}
}

// Anchor session and message id of the top discovery hit for modpack
func modpackAnchor(t *testing.T, db *state.DB) (string, float64) {
	t.Helper()
	disc := ssResults(ssSearch(t, SessionSearchArgs{Query: "modpack", Limit: 1, DB: db}))
	return disc[0]["session_id"].(string), disc[0]["match_message_id"].(float64)
}

func TestScrollShapeScrollReturnsAnchoredWindowWithoutBookends(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	sid, mid := modpackAnchor(t, db)
	result := ssSearch(t, SessionSearchArgs{SessionID: sid, AroundMessageID: mid, Window: 2, DB: db})
	if result["success"] != true || result["mode"] != "scroll" {
		t.Fatalf("result %v", result)
	}
	if _, ok := result["bookend_start"]; ok {
		t.Fatal("scroll carries bookend_start")
	}
	if _, ok := result["bookend_end"]; ok {
		t.Fatal("scroll carries bookend_end")
	}
	var anchored []map[string]any
	for _, m := range ssMessages(result, "messages") {
		if m["id"] == mid {
			anchored = append(anchored, m)
		}
	}
	if len(anchored) != 1 || anchored[0]["anchor"] != true {
		t.Fatalf("anchor rows %v", anchored)
	}
}

func TestScrollShapeScrollWindowClampedTo20(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	sid, mid := modpackAnchor(t, db)
	result := ssSearch(t, SessionSearchArgs{SessionID: sid, AroundMessageID: mid, Window: 999, DB: db})
	if result["window"] != float64(20) {
		t.Fatalf("window %v", result["window"])
	}
}

func TestScrollShapeScrollRejectsActiveDelegationChildInCurrentLineage(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_current", Source: "cli"})
	ssCreate(t, db, state.NewSession{ID: "s_delegate", Source: "delegate", ParentSessionID: "s_current"})
	mid := ssAppend(t, db, "s_delegate", "assistant", "live delegated result")
	result := ssSearch(t, SessionSearchArgs{SessionID: "s_delegate", AroundMessageID: mid, DB: db, CurrentSessionID: "s_current"})
	errText, _ := result["error"].(string)
	if result["success"] != false || !strings.Contains(strings.ToLower(errText), "current session") {
		t.Fatalf("result %v", result)
	}
}

func TestScrollPatternScrollForwardFromLastId(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_long", Source: "cli"})
	var ids []int64
	for i := 0; i < 20; i++ {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		ids = append(ids, ssAppend(t, db, "s_long", role, fmt.Sprintf("long session msg %d", i)))
	}
	v1 := ssSearch(t, SessionSearchArgs{SessionID: "s_long", AroundMessageID: ids[5], Window: 3, DB: db})
	m1 := ssMessages(v1, "messages")
	lastID := m1[len(m1)-1]["id"].(float64)
	v2 := ssSearch(t, SessionSearchArgs{SessionID: "s_long", AroundMessageID: lastID, Window: 3, DB: db})
	maxID := func(rows []map[string]any) float64 {
		m := -1.0
		for _, r := range rows {
			m = math.Max(m, r["id"].(float64))
		}
		return m
	}
	hasID := func(rows []map[string]any, id float64) bool {
		for _, r := range rows {
			if r["id"] == id {
				return true
			}
		}
		return false
	}
	m2 := ssMessages(v2, "messages")
	if maxID(m2) <= maxID(m1) || !hasID(m1, lastID) || !hasID(m2, lastID) {
		t.Fatalf("v1 %v v2 %v", m1, m2)
	}
}

func TestShapePrecedenceScrollArgsBeatQuery(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	sid, mid := modpackAnchor(t, db)
	result := ssSearch(t, SessionSearchArgs{Query: "modpack", SessionID: sid, AroundMessageID: mid, DB: db})
	if result["mode"] != "scroll" {
		t.Fatalf("mode %v", result["mode"])
	}
}

func TestShapePrecedenceSessionIdWithoutAnchorReads(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	if result := ssSearch(t, SessionSearchArgs{SessionID: "s_oldest", DB: db}); result["mode"] != "read" {
		t.Fatalf("mode %v", result["mode"])
	}
}

func TestReadShapeReadReturnsFullSession(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	result := ssSearch(t, SessionSearchArgs{SessionID: "s_oldest", DB: db})
	meta, _ := result["session_meta"].(map[string]any)
	if result["success"] != true || result["mode"] != "read" || result["session_id"] != "s_oldest" ||
		result["message_count"] != float64(5) || result["truncated"] != false || len(ssMessages(result, "messages")) != 5 ||
		meta["title"] != "Building the Modpack" {
		t.Fatalf("result %v", result)
	}
}

func TestReadShapeReadStripsAnsiSequencesFromMessages(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_ansi", Source: "cli"})
	ssAppend(t, db, "s_ansi", "user", "plain")
	ssAppend(t, db, "s_ansi", "assistant", "\x1b[31mred text\x1b[0m and more")
	result := ssSearch(t, SessionSearchArgs{SessionID: "s_ansi", DB: db})
	if result["success"] != true {
		t.Fatalf("result %v", result)
	}
	var rendered []string
	for _, m := range ssMessages(result, "messages") {
		if c := ssContent(m); c != "" {
			rendered = append(rendered, c)
		}
	}
	if !containsString(rendered, "red text and more") {
		t.Fatalf("rendered %q", rendered)
	}
	for _, text := range rendered {
		if strings.Contains(text, "\x1b") {
			t.Fatalf("escape survived in %q", text)
		}
	}
}

func TestReadShapeReadTruncatesLargeSession(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_big", Source: "cli"})
	for i := 0; i < 50; i++ {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		ssAppend(t, db, "s_big", role, fmt.Sprintf("m%d", i))
	}
	result := ssSearch(t, SessionSearchArgs{SessionID: "s_big", DB: db})
	if result["mode"] != "read" || result["message_count"] != float64(50) || result["truncated"] != true || len(ssMessages(result, "messages")) >= 50 {
		t.Fatalf("result %v", result)
	}
}

func TestReadShapeReadCapsOversizedMessageContent(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_huge", Source: "cli"})
	ssAppend(t, db, "s_huge", "user", "run it")
	ssAppend(t, db, "s_huge", "assistant", strings.Repeat("x", 80000))
	ssAppend(t, db, "s_huge", "user", "thanks")
	result := ssSearch(t, SessionSearchArgs{SessionID: "s_huge", DB: db})
	if result["mode"] != "read" || result["truncated"] != false {
		t.Fatalf("result %v", result)
	}
	var big map[string]any
	total := 0
	for _, m := range ssMessages(result, "messages") {
		total += len([]rune(ssContent(m)))
		if m["content_truncated"] == true && big == nil {
			big = m
		}
	}
	if big == nil || len([]rune(ssContent(big))) > 2001 || big["original_content_chars"] != float64(80000) || total >= 5000 {
		t.Fatalf("big %v total %d", big, total)
	}
}

func TestReadShapeTitleMatchEntryCapsContentLikeFtsHits(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_titled", Source: "cli"})
	if _, err := db.SetSessionTitle(context.Background(), "s_titled", "quasar ledger reconciliation"); err != nil {
		t.Fatal(err)
	}
	ssAppend(t, db, "s_titled", "user", "reconcile the quasar ledger")
	ssAppend(t, db, "s_titled", "tool", strings.Repeat("y", 80000))
	ssEnd(t, db, "s_titled", "cli_exit")
	result := ssSearch(t, SessionSearchArgs{Query: "quasar ledger reconciliation", DB: db, Detail: "full"})
	var entry map[string]any
	for _, r := range ssResults(result) {
		if r["matched_role"] == "session_title" {
			entry = r
			break
		}
	}
	if entry == nil {
		t.Fatalf("no title match in %v", result)
	}
	shaped := append(append(ssMessages(entry, "bookend_start"), ssMessages(entry, "messages")...), ssMessages(entry, "bookend_end")...)
	var big []map[string]any
	for _, m := range shaped {
		if m["original_content_chars"] == float64(80000) {
			big = append(big, m)
		}
	}
	if len(big) == 0 {
		t.Fatalf("no capped message in %v", shaped)
	}
	for _, m := range big {
		if m["content_truncated"] != true || len([]rune(ssContent(m))) > 4001 {
			t.Fatalf("capped message %v", m)
		}
	}
}

// Session id of a session link value with the profile segment dropped
func linkedSessionID(t *testing.T, link string) string {
	t.Helper()
	if !strings.HasPrefix(link, "@session:") {
		t.Fatalf("link %q", link)
	}
	value := strings.TrimPrefix(link, "@session:")
	if i := strings.LastIndex(value, "/"); i >= 0 {
		return value[i+1:]
	}
	return value
}

func TestSessionLinkLinkCarriesTheNamedProfile(t *testing.T) {
	if got := sessionLink(context.Background(), "s_oldest", "work"); got != "@session:work/s_oldest" {
		t.Fatalf("link %q", got)
	}
}

func TestSessionLinkEveryDiscoveryResultLinksToItsOwnSession(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	result := ssSearch(t, SessionSearchArgs{Query: "modpack", Limit: 5, DB: db})
	hits := ssResults(result)
	if len(hits) == 0 {
		t.Fatal("no results")
	}
	for _, entry := range hits {
		if linkedSessionID(t, entry["link"].(string)) != entry["session_id"] {
			t.Fatalf("entry %v", entry)
		}
	}
}

func TestCrossProfileReadBareIdNeverReadsAnotherProfilesStore(t *testing.T) {
	root := t.TempDir()
	db := newSessionSearchDB(t, root)
	otherHome := filepath.Join(root, "asdf_home")
	if err := os.MkdirAll(otherHome, 0o755); err != nil {
		t.Fatal(err)
	}
	other := newSessionSearchDB(t, otherHome)
	ssCreate(t, other, state.NewSession{ID: "s_far", Source: "cli"})
	ssAppend(t, other, "s_far", "user", "secret")
	prevDir := ProfileStoreHooks.Dir
	ProfileStoreHooks.Dir = func(string) (string, error) { return filepath.Join(root, "default_home"), nil }
	t.Cleanup(func() { ProfileStoreHooks.Dir = prevDir })
	raw := SessionSearch(context.Background(), SessionSearchArgs{SessionID: "s_far", DB: db})
	result := ssDecode(t, raw)
	if _, has := result["messages"]; result["success"] != false || has || strings.Contains(raw, "secret") {
		t.Fatalf("result %s", raw)
	}
	patchProfileStore(t, otherHome, true)
	named := ssSearch(t, SessionSearchArgs{SessionID: "s_far", Profile: "asdf", DB: db})
	if named["success"] != true || named["message_count"] != float64(1) {
		t.Fatalf("named %v", named)
	}
}

func TestCrossProfileReadCombinedValueAutosplits(t *testing.T) {
	root := t.TempDir()
	db := newSessionSearchDB(t, root)
	otherHome := filepath.Join(root, "other_home")
	if err := os.MkdirAll(otherHome, 0o755); err != nil {
		t.Fatal(err)
	}
	other := newSessionSearchDB(t, otherHome)
	ssCreate(t, other, state.NewSession{ID: "s_other", Source: "cli"})
	ssAppend(t, other, "s_other", "user", "hi")
	patchProfileStore(t, otherHome, true)
	for _, a := range []SessionSearchArgs{
		{SessionID: "asdf/s_other"},
		{SessionID: "asdf/s_other", Profile: "asdf"},
		{SessionID: "s_other", Profile: "asdf"},
	} {
		a.DB = db
		result := ssSearch(t, a)
		if result["success"] != true || result["mode"] != "read" || result["session_id"] != "s_other" {
			t.Fatalf("args %+v result %v", a, result)
		}
	}
}

// One interactive telegram session and eight cron sessions all matching the same query
func seedCronAndInteractive(t *testing.T, db *state.DB) {
	t.Helper()
	now := time.Now().Unix()
	ssCreate(t, db, state.NewSession{ID: "s_user", Source: "telegram"})
	ssExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", now-90000, "s_user")
	ssAppend(t, db, "s_user", "user", "how is the venom project going")
	ssAppend(t, db, "s_user", "assistant", "The venom project shipped its first milestone.")
	for i := 0; i < 8; i++ {
		sid := fmt.Sprintf("cron_%d", i)
		ssCreate(t, db, state.NewSession{ID: sid, Source: "cron"})
		ssExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", now-1000-int64(i), sid)
		ssAppend(t, db, sid, "user", "venom project daily status")
		ssAppend(t, db, sid, "assistant", "venom project venom project venom summary")
	}
}

func TestCronDemotionInteractiveSessionSurfacesAboveCron(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedCronAndInteractive(t, db)
	result := ssSearch(t, SessionSearchArgs{Query: "venom project", Limit: 1, DB: db})
	hits := ssResults(result)
	if result["success"] != true || ssCount(result) != 1 || hits[0]["source"] != "telegram" || hits[0]["session_id"] != "s_user" {
		t.Fatalf("result %v", result)
	}
}

func TestCronDemotionCronStillReachableWhenOnlyMatch(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	now := time.Now().Unix()
	ssCreate(t, db, state.NewSession{ID: "cron_only", Source: "cron"})
	ssExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", now-500, "cron_only")
	ssAppend(t, db, "cron_only", "user", "quarterly archive sweep")
	ssAppend(t, db, "cron_only", "assistant", "Archive sweep complete.")
	result := ssSearch(t, SessionSearchArgs{Query: "archive sweep", DB: db})
	if result["success"] != true || ssCount(result) != 1 || ssResults(result)[0]["source"] != "cron" {
		t.Fatalf("result %v", result)
	}
}

func TestCompactionSummaryFilteringIsCompactionSummaryDetectsPrefix(t *testing.T) {
	if !isCompactionSummary("[CONTEXT COMPACTION — REFERENCE ONLY] foo") || !isCompactionSummary("[CONTEXT SUMMARY]: old summary") {
		t.Fatal("prefixes not detected")
	}
	if isCompactionSummary("Hello, how can I help?") || isCompactionSummary("") || isCompactionSummary(nil) {
		t.Fatal("plain content detected")
	}
}

func TestCompactionSummaryFilteringCompactionSummaryExcludedFromBookendStart(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_compact", Source: "cli"})
	ssAppend(t, db, "s_compact", "user", "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted into the summary below. "+strings.Repeat("x", 50000))
	ssAppend(t, db, "s_compact", "user", "Fix the zorgblat rendering bug")
	for i := 0; i < 10; i++ {
		ssAppend(t, db, "s_compact", "user", fmt.Sprintf("setup step %d", i))
		ssAppend(t, db, "s_compact", "assistant", fmt.Sprintf("setup done %d", i))
	}
	ssAppend(t, db, "s_compact", "user", "investigate the frobnitz mob spawning in KubeJS")
	ssAppend(t, db, "s_compact", "assistant", "I'll look into the frobnitz mob spawning issue.")
	for i := 0; i < 5; i++ {
		ssAppend(t, db, "s_compact", "user", fmt.Sprintf("tail %d", i))
		ssAppend(t, db, "s_compact", "assistant", fmt.Sprintf("done tail %d", i))
	}
	result := ssSearch(t, SessionSearchArgs{Query: "frobnitz mob spawning", DB: db, Limit: 1})
	if result["success"] != true || len(ssResults(result)) < 1 {
		t.Fatalf("result %v", result)
	}
	entry := ssResults(result)[0]
	seenZorgblat := false
	for _, msg := range ssMessages(entry, "bookend_start") {
		if strings.Contains(ssContent(msg), "[CONTEXT COMPACTION") {
			t.Fatalf("handoff in bookend_start: %v", msg)
		}
		seenZorgblat = seenZorgblat || strings.Contains(ssContent(msg), "zorgblat")
	}
	if !seenZorgblat {
		t.Fatalf("bookend_start %v", entry["bookend_start"])
	}
}

func TestResolveToParentLegacyRotationDetectsCompression(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_parent", Source: "cli"})
	ssEnd(t, db, "s_parent", "compression")
	ssCreate(t, db, state.NewSession{ID: "s_child", Source: "cli", ParentSessionID: "s_parent"})
	root, hasCompression := resolveToParent(context.Background(), db, "s_child")
	if root != "s_parent" || !hasCompression {
		t.Fatalf("root %q compression %v", root, hasCompression)
	}
}

func TestResolveToParentChainWithMixedEdges(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_gp", Source: "cli"})
	ssEnd(t, db, "s_gp", "compression")
	ssCreate(t, db, state.NewSession{ID: "s_p", Source: "cli", ParentSessionID: "s_gp"})
	ssCreate(t, db, state.NewSession{ID: "s_c", Source: "cli", ParentSessionID: "s_p"})
	root, hasCompression := resolveToParent(context.Background(), db, "s_c")
	if root != "s_gp" || !hasCompression {
		t.Fatalf("root %q compression %v", root, hasCompression)
	}
}

func TestIsCompactedMessageActiveMessageReturnsFalse(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s1", Source: "cli"})
	mid := ssAppend(t, db, "s1", "user", "hello")
	if isCompactedMessage(context.Background(), db, mid) {
		t.Fatal("active message read as compacted")
	}
}

func TestIsCompactedMessageCompactedMessageReturnsTrue(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s1", Source: "cli"})
	mid := ssAppend(t, db, "s1", "user", "archived content")
	ssCompact(t, db, "s1", []map[string]any{{"role": "assistant", "content": "compacted summary"}})
	if !isCompactedMessage(context.Background(), db, mid) {
		t.Fatal("archived message not read as compacted")
	}
}

func TestInPlaceCompactionDiscoveryArchivedContentDiscoverableAfterCompaction(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_compact", Source: "cli"})
	ssAppend(t, db, "s_compact", "user", "The spectral phoenix only spawns during full moons")
	ssAppend(t, db, "s_compact", "assistant", "Spectral phoenix requires moonstone bait")
	ssCompact(t, db, "s_compact", []map[string]any{
		{"role": "user", "content": "Summary: spectral phoenix discussed"},
		{"role": "assistant", "content": "Acknowledged spectral phoenix info"},
	})
	result := ssSearch(t, SessionSearchArgs{Query: "spectral phoenix", DB: db, CurrentSessionID: "s_compact"})
	if result["success"] != true || ssCount(result) < 1 || ssResults(result)[0]["session_id"] != "s_compact" {
		t.Fatalf("result %v", result)
	}
}

func TestInPlaceCompactionDiscoveryLiveContentStillFilteredOnCurrentSession(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_live", Source: "cli"})
	ssAppend(t, db, "s_live", "user", "crystal golem farming route")
	result := ssSearch(t, SessionSearchArgs{Query: "crystal golem", DB: db, CurrentSessionID: "s_live"})
	if ssCount(result) != 0 {
		t.Fatalf("result %v", result)
	}
}

func TestLegacyRotationDiscoveryCompressionParentDiscoverableFromChild(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_parent", Source: "cli"})
	ssAppend(t, db, "s_parent", "user", "The void crystal mining requires diamond pickaxe")
	ssAppend(t, db, "s_parent", "assistant", "Void crystal found in the deep caverns")
	ssEnd(t, db, "s_parent", "compression")
	ssCreate(t, db, state.NewSession{ID: "s_child", Source: "cli", ParentSessionID: "s_parent"})
	ssAppend(t, db, "s_child", "user", "Continue void crystal work")
	result := ssSearch(t, SessionSearchArgs{Query: "void crystal", DB: db, CurrentSessionID: "s_child"})
	if result["success"] != true || ssCount(result) < 1 || !containsString(ssSessionIDs(result), "s_parent") {
		t.Fatalf("result %v", result)
	}
}

func TestDelegationExclusionDelegationParentExcludedFromChild(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_parent", Source: "cli"})
	ssAppend(t, db, "s_parent", "user", "nebula deployment infrastructure setup")
	ssAppend(t, db, "s_parent", "assistant", "Nebula deployment configured successfully")
	ssCreate(t, db, state.NewSession{ID: "s_child", Source: "cli", ParentSessionID: "s_parent"})
	ssAppend(t, db, "s_child", "user", "delegated nebula deployment subtask")
	result := ssSearch(t, SessionSearchArgs{Query: "nebula deployment", DB: db, CurrentSessionID: "s_child"})
	if ssCount(result) != 0 {
		t.Fatalf("result %v", result)
	}
}

// Compacted session whose archived rows hold the FTS hit and whose live tail is the handoff summary
func seedCompactedSession(t *testing.T, db *state.DB) {
	t.Helper()
	ssCreate(t, db, state.NewSession{ID: "s_both", Source: "cli"})
	ssAppend(t, db, "s_both", "user", "Kick off the obsidian gateway migration. "+strings.Repeat("o", 5000))
	ssAppend(t, db, "s_both", "assistant", "Starting the obsidian gateway migration plan.")
	for i := 0; i < 10; i++ {
		ssAppend(t, db, "s_both", "user", fmt.Sprintf("migration step %d", i))
		ssAppend(t, db, "s_both", "assistant", fmt.Sprintf("migration step %d done", i))
	}
	ssAppend(t, db, "s_both", "user", "the obsidian gateway needs a quartz keystone to activate")
	ssAppend(t, db, "s_both", "assistant", "Noted: quartz keystone required for the obsidian gateway.")
	for i := 0; i < 5; i++ {
		ssAppend(t, db, "s_both", "user", fmt.Sprintf("wrap-up %d", i))
		ssAppend(t, db, "s_both", "assistant", fmt.Sprintf("wrapped %d", i))
	}
	ssCompact(t, db, "s_both", []map[string]any{
		{"role": "user", "content": "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted into this summary. " + strings.Repeat("s", 50000)},
		{"role": "assistant", "content": "Continuing after compaction."},
	})
}

func TestCompactionDiscoveryBothLayersArchivedHitSurfacesWithBoundedSummaryFreeBookends(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedCompactedSession(t, db)
	result := ssSearch(t, SessionSearchArgs{Query: "quartz keystone", DB: db, CurrentSessionID: "s_both"})
	if result["success"] != true || ssCount(result) < 1 {
		t.Fatalf("result %v", result)
	}
	entry := ssResults(result)[0]
	if entry["session_id"] != "s_both" {
		t.Fatalf("entry %v", entry)
	}
	bookends := append(ssMessages(entry, "bookend_start"), ssMessages(entry, "bookend_end")...)
	for _, msg := range bookends {
		if strings.Contains(ssContent(msg), "[CONTEXT COMPACTION") || len([]rune(ssContent(msg))) > 1210 {
			t.Fatalf("bookend %v", msg)
		}
	}
	for _, msg := range ssMessages(entry, "messages") {
		if len([]rune(ssContent(msg))) > 4010 {
			t.Fatalf("window message %v", msg)
		}
	}
	found := false
	for _, msg := range ssMessages(entry, "bookend_start") {
		found = found || strings.Contains(ssContent(msg), "obsidian gateway migration")
	}
	if !found {
		t.Fatalf("bookend_start %v", entry["bookend_start"])
	}
}

func TestRewindExclusionCompactedMessagesStillSurfaceAlongsideRewind(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_mixed", Source: "cli"})
	ssAppend(t, db, "s_mixed", "user", "compaction archived content beta")
	ssCompact(t, db, "s_mixed", []map[string]any{{"role": "assistant", "content": "Summary of beta"}})
	mid2 := ssAppend(t, db, "s_mixed", "user", "rewound content gamma")
	ssExec(t, db, "UPDATE messages SET active = 0, compacted = 0 WHERE id = ?", mid2)
	compact := ssSearch(t, SessionSearchArgs{Query: "compaction archived content beta", DB: db, CurrentSessionID: "s_mixed"})
	if ssCount(compact) < 1 {
		t.Fatalf("compacted content hidden %v", compact)
	}
	rewind := ssSearch(t, SessionSearchArgs{Query: "rewound content gamma", DB: db, CurrentSessionID: "s_mixed"})
	for _, entry := range ssResults(rewind) {
		snippet, _ := entry["snippet"].(string)
		if entry["match_message_id"] == float64(mid2) || strings.Contains(strings.ToLower(snippet), "gamma") {
			t.Fatalf("rewound row surfaced %v", entry)
		}
	}
}

func TestLegacyContinuationPlusDelegationCompressionParentSurfacesButDelegateChildExcluded(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_gp", Source: "cli"})
	ssAppend(t, db, "s_gp", "user", "grandparent cosmic anomaly research data")
	ssEnd(t, db, "s_gp", "compression")
	ssCreate(t, db, state.NewSession{ID: "s_p", Source: "cli", ParentSessionID: "s_gp"})
	ssAppend(t, db, "s_p", "user", "parent cosmic anomaly follow-up notes")
	ssEnd(t, db, "s_p", "compression")
	ssCreate(t, db, state.NewSession{ID: "s_current", Source: "cli", ParentSessionID: "s_p"})
	ssCreate(t, db, state.NewSession{ID: "s_delegate", Source: "cli", ParentSessionID: "s_p"})
	ssAppend(t, db, "s_delegate", "assistant", "delegated cosmic anomaly subtask results")
	result := ssSearch(t, SessionSearchArgs{Query: "cosmic anomaly", DB: db, CurrentSessionID: "s_current"})
	sids := ssSessionIDs(result)
	if !(containsString(sids, "s_gp") || containsString(sids, "s_p")) || containsString(sids, "s_delegate") {
		t.Fatalf("sessions %v", sids)
	}
}

// A to B to C gateway /new chain where C is the empty current session
func seedGatewayNewResetChain(t *testing.T, db *state.DB) string {
	t.Helper()
	needle := "ibuprofen night-dose protocol"
	ssCreate(t, db, state.NewSession{ID: "s_aug12", Source: "telegram", SessionKey: "tg:user:1"})
	ssAppend(t, db, "s_aug12", "user", "older unrelated chat")
	ssEnd(t, db, "s_aug12", "session_reset")
	ssCreate(t, db, state.NewSession{ID: "s_night", Source: "telegram", ParentSessionID: "s_aug12", SessionKey: "tg:user:1",
		ModelConfig: map[string]any{"_reset_from": "s_aug12"}})
	ssExec(t, db, "UPDATE sessions SET title = ? WHERE id = ?", "Night ibuprofen plan", "s_night")
	ssAppend(t, db, "s_night", "user", "Remember the "+needle)
	ssAppend(t, db, "s_night", "assistant", "Noted "+needle+" at 21:00")
	ssEnd(t, db, "s_night", "session_reset")
	ssCreate(t, db, state.NewSession{ID: "s_today", Source: "telegram", ParentSessionID: "s_night", SessionKey: "tg:user:1",
		ModelConfig: map[string]any{"_reset_from": "s_night"}})
	return needle
}

func TestNewResetLineageDiscoverySessionResetParentDiscoverableFromChild(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedGatewayNewResetChain(t, db)
	result := ssSearch(t, SessionSearchArgs{Query: "ibuprofen", DB: db, CurrentSessionID: "s_today"})
	blob, _ := json.Marshal(result["results"])
	if result["success"] != true || ssCount(result) < 1 || !containsString(ssSessionIDs(result), "s_night") ||
		!strings.Contains(strings.ToLower(string(blob)), "ibuprofen") {
		t.Fatalf("result %v", result)
	}
}

func TestNewResetLineageDiscoveryCliNewSessionParentDiscoverableFromChild(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_cli_old", Source: "cli"})
	ssAppend(t, db, "s_cli_old", "user", "quartz lantern wiring diagram from yesterday")
	ssEnd(t, db, "s_cli_old", "new_session")
	ssCreate(t, db, state.NewSession{ID: "s_cli_new", Source: "cli", ParentSessionID: "s_cli_old"})
	result := ssSearch(t, SessionSearchArgs{Query: "quartz lantern", DB: db, CurrentSessionID: "s_cli_new"})
	if ssCount(result) < 1 || !containsString(ssSessionIDs(result), "s_cli_old") {
		t.Fatalf("result %v", result)
	}
}

func TestNewResetLineageDiscoveryBranchedParentStillExcluded(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_p", Source: "cli"})
	ssAppend(t, db, "s_p", "user", "zephyr crystal cache design")
	ssEnd(t, db, "s_p", "branched")
	ssCreate(t, db, state.NewSession{ID: "s_q", Source: "cli", ParentSessionID: "s_p", ModelConfig: map[string]any{"_branched_from": "s_p"}})
	ssAppend(t, db, "s_q", "user", "zephyr crystal cache design")
	result := ssSearch(t, SessionSearchArgs{Query: "zephyr crystal", DB: db, CurrentSessionID: "s_q"})
	if containsString(ssSessionIDs(result), "s_p") {
		t.Fatalf("branched parent surfaced %v", ssSessionIDs(result))
	}
}

func TestNewResetLineageDiscoveryTitleMatchResetParentNotDropped(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedGatewayNewResetChain(t, db)
	result := ssSearch(t, SessionSearchArgs{Query: "Night ibuprofen plan", DB: db, CurrentSessionID: "s_today"})
	if ssCount(result) < 1 || !containsString(ssSessionIDs(result), "s_night") {
		t.Fatalf("result %v", result)
	}
}

func TestNewResetLineageDiscoveryScrollIntoResetParentIsAllowed(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedGatewayNewResetChain(t, db)
	disc := ssSearch(t, SessionSearchArgs{Query: "ibuprofen", DB: db, CurrentSessionID: "s_today", Limit: 1})
	if ssCount(disc) < 1 {
		t.Fatalf("discovery %v", disc)
	}
	hit := ssResults(disc)[0]
	scrolled := ssSearch(t, SessionSearchArgs{SessionID: hit["session_id"], AroundMessageID: hit["match_message_id"], DB: db, CurrentSessionID: "s_today"})
	var contents []string
	for _, m := range ssMessages(scrolled, "messages") {
		contents = append(contents, ssContent(m))
	}
	if scrolled["success"] != true || scrolled["mode"] != "scroll" || !strings.Contains(strings.ToLower(strings.Join(contents, " ")), "ibuprofen") {
		t.Fatalf("scrolled %v", scrolled)
	}
}

func TestNewResetLineageBrowseResetParentAppearsInBrowse(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedGatewayNewResetChain(t, db)
	result := ssSearch(t, SessionSearchArgs{DB: db, CurrentSessionID: "s_today"})
	sids := ssSessionIDs(result)
	if result["mode"] != "browse" || containsString(sids, "s_today") || !containsString(sids, "s_night") {
		t.Fatalf("browse %v", sids)
	}
}

func TestNewResetLineageBrowseBrowseStillHidesLiveDelegationChild(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_main", Source: "cli"})
	ssAppend(t, db, "s_main", "user", "parent work")
	ssCreate(t, db, state.NewSession{ID: "s_delegate", Source: "cli", ParentSessionID: "s_main"})
	ssAppend(t, db, "s_delegate", "assistant", "subagent work")
	sids := ssSessionIDs(ssSearch(t, SessionSearchArgs{DB: db, CurrentSessionID: "s_other"}))
	if containsString(sids, "s_delegate") || !containsString(sids, "s_main") {
		t.Fatalf("browse %v", sids)
	}
}

func TestNewResetLineageBrowseBrowseListsLegacyPremarkerResetChild(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_old", Source: "telegram", SessionKey: "tg:legacy:1"})
	ssAppend(t, db, "s_old", "user", "legacy era chat")
	ssEnd(t, db, "s_old", "session_reset")
	ssCreate(t, db, state.NewSession{ID: "s_legacy_child", Source: "telegram", ParentSessionID: "s_old", SessionKey: "tg:legacy:1"})
	ssAppend(t, db, "s_legacy_child", "user", "current era chat")
	sids := ssSessionIDs(ssSearch(t, SessionSearchArgs{DB: db, CurrentSessionID: "s_other"}))
	if !containsString(sids, "s_legacy_child") {
		t.Fatalf("browse %v", sids)
	}
}

func ssUnix(year int, month time.Month, day int) int64 {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Unix()
}

func ssSetStarted(t *testing.T, db *state.DB, startedBySID map[string]int64) {
	t.Helper()
	for sid, ts := range startedBySID {
		ssExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", ts, sid)
	}
}

func TestDiscoveryTemporalNarrowingParseBound(t *testing.T) {
	type want struct {
		offset   *int64
		absolute *int64
		none     bool
		raises   bool
	}
	i64 := func(v int64) *int64 { return &v }
	cases := []struct {
		value any
		want  want
	}{
		{"24h", want{offset: i64(86400)}}, {"1d", want{offset: i64(86400)}}, {"2w", want{offset: i64(2 * 604800)}},
		{"7D", want{offset: i64(7 * 86400)}}, {" 3 d ", want{offset: i64(3 * 86400)}},
		{"2026-06-01", want{absolute: i64(ssUnix(2026, 6, 1))}}, {"2026-06-01T12:00:00Z", want{absolute: i64(ssUnix(2026, 6, 1) + 43200)}},
		{nil, want{none: true}}, {"", want{none: true}}, {"7x", want{raises: true}}, {"not-a-date", want{raises: true}},
	}
	for _, c := range cases {
		got, err := parseISOBound(c.value)
		switch {
		case c.want.raises:
			if err == nil {
				t.Fatalf("%v: expected a ValueError", c.value)
			}
		case c.want.none:
			if err != nil || got != nil {
				t.Fatalf("%v: got %v err %v", c.value, got, err)
			}
		case c.want.offset != nil:
			if err != nil || got == nil || math.Abs(float64(time.Now().Unix()-got.value)-float64(*c.want.offset)) >= 5 {
				t.Fatalf("%v: got %v err %v", c.value, got, err)
			}
		default:
			if err != nil || got == nil || got.value != *c.want.absolute {
				t.Fatalf("%v: got %v err %v", c.value, got, err)
			}
		}
	}
}

func TestDiscoveryTemporalNarrowingIsoWindowIsAppliedInSqlNotAsAPostFilter(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	prev := discoverScanLimit
	discoverScanLimit = 2
	t.Cleanup(func() { discoverScanLimit = prev })
	for i := 0; i < 4; i++ {
		sid := fmt.Sprintf("s_aug_%d", i)
		ssCreate(t, db, state.NewSession{ID: sid, Source: "cli"})
		ssAppend(t, db, sid, "user", "modpack modpack modpack modpack")
		ssAppend(t, db, sid, "assistant", strings.Repeat("modpack details ", 20))
		ssSetStarted(t, db, map[string]int64{sid: ssUnix(2026, 8, 10)})
	}
	ssCreate(t, db, state.NewSession{ID: "s_june", Source: "cli"})
	ssAppend(t, db, "s_june", "user", "modpack")
	ssAppend(t, db, "s_june", "assistant", "ok")
	ssSetStarted(t, db, map[string]int64{"s_june": ssUnix(2026, 6, 15)})
	result := ssSearch(t, SessionSearchArgs{Query: "modpack", Limit: 5, After: "2026-06-01", Before: "2026-07-01", DB: db})
	if result["success"] != true || strings.Join(ssSessionIDs(result), ",") != "s_june" {
		t.Fatalf("result %v", result)
	}
	if bad := ssSearch(t, SessionSearchArgs{Query: "modpack", After: "not-a-date", DB: db}); bad["success"] != false {
		t.Fatalf("invalid bound accepted %v", bad)
	}
}

// Session ids the inline executor returns for one modpack query with extra arguments
func inlineModpackRun(t *testing.T, host *fakeExecutorHost, extra map[string]any) map[string]bool {
	t.Helper()
	args := map[string]any{"query": "modpack", "limit": 5}
	for k, v := range extra {
		args[k] = v
	}
	raw, err := inlineToolExecutor("session_search")(context.Background(), host, args, InlineToolContext{TaskID: "task-1", ToolCallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	out := ssDecode(t, raw)
	if out["success"] != true {
		t.Fatalf("result %v", out)
	}
	set := map[string]bool{}
	for _, sid := range ssSessionIDs(out) {
		set[sid] = true
	}
	return set
}

func sameSet(got map[string]bool, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return true
}

func TestDiscoveryTemporalNarrowingRelativeBoundsReachTheToolThroughTheInlineExecutor(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	seedModpackSessions(t, db)
	now := time.Now().Unix()
	ssSetStarted(t, db, map[string]int64{"s_newest": now - 2*86400, "s_middle": now - 30*86400, "s_oldest": now - 60*86400})
	host := &fakeExecutorHost{recallDB: db, sessionID: "current"}
	if got := inlineModpackRun(t, host, map[string]any{"after": "7d"}); !sameSet(got, "s_newest") {
		t.Fatalf("after 7d %v", got)
	}
	if got := inlineModpackRun(t, host, map[string]any{"before": "7d"}); !sameSet(got, "s_middle", "s_oldest") {
		t.Fatalf("before 7d %v", got)
	}
	if got := inlineModpackRun(t, host, map[string]any{"exclude_session_ids": []any{"s_newest"}}); !sameSet(got, "s_middle", "s_oldest") {
		t.Fatalf("exclude %v", got)
	}
}

func TestDiscoverySessionExclusionExcludeSessionIdsDropsWholeLineage(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	ssCreate(t, db, state.NewSession{ID: "s_root", Source: "cli"})
	ssAppend(t, db, "s_root", "user", "unique lineage token alpha")
	ssAppend(t, db, "s_root", "assistant", "noted unique lineage token alpha")
	ssCreate(t, db, state.NewSession{ID: "s_child", Source: "cli", ParentSessionID: "s_root"})
	ssAppend(t, db, "s_child", "user", "follow-up unique lineage token alpha")
	ssAppend(t, db, "s_child", "assistant", "child unique lineage token alpha")
	ssSetStarted(t, db, map[string]int64{"s_root": ssUnix(2026, 6, 1), "s_child": ssUnix(2026, 6, 2)})
	found := ssSessionIDs(ssSearch(t, SessionSearchArgs{Query: "unique lineage token alpha", Limit: 5, DB: db}))
	if !containsString(found, "s_root") && !containsString(found, "s_child") {
		t.Fatalf("lineage not found %v", found)
	}
	excluded := ssSessionIDs(ssSearch(t, SessionSearchArgs{Query: "unique lineage token alpha", Limit: 5, ExcludeSessionIDs: []any{"s_child"}, DB: db}))
	if containsString(excluded, "s_root") || containsString(excluded, "s_child") {
		t.Fatalf("lineage survived exclusion %v", excluded)
	}
}

func TestBrowseDefaultLimitNoLimitBrowseReturnsTenOnEveryEntryPoint(t *testing.T) {
	db := newSessionSearchDB(t, t.TempDir())
	for i := 0; i < 12; i++ {
		sid := fmt.Sprintf("s_browse_%02d", i)
		ssCreate(t, db, state.NewSession{ID: sid, Source: "cli"})
		ssAppend(t, db, sid, "user", fmt.Sprintf("browse seed %d", i))
	}
	host := &fakeExecutorHost{recallDB: db, sessionID: "current"}
	ctx := context.Background()
	handler := Default.Get("session_search")
	if handler == nil {
		t.Fatal("session_search not registered")
	}
	inline, err := inlineToolExecutor("session_search")(ctx, host, map[string]any{}, InlineToolContext{TaskID: "task-1", ToolCallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		SessionSearch(ctx, SessionSearchArgs{DB: db}),
		handler.Handler(ctx, &Call{Name: "session_search", Args: map[string]any{}, Env: &Env{State: db}}),
		inline,
	} {
		result := ssDecode(t, raw)
		if result["mode"] != "browse" || len(ssResults(result)) != 10 {
			t.Fatalf("result %v", result)
		}
	}
	if got := len(ssResults(ssSearch(t, SessionSearchArgs{DB: db, Limit: 4}))); got != 4 {
		t.Fatalf("limit 4 gave %d", got)
	}
}

// The inline executor hands the unavailable notice back when the host has no recall database
func TestSessionSearchInlineExecutorWithoutRecallDatabase(t *testing.T) {
	raw, err := inlineToolExecutor("session_search")(context.Background(), &fakeExecutorHost{}, map[string]any{}, InlineToolContext{})
	if err != nil {
		t.Fatal(err)
	}
	result := ssDecode(t, raw)
	errText, _ := result["error"].(string)
	if result["success"] != false || !strings.HasPrefix(errText, state.SessionDBUnavailablePrefix) {
		t.Fatalf("result %v", result)
	}
}

// A store without the bounded browse surfaces the hermes RuntimeError text
func TestSessionSearchLegacyStoreErrorText(t *testing.T) {
	result := ssSearch(t, SessionSearchArgs{DB: &sessionSearchFakeStore{t: t}})
	if result["error"] != "Failed to list recent sessions: session database does not support bounded recent-session browse" {
		t.Fatalf("result %v", result)
	}
}
