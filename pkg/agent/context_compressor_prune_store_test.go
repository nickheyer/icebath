package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session store whose writers can fail, count calls or run a hook before delegating
type pruneSpyDB struct {
	*state.SessionDB
	archiveCalls  int
	archiveErr    error
	rewriteErr    error
	beforeArchive func()
}

func (d *pruneSpyDB) ArchiveAndCompact(ctx context.Context, sessionID string, compacted []map[string]any, o state.ArchiveCompactOptions) (int, error) {
	d.archiveCalls++
	if d.beforeArchive != nil {
		d.beforeArchive()
	}
	if d.archiveErr != nil {
		return 0, d.archiveErr
	}
	return d.SessionDB.ArchiveAndCompact(ctx, sessionID, compacted, o)
}

func (d *pruneSpyDB) RewritePrunedRows(ctx context.Context, sessionID string, changes []state.PruneChange, o state.PruneRewriteOptions) (int, error) {
	if d.rewriteErr != nil {
		return 0, d.rewriteErr
	}
	return d.SessionDB.RewritePrunedRows(ctx, sessionID, changes, o)
}

// Spy store with the in place writer shadowed away so only the full rewrite remains
type pruneNoRewriterDB struct {
	*pruneSpyDB
	RewritePrunedRows struct{}
}

type pruneStore struct {
	t    *testing.T
	ctx  context.Context
	path string
	db   *state.SessionDB
	raw  *sql.DB
	sid  string
}

// Fresh session database with the session created and the rows appended
func newPruneStore(t *testing.T, sid string, modelConfig map[string]any, rows []map[string]any) *pruneStore {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := state.NewSessionDB(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "telegram", ModelConfig: modelConfig}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	s := &pruneStore{t: t, ctx: ctx, path: path, db: db, raw: raw, sid: sid}
	if rows != nil {
		s.append(rows)
	}
	return s
}

func (s *pruneStore) append(rows []map[string]any) {
	if _, err := s.db.AppendMessagesBatch(s.ctx, s.sid, rows, state.AppendGuards{}, nil); err != nil {
		s.t.Fatal(err)
	}
}

func (s *pruneStore) conversation(o state.ConversationOptions) []map[string]any {
	rows, err := s.db.GetMessagesAsConversation(s.ctx, s.sid, o)
	if err != nil {
		s.t.Fatal(err)
	}
	return rows
}

func (s *pruneStore) contents(rows []map[string]any) []string {
	out := make([]string, len(rows))
	for i, m := range rows {
		out[i] = fmt.Sprint(m["content"])
	}
	return out
}

func (s *pruneStore) live() []string { return s.contents(s.conversation(state.ConversationOptions{})) }

// Runs a raw statement on a connection carrying the cjk tokenizer the FTS triggers name
func (s *pruneStore) exec(query string, args ...any) {
	conn, err := s.raw.Conn(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	defer conn.Close()
	state.LoadFTS5CJKExtension(s.ctx, conn)
	if _, err := conn.ExecContext(s.ctx, query, args...); err != nil {
		s.t.Fatal(err)
	}
}

// Every message row as id role content active compacted, ordered by id
func (s *pruneStore) rows() []string {
	q, err := s.raw.Query("SELECT id, role, content, active, compacted FROM messages WHERE session_id = ? ORDER BY id", s.sid)
	if err != nil {
		s.t.Fatal(err)
	}
	defer q.Close()
	var out []string
	for q.Next() {
		var id, active, compacted int64
		var role string
		var content sql.NullString
		if err := q.Scan(&id, &role, &content, &active, &compacted); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d|%s|%s|%d|%d", id, role, content.String, active, compacted))
	}
	return out
}

func (s *pruneStore) modelConfig(key string) (any, bool) {
	v, ok, err := s.db.GetModelConfigValue(s.sid, key)
	if err != nil {
		s.t.Fatal(err)
	}
	return v, ok
}

func (s *pruneStore) page() []string {
	rows, err := s.db.GetMessages(s.ctx, s.sid, state.GetMessagesOptions{IncludeCompacted: true})
	if err != nil {
		s.t.Fatal(err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		v, _ := r.Get("content")
		out[i] = fmt.Sprint(v)
	}
	return out
}

func (s *pruneStore) scalar(query string, args ...any) int64 {
	var n int64
	if err := s.raw.QueryRow(query, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// User start row then eight terminal pairs, the first three carrying large bodies
func restartHistory() []map[string]any {
	msgs := []map[string]any{{"role": "user", "content": "start"}}
	for i := range 8 {
		id := fmt.Sprintf("call_%d", i)
		content := "ok"
		if i < 3 {
			content = strings.Repeat(string(rune(65+i)), 24_000)
		}
		msgs = append(msgs, pruneCall(id), pruneResult(id, content))
	}
	return msgs
}

// Compressor bound to the session as a freshly built agent binds it, with the pruning knobs set
func restartCompressor(t *testing.T, store any, sid string) *ContextCompressor {
	c := newCompressorWith(t, 128_000, func(o *ContextCompressorOptions) { o.Model = "test/model" })
	c.bindSessionState(store, sid)
	c.proactivePruneTokens, c.proactivePruneMinResultChars, c.proactivePruneMinReclaimTokens = 48_000, 8_000, 4_096
	c.ProtectFirstN, c.ProtectLastN = 2, 4
	return c
}

func asInt(t *testing.T, v any) int {
	var n int
	if _, err := fmt.Sscan(fmt.Sprint(v), &n); err != nil {
		t.Fatalf("not an int: %v", v)
	}
	return n
}

func TestGatewayEvictionReloadKeepsPruneAndDurableRunway(t *testing.T) {
	s := newPruneStore(t, "GATEWAY_PRUNE_RESTART", map[string]any{"keep": "value"}, restartHistory())
	first := restartCompressor(t, s.db, s.sid)
	pruned, count := first.pruneToolResultsOnly(s.conversation(state.ConversationOptions{}), iptr(120_000))
	durable := s.conversation(state.ConversationOptions{})
	if count < 1 || fmt.Sprint(s.contents(durable)) != fmt.Sprint(s.contents(pruned)) || len(durable[2]["content"].(string)) >= 24_000 {
		t.Fatalf("prune not durable: %d", count)
	}
	runway, _ := s.modelConfig(ProactivePruneRearmModelConfigKey)
	keep, _ := s.modelConfig("keep")
	if keep != "value" || asInt(t, runway) <= first.messageBudgetTokens(durable) {
		t.Fatalf("runway %v keep %v", runway, keep)
	}
	resumed := restartCompressor(t, s.db, s.sid)
	if resumed.proactivePruneRearmTokens != asInt(t, runway) {
		t.Fatalf("runway not reloaded: %d", resumed.proactivePruneRearmTokens)
	}
	reloaded := s.conversation(state.ConversationOptions{})
	archived, _ := s.db.GetMessages(s.ctx, s.sid, state.GetMessagesOptions{IncludeInactive: true})
	result, second := resumed.pruneToolResultsOnly(reloaded, iptr(1_000_000))
	after, _ := s.db.GetMessages(s.ctx, s.sid, state.GetMessagesOptions{IncludeInactive: true})
	if !sameSlice(result, reloaded) || second != 0 || len(after) != len(archived) {
		t.Fatalf("second prune %d rows %d -> %d", second, len(archived), len(after))
	}
}

func TestFreshAgentRearmsAfterDurableHistoryRegrowthOnce(t *testing.T) {
	s := newPruneStore(t, "PRUNE_DURABLE_REGROWTH", nil, restartHistory())
	first := restartCompressor(t, s.db, s.sid)
	if _, n := first.pruneToolResultsOnly(s.conversation(state.ConversationOptions{}), iptr(120_000)); n < 1 {
		t.Fatal("first prune did nothing")
	}
	v, _ := s.modelConfig(ProactivePruneRearmModelConfigKey)
	firstRunway := asInt(t, v)
	s.append([]map[string]any{pruneCall("regrown_large"), pruneResult("regrown_large", strings.Repeat("z", 240_000)),
		pruneCall("tail_1"), pruneResult("tail_1", "ok"), pruneCall("tail_2"), pruneResult("tail_2", "ok")})
	resumed := restartCompressor(t, s.db, s.sid)
	grown := s.conversation(state.ConversationOptions{})
	if resumed.messageBudgetTokens(grown) < firstRunway {
		t.Fatal("regrowth short of the runway")
	}
	if _, n := resumed.pruneToolResultsOnly(grown, iptr(1_000_000)); n < 1 {
		t.Fatal("regrown history not pruned")
	}
	v, _ = s.modelConfig(ProactivePruneRearmModelConfigKey)
	secondRunway := asInt(t, v)
	if secondRunway <= firstRunway {
		t.Fatalf("runway %d not past %d", secondRunway, firstRunway)
	}
	restarted := restartCompressor(t, s.db, s.sid)
	durable := s.conversation(state.ConversationOptions{})
	result, third := restarted.pruneToolResultsOnly(durable, iptr(1_000_000))
	if !sameSlice(result, durable) || third != 0 || restarted.proactivePruneRearmTokens != secondRunway {
		t.Fatalf("third prune %d runway %d", third, restarted.proactivePruneRearmTokens)
	}
}

func TestPrunePersistenceFailureIsANoop(t *testing.T) {
	for _, writer := range []string{"rewrite_pruned_rows", "archive_and_compact"} {
		t.Run(writer, func(t *testing.T) {
			s := newPruneStore(t, "PRUNE_PERSISTENCE_FAILURE", nil, restartHistory())
			c := restartCompressor(t, s.db, s.sid)
			messages := s.conversation(state.ConversationOptions{})
			original := fmt.Sprint(s.contents(messages))
			spy := &pruneSpyDB{SessionDB: s.db}
			var store any = spy
			if writer == "archive_and_compact" {
				spy.archiveErr = errors.New("disk full")
				store = &pruneNoRewriterDB{pruneSpyDB: spy}
			} else {
				spy.rewriteErr = errors.New("disk full")
			}
			c.sessionDB = store
			result, count := c.pruneToolResultsOnly(messages, iptr(120_000))
			_, has := s.modelConfig(ProactivePruneRearmModelConfigKey)
			if !sameSlice(result, messages) || count != 0 || c.proactivePruneRearmTokens != 0 || has ||
				fmt.Sprint(s.contents(messages)) != original || fmt.Sprint(s.live()) != original {
				t.Fatalf("failed write was not a no-op: %d", count)
			}
		})
	}
}

func TestModelSwitchClearsDurableRunway(t *testing.T) {
	s := newPruneStore(t, "MODEL_SWITCH_CLEARS_RUNWAY", map[string]any{"keep": "value", ProactivePruneRearmModelConfigKey: 120_000}, nil)
	c := restartCompressor(t, s.db, s.sid)
	if c.proactivePruneRearmTokens != 120_000 {
		t.Fatalf("runway not loaded: %d", c.proactivePruneRearmTokens)
	}
	c.UpdateModel("other/model", 200_000, "", "", "", "")
	_, has := s.modelConfig(ProactivePruneRearmModelConfigKey)
	keep, _ := s.modelConfig("keep")
	if c.proactivePruneRearmTokens != 0 || has || keep != "value" {
		t.Fatalf("runway survived the switch: %d %v", c.proactivePruneRearmTokens, has)
	}
}

func TestPatchSessionModelConfigMergeAndDelete(t *testing.T) {
	s := newPruneStore(t, "PATCH_MODEL_CONFIG", map[string]any{"keep": "value", "drop": 1}, nil)
	if err := s.db.PatchModelConfig(s.sid, map[string]any{"drop": nil, "added": 7}); err != nil {
		t.Fatal(err)
	}
	session, _ := s.db.GetSession(s.ctx, s.sid)
	raw, _ := session.Get("model_config")
	got, err := pyjson.Loads(fmt.Sprint(raw))
	dumped, _ := pyjson.Dumps(got)
	if err != nil || dumped != `{"keep": "value", "added": 7}` {
		t.Fatalf("model config %v %v", raw, err)
	}
	if err := s.db.PatchModelConfig("NO_SUCH_SESSION", map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.PatchModelConfig(s.sid, map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

func TestInPlacePruneGrowsByChangedRowsOnly(t *testing.T) {
	s := newPruneStore(t, "PRUNE_IN_PLACE_GROWTH", nil, restartHistory())
	c := restartCompressor(t, s.db, s.sid)
	before := s.rows()
	messages := s.conversation(state.ConversationOptions{})
	pruned, count := c.pruneToolResultsOnly(messages, iptr(120_000))
	var changed []int
	for i := range messages {
		if fmt.Sprint(messages[i]["content"]) != fmt.Sprint(pruned[i]["content"]) {
			changed = append(changed, i)
		}
	}
	if count < 1 || len(changed) == 0 {
		t.Fatal("nothing pruned")
	}
	after := s.rows()
	var liveIDs, beforeIDs []string
	twins := 0
	for _, r := range after {
		f := strings.Split(r, "|")
		if f[3] == "1" {
			liveIDs = append(liveIDs, f[0])
		} else {
			twins++
			if f[4] != "1" {
				t.Fatalf("demoted twin not searchable: %s", r[:40])
			}
		}
	}
	for _, r := range before {
		beforeIDs = append(beforeIDs, strings.SplitN(r, "|", 2)[0])
	}
	if len(after) != len(before)+len(changed) || twins != len(changed) || fmt.Sprint(liveIDs) != fmt.Sprint(beforeIDs) {
		t.Fatalf("rows %d -> %d with %d changed", len(before), len(after), len(changed))
	}
	if fmt.Sprint(s.live()) != fmt.Sprint(s.contents(pruned)) || len(s.page()) != len(pruned) {
		t.Fatal("durable view differs from the pruned transcript")
	}
	for _, m := range pruned {
		if m[dbPersistedMarker] != true {
			t.Fatal("pruned row not stamped persisted")
		}
	}
}

func TestInPlacePruneDisplaySurvivesLaterCompactionAndBackfill(t *testing.T) {
	for _, shape := range []string{"first-generation", "carried", "carried-legacy-slots"} {
		t.Run(shape, func(t *testing.T) {
			s := newPruneStore(t, "PRUNE_IN_PLACE_DISPLAY", nil, restartHistory())
			if shape != "first-generation" {
				if _, err := s.db.ArchiveAndCompact(s.ctx, s.sid, s.conversation(state.ConversationOptions{}), state.ArchiveCompactOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if shape == "carried-legacy-slots" {
				s.exec("UPDATE messages SET display_order = NULL, display_identity = NULL WHERE session_id = ?", s.sid)
			}
			c := restartCompressor(t, s.db, s.sid)
			pruned, count := c.pruneToolResultsOnly(s.conversation(state.ConversationOptions{}), iptr(120_000))
			if count < 1 {
				t.Fatal("nothing pruned")
			}
			if n := s.scalar("SELECT COUNT(*) FROM messages WHERE session_id = ? AND (active = 1 OR compacted = 1) AND (display_order IS NULL OR display_identity IS NULL)", s.sid); n != 0 {
				t.Fatalf("%d rows without a display slot", n)
			}
			expected := s.contents(pruned)
			if fmt.Sprint(s.page()) != fmt.Sprint(expected) {
				t.Fatal("page differs right after the prune")
			}
			if _, err := s.db.ArchiveAndCompact(s.ctx, s.sid, s.conversation(state.ConversationOptions{}), state.ArchiveCompactOptions{}); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(s.page()) != fmt.Sprint(expected) {
				t.Fatal("page differs after a later compaction")
			}
			s.append([]map[string]any{{"role": "user", "content": "@notes.md"}})
			rowID := s.scalar("SELECT MAX(id) FROM messages WHERE session_id = ?", s.sid)
			if n, err := s.db.SetUserMessageContent(s.ctx, s.sid, rowID, "expanded notes", nil); err != nil || n != 1 {
				t.Fatalf("backfill %d %v", n, err)
			}
			if fmt.Sprint(s.page()) != fmt.Sprint(append(expected, "expanded notes")) {
				t.Fatal("page differs after the backfill")
			}
		})
	}
}

func TestInPlacePruneTwiceKeepsOneSearchHitPerUnchangedTurn(t *testing.T) {
	s := newPruneStore(t, "PRUNE_IN_PLACE_SEARCH", nil, restartHistory())
	c := restartCompressor(t, s.db, s.sid)
	if _, n := c.pruneToolResultsOnly(s.conversation(state.ConversationOptions{}), iptr(120_000)); n < 1 {
		t.Fatal("first prune did nothing")
	}
	var grown []map[string]any
	for i := 8; i < 16; i++ {
		id := fmt.Sprintf("call_%d", i)
		grown = append(grown, pruneCall(id), pruneResult(id, strings.Repeat(string(rune(65+i%26)), 24_000)))
	}
	s.append(grown)
	rowsBefore := len(s.rows())
	_, second := c.pruneToolResultsOnly(s.conversation(state.ConversationOptions{}), iptr(1_000_000))
	if second < 1 || len(s.rows())-rowsBefore != second {
		t.Fatalf("second prune %d grew rows by %d", second, len(s.rows())-rowsBefore)
	}
	hits, err := s.db.SearchMessages(s.ctx, "start", state.NewSearchMessagesOptions())
	n := 0
	for _, h := range hits {
		if sid, _ := h.Get("session_id"); sid == s.sid {
			n++
		}
	}
	if err != nil || n != 1 {
		t.Fatalf("search hits %d %v", n, err)
	}
}

// Prune over a held transcript through a call counting store, expecting a full no-op
func assertStalePruneNoOp(t *testing.T, s *pruneStore, c *ContextCompressor, held []map[string]any, rowsBefore []string, liveWant string) {
	t.Helper()
	spy := &pruneSpyDB{SessionDB: s.db}
	c.sessionDB = spy
	result, count := c.pruneToolResultsOnly(held, iptr(120_000))
	if !sameSlice(result, held) || count != 0 || spy.archiveCalls != 0 {
		t.Fatalf("stale prune committed: count=%d full=%d", count, spy.archiveCalls)
	}
	if rowsBefore != nil && fmt.Sprint(s.rows()) != fmt.Sprint(rowsBefore) {
		t.Fatal("rows changed")
	}
	if strings.Count(strings.Join(s.live(), "\x00"), liveWant) != 1 {
		t.Fatalf("%q lost its single active copy", liveWant)
	}
}

func TestInPlacePruneSkipsWhenAnotherWriterChangedTheStoredBody(t *testing.T) {
	s := newPruneStore(t, "PRUNE_IN_PLACE_STALE", nil, restartHistory())
	c := restartCompressor(t, s.db, s.sid)
	messages := s.conversation(state.ConversationOptions{})
	s.exec("UPDATE messages SET content = 'edited elsewhere' WHERE session_id = ? AND tool_call_id = 'call_0'", s.sid)
	assertStalePruneNoOp(t, s, c, messages, s.rows(), "edited elsewhere")
}

func TestInPlacePruneFallsBackToFullRewriteWhenARowCannotBeNamed(t *testing.T) {
	s := newPruneStore(t, "PRUNE_IN_PLACE_UNRESOLVED", nil, restartHistory())
	c := restartCompressor(t, s.db, s.sid)
	messages := s.conversation(state.ConversationOptions{})
	q, err := s.raw.Query("PRAGMA table_info(messages)")
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	for q.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := q.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name != "id" && name != "display_order" {
			columns = append(columns, name)
		}
	}
	q.Close()
	cols := strings.Join(columns, ", ")
	s.exec("INSERT INTO messages ("+cols+") SELECT "+cols+" FROM messages WHERE session_id = ? AND tool_call_id = 'call_0' AND active = 1", s.sid)
	spy := &pruneSpyDB{SessionDB: s.db}
	c.sessionDB = spy
	pruned, count := c.pruneToolResultsOnly(messages, iptr(120_000))
	if count < 1 || spy.archiveCalls != 1 || fmt.Sprint(s.live()) != fmt.Sprint(s.contents(pruned)) {
		t.Fatalf("full rewrite fallback: count=%d full=%d", count, spy.archiveCalls)
	}
}

func TestRewritePrunedRowsRefusesUserRows(t *testing.T) {
	s := newPruneStore(t, "PRUNE_IN_PLACE_USER", nil, restartHistory())
	messages := s.conversation(state.ConversationOptions{})
	before := s.rows()
	replacement := withoutKeys(messages[0], nil)
	replacement["content"] = "rewritten"
	_, err := s.db.RewritePrunedRows(s.ctx, s.sid, []state.PruneChange{{Original: messages[0], Replacement: replacement}}, state.PruneRewriteOptions{})
	var unresolved *state.PruneRowUnresolvedError
	if !errors.As(err, &unresolved) || fmt.Sprint(s.rows()) != fmt.Sprint(before) {
		t.Fatalf("user row rewrite not refused: %v", err)
	}
}

func TestAStaleGenerationWithAnUnstampedTrailingRowStillAborts(t *testing.T) {
	s := newPruneStore(t, "PRUNE_STALE_PREFIX", nil, restartHistory())
	c := restartCompressor(t, s.db, s.sid)
	messages := append(s.conversation(state.ConversationOptions{}), map[string]any{"role": "user", "content": "live turn"})
	s.exec("UPDATE messages SET content = 'edited elsewhere' WHERE session_id = ? AND tool_call_id = 'call_0'", s.sid)
	assertStalePruneNoOp(t, s, c, messages, s.rows(), "edited elsewhere")
}

// History with whitespace padded user and assistant text around the terminal pairs
func paddedRestartHistory() []map[string]any {
	msgs := restartHistory()
	msgs[0]["content"] = "  start \n"
	return append(msgs, map[string]any{"role": "assistant", "content": "  done \n"}, map[string]any{"role": "user", "content": "wrap up"})
}

func TestAFreshlyResumedUnchangedTranscriptIsNotStale(t *testing.T) {
	for _, loader := range []string{"reload", "resume_projection"} {
		t.Run(loader, func(t *testing.T) {
			s := newPruneStore(t, "PRUNE_NOT_STALE_WHITESPACE", nil, paddedRestartHistory())
			c := restartCompressor(t, s.db, s.sid)
			var messages []map[string]any
			if loader == "resume_projection" {
				var err error
				if messages, _, err = s.db.GetResumeConversations(s.ctx, s.sid); err != nil {
					t.Fatal(err)
				}
			} else {
				messages = s.conversation(state.ConversationOptions{IncludeRowIDs: true})
			}
			if ids, err := s.db.RewrittenHeldRowIDs(s.ctx, s.sid, messages); err != nil || len(ids) != 0 {
				t.Fatalf("unchanged rows reported rewritten: %v %v", ids, err)
			}
			spy := &pruneSpyDB{SessionDB: s.db}
			c.sessionDB = spy
			result, count := c.pruneToolResultsOnly(messages, iptr(120_000))
			if (sameSlice(result, messages) && count == 0) || spy.archiveCalls != 0 {
				t.Fatalf("prune disabled or not in place: count=%d full=%d", count, spy.archiveCalls)
			}
		})
	}
}

func TestAnAssistantToolCallsRewriteIsStaleWithAndWithoutASnapshot(t *testing.T) {
	s := newPruneStore(t, "PRUNE_STALE_TOOL_CALLS", nil, restartHistory())
	messages := s.conversation(state.ConversationOptions{IncludeRowIDs: true})
	var held map[string]any
	for _, m := range messages {
		if m["role"] == "assistant" && m["tool_calls"] != nil {
			held = m
			break
		}
	}
	stored := held["tool_calls"].([]any)[0]
	fn := map[string]any{"name": state.ToolCallField(state.ToolCallField(stored, "function"), "name"), "arguments": `{"cmd":"winner"}`}
	call := map[string]any{"id": state.ToolCallField(stored, "id"), "type": state.ToolCallField(stored, "type"), "function": fn}
	s.exec("UPDATE messages SET tool_calls = ? WHERE session_id = ? AND id = ?", mustDumps(t, []any{call}), s.sid, held["_row_id"])
	replacement := withoutKeys(held, nil)
	replacement["content"] = "pruned stub"
	_, err := s.db.RewritePrunedRows(s.ctx, s.sid, []state.PruneChange{{Original: withoutKeys(held, nil), Replacement: replacement}}, state.PruneRewriteOptions{})
	var stale *state.PruneRowStaleError
	if !errors.As(err, &stale) {
		t.Fatalf("stale call arguments not refused: %v", err)
	}
	c := restartCompressor(t, s.db, s.sid)
	before := s.rows()
	spy := &pruneSpyDB{SessionDB: s.db}
	c.sessionDB = spy
	result, count := c.pruneToolResultsOnly(messages, iptr(120_000))
	if !sameSlice(result, messages) || count != 0 || spy.archiveCalls != 0 || fmt.Sprint(s.rows()) != fmt.Sprint(before) {
		t.Fatalf("stale assistant row committed: count=%d full=%d", count, spy.archiveCalls)
	}
}

// Held transcript with an unwritten user row spliced before the first durable row after the user row
func spliceUnflushed(messages []map[string]any, afterRow func(i int, m map[string]any) bool) []map[string]any {
	for i, m := range messages {
		if m["_row_id"] != nil && afterRow(i, m) {
			out := append([]map[string]any{}, messages[:i]...)
			out = append(out, map[string]any{"role": "user", "content": "live turn not yet flushed"})
			return append(out, messages[i:]...)
		}
	}
	return nil
}

func firstUserRow(messages []map[string]any) (int, map[string]any) {
	for i, m := range messages {
		if m["role"] == "user" {
			return i, m
		}
	}
	return -1, nil
}

func rowIDOf(m map[string]any) int64 {
	var n int64
	fmt.Sscan(fmt.Sprint(m["_row_id"]), &n)
	return n
}

func TestAConcurrentUserEditStaysActiveWhenThePruneFallsBack(t *testing.T) {
	s := newPruneStore(t, "PRUNE_STALE_USER_EDIT_FALLBACK", nil, restartHistory())
	c := restartCompressor(t, s.db, s.sid)
	messages := s.conversation(state.ConversationOptions{IncludeRowIDs: true})
	_, user := firstUserRow(messages)
	if n, err := s.db.SetUserMessageContent(s.ctx, s.sid, rowIDOf(user), "edited by the user elsewhere", nil); err != nil || n != 1 {
		t.Fatalf("edit %d %v", n, err)
	}
	held := spliceUnflushed(messages, func(_ int, m map[string]any) bool { return rowIDOf(m) > rowIDOf(user) })
	assertStalePruneNoOp(t, s, c, held, s.rows(), "edited by the user elsewhere")
	for _, text := range s.live() {
		if text == "start" {
			t.Fatal("stale pre-edit body republished")
		}
	}
}

func TestTheFallbackStillCommitsWhenNoHeldRowWasRewritten(t *testing.T) {
	s := newPruneStore(t, "PRUNE_FALLBACK_CLEAN_COMMIT", nil, restartHistory())
	c := restartCompressor(t, s.db, s.sid)
	messages := s.conversation(state.ConversationOptions{IncludeRowIDs: true})
	first, _ := firstUserRow(messages)
	held := spliceUnflushed(messages, func(i int, _ map[string]any) bool { return i > first })
	spy := &pruneSpyDB{SessionDB: s.db}
	c.sessionDB = spy
	result, count := c.pruneToolResultsOnly(held, iptr(120_000))
	live := strings.Join(s.live(), "\x00")
	if (sameSlice(result, held) && count == 0) || spy.archiveCalls != 1 ||
		strings.Count("\x00"+live+"\x00", "\x00start\x00") != 1 || strings.Count(live, "live turn not yet flushed") != 1 {
		t.Fatalf("clean fallback did not commit once: count=%d full=%d", count, spy.archiveCalls)
	}
}

func TestAUserEditLandingAfterThePrecheckStaysActiveInTheCommit(t *testing.T) {
	s := newPruneStore(t, "PRUNE_CAS_COMMIT_WINDOW", nil, restartHistory())
	c := restartCompressor(t, s.db, s.sid)
	messages := s.conversation(state.ConversationOptions{IncludeRowIDs: true})
	_, user := firstUserRow(messages)
	held := spliceUnflushed(messages, func(_ int, m map[string]any) bool { return rowIDOf(m) > rowIDOf(user) })
	other, err := state.NewSessionDB(s.ctx, s.path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	spy := &pruneSpyDB{SessionDB: s.db, beforeArchive: func() {
		if n, err := other.SetUserMessageContent(s.ctx, s.sid, rowIDOf(user), "edited inside the window", nil); err != nil || n != 1 {
			t.Errorf("edit inside the window %d %v", n, err)
		}
	}}
	c.sessionDB = spy
	result, count := c.pruneToolResultsOnly(held, iptr(120_000))
	live := s.live()
	if !sameSlice(result, held) || count != 0 || strings.Count(strings.Join(live, "\x00"), "edited inside the window") != 1 {
		t.Fatalf("commit window edit lost: count=%d", count)
	}
	for _, text := range live {
		if text == "start" {
			t.Fatal("stale pre-edit body republished")
		}
	}
}

type runwayReadErrDB struct{ *state.SessionDB }

func (runwayReadErrDB) GetSessionModelConfigValue(context.Context, string, string, any) (any, error) {
	return nil, errors.New("database is locked")
}

func TestDurableRunwayReadErrorSkipsTheRestore(t *testing.T) {
	s := newPruneStore(t, "RUNWAY_READ_ERROR", map[string]any{ProactivePruneRearmModelConfigKey: 120_000}, nil)
	c := restartCompressor(t, s.db, s.sid)
	if c.proactivePruneRearmTokens != 120_000 {
		t.Fatalf("stored int64 runway not decoded: %d", c.proactivePruneRearmTokens)
	}
	c.proactivePruneRearmTokens = 7
	c.sessionDB = runwayReadErrDB{s.db}
	c.loadProactivePruneRearmTokens()
	if c.proactivePruneRearmTokens != 7 {
		t.Fatalf("failed read overwrote the runway: %d", c.proactivePruneRearmTokens)
	}
	if found, v := c.durableRead("get_session_model_config_value", "proactive prune runway", false, 0, nil, nil, ProactivePruneRearmModelConfigKey, 0); found || v != 0 {
		t.Fatalf("failed read reported %v %v", found, v)
	}
	if err := s.db.PatchModelConfig(s.sid, map[string]any{ProactivePruneRearmModelConfigKey: 90_000.9}); err != nil {
		t.Fatal(err)
	}
	c.sessionDB = s.db
	c.loadProactivePruneRearmTokens()
	if c.proactivePruneRearmTokens != 90_000 {
		t.Fatalf("stored float runway not truncated like int(): %d", c.proactivePruneRearmTokens)
	}
}

func TestInPlacePruneFallsBackOnlyOnANonStaleUnresolvedRow(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		archives int
	}{
		{"unresolved", &state.PruneRowUnresolvedError{Msg: "row 7 has no id"}, 1},
		{"stale", &state.PruneRowStaleError{Msg: "row 7 changed"}, 0},
		{"other", errors.New("disk full"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newPruneStore(t, "PRUNE_FALLBACK_"+strings.ToUpper(tc.name), nil, restartHistory())
			c := restartCompressor(t, s.db, s.sid)
			messages := s.conversation(state.ConversationOptions{})
			spy := &pruneSpyDB{SessionDB: s.db, rewriteErr: tc.err}
			c.sessionDB = spy
			_, count := c.pruneToolResultsOnly(messages, iptr(120_000))
			if spy.archiveCalls != tc.archives || (tc.archives == 0 && count != 0) || (tc.archives == 1 && count == 0) {
				t.Fatalf("archives %d count %d", spy.archiveCalls, count)
			}
		})
	}
}

func TestInPlacePruneLeavesUnwrittenSuffixToTheFlush(t *testing.T) {
	s := newPruneStore(t, "PRUNE_IN_PLACE_SUFFIX", nil, restartHistory())
	c := restartCompressor(t, s.db, s.sid)
	a := &AIAgent{sessionDB: s.db, sessionID: s.sid, model: "test/model", quietMode: true, sessionPersistLock: &pythread.RLock{}, sessionDBCreated: true}
	messages := append(s.conversation(state.ConversationOptions{}), map[string]any{"role": "user", "content": "new turn"})
	pruned, count := c.pruneToolResultsOnly(messages, iptr(120_000))
	if count < 1 {
		t.Fatalf("count = %d", count)
	}
	if v, _ := pruned[len(pruned)-1]["_db_persisted"].(bool); v {
		t.Fatal("trailing live turn stamped persisted by the prune")
	}
	if slices.Contains(s.live(), "new turn") {
		t.Fatal("prune wrote the unflushed suffix")
	}
	if ok, err := a.flushMessagesToSessionDB(pruned); !ok || err != nil {
		t.Fatalf("flush failed: %v %v", ok, err)
	}
	durable := s.live()
	if !slices.Equal(durable, s.contents(pruned)) {
		t.Fatalf("durable %q\nwant %q", durable, s.contents(pruned))
	}
	n := 0
	for _, c := range durable {
		if c == "new turn" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("new turn stored %d times", n)
	}
}
