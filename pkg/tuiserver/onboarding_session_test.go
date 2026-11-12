package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Facts every onboarding session case starts its runs with
var onbSessionFacts = tools.OnboardingFacts{OS: "macos", Arch: "arm64", MachineKind: "Mac", FactsSource: "local", Plugins: []string{"blender"}, Tours: true}

// Launch root of one case with an openrouter key to run on
func onboardingSessionRoot(t *testing.T) string {
	t.Helper()
	root := onboardingLaunchRoot(t)
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("OPENROUTER_API_KEY=sk-or-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// Empty launch root of one case, its launch store, multiplex latch and served profiles restored after
func onboardingLaunchRoot(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	mkdirAll(t, root)
	t.Setenv("ICEBATH_HOME", root)
	setLaunchHome(t, root)
	freshLaunchStore(t)
	t.Cleanup(tools.ConfineInstallProbeForTests(t.TempDir()))
	prevSkills := tools.AllSkillNames
	tools.AllSkillNames = skills.AllSkillNames
	t.Cleanup(func() { tools.AllSkillNames = prevSkills })
	prevMultiplex := provider.IsMultiplexActive(context.Background())
	provider.SetMultiplexActive(false)
	servedProfileMu.Lock()
	prevServed := servedProfileHomes
	servedProfileHomes = map[string]bool{}
	servedProfileMu.Unlock()
	tools.OnboardingDueResetForTests()
	t.Cleanup(func() {
		provider.SetMultiplexActive(prevMultiplex)
		servedProfileMu.Lock()
		servedProfileHomes = prevServed
		servedProfileMu.Unlock()
		tools.OnboardingDueResetForTests()
	})
	return root
}

// Drops the launch store handle for the case, the store of its home opened on first use and released after
func freshLaunchStore(t *testing.T) {
	t.Helper()
	dbMu.Lock()
	savedDB, savedErr := sharedDB, dbError
	sharedDB, dbError = nil, ""
	dbMu.Unlock()
	t.Cleanup(func() {
		dbMu.Lock()
		opened := sharedDB
		sharedDB, dbError = savedDB, savedErr
		dbMu.Unlock()
		if opened != nil && opened != savedDB {
			state.Release(opened)
		}
	})
}

// One RPC the onboarding start drove, its kind and params
type onbClientCall struct {
	kind   string
	params map[string]any
}

// Recorder of the RPCs onboarding.start drives, a submit storing its user turn as the real turn does
type onbClient struct {
	mu    sync.Mutex
	calls []onbClientCall
}

func (c *onbClient) record(kind string, params map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, onbClientCall{kind, params})
}

func (c *onbClient) snapshot() []onbClientCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]onbClientCall(nil), c.calls...)
}

// Submit double that stores the user turn of the hidden opening
func (c *onbClient) storingSubmit(t *testing.T) Handler {
	return func(ctx context.Context, rid any, params map[string]any) map[string]any {
		c.record("submit", params)
		sid := strings.TrimPrefix(relayString(params["session_id"]), "rt-")
		if _, err := getDB(ctx).AppendMessage(ctx, sid, map[string]any{"role": "user", "content": params["text"]}, state.AppendGuards{}); err != nil {
			t.Error(err)
		}
		return map[string]any{"result": map[string]any{"status": "streaming"}}
	}
}

// Installs the resume and submit recorders and answers the local Desktop context only it starts a first run in
func onboardingSessionClient(t *testing.T) (*onbClient, context.Context) {
	t.Helper()
	client := &onbClient{}
	swapMethod(t, "session.resume", func(_ context.Context, rid any, params map[string]any) map[string]any {
		client.record("resume", params)
		return map[string]any{"result": map[string]any{"session_id": "rt-" + relayString(params["session_id"])}}
	})
	swapMethod(t, "prompt.submit", client.storingSubmit(t))
	return client, tools.WithServingLocalDesktop(context.Background(), true)
}

// Result member of one reply through the real request path
func onbResult(t *testing.T, ctx context.Context, method string, params map[string]any) map[string]any {
	t.Helper()
	reply := welcomeClaimCall(ctx, method, params)
	result, isDict := reply["result"].(map[string]any)
	if !isDict {
		t.Fatalf("%s answered %v", method, reply)
	}
	return result
}

// Starts the onboarding record of a new desktop chat in db
func onbStartRecord(t *testing.T, db *state.DB, sid string) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "desktop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.StartOnboarding(context.Background(), db, sid, onbSessionFacts); err != nil {
		t.Fatal(err)
	}
}

// Phase of the stored run of one chat
func onbPhase(db *state.DB, sid string) any { return tools.LoadOnboardingRecord(db, sid)["phase"] }

// Server request frames and events of the session request sinks, every client answering
type onbRequestSinks struct {
	mu     sync.Mutex
	frames []*pyjson.Object
	events [][2]any
}

func (s *onbRequestSinks) firstFrameID() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.frames) == 0 {
		return nil
	}
	return s.frames[0].GetOr("id", nil)
}

func (s *onbRequestSinks) eventList() [][2]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]any(nil), s.events...)
}

// Binds recording request sinks for the case and the bound set back after it
func installOnbRequestSinks(t *testing.T) *onbRequestSinks {
	t.Helper()
	sinks := &onbRequestSinks{}
	write, emit, answerable, clients := tuigateway.CurrentSinks()
	tuigateway.BindSinks(func(frame *pyjson.Object) {
		sinks.mu.Lock()
		sinks.frames = append(sinks.frames, frame)
		sinks.mu.Unlock()
	}, func(kind, _ string, payload map[string]any) {
		sinks.mu.Lock()
		sinks.events = append(sinks.events, [2]any{kind, payload})
		sinks.mu.Unlock()
	}, func(string) bool { return true }, clients)
	t.Cleanup(func() { tuigateway.BindSinks(write, emit, answerable, clients) })
	return sinks
}

// Setup beat card waiting on the client in the background, its reply delivered on the channel
func waitingSetupCard(t *testing.T, ctx context.Context, sid string) <-chan *pyjson.Object {
	t.Helper()
	reply := make(chan *pyjson.Object, 1)
	go func() {
		reply <- setupBeatRequest(ctx, sid, map[string]any{"beat": "theme", "card": map[string]any{"picker": "accent"}})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for tuigateway.PendingKind(sid) != "setup_beat" {
		if time.Now().After(deadline) {
			t.Fatal("the setup card never waited on the client")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return reply
}

// Reply of a waiting card once it returned, failing after five seconds
func cardReply(t *testing.T, reply <-chan *pyjson.Object) *pyjson.Object {
	t.Helper()
	select {
	case got := <-reply:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting card never returned")
	}
	return nil
}

// Registers a live session record for the case
func onbLiveSession(t *testing.T, sid string, fields map[string]any) {
	t.Helper()
	tuigateway.SetSessionRecord(sid, tuigateway.NewSession(fields))
	t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
}

func TestStartCreatesOneOnboardingChatAndPlaysTheOpeningOnce(t *testing.T) {
	onboardingSessionRoot(t)
	client, ctx := onboardingSessionClient(t)
	started := onbResult(t, ctx, "onboarding.start", map[string]any{"locale": "en-IN"})
	stored, _ := started["stored_session_id"].(string)
	db := getDB(ctx)
	runID := tools.LoadOnboardingRecord(db, stored)["run_id"]
	if want := map[string]any{"session_id": "rt-" + stored, "stored_session_id": stored, "resumed": false, "run_id": runID}; !reflect.DeepEqual(started, want) {
		t.Fatalf("started %v want %v", started, want)
	}
	if phase := onbPhase(db, stored); phase != "onboarding" {
		t.Fatalf("phase %v", phase)
	}
	calls := client.snapshot()
	if len(calls) < 2 || calls[0].kind != "resume" || !reflect.DeepEqual(calls[0].params, map[string]any{"profile": "default", "session_id": stored, "source": "desktop"}) {
		t.Fatalf("calls %v", calls)
	}
	text, _ := calls[1].params["text"].(string)
	if calls[1].kind != "submit" || calls[1].params["display_kind"] != "hidden" || !strings.Contains(text, "first-run onboarding") || !strings.Contains(text, "setup_beat") {
		t.Fatalf("opening submit %v", calls[1])
	}
	if got := onbResult(t, ctx, "onboarding.state", nil)["session_id"]; got != stored {
		t.Fatalf("state session_id %v", got)
	}
	again := onbResult(t, ctx, "onboarding.start", nil)
	if again["stored_session_id"] != stored || again["resumed"] != true || len(client.snapshot()) != 3 {
		t.Fatalf("again %v calls %d", again, len(client.snapshot()))
	}
	if onbResult(t, ctx, "onboarding.start", map[string]any{"rerun": true})["stored_session_id"] == stored {
		t.Fatal("a rerun reopened the old chat")
	}
}

func TestARestartBeforeTheOpeningTurnPlaysTheOpeningOnRelaunch(t *testing.T) {
	onboardingSessionRoot(t)
	client, ctx := onboardingSessionClient(t)
	swapMethod(t, "prompt.submit", func(_ context.Context, rid any, params map[string]any) map[string]any {
		client.record("lost", params)
		return map[string]any{"result": map[string]any{"status": "streaming"}}
	})
	first := onbResult(t, ctx, "onboarding.start", nil)
	stored, _ := first["stored_session_id"].(string)
	swapMethod(t, "prompt.submit", client.storingSubmit(t))
	reopened := onbResult(t, ctx, "onboarding.start", nil)
	if want := map[string]any{"session_id": "rt-" + stored, "stored_session_id": stored, "resumed": true, "run_id": first["run_id"]}; !reflect.DeepEqual(reopened, want) {
		t.Fatalf("reopened %v want %v", reopened, want)
	}
	kinds := []string{}
	for _, call := range client.snapshot() {
		kinds = append(kinds, call.kind)
	}
	if !reflect.DeepEqual(kinds, []string{"resume", "lost", "resume", "submit"}) {
		t.Fatalf("kinds %v", kinds)
	}
}

func TestTheTurnNoteRidesAUserTurnOnlyWhileTheRunIsOpen(t *testing.T) {
	onboardingSessionRoot(t)
	ctx := context.Background()
	db := getDB(ctx)
	onbStartRecord(t, db, "onb")
	if _, err := tools.AdvanceOnboarding(ctx, db, "onb", tools.OnbShown{Beat: "name"}); err != nil {
		t.Fatal(err)
	}
	a := testAgent(t, agent.AIAgentOptions{SessionDB: db, SessionID: "onb", Platform: "desktop", EnabledToolsets: []string{"onboarding", "terminal"}})
	if !a.ValidToolNames()["setup_beat"] {
		t.Fatalf("valid tools %v", a.ValidToolNames())
	}
	stageOnboardingTurnNote(ctx, "ui", a, true)
	if notes := a.GatewayTurnContextNotes(); notes != "" {
		t.Fatalf("notes on an empty history %q", notes)
	}
	stageOnboardingTurnNote(ctx, "ui", a, false)
	if notes := a.GatewayTurnContextNotes(); !strings.Contains(notes, `setup_beat(beat="name")`) {
		t.Fatalf("notes %q", notes)
	}
}

func TestSkipSetupEndsTheRunAndClosesTheWaitingCard(t *testing.T) {
	root := onboardingSessionRoot(t)
	ctx := context.Background()
	db := getDB(ctx)
	onbStartRecord(t, db, "onb")
	sid := "ui-onb"
	onbLiveSession(t, sid, map[string]any{"session_key": "onb", "source": "desktop"})
	sinks := installOnbRequestSinks(t)
	reply := waitingSetupCard(t, ctx, sid)
	if got := onbResult(t, ctx, "onboarding.skip", map[string]any{"session_id": sid}); !reflect.DeepEqual(got, map[string]any{"skipped": true}) {
		t.Fatalf("skip %v", got)
	}
	if got := cardReply(t, reply); got != nil {
		t.Fatalf("card reply %v", got)
	}
	want := [][2]any{{"request.cancel", map[string]any{"id": sinks.firstFrameID(), "method": "setup_beat", "reason": "resolved"}}}
	if events := sinks.eventList(); !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v want %v", events, want)
	}
	if phase := onbPhase(db, "onb"); phase != "skipped" {
		t.Fatalf("phase %v", phase)
	}
	section := onboardingSection(t, root)
	if section["run"] != false || onboardingSeen(section)["profile_build_offered"] != true {
		t.Fatalf("onboarding section %v", section)
	}
}

func TestSkipThatReadTheIDBeforeARotationEndsTheRunAtTheTip(t *testing.T) {
	root := onboardingSessionRoot(t)
	ctx := context.Background()
	db := getDB(ctx)
	onbStartRecord(t, db, "parent")
	if err := db.PublishCompressionChild(ctx, state.CompressionChildPublish{ParentSessionID: "parent", ChildSessionID: "child", Source: "desktop",
		Messages: []map[string]any{{"role": "user", "content": "summary"}}}); err != nil {
		t.Fatal(err)
	}
	onbLiveSession(t, "ui-onb", map[string]any{"session_key": "parent", "source": "desktop"})
	if got := onbResult(t, ctx, "onboarding.skip", map[string]any{"session_id": "ui-onb"}); !reflect.DeepEqual(got, map[string]any{"skipped": true}) {
		t.Fatalf("skip %v", got)
	}
	if phase := onbPhase(db, "child"); phase != "skipped" {
		t.Fatalf("child phase %v", phase)
	}
	if active := tools.ActiveOnboardingSessionID(db); active != "" {
		t.Fatalf("active %q", active)
	}
	if run := onboardingSection(t, root)["run"]; run != false {
		t.Fatalf("run %v", run)
	}
}

func TestARerunEndsTheRunItReplacesSoFinishingTheNewOneClosesTheFlag(t *testing.T) {
	root := onboardingSessionRoot(t)
	_, ctx := onboardingSessionClient(t)
	broadcast := &frameRecorder{}
	tuigateway.RegisterLiveTransport(broadcast)
	t.Cleanup(func() { tuigateway.UnregisterLiveTransport(broadcast) })
	old := onbResult(t, ctx, "onboarding.start", nil)
	newID, _ := onbResult(t, ctx, "onboarding.start", map[string]any{"rerun": true})["stored_session_id"].(string)
	db := getDB(ctx)
	oldID, _ := old["stored_session_id"].(string)
	if ended := tools.LoadOnboardingRecord(db, oldID)["ended_by"]; ended != "replaced" {
		t.Fatalf("ended_by %v", ended)
	}
	for range 2 {
		if _, err := tools.AdvanceOnboarding(ctx, db, newID, tools.OnbEnd{Outcome: "skipped"}); err != nil {
			t.Fatal(err)
		}
	}
	got := [][2]any{}
	broadcast.mu.Lock()
	for _, frame := range broadcast.frames {
		params, _ := frame.GetOr("params", nil).(*pyjson.Object)
		if params != nil && params.GetOr("type", nil) == "onboarding.ended" {
			got = append(got, [2]any{"onboarding.ended", pyjson.Plain(params.GetOr("payload", nil))})
		}
	}
	broadcast.mu.Unlock()
	want := [][2]any{
		{"onboarding.ended", map[string]any{"run_id": old["run_id"], "stored_session_id": oldID, "outcome": "replaced"}},
		{"onboarding.ended", map[string]any{"run_id": tools.LoadOnboardingRecord(db, newID)["run_id"], "stored_session_id": newID, "outcome": "skipped"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("broadcast %v want %v", got, want)
	}
	if active := tools.ActiveOnboardingSessionID(db); active != "" {
		t.Fatalf("active %q", active)
	}
	if run := onboardingSection(t, root)["run"]; run != false {
		t.Fatalf("run %v", run)
	}
}

func TestARerunNeverWritesTheInstallFlagOffAndHealsLeftoverOpenRuns(t *testing.T) {
	root := onboardingSessionRoot(t)
	_, ctx := onboardingSessionClient(t)
	db := getDB(ctx)
	for _, leftover := range []string{"crash-a", "crash-b"} {
		onbStartRecord(t, db, leftover)
	}
	if got := onbResult(t, ctx, "onboarding.start", nil)["stored_session_id"]; got != "crash-b" {
		t.Fatalf("start reopened %v", got)
	}
	if a, b := onbPhase(db, "crash-a"), onbPhase(db, "crash-b"); a != "skipped" || b != "onboarding" {
		t.Fatalf("phases %v %v", a, b)
	}
	newID, _ := onbResult(t, ctx, "onboarding.start", map[string]any{"rerun": true})["stored_session_id"].(string)
	if b, n := onbPhase(db, "crash-b"), onbPhase(db, newID); b != "skipped" || n != "onboarding" {
		t.Fatalf("phases %v %v", b, n)
	}
	section := onboardingSection(t, root)
	if section["run"] != true || onboardingSeen(section)["profile_build_offered"] != nil {
		t.Fatalf("a rerun wrote the install flag off, section %v", section)
	}
}

func TestARerunClosesTheCardWaitingInTheChatItReplaces(t *testing.T) {
	onboardingSessionRoot(t)
	_, ctx := onboardingSessionClient(t)
	old, _ := onbResult(t, ctx, "onboarding.start", nil)["stored_session_id"].(string)
	sid := "ui-old"
	onbLiveSession(t, sid, map[string]any{"session_key": old, "source": "desktop"})
	sinks := installOnbRequestSinks(t)
	reply := waitingSetupCard(t, ctx, sid)
	onbResult(t, ctx, "onboarding.start", map[string]any{"rerun": true})
	if got := cardReply(t, reply); got != nil {
		t.Fatalf("card reply %v", got)
	}
	if events := sinks.eventList(); len(events) == 0 || events[0][0] != "request.cancel" {
		t.Fatalf("events %v", events)
	}
}

func TestDeletingTheOnboardingChatEndsItsRunAndClosesTheFlag(t *testing.T) {
	onboardingSessionRoot(t)
	ctx := context.Background()
	onbStartRecord(t, getDB(ctx), "onb")
	if got := onbResult(t, ctx, "session.delete", map[string]any{"session_id": "onb"}); !reflect.DeepEqual(got, map[string]any{"deleted": "onb"}) {
		t.Fatalf("delete %v", got)
	}
	want := map[string]any{"run": false, "eligible": false, "session_id": nil, "run_id": nil}
	if got := onbResult(t, ctx, "onboarding.state", nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("state %v want %v", got, want)
	}
}

func TestTheOnboardingFactsAreReadInTheDefaultProfile(t *testing.T) {
	root := onboardingSessionRoot(t)
	_, ctx := onboardingSessionClient(t)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("display:\n  in_app_tours: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "profiles", "work")
	mkdirAll(t, filepath.Join(work, "skills"))
	if err := os.WriteFile(filepath.Join(work, "config.yaml"), []byte("model: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setLaunchHome(t, work)
	t.Setenv("ICEBATH_HOME", work)
	swapMethod(t, "prompt.submit", func(context.Context, any, map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{}}
	})
	stored, _ := onbResult(t, ctx, "onboarding.start", nil)["stored_session_id"].(string)
	rootDB, err := state.NewSessionDB(ctx, filepath.Join(root, "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer rootDB.Close()
	facts, _ := tools.LoadOnboardingRecord(rootDB, stored)["facts"].(map[string]any)
	if facts == nil || facts["tours"] != false {
		t.Fatalf("facts %v were not read in the default profile", facts)
	}
}

func TestAStartOutsideTheLocalDesktopSchedulesNothing(t *testing.T) {
	root := onboardingSessionRoot(t)
	reply := welcomeClaimCall(context.Background(), "onboarding.start", nil)
	if code := welcomeClaimErrorCode(reply); !pyjson.Equal(code, 5080) {
		t.Fatalf("reply %v", reply)
	}
	if _, explicit := onboardingSection(t, root)["run"]; explicit {
		t.Fatal("onboarding.run was written")
	}
}

// Compute host supervisor of the case, its child acking every respond frame with one cancelled request
func ackingHostSupervisor(t *testing.T) (*HostSupervisor, func() [][2]any) {
	t.Helper()
	sup, _ := testSupervisor(t)
	var mu sync.Mutex
	var forwarded [][2]any
	sup.sendFrameFn = func(frame *pyjson.Object) error {
		if frame.GetOr("type", nil) == "respond" {
			mu.Lock()
			forwarded = append(forwarded, [2]any{frame.GetOr("sid", nil), pyjson.Plain(frame.GetOr("params", nil))})
			mu.Unlock()
			go sup.handleHostFrame(pyjson.ObjectOf("type", "respond.ack", "request_id", frame.GetOr("request_id", nil),
				"response", pyjson.ObjectOf("result", pyjson.ObjectOf("cancelled", int64(1)))))
		}
		return nil
	}
	computeHostSupervisorMu.Lock()
	prev := computeHostSupervisor
	computeHostSupervisor = sup
	computeHostSupervisorMu.Unlock()
	t.Cleanup(func() {
		computeHostSupervisorMu.Lock()
		computeHostSupervisor = prev
		computeHostSupervisorMu.Unlock()
	})
	return sup, func() [][2]any {
		mu.Lock()
		defer mu.Unlock()
		return append([][2]any(nil), forwarded...)
	}
}

func TestSkipWithdrawsTheCardAComputeHostChildWaitsOn(t *testing.T) {
	root := onboardingSessionRoot(t)
	writeFile(t, filepath.Join(root, "config.yaml"), "dashboard:\n  turn_isolation: true\n")
	ctx := context.Background()
	_, forwarded := ackingHostSupervisor(t)
	db := getDB(ctx)
	onbStartRecord(t, db, "onb")
	sid := "ui-onb"
	onbLiveSession(t, sid, map[string]any{"session_key": "onb", "agent": nil, "source": "desktop",
		"agent_ready": &pythread.Event{}, "_compute_host_active": true, "history_lock": &sync.Mutex{}})
	installOnbRequestSinks(t)
	relayComputeHostRPC(ctx, pyjson.ObjectOf("jsonrpc", "2.0", "id", "srq-host", "method", "setup_beat",
		"params", pyjson.ObjectOf("session_id", sid, "beat", "theme", "card", pyjson.ObjectOf("picker", "accent"))))
	if got := onbResult(t, ctx, "onboarding.skip", map[string]any{"session_id": sid}); !reflect.DeepEqual(got, map[string]any{"skipped": true}) {
		t.Fatalf("skip %v", got)
	}
	want := [][2]any{{sid, map[string]any{"cancel": map[string]any{"method": "setup_beat", "reason": "resolved"}}}}
	if got := forwarded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("forwarded %v want %v", got, want)
	}
	if phase := onbPhase(db, "onb"); phase != "skipped" {
		t.Fatalf("phase %v", phase)
	}
}
