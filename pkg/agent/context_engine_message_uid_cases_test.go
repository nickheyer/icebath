package agent

import (
	"context"
	"fmt"
	"iter"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

const messageUIDLen = 32

func muid(c string) string { return strings.Repeat(c, messageUIDLen) }

func TestASelectContextSelectionIsStrippedBeforeTheProvider(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	engine := &selectingEngine{stubEngine: *newStubEngine(1000, 0.75), selectFn: func(_, convo []map[string]any, _ map[string]any) []map[string]any {
		return append([]map[string]any(nil), convo...)
	}}
	a := &AIAgent{contextCompressor: engine, sessionID: "s"}
	history := []map[string]any{
		{"role": "user", "content": "q", "message_uid": muid("a"), "timestamp": 1.0, "_row_id": 1, "_absorbed_message_uids": []any{muid("b")}},
		{"role": "assistant", "content": "", "message_uid": muid("c"), "timestamp": 2.0,
			"tool_calls":      []any{map[string]any{"id": "call_x", "type": "function", "function": map[string]any{"name": "t", "arguments": "{}"}}},
			"_tool_call_uids": map[string]any{"call_x": muid("d")}},
		{"role": "tool", "content": "r", "tool_call_id": "call_x", "message_uid": muid("e"), "timestamp": 3.0, "_tool_call_uid": muid("d")},
	}
	api := make([]map[string]any, len(history))
	for i, m := range history {
		api[i] = map[string]any{}
		for k, v := range m {
			if !state.PersistenceOnlyMessageFields[k] {
				api[i][k] = v
			}
		}
	}
	out := applyContextEngineSelection(a, api, history, history[0])
	var contents []any
	for _, m := range out {
		contents = append(contents, m["content"])
		for key := range state.PersistenceOnlyMessageFields {
			if _, ok := m[key]; ok {
				t.Fatalf("%s reached the wire %v", key, m)
			}
		}
	}
	if !reflect.DeepEqual(contents, []any{"q", "", "r"}) || state.ToolCallField(out[1]["tool_calls"].([]any)[0], "id") != "call_x" {
		t.Fatalf("out %v", out)
	}
	for _, m := range history {
		if !pyTruthy(m["message_uid"]) {
			t.Fatalf("history touched %v", m)
		}
	}
}

func TestTheInflightTaskRestatedOntoTheCarrierRecordsItsUID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	compressor := &ContextCompressor{}
	compressor.quietMode = true
	carrier := map[string]any{"role": "user", "content": state.SummaryPrefix + "\n\nwhat happened\n\n" + state.SummaryEndMarker,
		state.CompressedSummaryMetadataKey: true, "message_uid": muid("s")}
	inflight := map[string]any{"role": "user", "content": "finish the task", "message_uid": muid("a")}
	out := compressor.reappendInflightUserTask([]map[string]any{carrier}, inflight)
	if len(out) != 1 || reflect.ValueOf(out[0]).Pointer() != reflect.ValueOf(carrier).Pointer() || !strings.Contains(carrier["content"].(string), inflightTaskReplayHeader) {
		t.Fatalf("out %v", out)
	}
	if carrier["message_uid"] != muid("s") || !reflect.DeepEqual(state.UIDList(carrier["_absorbed_message_uids"]), []string{muid("a")}) {
		t.Fatalf("carrier %v", carrier)
	}
}

func TestTheRealUserAnchorFoldedIntoScaffoldingKeepsTheAnchorsUID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	target := map[string]any{"role": "user", "content": "[todo snapshot]", "message_uid": muid("t")}
	anchor := map[string]any{"role": "user", "content": "the real ask", "message_uid": muid("a"), "_absorbed_message_uids": []any{muid("b")}}
	mergeAnchorIntoUserMessage(target, anchor)
	if target["content"] != "the real ask\n\n[todo snapshot]" || target["message_uid"] != muid("a") ||
		!reflect.DeepEqual(state.UIDList(target["_absorbed_message_uids"]), []string{muid("b"), muid("t")}) {
		t.Fatalf("target %v", target)
	}
	compressed := []map[string]any{{"role": "user", "content": "earlier"}, {"role": "assistant", "content": "ok"}, {"role": "user", "content": "[todo snapshot]"}}
	fresh := map[string]any{}
	for k, v := range anchor {
		fresh[k] = v
	}
	fresh["_absorbed_message_uids"] = []any{}
	if got := insertRealUserAnchor(&compressed, fresh); got != "merged" {
		t.Fatalf("insert %q", got)
	}
	last := compressed[len(compressed)-1]
	if last["message_uid"] != muid("a") {
		t.Fatalf("last %v", last)
	}
	if _, ok := last["_absorbed_message_uids"]; ok {
		t.Fatalf("scaffolding absorbed something %v", last)
	}
}

func TestMicroCompactionsAdjacentUserMergeRecordsTheWitness(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	first := map[string]any{"role": "user", "content": "one", "message_uid": muid("1")}
	second := map[string]any{"role": "user", "content": "two", "message_uid": muid("2")}
	merged := (&ContextCompressor{}).mergeAdjacentUserTurns([]map[string]any{first, second})
	if len(merged) != 1 || reflect.ValueOf(merged[0]).Pointer() != reflect.ValueOf(first).Pointer() || first["content"] != "one\n\ntwo" {
		t.Fatalf("merged %v", merged)
	}
	if first["message_uid"] != muid("1") || !reflect.DeepEqual(state.UIDList(first["_absorbed_message_uids"]), []string{muid("2")}) {
		t.Fatalf("first %v", first)
	}
}

// Engine recording what compress and on_turn_complete receive, compress returning a summary and the last two copies
type capturingEngine struct {
	stubEngine
	compressInput        []map[string]any
	turnCompleteMessages []map[string]any
	summaryUID           string
}

func newCapturingEngine() *capturingEngine {
	e := &capturingEngine{stubEngine: *newStubEngine(256000, 0.5)}
	e.CompressionCount = 1
	return e
}

func (e *capturingEngine) Name() string                      { return "capturing" }
func (e *capturingEngine) ShouldCompress(*int) bool          { return false }
func (e *capturingEngine) UpdateFromResponse(map[string]any) {}

func (e *capturingEngine) Compress(messages []map[string]any, _ *int, _ *string, _ bool, _ string) ([]map[string]any, error) {
	e.compressInput = make([]map[string]any, len(messages))
	for i, m := range messages {
		e.compressInput[i] = shallowDict(m)
	}
	summary := map[string]any{"role": "user", "content": "[CONTEXT COMPACTION] summary of prior turns"}
	if e.summaryUID != "" {
		summary["message_uid"] = e.summaryUID
	}
	out := []map[string]any{summary}
	for _, m := range messages[len(messages)-2:] {
		out = append(out, shallowDict(m))
	}
	return out, nil
}

func (e *capturingEngine) OnTurnComplete(messages []map[string]any, _, _ map[string]any) {
	e.turnCompleteMessages = messages
}

// Real openrouter agent on the session store with the capturing engine and in place compaction
func uidAgent(t *testing.T, db *state.SessionDB, sid string) *AIAgent {
	t.Helper()
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	a, err := NewAIAgent(context.Background(), AIAgentOptions{APIKey: "test-key", BaseURL: "https://openrouter.ai/api/v1", Model: "test/model",
		QuietMode: true, SessionDB: db, SessionID: sid, SkipContextFiles: true, SkipMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.compressionInPlace = true
	a.contextCompressor = newCapturingEngine()
	return a
}

// Fresh state db under a temp home
func uidDB(t *testing.T) *state.SessionDB {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	db, err := state.Open(filepath.Join(h, "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Active rows of the session in id order, content and uid as stored
func activeRows(t *testing.T, db *state.SessionDB, sid string) []map[string]any {
	t.Helper()
	rows, err := db.GetMessagesAsConversation(context.Background(), sid, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Seeds alternating user and assistant rows and returns them as stored
func seedUIDRows(t *testing.T, db *state.SessionDB, sid string, n int) []map[string]any {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Model: "test/model"}); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if _, err := db.AppendMessage(context.Background(), sid, map[string]any{"role": role, "content": fmt.Sprintf("seed msg %d", i)}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	return activeRows(t, db, sid)
}

func uidsOf(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r["message_uid"]
	}
	return out
}

func contentsOf(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r["content"]
	}
	return out
}

func liveDicts(messages []*state.Message) []map[string]any {
	out := make([]map[string]any, len(messages))
	for i, m := range messages {
		out[i] = state.MessageToDict(m)
	}
	return out
}

func TestCompressInputAfterAColdRestoreCarriesEveryRowsUID(t *testing.T) {
	db := uidDB(t)
	const sid = "20260928_120000_uid"
	stored := seedUIDRows(t, db, sid, 6)
	ctx := context.Background()
	restored, err := db.GetMessagesAsConversation(ctx, sid, state.ConversationOptions{RepairAlternation: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range restored {
		if _, ok := m["_row_id"]; ok {
			t.Fatalf("restore carried a row id %v", m)
		}
	}
	a := uidAgent(t, db, sid)
	a.sessionDBCreated = true
	a.lastFlushedDBIdx = len(restored)
	sys, approx := "sys", 100_000
	compressed, _, err := compressContext(ctx, a, restored, &sys, compressContextOptions{ApproxTokens: &approx})
	if err != nil {
		t.Fatal(err)
	}
	engine := a.contextCompressor.(*capturingEngine)
	if !reflect.DeepEqual(uidsOf(engine.compressInput), uidsOf(stored)) {
		t.Fatalf("compress input uids %v want %v", uidsOf(engine.compressInput), uidsOf(stored))
	}
	active := activeRows(t, db, sid)
	if !reflect.DeepEqual(contentsOf(active), []any{"[CONTEXT COMPACTION] summary of prior turns", "seed msg 4", "seed msg 5"}) {
		t.Fatalf("active %v", contentsOf(active))
	}
	if !reflect.DeepEqual(uidsOf(active[1:]), []any{stored[4]["message_uid"], stored[5]["message_uid"]}) {
		t.Fatalf("tail uids %v", uidsOf(active[1:]))
	}
	if uid, _ := active[0]["message_uid"].(string); len(uid) != messageUIDLen {
		t.Fatalf("summary uid %q", uid)
	}
	if !reflect.DeepEqual(uidsOf(compressed), uidsOf(active)) {
		t.Fatalf("live uids %v active %v", uidsOf(compressed), uidsOf(active))
	}
}

func TestAnEngineAuthoredUIDSurvivesTheCommit(t *testing.T) {
	for _, inPlace := range []bool{true, false} {
		name := "rotation"
		if inPlace {
			name = "in_place"
		}
		t.Run(name, func(t *testing.T) {
			db := uidDB(t)
			sid := "20260928_120050_rotate"
			if inPlace {
				sid = "20260928_120050_inplace"
			}
			stored := seedUIDRows(t, db, sid, 6)
			ctx := context.Background()
			restored := activeRows(t, db, sid)
			a := uidAgent(t, db, sid)
			engine := newCapturingEngine()
			engine.summaryUID = muid("e")
			a.contextCompressor = engine
			a.compressionInPlace = inPlace
			a.sessionDBCreated = true
			a.lastFlushedDBIdx = len(restored)
			sys, approx := "sys", 100_000
			compressed, _, err := compressContext(ctx, a, restored, &sys, compressContextOptions{ApproxTokens: &approx})
			if err != nil {
				t.Fatal(err)
			}
			committed := a.sessionID
			if !inPlace && committed == sid {
				t.Fatal("rotation published no child session")
			}
			active := activeRows(t, db, committed)
			if len(active) < 3 || active[0]["content"] != "[CONTEXT COMPACTION] summary of prior turns" || active[0]["message_uid"] != muid("e") {
				t.Fatalf("active %v", active)
			}
			if !reflect.DeepEqual(uidsOf(active[1:3]), []any{stored[4]["message_uid"], stored[5]["message_uid"]}) {
				t.Fatalf("tail uids %v", uidsOf(active[1:3]))
			}
			if !reflect.DeepEqual(uidsOf(compressed[:3]), uidsOf(active[:3])) {
				t.Fatalf("live %v active %v", uidsOf(compressed[:3]), uidsOf(active[:3]))
			}
		})
	}
}

func TestTurnFlushStampsUIDsOnTheLiveDictsPostLlmCallHandsOver(t *testing.T) {
	db := uidDB(t)
	const sid = "20260928_120100_flush"
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Model: "test/model"}); err != nil {
		t.Fatal(err)
	}
	a := uidAgent(t, db, sid)
	a.sessionDBCreated = true
	messages := []*state.Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}}
	idx := 0
	a.persistUserMessageIdx = &idx
	if err := a.persistSession(&messages, nil); err != nil {
		t.Fatal(err)
	}
	live := liveDicts(messages)
	for _, m := range live {
		if uid, _ := m["message_uid"].(string); len(uid) != messageUIDLen {
			t.Fatalf("live row without uid %v", m)
		}
	}
	if !reflect.DeepEqual(uidsOf(live), uidsOf(activeRows(t, db, sid))) {
		t.Fatalf("live %v stored %v", uidsOf(live), uidsOf(activeRows(t, db, sid)))
	}
}

func TestPersistOverrideKeepsTheUID(t *testing.T) {
	db := uidDB(t)
	const sid = "20260928_120200_override"
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Model: "test/model"}); err != nil {
		t.Fatal(err)
	}
	a := uidAgent(t, db, sid)
	a.sessionDBCreated = true
	messages := []*state.Message{{Role: "user", Content: "api-only variant with injected context"}}
	idx := 0
	a.persistUserMessageIdx = &idx
	a.persistUserMessageOverride = "what the user typed"
	if err := a.persistSession(&messages, nil); err != nil {
		t.Fatal(err)
	}
	uid := state.MessageToDict(messages[0])["message_uid"]
	rows := liveDicts(messages)
	a.applyPersistUserMessageOverride(rows)
	if rows[0]["content"] != "what the user typed" || rows[0]["message_uid"] != uid {
		t.Fatalf("row %v uid %v", rows[0], uid)
	}
	active := activeRows(t, db, sid)
	if len(active) != 1 || active[0]["content"] != "what the user typed" || active[0]["message_uid"] != uid {
		t.Fatalf("stored %v", active)
	}
}

func TestRestartAfterAMergeStillSeesTheCompositesConstituents(t *testing.T) {
	db := uidDB(t)
	const sid = "20260928_120300_witness"
	ctx := context.Background()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Model: "test/model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": "user", "content": "unanswered before the crash"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	danglingUID := activeRows(t, db, sid)[0]["message_uid"]
	historyRows, err := db.GetMessagesAsConversation(ctx, sid, state.ConversationOptions{RepairAlternation: true, IncludeRowIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	a := uidAgent(t, db, sid)
	a.sessionDBCreated = true
	history := make([]*state.Message, len(historyRows))
	for i, r := range historyRows {
		history[i] = state.MessageFromDict(r)
	}
	prompt := &state.Message{Role: "user", Content: "next prompt"}
	messages := append(append([]*state.Message(nil), history...), prompt)
	idx := 1
	a.persistUserMessageIdx = &idx
	if err := a.persistSession(&messages, history); err != nil {
		t.Fatal(err)
	}
	promptUID := state.MessageToDict(prompt)["message_uid"]
	if _, err := repairMessageSequenceWithCursor(a, &messages); err != nil {
		t.Fatal(err)
	}
	survivor := state.MessageToDict(messages[0])
	if len(messages) != 1 || survivor["message_uid"] != danglingUID || !reflect.DeepEqual(state.UIDList(survivor["_absorbed_message_uids"]), []string{promptUID.(string)}) {
		t.Fatalf("messages %v prompt uid %v", liveDicts(messages), promptUID)
	}
	messages = append(messages, &state.Message{Role: "assistant", Content: "reply"})
	if err := a.persistSession(&messages, nil); err != nil {
		t.Fatal(err)
	}
	again, err := db.GetMessagesAsConversation(ctx, sid, state.ConversationOptions{RepairAlternation: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range again {
		if m["message_uid"] != danglingUID {
			continue
		}
		if !strings.HasPrefix(fmt.Sprint(m["content"]), "unanswered before the crash") || !slicesContainsString(state.UIDList(m["_absorbed_message_uids"]), promptUID.(string)) {
			t.Fatalf("restored survivor %v", m)
		}
		return
	}
	t.Fatalf("survivor missing %v", again)
}

func slicesContainsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func uidToolCall(id string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "t", "arguments": "{}"}}
}

// Plain form of an ordered uid map, nil when the value is no mapping
func plainUIDMap(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return pyjson.Plain(t).(map[string]any)
	case interface{ PyItems() iter.Seq2[any, any] }:
		out := map[string]any{}
		for k, val := range t.PyItems() {
			out[fmt.Sprint(k)] = pyjson.Plain(val)
		}
		return out
	}
	return nil
}

func toolCallUIDs(m *state.Message) map[string]any {
	return plainUIDMap(state.MessageToDict(m)["_tool_call_uids"])
}

func TestToolResultFlushedAfterItsCallPairsThroughTheLiveList(t *testing.T) {
	db := uidDB(t)
	const sid = "20260928_120400_tool"
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Model: "test/model"}); err != nil {
		t.Fatal(err)
	}
	a := uidAgent(t, db, sid)
	a.sessionDBCreated = true
	assistant := state.MessageFromDict(map[string]any{"role": "assistant", "content": "", "tool_calls": []any{uidToolCall("call_x")}})
	messages := []*state.Message{{Role: "user", Content: "q"}, assistant}
	idx := 0
	a.persistUserMessageIdx = &idx
	if err := a.persistSession(&messages, nil); err != nil {
		t.Fatal(err)
	}
	uid := toolCallUIDs(assistant)["call_x"]
	result := state.MessageFromDict(map[string]any{"role": "tool", "content": "r", "tool_call_id": "call_x", "tool_name": "t"})
	messages = append(messages, result)
	if err := a.persistSession(&messages, nil); err != nil {
		t.Fatal(err)
	}
	if uid == nil || state.MessageToDict(result)["_tool_call_uid"] != uid {
		t.Fatalf("result %v uid %v", state.MessageToDict(result), uid)
	}
	for _, m := range activeRows(t, db, sid) {
		if m["role"] == "tool" && m["_tool_call_uid"] != uid {
			t.Fatalf("stored tool row %v", m)
		}
	}
}

func TestAReusedIDMergeSurvivesRewriteAndRestore(t *testing.T) {
	db := uidDB(t)
	const sid = "20260928_120600_reused"
	ctx := context.Background()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Model: "test/model"}); err != nil {
		t.Fatal(err)
	}
	a := uidAgent(t, db, sid)
	a.sessionDBCreated = true
	first := state.MessageFromDict(map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{uidToolCall("call_x")}})
	second := state.MessageFromDict(map[string]any{"role": "assistant", "content": "", "tool_calls": []any{uidToolCall("call_x")}})
	result := state.MessageFromDict(map[string]any{"role": "tool", "content": "r", "tool_call_id": "call_x", "tool_name": "t"})
	messages := []*state.Message{{Role: "user", Content: "q"}, first, second, result}
	idx := 0
	a.persistUserMessageIdx = &idx
	if err := a.persistSession(&messages, nil); err != nil {
		t.Fatal(err)
	}
	occurrences := []any{toolCallUIDs(first)["call_x"], toolCallUIDs(second)["call_x"]}
	if occurrences[0] == occurrences[1] || state.MessageToDict(result)["_tool_call_uid"] != occurrences[1] {
		t.Fatalf("occurrences %v result %v", occurrences, state.MessageToDict(result))
	}
	if _, err := repairMessageSequenceWithCursor(a, &messages); err != nil {
		t.Fatal(err)
	}
	if got := toolCallUIDs(first); !reflect.DeepEqual(got, map[string]any{"call_x": occurrences}) {
		t.Fatalf("folded uids %v", got)
	}
	if err := a.persistSession(&messages, nil); err != nil {
		t.Fatal(err)
	}
	again, err := db.GetMessagesAsConversation(ctx, sid, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var storedCall, storedTool map[string]any
	for _, m := range again {
		if storedCall == nil && m["role"] == "assistant" && pyTruthy(m["tool_calls"]) {
			storedCall = m
		}
		if storedTool == nil && m["role"] == "tool" {
			storedTool = m
		}
	}
	if !reflect.DeepEqual(plainUIDMap(storedCall["_tool_call_uids"]), map[string]any{"call_x": occurrences}) {
		t.Fatalf("stored uids %v", storedCall)
	}
	if storedTool["_tool_call_uid"] != occurrences[1] {
		t.Fatalf("stored tool row %v", storedTool)
	}
}

func TestAnAssistantMergeRewritePersistsTheUnionedToolCallUIDs(t *testing.T) {
	db := uidDB(t)
	const sid = "20260928_120500_union"
	ctx := context.Background()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Model: "test/model"}); err != nil {
		t.Fatal(err)
	}
	a := uidAgent(t, db, sid)
	a.sessionDBCreated = true
	survivor := &state.Message{Role: "assistant", Content: "let me check"}
	caller := state.MessageFromDict(map[string]any{"role": "assistant", "content": "", "tool_calls": []any{uidToolCall("call_2")}})
	result := state.MessageFromDict(map[string]any{"role": "tool", "content": "r", "tool_call_id": "call_2", "tool_name": "t"})
	user := &state.Message{Role: "user", Content: "q"}
	messages := []*state.Message{user, survivor, caller, result}
	idx := 0
	a.persistUserMessageIdx = &idx
	if err := a.persistSession(&messages, nil); err != nil {
		t.Fatal(err)
	}
	uids := toolCallUIDs(caller)
	if len(uids) != 1 || uids["call_2"] == nil || toolCallUIDs(survivor) != nil || state.MessageToDict(result)["_tool_call_uid"] != uids["call_2"] {
		t.Fatalf("uids %v survivor %v result %v", uids, state.MessageToDict(survivor), state.MessageToDict(result))
	}
	if _, err := repairMessageSequenceWithCursor(a, &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0] != user || messages[1] != survivor || messages[2] != result || !reflect.DeepEqual(toolCallUIDs(survivor), uids) {
		t.Fatalf("messages %v", liveDicts(messages))
	}
	if err := a.persistSession(&messages, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(toolCallUIDs(survivor), uids) {
		t.Fatalf("survivor uids %v", toolCallUIDs(survivor))
	}
	again, err := db.GetMessagesAsConversation(ctx, sid, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range again {
		if m["role"] == "assistant" && strings.HasPrefix(fmt.Sprint(m["content"]), "let me check") {
			calls, _ := m["tool_calls"].([]any)
			if len(calls) != 1 || state.ToolCallField(calls[0], "id") != "call_2" || !reflect.DeepEqual(plainUIDMap(m["_tool_call_uids"]), uids) {
				t.Fatalf("stored survivor %v", m)
			}
		}
		if m["role"] == "tool" && m["_tool_call_uid"] != uids["call_2"] {
			t.Fatalf("stored tool row %v", m)
		}
	}
}
