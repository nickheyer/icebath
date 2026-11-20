package a2a

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func mustLoads(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

func path(t *testing.T, v any, keys ...any) any {
	t.Helper()
	for _, k := range keys {
		switch key := k.(type) {
		case string:
			v, _ = get(v, key)
		case int:
			v = v.([]any)[key]
		}
	}
	return v
}

func TestCardShape(t *testing.T) {
	card := BuildAgentCard(AgentCardOptions{Name: "icebath-test", URL: "http://localhost:9900/", Description: "test"})
	if path(t, card, "name") != "icebath-test" {
		t.Fatal("name")
	}
	for _, k := range []string{"protocolVersion", "preferredTransport", "securitySchemes", "security"} {
		if _, ok := card.Get(k); ok {
			t.Fatalf("card carries %s", k)
		}
	}
	if path(t, card, "supportedInterfaces", 0, "protocolBinding") != "JSONRPC" || path(t, card, "supportedInterfaces", 0, "protocolVersion") != "1.0" ||
		path(t, card, "supportedInterfaces", 0, "url") != "http://localhost:9900/" {
		t.Fatal("interface")
	}
	if path(t, card, "provider", "organization") != "Icebath Agent" || path(t, card, "provider", "url") != "http://localhost:9900/" {
		t.Fatal("provider")
	}
	if path(t, card, "capabilities", "streaming") != false || len(path(t, card, "skills").([]any)) != 0 {
		t.Fatal("capabilities")
	}
	tenant := BuildAgentCard(AgentCardOptions{Name: "n", URL: "u", Tenant: "acme"})
	if path(t, tenant, "supportedInterfaces", 0, "tenant") != "acme" {
		t.Fatal("tenant")
	}
}

func TestCardAuthRequired(t *testing.T) {
	t.Setenv("A2A_PROVIDER_ORG", "Org")
	t.Setenv("A2A_PROVIDER_URL", "https://org.example/")
	card := BuildAgentCard(AgentCardOptions{Name: "n", URL: "u", AuthRequired: true, Streaming: true, PushNotifications: true})
	if path(t, card, "securitySchemes", "bearer", "scheme") != "bearer" || path(t, card, "securitySchemes", "bearer", "type") != "http" {
		t.Fatal("scheme")
	}
	if _, ok := path(t, card, "security", 0).(*pyjson.Object).Get("bearer"); !ok {
		t.Fatal("security")
	}
	if path(t, card, "provider", "organization") != "Org" || path(t, card, "provider", "url") != "https://org.example/" ||
		path(t, card, "capabilities", "pushNotifications") != true {
		t.Fatal("provider env and capabilities")
	}
}

func TestSkillsFromToolsetNames(t *testing.T) {
	skills := SkillsFromToolsets([]string{"web", "terminal", "web"})
	if len(skills) != 2 || path(t, skills[0], "id") != "toolset.terminal" || path(t, skills[1], "name") != "web" {
		t.Fatalf("skills %v", skills)
	}
	if path(t, skills[1], "description") != "Icebath 'web' capabilities" {
		t.Fatal("description")
	}
	general := SkillsFromToolsets(nil)
	if len(general) != 1 || path(t, general[0], "id") != "general" {
		t.Fatal("general fallback")
	}
}

func TestSkillsFromToolsetMappingIncludesToolTags(t *testing.T) {
	var many []string
	for i := range 12 {
		many = append(many, "t"+string(rune('a'+i)))
	}
	skills := SkillsFromToolsets(map[string][]string{"web": {"web_search", "web_extract"}, "terminal": {"terminal"}, "big": many})
	web := skills[2]
	tags := path(t, web, "tags").([]any)
	if path(t, web, "name") != "web" || !slices.Contains(tags, any("web_search")) || !slices.Contains(tags, any("web_extract")) {
		t.Fatalf("web %v", tags)
	}
	if n := len(path(t, skills[0], "tags").([]any)); n != 11 {
		t.Fatalf("name plus ten tools, got %d", n)
	}
}

func TestTextPartHasNoKind(t *testing.T) {
	part := TextPart("hi")
	if _, ok := part.Get("kind"); ok || path(t, part, "text") != "hi" || path(t, part, "mediaType") != "text/plain" {
		t.Fatal("text part")
	}
}

func TestTextMessageRoundtrip(t *testing.T) {
	msg := TextMessage(RoleUser, "hello", "ctx-1")
	if ExtractText(msg) != "hello" || path(t, msg, "role") != RoleUser || path(t, msg, "contextId") != "ctx-1" {
		t.Fatal("roundtrip")
	}
	if id := path(t, msg, "messageId").(string); len(id) != 32 {
		t.Fatalf("message id %q", id)
	}
	if _, ok := TextMessage(RoleAgent, "x", "").Get("contextId"); ok {
		t.Fatal("blank context id must be omitted")
	}
}

func TestExtractTextFromParams(t *testing.T) {
	params := mustLoads(t, `{"message": {"parts": [{"text": "a"}, {"text": "b"}, "skip", 3]}}`)
	if got := ExtractText(params); got != "a\nb" {
		t.Fatalf("got %q", got)
	}
	if got := ExtractText(mustLoads(t, `{"message": "nope"}`)); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractTextToleratesV03Parts(t *testing.T) {
	msg := mustLoads(t, `{"parts": [{"kind": "text", "text": "v03"}, {"type": "text", "text": "old"}]}`)
	if got := ExtractText(msg); got != "v03\nold" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractTextRendersFileAndDataParts(t *testing.T) {
	msg := mustLoads(t, `{"parts": [{"url": "https://x/doc.pdf", "mediaType": "application/pdf", "filename": "doc.pdf"}, {"data": {"k": "v"}, "mediaType": "application/json"}, {"text": "the words", "mediaType": "text/plain"}]}`)
	want := "[file: doc.pdf] https://x/doc.pdf (application/pdf)\n[data (application/json)]\n{\"k\": \"v\"}\nthe words"
	if got := ExtractText(msg); got != want {
		t.Fatalf("got %q", got)
	}
	bare := mustLoads(t, `{"parts": [{"url": "https://x/a"}, {"data": [1, "é"]}]}`)
	if got := ExtractText(bare); got != "[file] https://x/a\n[data (application/json)]\n[1, \"é\"]" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractTextHandlesV03FilePart(t *testing.T) {
	msg := mustLoads(t, `{"parts": [{"file": {"fileWithUri": "https://x/f.png", "name": "f.png", "mimeType": "image/png"}}]}`)
	if got := ExtractText(msg); got != "[file: f.png] https://x/f.png (image/png)" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractTextHandlesRawFilePart(t *testing.T) {
	msg := mustLoads(t, `{"parts": [{"raw": "aGVsbG8=", "filename": "hello.txt", "mediaType": "text/plain"}]}`)
	if got := ExtractText(msg); got != "[file: hello.txt] 8 bytes base64-encoded (text/plain)" {
		t.Fatalf("got %q", got)
	}
}

func TestContextIDExtractedFromMessage(t *testing.T) {
	params := mustLoads(t, `{"message": {"contextId": "ctx-in", "messageId": "m-1"}, "contextId": "ctx-top"}`)
	if ExtractContextID(params) != "ctx-in" || ExtractMessageID(params) != "m-1" {
		t.Fatal("message ids")
	}
	if ExtractMessageID(mustLoads(t, `{"message": "x"}`)) != "" {
		t.Fatal("non dict message")
	}
}

func TestContextIDLegacyTopLevel(t *testing.T) {
	if got := ExtractContextID(mustLoads(t, `{"message": {"parts": []}, "contextId": "ctx-legacy"}`)); got != "ctx-legacy" {
		t.Fatalf("got %q", got)
	}
	if got := ExtractContextID(mustLoads(t, `{}`)); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestMessageContextIDIsStableAndScoped(t *testing.T) {
	a, b := MessageContextID("peer", "m-1"), MessageContextID("peer", "m-1")
	if a != b || a == MessageContextID("other", "m-1") || !regexp.MustCompile(`^ctx-[0-9a-f]{16}$`).MatchString(a) {
		t.Fatalf("context id %q", a)
	}
	if !regexp.MustCompile(`^task-[0-9a-f]{16}$`).MatchString(NewTaskID()) || !regexp.MustCompile(`^ctx-[0-9a-f]{16}$`).MatchString(NewContextID()) {
		t.Fatal("generated ids")
	}
}

func TestCompletedTaskShape(t *testing.T) {
	task := BuildTask("t1", "c1", StateCompleted, "done")
	if path(t, task, "status", "state") != StateCompleted || path(t, task, "status", "message", "role") != RoleAgent {
		t.Fatal("status")
	}
	if ExtractText(path(t, task, "artifacts", 0).(*pyjson.Object)) != "done" {
		t.Fatal("artifact")
	}
	if _, ok := task.Get("createdAt"); ok {
		t.Fatal("createdAt must not serialize")
	}
}

func TestFailedTaskHasMessageNoArtifacts(t *testing.T) {
	task := BuildTask("t1", "c1", StateFailed, "boom")
	if _, ok := task.Get("artifacts"); ok || ExtractText(path(t, task, "status", "message").(*pyjson.Object)) != "boom" {
		t.Fatal("failed task")
	}
	if _, ok := path(t, BuildTask("t", "c", StateWorking, ""), "status").(*pyjson.Object).Get("message"); ok {
		t.Fatal("blank agent text")
	}
}

func TestTimestampsHaveMillisecondPrecision(t *testing.T) {
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`).MatchString(NowISO()) {
		t.Fatal(NowISO())
	}
}

func TestJsonrpcResultAndError(t *testing.T) {
	r, _ := pyjson.Dumps(JsonrpcResult(1, "ok"))
	e, _ := pyjson.Dumps(JsonrpcError("x", ErrMethodNotFound, "nope"))
	if r != `{"jsonrpc": "2.0", "id": 1, "result": "ok"}` || e != `{"jsonrpc": "2.0", "id": "x", "error": {"code": -32601, "message": "nope"}}` {
		t.Fatalf("%s %s", r, e)
	}
}

func TestCustomErrorCodesClearOfSpecReserved(t *testing.T) {
	for _, c := range []int{ErrUnauthorized, ErrRateLimited, ErrUntrustedPeer} {
		if c >= -32003 && c <= -32001 {
			t.Fatalf("code %d squats on the spec block", c)
		}
	}
	if ErrTaskNotFound != -32001 || ErrTaskNotCancelable != -32002 || ErrParse != -32700 || ErrInvalidParams != -32602 {
		t.Fatal("spec codes")
	}
}

func TestSendMessageResponseOneof(t *testing.T) {
	task := BuildTask("t1", "c1", StateWorking, "")
	if path(t, SendMessageResponse(task), "task") != any(task) {
		t.Fatal("task member")
	}
	msg := TextMessage(RoleAgent, "hi", "")
	if path(t, SendMessageResponse(msg), "message") != any(msg) {
		t.Fatal("message member")
	}
	if UnwrapSendMessageResponse(obj("task", task)) != any(task) || UnwrapSendMessageResponse(obj("message", msg)) != any(msg) {
		t.Fatal("unwrap")
	}
	if UnwrapSendMessageResponse("bare") != "bare" || path(t, StreamTask(task), "task") != any(task) {
		t.Fatal("legacy pass through")
	}
}

func TestStatusUpdateShape(t *testing.T) {
	u := StatusUpdate("t1", "c1", StateWorking, "")
	if path(t, u, "statusUpdate", "taskId") != "t1" || path(t, u, "statusUpdate", "contextId") != "c1" || path(t, u, "statusUpdate", "status", "state") != StateWorking {
		t.Fatal("status update")
	}
	for _, k := range []string{"kind", "final"} {
		if _, ok := path(t, u, "statusUpdate").(*pyjson.Object).Get(k); ok {
			t.Fatalf("carries %s", k)
		}
	}
}

func TestStatusUpdateWithMessage(t *testing.T) {
	u := StatusUpdate("t1", "c1", StateInputRequired, "which one?")
	if ExtractText(path(t, u, "statusUpdate", "status", "message").(*pyjson.Object)) != "which one?" {
		t.Fatal("message")
	}
}

func TestArtifactUpdateShape(t *testing.T) {
	u := ArtifactUpdate("t1", "c1", "chunk")
	if path(t, u, "artifactUpdate", "taskId") != "t1" || ExtractText(path(t, u, "artifactUpdate", "artifact").(*pyjson.Object)) != "chunk" {
		t.Fatal("artifact update")
	}
}

func TestSSEDataFraming(t *testing.T) {
	frame, err := SSEData(obj("a", "é"), nil)
	if err != nil || frame != "data: {\"a\": \"é\"}\n\n" {
		t.Fatalf("%q %v", frame, err)
	}
}

func TestSSEDataJsonrpcEnvelope(t *testing.T) {
	frame, err := SSEData(StreamTask(obj("id", "t1")), 7)
	if err != nil || frame != "data: {\"jsonrpc\": \"2.0\", \"id\": 7, \"result\": {\"task\": {\"id\": \"t1\"}}}\n\n" {
		t.Fatalf("%q %v", frame, err)
	}
}

func TestSSEDataNoEnvelopeWithoutReqID(t *testing.T) {
	frame, _ := SSEData(obj("statusUpdate", obj()), nil)
	if strings.Contains(frame, "jsonrpc") {
		t.Fatal(frame)
	}
}

func TestSSEDoneMarker(t *testing.T) {
	if SSEDone() != ": done\n\n" {
		t.Fatal(SSEDone())
	}
}

func TestTrackTurnIncrements(t *testing.T) {
	tr := NewTurnTracker()
	if tr.Track("c1") != 1 || tr.Track("c1") != 2 || tr.Track("c2") != 1 {
		t.Fatal("track")
	}
	base := time.Now()
	nowTime = func() time.Time { return base.Add(2 * time.Hour) }
	defer func() { nowTime = time.Now }()
	if tr.Track("c2") != 1 {
		t.Fatal("idle contexts must prune after an hour")
	}
}

func TestResetTurnsClears(t *testing.T) {
	tr := NewTurnTracker()
	tr.Track("c1")
	tr.Track("c1")
	tr.Reset("c1")
	if tr.Track("c1") != 1 {
		t.Fatal("reset")
	}
}

func TestMaxPingpongTurnsEnvOverride(t *testing.T) {
	for _, c := range []struct {
		env  string
		want int
	}{{"10", 10}, {"50", 20}, {"0", 1}, {"junk", 5}} {
		t.Setenv("A2A_MAX_PINGPONG_TURNS", c.env)
		if got := MaxPingpongTurns(); got != c.want {
			t.Fatalf("%s: %d", c.env, got)
		}
	}
	os.Unsetenv("A2A_MAX_PINGPONG_TURNS")
	if MaxPingpongTurns() != 5 {
		t.Fatal("default")
	}
}

func TestBlocksOverLimit(t *testing.T) {
	t.Setenv("A2A_RATE_LIMIT", "3")
	rl := NewRateLimiter()
	for i := range 3 {
		if !rl.Allow("peer-2") {
			t.Fatalf("request %d", i)
		}
	}
	if rl.Allow("peer-2") {
		t.Fatal("fourth request must be limited")
	}
	base := time.Now()
	nowTime = func() time.Time { return base.Add(61 * time.Second) }
	defer func() { nowTime = time.Now }()
	if !rl.Allow("peer-2") {
		t.Fatal("window must slide")
	}
}

func TestSeparatePerIdentity(t *testing.T) {
	t.Setenv("A2A_RATE_LIMIT", "1")
	rl := NewRateLimiter()
	if !rl.Allow("a") || !rl.Allow("b") || rl.Allow("a") {
		t.Fatal("buckets per identity")
	}
}

func TestRecordLatencyUpdatesAverage(t *testing.T) {
	m := NewMetrics()
	if m.AvgLatency() != 0 {
		t.Fatal("empty average")
	}
	m.RecordLatency(0.1)
	m.RecordLatency(0.3)
	m.InboundTotal.Add(2)
	snap := m.Snapshot()
	if path(t, snap, "avg_latency_ms") != 200.0 || path(t, snap, "inbound_total") != int64(2) || snap.Keys[0] != "uptime_seconds" || snap.Keys[len(snap.Keys)-1] != "avg_latency_ms" {
		t.Fatalf("snapshot %v", snap.Keys)
	}
	for range 150 {
		m.RecordLatency(1)
	}
	if m.AvgLatency() != 1 {
		t.Fatal("only the last hundred latencies count")
	}
}

func TestCompleteIsIdempotent(t *testing.T) {
	s := NewTaskStore()
	s.Create("t1", "c1", "p", "", "", "")
	if rec, ok := s.Complete("t1", StateCompleted, "r"); !ok || rec.State != StateCompleted || rec.CompletedAt == nil {
		t.Fatal("first complete")
	}
	if _, ok := s.Complete("t1", StateFailed, "x"); ok {
		t.Fatal("second complete must be a no op")
	}
	s.SetState("t1", StateWorking)
	if rec, _ := s.Get("t1", "", ""); rec.State != StateCompleted || rec.Reply != "r" {
		t.Fatal("terminal state must stick")
	}
	if _, ok := s.Complete("ghost", StateCompleted, ""); ok {
		t.Fatal("unknown task")
	}
}

func TestWatchResolvesOnComplete(t *testing.T) {
	s := NewTaskStore()
	s.Create("t1", "c1", "p", "", "", "")
	w := s.Watch("t1", "", "")
	if w.Done() {
		t.Fatal("watch resolved early")
	}
	go s.Complete("t1", StateCompleted, "answer")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	state, reply, err := w.Result(ctx)
	if err != nil || state != StateCompleted || reply != "answer" {
		t.Fatalf("%s %s %v", state, reply, err)
	}
	if s.Watch("ghost", "", "") != nil {
		t.Fatal("ghost watch")
	}
}

func TestWatchTerminalResolvesImmediately(t *testing.T) {
	s := NewTaskStore()
	s.Create("t1", "c1", "p", "", "", "")
	s.Complete("t1", StateFailed, "nope")
	w := s.Watch("t1", "", "")
	state, reply, _ := w.Result(context.Background())
	if !w.Done() || state != StateFailed || reply != "nope" {
		t.Fatal("terminal watch")
	}
}

func TestFailOrphans(t *testing.T) {
	s := NewTaskStore()
	s.Create("t-old", "c1", "p", "", "", "")
	s.Create("t-new", "c1", "p", "", "", "")
	rec, _ := s.tasks.Get("t-old")
	rec.CreatedAt = nowSeconds() - 600
	if got := s.FailOrphans(300, nil); !slices.Equal(got, []string{"t-old"}) {
		t.Fatalf("failed %v", got)
	}
	if r, _ := s.Get("t-old", "", ""); r.State != StateFailed || r.Reply != "[task orphaned — no reply produced]" {
		t.Fatal("orphan state")
	}
	if r, _ := s.Get("t-new", "", ""); r.State != StateSubmitted {
		t.Fatal("fresh task")
	}
	if got := s.FailOrphans(300, nil); len(got) != 0 {
		t.Fatalf("second pass %v", got)
	}
	s.Create("t-kept", "c1", "p", "", "", "")
	kept, _ := s.tasks.Get("t-kept")
	kept.CreatedAt = nowSeconds() - 600
	if got := s.FailOrphans(300, map[string]struct{}{"t-kept": {}}); len(got) != 0 {
		t.Fatal("excluded task")
	}
}

func taskIDs(recs []TaskRecord) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.TaskID)
	}
	return out
}

func TestListNewestFirstWithFilters(t *testing.T) {
	s := NewTaskStore()
	s.Create("t1", "c1", "p", "", "", "")
	s.Create("t2", "c2", "p", "", "", "")
	s.Create("t3", "c1", "p", "", "", "")
	s.Complete("t1", StateCompleted, "")
	recs, _, _, _ := s.List(ListOptions{ContextID: "c1"})
	if !slices.Equal(taskIDs(recs), []string{"t3", "t1"}) {
		t.Fatalf("by context %v", taskIDs(recs))
	}
	recs, _, _, _ = s.List(ListOptions{State: StateSubmitted})
	if !slices.Equal(taskIDs(recs), []string{"t3", "t2"}) {
		t.Fatalf("by state %v", taskIDs(recs))
	}
	page, next, total, err := s.List(ListOptions{PageSize: "2"})
	if err != nil || !slices.Equal(taskIDs(page), []string{"t3", "t2"}) || next != 2 || total != 3 {
		t.Fatalf("page %v next %d total %d %v", taskIDs(page), next, total, err)
	}
	page, next, _, _ = s.List(ListOptions{PageSize: 2, Offset: 2})
	if !slices.Equal(taskIDs(page), []string{"t1"}) || next != 0 {
		t.Fatal("last page")
	}
	if _, _, _, err := s.List(ListOptions{PageSize: "many"}); err == nil {
		t.Fatal("int of a non numeric page size must raise")
	}
}

func TestScopeHidesOtherAgentsAndTenants(t *testing.T) {
	s := NewTaskStore()
	s.Create("t1", "c1", "p", "alpha", "acme", "in-1")
	if _, ok := s.Get("t1", "beta", ""); ok {
		t.Fatal("agent scope")
	}
	if _, ok := s.Get("t1", "", "other"); ok {
		t.Fatal("tenant scope")
	}
	if rec, ok := s.Get("t1", "alpha", "acme"); !ok || rec.InputID != "in-1" {
		t.Fatal("in scope")
	}
	if recs, _, _, _ := s.List(ListOptions{AgentSlug: "beta"}); len(recs) != 0 {
		t.Fatal("scoped list")
	}
	if ToTask(TaskRecord{TaskID: "t", ContextID: "c", State: StateCompleted, Reply: "r"}, false).Len() != 3 {
		t.Fatal("to task without artifacts")
	}
}

func TestPushConfigLifecycle(t *testing.T) {
	s := NewTaskStore()
	s.Create("t1", "c1", "p", "", "", "")
	cfg := s.SetPushConfig("t1", "https://example.com/hook", "", "")
	id := path(t, cfg, "configId").(string)
	if !strings.HasPrefix(id, "cfg-") || len(id) != 16 || path(t, cfg, "createdAt") == "" || path(t, cfg, "pushNotificationConfig", "url") != "https://example.com/hook" {
		t.Fatal("config view")
	}
	if s.GetPushConfig("t1", "cfg-other", "", "") != nil || s.GetPushConfig("t1", id, "", "") == nil || len(s.ListPushConfigs("t1", "", "")) != 1 {
		t.Fatal("config lookup")
	}
	if s.PopPushURL("t1") != "https://example.com/hook" || s.PopPushURL("t1") != "" {
		t.Fatal("pop consumes the url")
	}
	if s.SetPushConfig("ghost", "https://x/", "", "") != nil || s.PopPushURL("ghost") != "" {
		t.Fatal("ghost task")
	}
	s.SetPushConfig("t1", "https://example.com/again", "", "")
	if !s.DeletePushConfig("t1", "", "", "") || s.DeletePushConfig("t1", "", "", "") || len(s.ListPushConfigs("t1", "", "")) != 0 {
		t.Fatal("delete")
	}
}

func TestTerminalTasksTrimToTheCap(t *testing.T) {
	s := NewTaskStore()
	for i := range maxTerminalTasks + 5 {
		id := "t" + strings.Repeat("x", i)
		s.Create(id, "c", "p", "", "", "")
		s.Complete(id, StateCompleted, "")
	}
	if s.tasks.Len() != maxTerminalTasks {
		t.Fatalf("kept %d", s.tasks.Len())
	}
	if _, ok := s.Get("t", "", ""); ok {
		t.Fatal("oldest terminal task must be trimmed")
	}
}

func TestPersistAndLoad(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	PersistMessage(ctx, "ctx-1", "user", "héllo", "t1", "")
	PersistMessage(ctx, "ctx-1", "agent", "reply", "t1", "in-9")
	convo := LoadConversation(ctx, "ctx-1", 50)
	if len(convo) != 2 || path(t, convo[0], "text") != "héllo" || path(t, convo[1], "role") != "agent" {
		t.Fatalf("convo %v", convo)
	}
	if _, ok := convo[0].Get("input_id"); ok || convo[0].Keys[0] != "ts" {
		t.Fatal("entry shape")
	}
	if len(LoadConversation(ctx, "ctx-1", 1)) != 1 || !LoggedInput(ctx, "ctx-1", "in-9") || LoggedInput(ctx, "ctx-1", "in-8") {
		t.Fatal("limit and logged input")
	}
}

func TestLoadConversationSkipsNonDictLines(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	PersistMessage(ctx, "ctx-mixed", "user", "hello", "t1", "")
	fh, err := os.OpenFile(convPath(ctx, "ctx-mixed"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString("42\n{broken\n\n")
	fh.Close()
	convo := LoadConversation(ctx, "ctx-mixed", 50)
	if len(convo) != 1 || path(t, convo[0], "text") != "hello" {
		t.Fatalf("convo %v", convo)
	}
}

func TestListConversations(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := ListConversations(ctx); len(got) != 0 {
		t.Fatal("missing directory lists nothing")
	}
	PersistMessage(ctx, "ctx-b", "user", "x", "", "")
	PersistMessage(ctx, "ctx/a..", "user", "x", "", "")
	PersistMessage(ctx, "", "user", "x", "", "")
	if got := ListConversations(ctx); !slices.Equal(got, []string{"ctx-b", "ctxa", "default"}) {
		t.Fatalf("got %v", got)
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := LoadConversation(context.Background(), "nope", 50); got == nil || len(got) != 0 {
		t.Fatal("missing conversation")
	}
}

func TestConversationPathFollowsProfileOverride(t *testing.T) {
	dir := t.TempDir()
	ctx := home.WithOverride(context.Background(), dir)
	if got := convPath(ctx, "ctx-1"); got != filepath.Join(dir, "a2a_conversations", "ctx-1.jsonl") {
		t.Fatalf("got %s", got)
	}
}
