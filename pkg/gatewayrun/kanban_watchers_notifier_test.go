package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
)

// One recorded kanban ping
type kanbanSent struct {
	chatID   string
	text     string
	metadata map[string]any
}

// Telegram adapter recording every send and accepting every wake turn
type kanbanRecordingAdapter struct {
	*gateway.BasePlatformAdapter
	mu       sync.Mutex
	sent     []kanbanSent
	handled  []*gateway.MessageEvent
	failSend bool
	failWake bool
}

func (a *kanbanRecordingAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *kanbanRecordingAdapter) Disconnect(context.Context) error            { return nil }
func (a *kanbanRecordingAdapter) Send(_ context.Context, chatID, content, _ string, metadata map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if metadata == nil {
		metadata = map[string]any{}
	}
	a.sent = append(a.sent, kanbanSent{chatID, content, metadata})
	if a.failSend {
		return gateway.SendResult{}, &kanban.RuntimeError{Msg: "transient send failure"}
	}
	return gateway.SendResult{Success: true}, nil
}
func (a *kanbanRecordingAdapter) HandleMessage(_ context.Context, event *gateway.MessageEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handled = append(a.handled, event)
	if a.failWake {
		return &kanban.RuntimeError{Msg: "simulated wake failure"}
	}
	event.SetGatewayAccepted(true)
	return nil
}
func (a *kanbanRecordingAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

// Recording Telegram adapter
func newKanbanRecordingAdapter() *kanbanRecordingAdapter {
	return newKanbanRecordingAdapterOn(gateway.PlatformTelegram)
}

// Recording adapter of a platform
func newKanbanRecordingAdapterOn(platform gateway.Platform) *kanbanRecordingAdapter {
	a := &kanbanRecordingAdapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), platform, a)
	return a
}

// Hermetic home with a kanban database pinned to name and initialised
func kanbanTestDB(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "home"))
	dbPath := filepath.Join(root, name)
	t.Setenv("ICEBATH_KANBAN_DB", dbPath)
	if _, err := kanban.InitDB(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
	return root
}

// Connection to the pinned kanban database closed at test end
func kanbanTestConn(t *testing.T) state.SQLiteConn {
	t.Helper()
	conn, err := kanban.Connect(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func kstr(s string) *string { return &s }

// Task created with title, assignee and an optional session id
func kanbanCreate(t *testing.T, conn state.SQLiteConn, title, assignee string, sessionID *string) string {
	t.Helper()
	tid, err := kanban.CreateTask(context.Background(), conn, kanban.CreateTaskOptions{Title: title, Assignee: kstr(assignee), SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	return tid
}

// Subscribes a chat to a task
func kanbanSubscribe(t *testing.T, conn state.SQLiteConn, o kanban.NotifySubOptions) {
	t.Helper()
	if err := kanban.AddNotifySub(context.Background(), conn, o); err != nil {
		t.Fatal(err)
	}
}

// Completes a task with a summary
func kanbanComplete(t *testing.T, conn state.SQLiteConn, tid, summary string) bool {
	t.Helper()
	ok, err := kanban.CompleteTask(context.Background(), conn, tid, kanban.CompleteTaskOptions{Summary: kstr(summary)})
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// Appends a task scoped event with an optional payload
func kanbanAppend(t *testing.T, conn state.SQLiteConn, tid, kind string, payload *pyjson.Object) {
	t.Helper()
	var body any
	if payload != nil {
		body = payload
	}
	if err := kanban.AppendTaskEvent(context.Background(), conn, tid, kind, body, nil); err != nil {
		t.Fatal(err)
	}
}

// Ordered JSON object from alternating keys and values
func kobj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Bare running runner serving Telegram with the adapter, holding a dispatcher lock handle
func makeKanbanRunner(t *testing.T, adapter gateway.PlatformAdapter) *GatewayRunner {
	t.Helper()
	r := allocGatewayRunner()
	r.running = true
	r.adapters = adapterDict(gateway.PlatformTelegram, adapter)
	r.kanbanSubFailCounts = map[kanbanSubKey]int{}
	f, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	r.kanbanDispatcherLockHandle = f
	return r
}

// Runner whose active profile is name
func setKanbanActiveProfile(r *GatewayRunner, name string) {
	r.primaryProfileName = name
	r.kanbanNotifierProfile = name
}

// Runs one notifier tick, the startup delay skipped and the gateway stopped at the first tick sleep
func runOneNotifierTick(t *testing.T, r *GatewayRunner) {
	t.Helper()
	prev := asyncioSleep
	asyncioSleep = func(ctx context.Context, seconds float64) error {
		if seconds == 5 {
			return nil
		}
		r.running = false
		return nil
	}
	defer func() { asyncioSleep = prev }()
	if err := r.kanbanNotifierWatcher(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
}

// Unseen terminal events of a Telegram subscription
func unseenTerminalEventsFor(t *testing.T, tid, chatID string) []*kanban.Event {
	t.Helper()
	conn, err := kanban.Connect(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, events, err := kanban.UnseenEventsForSub(context.Background(), conn, tid, "telegram", chatID, nil,
		[]string{"completed", "blocked", "gave_up", "crashed", "timed_out"})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// Notify subscriptions of one task
func kanbanSubsOf(t *testing.T, tid string) []*pyjson.Object {
	t.Helper()
	conn, err := kanban.Connect(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	subs, err := kanban.ListNotifySubs(context.Background(), conn, &tid, kanban.NotifyOwnerScope{})
	if err != nil {
		t.Fatal(err)
	}
	return subs
}

func TestKanbanNotifierReplaysTelegramDmTopicDeliveryMetadata(t *testing.T) {
	kanbanTestDB(t, "dm-topic-metadata.db")
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "dm topic task", "worker", kstr("agent:main:telegram:dm:chat-1"))
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-1", ThreadID: kstr("20197"),
		DeliveryMode: kstr("notify+wake"), DeliveryMetadata: map[string]any{"chat_type": "dm", "direct_messages_topic_id": "20197",
			"telegram_dm_topic_reply_fallback": true, "telegram_reply_to_message_id": "462", "thread_id": "20197"}})
	kanbanComplete(t, conn, tid, "done")

	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))

	if len(adapter.sent) != 1 {
		t.Fatalf("sent %d", len(adapter.sent))
	}
	want := map[string]any{"chat_type": "dm", "direct_messages_topic_id": "20197", "telegram_dm_topic_reply_fallback": true,
		"telegram_reply_to_message_id": "462", "thread_id": "20197"}
	got := adapter.sent[0].metadata
	if len(got) != len(want) {
		t.Fatalf("metadata %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("metadata %s = %v want %v", k, got[k], v)
		}
	}
	if len(adapter.handled) != 1 {
		t.Fatalf("handled %d", len(adapter.handled))
	}
	src := adapter.handled[0].Source
	if src.ChatType != "dm" || src.ThreadID == nil || *src.ThreadID != "20197" {
		t.Fatalf("source %+v", src)
	}
}

func TestActiveNamedProfileSubscriptionIsDelivered(t *testing.T) {
	kanbanTestDB(t, "actionable-block.db")
	reason := "AGE-39 — https://linear.example/AGE-39 — publishing verified."
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "approval", "publisher", nil)
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-1", NotifierProfile: kstr("main")})
	if _, err := kanban.BlockTaskWithKind(context.Background(), conn, tid, &reason, kstr("needs_input"), nil); err != nil {
		t.Fatal(err)
	}

	adapter := newKanbanRecordingAdapter()
	r := makeKanbanRunner(t, adapter)
	setKanbanActiveProfile(r, "main")
	runOneNotifierTick(t, r)

	if len(adapter.sent) != 1 {
		t.Fatalf("sent %d", len(adapter.sent))
	}
	msg := adapter.sent[0].text
	if !strings.Contains(msg, tid) || !strings.Contains(msg, "blocked") {
		t.Fatalf("message %q", msg)
	}
}

func TestNonDispatchGatewayClaimsOnlyItsProfileSubscriptions(t *testing.T) {
	kanbanTestDB(t, "cross-profile-notifier.db")
	conn := kanbanTestConn(t)
	foreign := kanbanCreate(t, conn, "default-owned", "worker", nil)
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: foreign, Platform: "telegram", ChatID: "default-chat", NotifierProfile: kstr("default")})
	kanbanComplete(t, conn, foreign, "default done")
	owned := kanbanCreate(t, conn, "writer-owned", "worker", nil)
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: owned, Platform: "telegram", ChatID: "writer-chat", NotifierProfile: kstr("writer")})
	kanbanComplete(t, conn, owned, "writer done")

	adapter := newKanbanRecordingAdapter()
	r := makeKanbanRunner(t, adapter)
	setKanbanActiveProfile(r, "writer")
	r.kanbanDispatcherLockHandle = nil
	runOneNotifierTick(t, r)

	if len(adapter.sent) != 1 || adapter.sent[0].chatID != "writer-chat" || !strings.Contains(adapter.sent[0].text, owned) {
		t.Fatalf("sent %+v", adapter.sent)
	}
	if n := len(unseenTerminalEventsFor(t, foreign, "default-chat")); n != 1 {
		t.Fatalf("foreign unseen %d", n)
	}
}

func TestLegacySubscriptionRequiresConfirmedDispatcherLockOwner(t *testing.T) {
	root := kanbanTestDB(t, "legacy-lock-owner.db")
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "legacy", "worker", nil)
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "legacy-chat"})
	kanbanComplete(t, conn, tid, "legacy done")

	startupAdapter := newKanbanRecordingAdapter()
	startup := makeKanbanRunner(t, startupAdapter)
	startup.kanbanDispatcherLockHandle = nil
	runOneNotifierTick(t, startup)
	if len(startupAdapter.sent) != 0 {
		t.Fatalf("startup sent %+v", startupAdapter.sent)
	}
	if n := len(unseenTerminalEventsFor(t, tid, "legacy-chat")); n != 1 {
		t.Fatalf("unseen %d", n)
	}

	lockPath := filepath.Join(root, ".dispatcher.lock")
	winner, winnerState := acquireSingletonLock(lockPath)
	loser, loserState := acquireSingletonLock(lockPath)
	defer releaseSingletonLock(loser)
	defer releaseSingletonLock(winner)
	if winnerState != "held" || loserState != "contended" {
		t.Fatalf("states %s %s", winnerState, loserState)
	}

	loserAdapter := newKanbanRecordingAdapter()
	loserRunner := makeKanbanRunner(t, loserAdapter)
	loserRunner.kanbanDispatcherLockHandle = loser
	runOneNotifierTick(t, loserRunner)
	if len(loserAdapter.sent) != 0 {
		t.Fatalf("loser sent %+v", loserAdapter.sent)
	}
	if n := len(unseenTerminalEventsFor(t, tid, "legacy-chat")); n != 1 {
		t.Fatalf("unseen %d", n)
	}

	winnerAdapter := newKanbanRecordingAdapter()
	winnerRunner := makeKanbanRunner(t, winnerAdapter)
	winnerRunner.kanbanDispatcherLockHandle = winner
	runOneNotifierTick(t, winnerRunner)
	if len(winnerAdapter.sent) != 1 || winnerAdapter.sent[0].chatID != "legacy-chat" || !strings.Contains(winnerAdapter.sent[0].text, tid) {
		t.Fatalf("winner sent %+v", winnerAdapter.sent)
	}
}

func TestNotifierRedeliversSameKindOnDispatchCycle(t *testing.T) {
	kanbanTestDB(t, "redeliver-cycle.db")
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "cycle test", "worker", nil)
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-1"})
	kanbanAppend(t, conn, tid, "crashed", nil)

	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 1 || !strings.Contains(strings.ToLower(adapter.sent[0].text), "stopped unexpectedly") {
		t.Fatalf("sent %+v", adapter.sent)
	}
	if subs := kanbanSubsOf(t, tid); len(subs) != 1 {
		t.Fatalf("Subscription must survive a crashed event so a respawn-cycle second crash also notifies the user (issue #21398).")
	}
	kanbanAppend(t, conn, tid, "crashed", nil)

	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 2 {
		t.Fatalf("Second crashed event should also notify; got %d deliveries", len(adapter.sent))
	}
	if !strings.Contains(strings.ToLower(adapter.sent[1].text), "stopped unexpectedly") {
		t.Fatalf("second %q", adapter.sent[1].text)
	}
}

func TestNotifierSubscriptionSurvivesDoneReopenUntilArchive(t *testing.T) {
	kanbanTestDB(t, "done-reopen-archive.db")
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "review continuation", "worker", kstr("origin-session"))
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "origin-chat", ThreadID: kstr("origin-thread"),
		UserID: kstr("origin-user"), ChatType: kstr("group"), NotifierProfile: kstr("reviewer"), DeliveryMode: kstr("notify+wake")})
	if !kanbanComplete(t, conn, tid, "first completion") {
		t.Fatal("complete failed")
	}

	adapter := newKanbanRecordingAdapter()
	tick := func() {
		r := makeKanbanRunner(t, adapter)
		setKanbanActiveProfile(r, "reviewer")
		runOneNotifierTick(t, r)
	}
	tick()
	if len(adapter.sent) != 1 || len(adapter.handled) != 1 {
		t.Fatalf("sent %d handled %d", len(adapter.sent), len(adapter.handled))
	}
	if adapter.sent[0].chatID != "origin-chat" || adapter.sent[0].metadata["thread_id"] != "origin-thread" {
		t.Fatalf("sent %+v", adapter.sent[0])
	}
	if src := adapter.handled[0].Source; *src.ThreadID != "origin-thread" || src.Profile == nil || *src.Profile != "reviewer" {
		t.Fatalf("source %+v", src)
	}
	subs := kanbanSubsOf(t, tid)
	if len(subs) != 1 {
		t.Fatal("completion must retain the origin subscription")
	}
	first, _ := subs[0].Get("last_event_id")

	tick()
	if len(adapter.sent) != 1 || len(adapter.handled) != 1 {
		t.Fatalf("replay sent %d handled %d", len(adapter.sent), len(adapter.handled))
	}

	ctx := context.Background()
	if err := kanban.WriteTxn(ctx, conn, false, func() error {
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'ready' WHERE id = ?", tid); err != nil {
			return err
		}
		return kanban.AppendTaskEvent(ctx, conn, tid, "status", kobj("status", "ready"), nil)
	}); err != nil {
		t.Fatal(err)
	}
	if !kanbanComplete(t, conn, tid, "corrected completion") {
		t.Fatal("recomplete failed")
	}
	tick()
	if len(adapter.sent) != 3 || len(adapter.handled) != 2 {
		t.Fatalf("reopen sent %d handled %d", len(adapter.sent), len(adapter.handled))
	}
	for _, s := range adapter.sent {
		if s.chatID != "origin-chat" {
			t.Fatalf("chat %s", s.chatID)
		}
	}
	if src := adapter.handled[1].Source; *src.ThreadID != "origin-thread" || *src.Profile != "reviewer" {
		t.Fatalf("source %+v", src)
	}
	subs = kanbanSubsOf(t, tid)
	if len(subs) != 1 {
		t.Fatalf("subs %d", len(subs))
	}
	if cur, _ := subs[0].Get("last_event_id"); cur.(int64) <= first.(int64) {
		t.Fatalf("cursor %v <= %v", cur, first)
	}
	if ok, err := kanban.ArchiveTask(ctx, conn, tid, nil); err != nil || !ok {
		t.Fatalf("archive %v %v", ok, err)
	}
	tick()
	if len(adapter.sent) != 3 || len(adapter.handled) != 2 {
		t.Fatalf("archive sent %d handled %d", len(adapter.sent), len(adapter.handled))
	}
	if subs := kanbanSubsOf(t, tid); len(subs) != 0 {
		t.Fatalf("subs after archive %d", len(subs))
	}
}

func TestNotifierWakeupUsesSubscriptionChatType(t *testing.T) {
	kanbanTestDB(t, "chat-type-wakeup.db")
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "dm requester", "worker", kstr("origin-session"))
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-dm", ChatType: kstr("dm"), DeliveryMode: kstr("notify+wake")})
	kanbanComplete(t, conn, tid, "done")

	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 1 || len(adapter.handled) != 1 {
		t.Fatalf("sent %d handled %d", len(adapter.sent), len(adapter.handled))
	}
	src := adapter.handled[0].Source
	if src.ChatType != "dm" {
		t.Fatalf("chat type %s", src.ChatType)
	}
	key, err := gateway.BuildSessionKey(context.Background(), src, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if key != "agent:main:telegram:dm:chat-dm" || strings.Contains(key, ":group:") {
		t.Fatalf("wake key %s", key)
	}
}

func TestKanbanNotifierIsolatesPerSubscriptionFailure(t *testing.T) {
	kanbanTestDB(t, "isolation.db")
	conn := kanbanTestConn(t)
	bad := kanbanCreate(t, conn, "bad task", "worker", nil)
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: bad, Platform: "telegram", ChatID: "chat-bad"})
	kanbanComplete(t, conn, bad, "done")
	good := kanbanCreate(t, conn, "good task", "worker", nil)
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: good, Platform: "telegram", ChatID: "chat-good"})
	kanbanComplete(t, conn, good, "done")

	prevClaim, prevList := kanbanClaimUnseenEventsForSub, kanbanListNotifySubs
	t.Cleanup(func() { kanbanClaimUnseenEventsForSub, kanbanListNotifySubs = prevClaim, prevList })
	kanbanClaimUnseenEventsForSub = func(ctx context.Context, c state.SQLiteConn, taskID, platform, chatID string, threadID *string, kinds []string) (int64, int64, []*kanban.Event, error) {
		if taskID == bad {
			return 0, 0, nil, &kanban.RuntimeError{Msg: "simulated DB corruption for bad task"}
		}
		return prevClaim(ctx, c, taskID, platform, chatID, threadID, kinds)
	}
	kanbanListNotifySubs = func(ctx context.Context, c state.SQLiteConn, taskID *string, scope kanban.NotifyOwnerScope) ([]*pyjson.Object, error) {
		subs, err := prevList(ctx, c, taskID, scope)
		sort.SliceStable(subs, func(i, j int) bool {
			return subText(subs[i], "task_id") == bad && subText(subs[j], "task_id") != bad
		})
		return subs, err
	}

	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 1 || !strings.Contains(adapter.sent[0].text, good) {
		t.Fatalf("sent %+v", adapter.sent)
	}
}

func TestNotifierDeliversBlockLoopDetectedTriagePing(t *testing.T) {
	kanbanTestDB(t, "block-loop.db")
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "loops forever", "worker", nil)
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-1"})
	kanbanAppend(t, conn, tid, "block_loop_detected", kobj("reason", "needs credentials", "kind", "needs_input",
		"recurrences", int64(2), "limit", int64(kanban.BlockRecurrenceLimit)))

	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 1 {
		t.Fatal("block_loop_detected must produce a notification")
	}
	if text := adapter.sent[0].text; !strings.Contains(text, tid) || !strings.Contains(text, "needs credentials") {
		t.Fatalf("text %q", text)
	}
	_, remaining, err := kanban.UnseenEventsForSub(context.Background(), conn, tid, "telegram", "chat-1", nil, []string{"block_loop_detected"})
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining %v %v", remaining, err)
	}
}

// Block loop ping of a payload rendered under a fixed head
func fmtBlockLoop(t *testing.T, payload *pyjson.Object) string {
	t.Helper()
	out, err := kanbanEventFormatters["block_loop_detected"](context.Background(), &kanban.Event{Payload: payload}, &kanbanNotification{head: "H123"})
	if err != nil {
		t.Fatal(err)
	}
	return out.msg
}

func TestBlockLoopTechnicalKindUsesNeutralOrchestrationWording(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	msg := fmtBlockLoop(t, kobj("reason", "waiting on upstream", "kind", "transient", "recurrences", int64(2)))
	if !strings.Contains(msg, "for orchestration attention") || strings.Contains(msg, "human decision") ||
		!strings.Contains(msg, "TRIAGE") || !strings.Contains(msg, "waiting on upstream") {
		t.Fatalf("msg %q", msg)
	}
}

func TestBlockLoopOwnerInputKeepsDecisionWording(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	msg := fmtBlockLoop(t, kobj("reason", "Which API key should this use?", "kind", "needs_input", "recurrences", int64(2),
		"limit", int64(kanban.BlockRecurrenceLimit)))
	if !strings.Contains(msg, "needs a human decision") || strings.Contains(msg, "for orchestration attention") ||
		!strings.Contains(msg, "Which API key should this use?") {
		t.Fatalf("msg %q", msg)
	}
}

// Text of the single wake turn the adapter handled
func kanbanWakeText(t *testing.T, adapter *kanbanRecordingAdapter) string {
	t.Helper()
	if len(adapter.handled) != 1 {
		t.Fatalf("expected exactly one wake turn, got %d", len(adapter.handled))
	}
	return adapter.handled[0].Text
}

// Task handed to review with a summary and a dm subscription in the mode
func reviewHandoffTask(t *testing.T, mode, summary string) string {
	t.Helper()
	ctx := context.Background()
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "implement the thing", "worker", kstr("agent:main:telegram:dm:chat-1"))
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-1", ChatType: kstr("dm"), DeliveryMode: kstr(mode)})
	if _, err := kanban.ClaimTask(ctx, conn, tid, nil, nil); err != nil {
		t.Fatal(err)
	}
	task, err := kanban.GetTask(ctx, conn, tid)
	if err != nil {
		t.Fatal(err)
	}
	ok, _, err := kanban.RequestReview(ctx, conn, tid, kanban.RequestReviewOptions{Summary: kstr(summary), ExpectedRunID: task.CurrentRunID})
	if err != nil || !ok {
		t.Fatalf("request review %v %v", ok, err)
	}
	return tid
}

const reviewHandoffSummary = "PR ready: https://example.invalid/pr/7\nfull details below"

func TestReviewRequestedWakesTheOriginSession(t *testing.T) {
	kanbanTestDB(t, "review-wake.db")
	tid := reviewHandoffTask(t, "notify+wake", reviewHandoffSummary)
	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 1 {
		t.Fatal("the passive review ping is unchanged")
	}
	wake := kanbanWakeText(t, adapter)
	if !strings.Contains(wake, tid) || !strings.Contains(wake, "PR ready: https://example.invalid/pr/7") {
		t.Fatalf("wake %q", wake)
	}
}

func TestBlockLoopDetectedWakesTheOriginSession(t *testing.T) {
	kanbanTestDB(t, "triage-wake.db")
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "loops forever", "worker", kstr("agent:main:telegram:dm:chat-1"))
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-1", ChatType: kstr("dm"), DeliveryMode: kstr("notify+wake")})
	kanbanAppend(t, conn, tid, "block_loop_detected", kobj("reason", "needs credentials", "kind", "needs_input",
		"recurrences", int64(2), "limit", int64(kanban.BlockRecurrenceLimit)))
	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 1 || !strings.Contains(kanbanWakeText(t, adapter), tid) {
		t.Fatalf("sent %d", len(adapter.sent))
	}
}

func TestReviewRequestedDoesNotWakeANotifyOnlySubscription(t *testing.T) {
	kanbanTestDB(t, "review-notify.db")
	reviewHandoffTask(t, "notify", reviewHandoffSummary)
	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.sent) != 1 || len(adapter.handled) != 0 {
		t.Fatalf("notify-only subscriptions must not be woken by a review handoff: sent %d handled %d", len(adapter.sent), len(adapter.handled))
	}
}
