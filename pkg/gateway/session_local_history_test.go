package gateway

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
)

const recoverSummary = state.SummaryPrefix + "\nearlier turns summarized"

// Stand in runner with a real session store, a live adapter map and no cached agent
type recoverTestRunner struct {
	store    *SessionStore
	adapters *pydict.Dict[Platform, PlatformAdapter]
	profiles *pydict.Dict[string, *pydict.Dict[Platform, PlatformAdapter]]
	registry *SessionAuthorities
	launch   *SessionAuthority
	draining bool
	evicted  []string
	restored func(entry *SessionEntry) *SessionSource
}

func (r *recoverTestRunner) SessionRuntimeDescriptor() map[string]any          { return nil }
func (r *recoverTestRunner) Draining() bool                                    { return r.draining }
func (r *recoverTestRunner) Running() bool                                     { return true }
func (r *recoverTestRunner) SessionStore() *SessionStore                       { return r.store }
func (r *recoverTestRunner) MultiplexProfiles() bool                           { return false }
func (r *recoverTestRunner) SessionAuthorities() *SessionAuthorities           { return r.registry }
func (r *recoverTestRunner) SessionAuthority() *SessionAuthority               { return r.launch }
func (r *recoverTestRunner) EvictCachedAgent(route string)                     { r.evicted = append(r.evicted, route) }
func (r *recoverTestRunner) Adapters() *pydict.Dict[Platform, PlatformAdapter] { return r.adapters }
func (r *recoverTestRunner) AdapterForSource(ctx context.Context, source *SessionSource) (PlatformAdapter, error) {
	adapter, _ := r.adapters.Get(source.Platform)
	return adapter, nil
}
func (r *recoverTestRunner) AdaptersForProfile(profile *string) *pydict.Dict[Platform, PlatformAdapter] {
	if profile == nil {
		return r.adapters
	}
	adapters, _ := r.profiles.Get(*profile)
	return adapters
}
func (r *recoverTestRunner) ProfileAdapterMap(name string) *pydict.Dict[Platform, PlatformAdapter] {
	if adapters, _ := r.profiles.Get(name); adapters != nil {
		return adapters
	}
	adapters := pydict.New[Platform, PlatformAdapter]()
	r.profiles.Set(name, adapters)
	return adapters
}
func (r *recoverTestRunner) RestoredSource(ctx context.Context, entry *SessionEntry) (*SessionSource, error) {
	if r.restored != nil {
		return r.restored(entry), nil
	}
	return entry.Origin, nil
}

// Authority over a real store and state database, as initialize_session_authority builds one
func newRecoverAuthority(t *testing.T) (context.Context, *SessionAuthority, *recoverTestRunner) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	epoch, err := db.BeginRuntimeEpoch(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(ctx, filepath.Join(t.TempDir(), "sessions"), DefaultGatewayConfig(ctx), nil)
	store.PinDB(db)
	key, err := pypath.Resolve(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	store.bindLocalAuthorityEpoch(key, epoch, true)
	runner := &recoverTestRunner{store: store, adapters: pydict.New[Platform, PlatformAdapter](), profiles: pydict.New[string, *pydict.Dict[Platform, PlatformAdapter]]()}
	a := NewSessionAuthority(runner, "fixture", "first", db, epoch)
	runner.launch = a
	return ctx, a, runner
}

func recoverActor() Principal {
	return Principal{Subject: "owner", ProfileID: "fixture", TransportID: "sock",
		Capabilities: map[string]bool{"session:create": true, "session:read": true, "session:control": true}}
}

// Installs the prefill loaders the process wiring installs from the CLI config loader
func installPrefillLoaders(t *testing.T) {
	t.Helper()
	prevResolve, prevLoad := ResolvePrefillMessagesFile, LoadPrefillMessages
	ResolvePrefillMessagesFile = func(ctx context.Context, cfg map[string]any) string {
		if text := strings.TrimSpace(state.SessionEnv(ctx, "ICEBATH_PREFILL_MESSAGES_FILE", "")); text != "" {
			return text
		}
		if text, _ := cfg["prefill_messages_file"].(string); strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
		agentCfg, _ := cfg["agent"].(map[string]any)
		text, _ := agentCfg["prefill_messages_file"].(string)
		return strings.TrimSpace(text)
	}
	LoadPrefillMessages = func(ctx context.Context, filePath, baseDir string) []any {
		if filePath == "" {
			return []any{}
		}
		if !filepath.IsAbs(filePath) {
			filePath = filepath.Join(baseDir, filePath)
		}
		raw, err := os.ReadFile(filePath)
		if err != nil {
			return []any{}
		}
		decoded, err := pyjson.Loads(string(raw))
		if err != nil {
			return []any{}
		}
		list, _ := pyjson.Plain(decoded).([]any)
		return list
	}
	t.Cleanup(func() { ResolvePrefillMessagesFile, LoadPrefillMessages = prevResolve, prevLoad })
}

// Creates a local session through create_local_session and names its physical transcript
func seedRecoverSession(t *testing.T, ctx context.Context, a *SessionAuthority, actor Principal, params map[string]any) (SessionRef, string) {
	t.Helper()
	installPrefillLoaders(t)
	ref, err := createLocalSession(ctx, a, actor, params, localCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	physical, err := receiptEntryTarget(ctx, a.DB, ref.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return ref, physical
}

// Fixture session of the resume cases, a frozen gui launch with no toolsets
func recoverLocalSession(t *testing.T) (context.Context, *SessionAuthority, Principal, SessionRef, string) {
	t.Helper()
	ctx, a, _ := newRecoverAuthority(t)
	actor := recoverActor()
	ref, physical := seedRecoverSession(t, ctx, a, actor, map[string]any{
		"request_id": "resume", "source": "gui", "cwd": t.TempDir(), "model": "frozen", "toolsets": []any{}})
	return ctx, a, actor, ref, physical
}

func recoverAppend(t *testing.T, ctx context.Context, db *state.SessionDB, sid string, texts ...string) {
	t.Helper()
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": role, "content": text}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
}

func recoverCompact(t *testing.T, ctx context.Context, db *state.SessionDB, physical, mode string) string {
	t.Helper()
	compacted := []map[string]any{{"role": "user", "content": recoverSummary}, {"role": "user", "content": "TAIL_Q"},
		{"role": "assistant", "content": "TAIL_A"}}
	if mode == "in_place" {
		if _, err := db.ArchiveAndCompact(ctx, physical, compacted, state.ArchiveCompactOptions{TailCount: 2}); err != nil {
			t.Fatal(err)
		}
		return physical
	}
	if !db.TryAcquireCompressionLock(physical, "fixture", 300.0) {
		t.Fatal("compression lock not acquired")
	}
	defer db.ReleaseCompressionLock(physical, "fixture")
	holder := "fixture"
	if err := db.PublishCompressionChild(ctx, state.CompressionChildPublish{ParentSessionID: physical, ChildSessionID: "child",
		Source: "gui", Messages: compacted, CompressionLockHolder: &holder}); err != nil {
		t.Fatal(err)
	}
	return "child"
}

func recoverRowID(t *testing.T, v any) int64 {
	t.Helper()
	n, err := pyint.Of(v)
	if err != nil {
		t.Fatal(err)
	}
	return n.Int64()
}

// Stored ids of the active messages of a physical session
func recoverStoredIDs(t *testing.T, ctx context.Context, db *state.SessionDB, sid string) []int64 {
	t.Helper()
	messages, err := db.GetMessages(ctx, sid, state.GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(messages))
	for i, m := range messages {
		id, _ := m.Get("id")
		ids[i] = recoverRowID(t, id)
	}
	return ids
}

func recoverLiveIDs(t *testing.T, rows []map[string]any) []int64 {
	t.Helper()
	ids := []int64{}
	for _, row := range rows {
		if !pyTruthy(row["archived"]) {
			ids = append(ids, recoverRowID(t, row["row_id"]))
		}
	}
	return ids
}

func recoverShape(rows []map[string]any) [][2]any {
	shape := make([][2]any, len(rows))
	for i, row := range rows {
		shape[i] = [2]any{row["content"], pyTruthy(row["archived"])}
	}
	return shape
}

func recoverDumpLen(t *testing.T, rows []map[string]any) int {
	t.Helper()
	text, err := pyjson.Dumps(rows)
	if err != nil {
		t.Fatal(err)
	}
	return len(text)
}

func TestResumeShowsTheTurnsACompressionArchived(t *testing.T) {
	for _, mode := range []string{"in_place", "rotated"} {
		t.Run(mode, func(t *testing.T) {
			ctx, a, _, ref, physical := recoverLocalSession(t)
			db := a.DB
			recoverAppend(t, ctx, db, physical, "OLD_Q", "OLD_A", "REWOUND_Q", "REWOUND_A")
			messages, err := db.GetMessages(ctx, physical, state.GetMessagesOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var rewound int64
			for _, m := range messages {
				if content, _ := m.Get("content"); content == "REWOUND_Q" {
					id, _ := m.Get("id")
					rewound = recoverRowID(t, id)
				}
			}
			if _, err := db.RewindToMessage(ctx, physical, rewound, state.RewindOptions{}); err != nil {
				t.Fatal(err)
			}
			recoverAppend(t, ctx, db, physical, "TAIL_Q", "TAIL_A")
			tip := recoverCompact(t, ctx, db, physical, mode)
			rows, err := localHistory(ctx, a, ref, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := [][2]any{{"OLD_Q", true}, {"OLD_A", true}, {recoverSummary, false}, {"TAIL_Q", false}, {"TAIL_A", false}}
			if got := recoverShape(rows); !reflect.DeepEqual(got, want) {
				t.Fatalf("rows %v", got)
			}
			if got, stored := recoverLiveIDs(t, rows), recoverStoredIDs(t, ctx, db, tip); !reflect.DeepEqual(got, stored) {
				t.Fatalf("live row ids %v, stored %v", got, stored)
			}
		})
	}
}

func TestCliUndoPrefixKeepsTheSummaryAfterARotatedCompression(t *testing.T) {
	ctx, a, _, ref, physical := recoverLocalSession(t)
	db := a.DB
	recoverAppend(t, ctx, db, physical, "OLD_Q", "OLD_A", "TAIL_Q", "TAIL_A")
	recoverCompact(t, ctx, db, physical, "rotated")
	recoverAppend(t, ctx, db, "child", "NEW_Q", "NEW_A")
	rows, err := localHistory(ctx, a, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	var live []map[string]any
	for _, row := range rows {
		if !pyTruthy(row["archived"]) {
			live = append(live, row)
		}
	}
	indices := state.UserTurnIndices(live)
	prefix, turn, err := state.HistoryBeforeUserOriginatedTurn(live, indices[len(indices)-1])
	if err != nil {
		t.Fatal(err)
	}
	if turn["content"] != "NEW_Q" {
		t.Fatalf("turn %v", turn["content"])
	}
	var contents []any
	for _, m := range prefix {
		contents = append(contents, m["content"])
	}
	if !reflect.DeepEqual(contents, []any{recoverSummary, "TAIL_Q", "TAIL_A"}) {
		t.Fatalf("prefix %v", contents)
	}
}

func TestALongArchivedHistoryKeepsTheResumeReplyBounded(t *testing.T) {
	ctx, a, _, ref, physical := recoverLocalSession(t)
	db := a.DB
	for n := 0; n < 40; n++ {
		recoverAppend(t, ctx, db, physical, fmt.Sprintf("Q%d ", n)+strings.Repeat("x", 120_000), fmt.Sprintf("A%d", n))
	}
	recoverAppend(t, ctx, db, physical, "TAIL_Q", "TAIL_A")
	tip := recoverCompact(t, ctx, db, physical, "in_place")
	for n := 0; n < 24; n++ {
		recoverAppend(t, ctx, db, tip, fmt.Sprintf("LIVE%d ", n)+strings.Repeat("l", 100_000), fmt.Sprintf("R%d", n))
	}
	rows, err := localHistory(ctx, a, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if 2*recoverDumpLen(t, rows) >= 8*1024*1024 {
		t.Fatalf("reply rows take %d bytes twice over", recoverDumpLen(t, rows))
	}
	if !pyTruthy(rows[0]["archived_truncated"]) || rows[len(rows)-1]["content"] != "R23" {
		t.Fatalf("first %v last %v", rows[0]["archived_truncated"], rows[len(rows)-1]["content"])
	}
	if got, stored := recoverLiveIDs(t, rows), recoverStoredIDs(t, ctx, db, tip); !reflect.DeepEqual(got, stored) {
		t.Fatalf("live row ids %v, stored %v", got, stored)
	}
	var archived []string
	for _, row := range rows {
		if pyTruthy(row["archived"]) && row["role"] == "user" {
			archived = append(archived, row["content"].(string)[:4])
		}
	}
	if len(archived) == 0 || archived[len(archived)-1] != "Q39 " {
		t.Fatalf("archived %v", archived)
	}
	for _, head := range archived {
		if head == "Q0 x" {
			t.Fatal("the oldest archived row was kept")
		}
	}
}

func TestARowAppendedDuringPrepareCountsAgainstTheArchiveBudget(t *testing.T) {
	ctx, a, _, ref, physical := recoverLocalSession(t)
	db := a.DB
	for n := 0; n < 28; n++ {
		role := "user"
		if n%2 == 1 {
			role = "assistant"
		}
		fields := map[string]any{"role": role, "content": fmt.Sprintf("%d:", n) + strings.Repeat("h", 100*1024)}
		if _, err := db.AppendMessage(ctx, physical, fields, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ArchiveAndCompact(ctx, physical, []map[string]any{{"role": "user", "content": recoverSummary}}, state.ArchiveCompactOptions{}); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareHistory(ctx, a, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, physical, map[string]any{"role": "assistant", "content": strings.Repeat("n", 2*1024*1024)}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	reply, err := localHistory(ctx, a, ref, prepared)
	if err != nil {
		t.Fatal(err)
	}
	freshPrepared, err := prepareHistory(ctx, a, ref)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := localHistory(ctx, a, ref, freshPrepared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply[len(reply)-1]["content"].(string), "nnn") {
		t.Fatal("the raced row is not the last live row")
	}
	if !pyTruthy(reply[0]["archived_truncated"]) {
		t.Fatal("the raced reply is not marked truncated")
	}
	if recoverDumpLen(t, reply) > recoverDumpLen(t, fresh) {
		t.Fatalf("raced reply %d bytes, fresh %d", recoverDumpLen(t, reply), recoverDumpLen(t, fresh))
	}
	if 2*recoverDumpLen(t, reply) >= 8*1024*1024 {
		t.Fatalf("raced reply rows take %d bytes twice over", recoverDumpLen(t, reply))
	}
}
