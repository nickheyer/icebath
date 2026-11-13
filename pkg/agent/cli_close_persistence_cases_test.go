package agent

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session store under a private home with the given session row created first when source is set
func closeCaseDB(t *testing.T, sessionID, source string) *state.SessionDB {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
	db, err := state.NewSessionDB(t.Context(), filepath.Join(tmp, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if source != "" {
		if err := db.CreateSession(state.NewSession{ID: sessionID, Source: source}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// Persistence seam of a real agent whose live transcript shares the given dicts
func closeCaseAgent(db *state.SessionDB, sessionID string, sessionMessages []map[string]any) *AIAgent {
	a := &AIAgent{sessionDB: db, sessionDBCreated: true, sessionID: sessionID, platform: "cli", model: "test-model",
		cachedSystemPrompt: "test system prompt", sessionPersistLock: &pythread.RLock{}}
	a.SetUnflushedSessionMessagesForTests(sessionMessages)
	return a
}

// Builds the worker's live rows from shared dicts the way turn setup does, history rows by identity
func closeCaseWorkerRows(a *AIAgent, messages, history []map[string]any) ([]*state.Message, []*state.Message) {
	live := make([]*state.Message, len(messages))
	for i, m := range messages {
		live[i] = state.MessageFromDict(m)
	}
	func() {
		defer persistLock(a)()
		a.shareSessionMessageOrigins(live, messages, true)
	}()
	var hist []*state.Message
	for _, h := range history {
		for i, m := range messages {
			if rowIdentity(m) == rowIdentity(h) {
				hist = append(hist, live[i])
			}
		}
	}
	return live, hist
}

func closeCaseContents(t *testing.T, db *state.SessionDB, sessionID string) []any {
	t.Helper()
	rows, err := db.GetMessagesAsConversation(t.Context(), sessionID, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var contents []any
	for _, row := range rows {
		contents = append(contents, row["content"])
	}
	return contents
}

func closeCaseAppend(t *testing.T, db *state.SessionDB, sessionID string, rows []map[string]any) {
	t.Helper()
	for _, m := range rows {
		if _, err := db.AppendMessage(t.Context(), sessionID, map[string]any{"role": m["role"], "content": m["content"]}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCliClosePreflushResumedPrefixIsNotDuplicated(t *testing.T) {
	sessionID := "cli-close-preflush-resume"
	db := closeCaseDB(t, sessionID, "cli")
	loaded := []map[string]any{{"role": "user", "content": "old prompt"}, {"role": "assistant", "content": "old answer"}}
	closeCaseAppend(t, db, sessionID, loaded)
	liveMessages := append(append([]map[string]any{}, loaded...), map[string]any{"role": "user", "content": "new prompt"})
	a := closeCaseAgent(db, sessionID, []map[string]any{})
	enteredFlush, releaseFlush := make(chan struct{}), make(chan struct{})
	realAppend := sessionDBAppendBatch
	flushCalls := 0
	sessionDBAppendBatch = func(d SessionStore, ctx context.Context, sid string, rows []map[string]any, g state.AppendGuards, chunk *int) (int, error) {
		flushCalls++
		if flushCalls == 1 {
			close(enteredFlush)
			select {
			case <-releaseFlush:
			case <-time.After(5 * time.Second):
				t.Error("flush never released")
			}
		}
		return realAppend(d, ctx, sid, rows, g, chunk)
	}
	t.Cleanup(func() { sessionDBAppendBatch = realAppend })
	live, hist := closeCaseWorkerRows(a, liveMessages, loaded)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		if err := a.persistSession(&live, hist); err != nil {
			t.Error(err)
		}
	}()
	if !signalledWithin(enteredFlush, 5*time.Second) {
		t.Fatal("worker never reached its flush")
	}
	cliHistory := append(append([]map[string]any{}, loaded...), map[string]any{"role": "user", "content": "ui prompt"})
	closeStarted, closeFinished := make(chan struct{}), make(chan struct{})
	go func() {
		close(closeStarted)
		a.PersistActiveSessionBeforeClose(context.Background(), func() []map[string]any { return cliHistory })
		close(closeFinished)
	}()
	if !signalledWithin(closeStarted, 5*time.Second) {
		t.Fatal("close never started")
	}
	if signalledWithin(closeFinished, 100*time.Millisecond) {
		t.Fatal("close flushed while the worker held the persistence lock")
	}
	close(releaseFlush)
	if !signalledWithin(workerDone, 5*time.Second) || !signalledWithin(closeFinished, 5*time.Second) {
		t.Fatal("the worker or the close never finished")
	}
	if got := closeCaseContents(t, db, sessionID); !reflect.DeepEqual(got, []any{"old prompt", "old answer", "new prompt"}) {
		t.Fatalf("stored %v", got)
	}
}

func TestCliCloseHandsStagedUserMarkerToTurnStart(t *testing.T) {
	sessionID := "cli-close-staged-user"
	db := closeCaseDB(t, sessionID, "cli")
	prefix := []map[string]any{{"role": "user", "content": "old prompt"}, {"role": "assistant", "content": "old answer"}}
	a := closeCaseAgent(db, sessionID, prefix)
	if outcome := a.flushMessagesToSessionDBWithHistory(prefix, []map[string]any{}); outcome == state.FlushFailed {
		t.Fatalf("flush outcome %v", outcome)
	}
	staged := map[string]any{"role": "user", "content": "new prompt"}
	cliHistory := append(append([]map[string]any{}, prefix...), staged)
	a.pendingCLIUserMessage = staged
	a.PersistActiveSessionBeforeClose(context.Background(), func() []map[string]any { return cliHistory })
	if staged[state.DBPersistedMarker] != true {
		t.Fatalf("staged dict not stamped durable: %v", staged)
	}
	workerMessages := append(append([]map[string]any{}, prefix...), staged)
	live, hist := closeCaseWorkerRows(a, workerMessages, prefix)
	if err := a.persistSession(&live, hist); err != nil {
		t.Fatal(err)
	}
	if got := closeCaseContents(t, db, sessionID); !reflect.DeepEqual(got, []any{"old prompt", "old answer", "new prompt"}) {
		t.Fatalf("stored %v", got)
	}
}

func TestCliCloseUsesCleanOverrideForShortenedPendingSnapshot(t *testing.T) {
	sessionID := "cli-close-shortened-noted-pending"
	db := closeCaseDB(t, sessionID, "cli")
	prefix := []map[string]any{{"role": "user", "content": "old prompt"}, {"role": "assistant", "content": "old answer"}}
	closeCaseAppend(t, db, sessionID, prefix)
	a := closeCaseAgent(db, sessionID, []map[string]any{})
	staged := map[string]any{"role": "user", "content": "[MODEL NOTE]\n\nnew prompt"}
	a.pendingCLIUserMessage = staged
	idx := len(prefix)
	a.persistUserMessageIdx = &idx
	a.persistUserMessageOverride = "new prompt"
	a.persistUserMessageTimestamp = nil
	cliHistory := append(append([]map[string]any{}, prefix...), staged)
	a.PersistActiveSessionBeforeClose(context.Background(), func() []map[string]any { return cliHistory })
	if got := closeCaseContents(t, db, sessionID); !reflect.DeepEqual(got, []any{"old prompt", "old answer", "new prompt"}) {
		t.Fatalf("stored %v", got)
	}
	if staged[state.DBPersistedMarker] != true {
		t.Fatalf("staged dict not stamped durable: %v", staged)
	}
}

func TestCliCloseBuildsPromptBeforeCreatingFirstSessionRow(t *testing.T) {
	sessionID := "cli-close-first-turn"
	db := closeCaseDB(t, sessionID, "")
	ctx := context.Background()
	a, err := NewAIAgent(ctx, AIAgentOptions{Model: "test-model", APIKey: "k", BaseURL: "http://127.0.0.1:9/v1", Provider: "openai",
		Platform: "cli", SessionID: sessionID, SessionDB: db, QuietMode: true, SkipContextFiles: true, SkipMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	if row, err := db.GetSession(ctx, sessionID); err != nil || row != nil {
		t.Fatalf("session row exists before the close: %v %v", row, err)
	}
	a.SetUnflushedSessionMessagesForTests([]map[string]any{})
	a.sessionDBCreated = false
	a.cachedSystemPrompt = ""
	staged := map[string]any{"role": "user", "content": "first prompt"}
	a.pendingCLIUserMessage = staged
	a.PersistActiveSessionBeforeClose(ctx, func() []map[string]any { return []map[string]any{staged} })
	session, err := db.GetSession(ctx, sessionID)
	if err != nil || session == nil {
		t.Fatalf("session row missing: %v", err)
	}
	prompt, _ := session.Get("system_prompt")
	if prompt == nil || prompt == "" || prompt != a.cachedSystemPrompt {
		t.Fatalf("system_prompt %v, built %q", prompt, a.cachedSystemPrompt)
	}
	if got := closeCaseContents(t, db, sessionID); !reflect.DeepEqual(got, []any{"first prompt"}) {
		t.Fatalf("stored %v", got)
	}
}

func TestCloseStampsReachTheTurnRowCopiedBeforeTheClose(t *testing.T) {
	sessionID := "cli-close-after-turn-setup"
	db := closeCaseDB(t, sessionID, "cli")
	prefix := []map[string]any{{"role": "user", "content": "old prompt"}, {"role": "assistant", "content": "old answer"}}
	a := closeCaseAgent(db, sessionID, prefix)
	if outcome := a.flushMessagesToSessionDBWithHistory(prefix, []map[string]any{}); outcome == state.FlushFailed {
		t.Fatalf("flush outcome %v", outcome)
	}
	staged := map[string]any{"role": "user", "content": "new prompt"}
	cliHistory := append(append([]map[string]any{}, prefix...), staged)
	a.pendingCLIUserMessage = staged
	live, hist := closeCaseWorkerRows(a, cliHistory, prefix)
	turnRow := live[len(live)-1]
	a.PersistActiveSessionBeforeClose(context.Background(), func() []map[string]any { return cliHistory })
	if staged[state.DBPersistedMarker] != true {
		t.Fatalf("staged dict not stamped durable: %v", staged)
	}
	func() {
		defer persistLock(a)()
		if rowID, ok := liveRowID(a, turnRow); !ok || rowID != staged["_row_id"] {
			t.Fatalf("turn row id %v %v, staged %v", rowID, ok, staged["_row_id"])
		}
	}()
	if !turnRow.Persisted {
		t.Fatal("turn row missed the close stamp")
	}
	if err := a.persistSession(&live, hist); err != nil {
		t.Fatal(err)
	}
	if got := closeCaseContents(t, db, sessionID); !reflect.DeepEqual(got, []any{"old prompt", "old answer", "new prompt"}) {
		t.Fatalf("stored %v", got)
	}
	if !a.sharedDictPersisted(staged) {
		t.Fatal("staged dict not durable through its turn row")
	}
}

// Waits for a signal up to the given time, true when it arrived
func signalledWithin(ch chan struct{}, wait time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(wait):
		return false
	}
}
