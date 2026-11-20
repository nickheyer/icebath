package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Transport of a viewer connection that drops every frame
type picksTransport struct{}

func (picksTransport) Write(*pyjson.Object) bool { return true }
func (picksTransport) Close()                    {}

// Telegram chat compressed once on a real runner, resumed by its segment id with the mount fences
type compressedPicksChat struct {
	ctx    context.Context
	runner *GatewayRunner
	source *gateway.SessionSource
	route  string
	viewer *gateway.AuthorityConnection
	root   string
	fences *pyjson.Object
}

func newCompressedPicksChat(t *testing.T) *compressedPicksChat {
	t.Helper()
	ctx := context.Background()
	runner, err := NewGatewayRunner(ctx, gateway.DefaultGatewayConfig(ctx))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := gateway.InitializeSessionAuthority(ctx, runner, "default", "owner", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	userID := "u-4242"
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "4242", UserID: &userID, ChatType: "dm"}
	ref, err := authority.Register(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	root := ref.SessionID
	route, err := runner.sessionStore.GenerateSessionKey(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	db := authority.DB
	if _, err := db.AppendMessage(ctx, root, map[string]any{"role": "user", "content": "first"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if err := db.PublishCompressionChild(ctx, state.CompressionChildPublish{ParentSessionID: root, ChildSessionID: "seg-1", Source: "telegram",
		Messages: []map[string]any{{"role": "user", "content": "summary"}}}); err != nil {
		t.Fatal(err)
	}
	if entry, err := runner.sessionStore.AdvanceCompressionSession(ctx, route, root, "seg-1"); err != nil || entry == nil {
		t.Fatalf("advance %v %v", entry, err)
	}
	origin, err := pyjson.Dumps(source.ToDict())
	if err != nil {
		t.Fatal(err)
	}
	chatID, chatType, telegram := source.ChatID, "dm", "telegram"
	if err := db.RecordGatewaySessionPeer(ctx, "seg-1", state.GatewayPeer{Source: telegram, UserID: source.UserID, SessionKey: &route,
		ChatID: &chatID, ChatType: &chatType, OriginJSON: &origin}); err != nil {
		t.Fatal(err)
	}
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_revision=7, runtime_generation=3 WHERE id='seg-1'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	viewer, err := gateway.NewAuthorityConnection(authority, picksTransport{}, map[string]any{
		"provider": "local", "user_id": "viewer", "profile_id": "default", "instance_id": "owner",
		"capabilities": []any{"session:read", "session:control"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = viewer.Close(ctx)
		runner.sessionStore.CloseAllDBHandles()
	})
	chat := &compressedPicksChat{ctx: ctx, runner: runner, source: source, route: route, viewer: viewer, root: root}
	resumed := chat.dispatch(t, pyjson.ObjectOf("id", int64(1), "method", "session.resume", "params", pyjson.ObjectOf("session_id", "seg-1")))
	result := picksField(resumed, "result")
	if sid, _ := result.Get("session_id"); sid != root {
		t.Fatalf("resumed %s", picksDump(resumed))
	}
	chat.fences = result
	return chat
}

// Response of one request on the viewer connection
func (c *compressedPicksChat) dispatch(t *testing.T, request *pyjson.Object) *pyjson.Object {
	t.Helper()
	response, err := c.viewer.Dispatch(c.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// Session mutate request carrying the fences a viewer holds
func picksMutate(sid string, fences *pyjson.Object, operation string, payload *pyjson.Object, requestID string) *pyjson.Object {
	revision, _ := fences.Get("revision")
	generation, _ := fences.Get("execution_generation")
	return pyjson.ObjectOf("id", requestID, "method", "session.mutate", "params", pyjson.ObjectOf(
		"session_id", sid, "request_id", requestID, "operation", operation, "payload", payload,
		"expected_revision", revision, "expected_generation", generation))
}

// Object under a key, an empty object when absent
func picksField(obj *pyjson.Object, key string) *pyjson.Object {
	if obj == nil {
		return pyjson.NewObject()
	}
	value, _ := obj.Get(key)
	if out, ok := pyjson.AsObject(value); ok {
		return out
	}
	return pyjson.NewObject()
}

// Session info of the chat as a viewer reads it
func (c *compressedPicksChat) info(t *testing.T) *pyjson.Object {
	t.Helper()
	return c.dispatch(t, pyjson.ObjectOf("id", int64(3), "method", "session.info", "params", pyjson.ObjectOf("session_id", c.root)))
}

// test_messaging_session_picks.py test_a_reasoning_pick_on_a_compressed_messaging_chat_is_the_chats_next_turn_level
func TestAReasoningPickOnACompressedMessagingChatIsTheChatsNextTurnLevel(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	chat := newCompressedPicksChat(t)
	before := chat.dispatch(t, pyjson.ObjectOf("id", int64(2), "method", "session.info", "params", pyjson.ObjectOf("session_id", chat.root)))
	if effort, _ := picksField(before, "result").Get("reasoning_effort"); func() bool { _, ok := effort.(string); return !ok }() {
		t.Fatalf("before %s", picksDump(before))
	}

	picked := chat.dispatch(t, picksMutate("seg-1", chat.fences, "reasoning", pyjson.ObjectOf("effort", "high"), "pick-high"))

	if effort, _ := picksField(picked, "result").Get("effort"); effort != "high" {
		t.Fatalf("picked %s", picksDump(picked))
	}
	level, err := chat.runner.ResolveSessionReasoningConfig(chat.ctx, chat.source, chat.route, "")
	if err != nil || level == nil || level["effort"] != "high" {
		t.Fatalf("level %v %v", level, err)
	}
	info := chat.info(t)
	if effort, _ := picksField(info, "result").Get("reasoning_effort"); effort != "high" {
		t.Fatalf("info %s", picksDump(info))
	}
}

// JSON text of a response with sorted keys, equal for equal mappings in any order
func picksDump(v any) string {
	text, err := pyjson.Dumps(v, pyjson.SortKeys(true))
	if err != nil {
		return pyrepr.Repr(v)
	}
	return text
}

// test_messaging_session_picks.py test_a_messaging_pick_is_fenced_by_its_receipt_and_revision
func TestAMessagingPickIsFencedByItsReceiptAndRevision(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	chat := newCompressedPicksChat(t)
	revision := func(obj *pyjson.Object) int64 {
		value, _ := obj.Get("revision")
		n, _ := value.(int64)
		return n
	}
	base := revision(chat.fences)
	first := chat.dispatch(t, picksMutate(chat.root, chat.fences, "reasoning", pyjson.ObjectOf("effort", "high"), "pick-a"))
	if revision(picksField(first, "result")) != base+1 {
		t.Fatalf("first %s", picksDump(first))
	}
	second := chat.dispatch(t, picksMutate(chat.root, picksField(first, "result"), "reasoning", pyjson.ObjectOf("effort", "low"), "pick-b"))
	if revision(picksField(second, "result")) != base+2 {
		t.Fatalf("second %s", picksDump(second))
	}

	retried := chat.dispatch(t, picksMutate(chat.root, chat.fences, "reasoning", pyjson.ObjectOf("effort", "high"), "pick-a"))

	if picksDump(picksField(retried, "result")) != picksDump(picksField(first, "result")) {
		t.Fatalf("retried %s first %s", picksDump(retried), picksDump(first))
	}
	level, err := chat.runner.ResolveSessionReasoningConfig(chat.ctx, chat.source, chat.route, "")
	if err != nil || level == nil || level["effort"] != "low" {
		t.Fatalf("level %v %v", level, err)
	}
	reason := func(obj *pyjson.Object) any {
		value, _ := picksField(picksField(obj, "error"), "data").Get("reason")
		return value
	}
	reused := chat.dispatch(t, picksMutate(chat.root, chat.fences, "reasoning", pyjson.ObjectOf("effort", "medium"), "pick-a"))
	if reason(reused) != "admission_conflict" {
		t.Fatalf("reused %s", picksDump(reused))
	}
	stale := chat.dispatch(t, picksMutate(chat.root, chat.fences, "reasoning", pyjson.ObjectOf("effort", "medium"), "pick-c"))
	if reason(stale) != "revision_conflict" {
		t.Fatalf("stale %s", picksDump(stale))
	}
	level, err = chat.runner.ResolveSessionReasoningConfig(chat.ctx, chat.source, chat.route, "")
	if err != nil || level["effort"] != "low" {
		t.Fatalf("level %v %v", level, err)
	}
}

// test_messaging_session_picks.py test_a_messaging_chats_effort_wire_is_clamped_for_its_api_mode
func TestAMessagingChatsEffortWireIsClampedForItsAPIMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("model:\n  default: gpt-5.6\n  provider: openai-codex\n  api_mode: codex_app_server\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chat := newCompressedPicksChat(t)
	picked := chat.dispatch(t, picksMutate(chat.root, chat.fences, "reasoning", pyjson.ObjectOf("effort", "ultra"), "pick-ultra"))
	if effort, _ := picksField(picked, "result").Get("effort"); effort != "ultra" {
		t.Fatalf("picked %s", picksDump(picked))
	}

	info := picksField(chat.info(t), "result")

	model, _ := info.Get("model")
	wire, _ := info.Get("reasoning_effort_wire")
	if model != "gpt-5.6" || wire != "ultra" {
		t.Fatalf("info %s", picksDump(info))
	}
}

// test_messaging_session_picks.py test_a_model_pick_on_a_messaging_chat_is_the_chats_next_turn_model
func TestAModelPickOnAMessagingChatIsTheChatsNextTurnModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("model:\n  provider: custom\n  base_url: http://127.0.0.1:9/v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chat := newCompressedPicksChat(t)

	picked := chat.dispatch(t, picksMutate(chat.root, chat.fences, "model", pyjson.ObjectOf("model", "picked", "reasoning", "low"), "pick"))

	if model, _ := picksField(picked, "result").Get("model"); model != "picked" {
		t.Fatalf("picked %s", picksDump(picked))
	}
	if override := chat.runner.SessionModelOverride(chat.route); override["model"] != "picked" {
		t.Fatalf("override %v", override)
	}
	if stored, _ := chat.runner.sessionStore.GetModelOverride(chat.ctx, chat.route).Get("model"); stored != "picked" {
		t.Fatalf("stored %v", stored)
	}
	level, err := chat.runner.ResolveSessionReasoningConfig(chat.ctx, chat.source, chat.route, "picked")
	if err != nil || level == nil || level["effort"] != "low" {
		t.Fatalf("level %v %v", level, err)
	}
	info := picksField(chat.info(t), "result")
	model, _ := info.Get("model")
	effort, _ := info.Get("reasoning_effort")
	if model != "picked" || effort != "low" {
		t.Fatalf("info %s", picksDump(info))
	}
}

// test_messaging_session_picks.py test_a_cold_model_pick_resolves_against_the_chats_saved_provider
func TestAColdModelPickResolvesAgainstTheChatsSavedProvider(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	chat := newCompressedPicksChat(t)
	saved := map[string]any{"model": "saved-model", "provider": "custom", "base_url": "http://127.0.0.1:9/v1"}
	if err := chat.runner.sessionStore.SetModelOverride(chat.ctx, chat.route, saved); err != nil {
		t.Fatal(err)
	}
	if override := chat.runner.SessionModelOverride(chat.route); override != nil {
		t.Fatalf("in memory before the pick %v", override)
	}

	picked := chat.dispatch(t, picksMutate(chat.root, chat.fences, "model", pyjson.ObjectOf("model", "picked"), "cold-pick"))

	if model, _ := picksField(picked, "result").Get("model"); model != "picked" {
		t.Fatalf("picked %s", picksDump(picked))
	}
	if provider, _ := picksField(picked, "result").Get("provider"); provider != "custom" {
		t.Fatalf("switch did not resolve against the saved provider %s", picksDump(picked))
	}
	stored := chat.runner.sessionStore.GetModelOverride(chat.ctx, chat.route)
	model, _ := stored.Get("model")
	provider, _ := stored.Get("provider")
	baseURL, _ := stored.Get("base_url")
	if model != "picked" || provider != "custom" || baseURL != "http://127.0.0.1:9/v1" {
		t.Fatalf("stored %s", picksDump(stored))
	}
}
