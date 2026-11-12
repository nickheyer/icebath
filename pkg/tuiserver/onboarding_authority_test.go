package tuiserver

import (
	"context"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Facts every authority onboarding case reads in place of the host
var onbAuthorityFacts = tools.OnboardingFacts{OS: "macos", Arch: "arm64", MachineKind: "Mac", FactsSource: "local"}

// Runner of the gateway the desktop cases connect to, each admitted turn recorded and answered ok
type onbAuthorityRunner struct {
	store     *gateway.SessionStore
	db        *state.SessionDB
	mu        sync.Mutex
	authority *gateway.SessionAuthority
	registry  *gateway.SessionAuthorities
	multiplex bool
	answers   bool
	turns     []*gateway.MessageEvent
	adapters  *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
	profiles  map[string]*pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
}

func (r *onbAuthorityRunner) Adapters() *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	return r.adapters
}

func (r *onbAuthorityRunner) ProfileAdapterMap(name string) *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.profiles[name] == nil {
		r.profiles[name] = pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	}
	return r.profiles[name]
}

func (r *onbAuthorityRunner) SessionRuntimeDescriptor() map[string]any           { return nil }
func (r *onbAuthorityRunner) Draining() bool                                     { return false }
func (r *onbAuthorityRunner) Running() bool                                      { return true }
func (r *onbAuthorityRunner) SessionStore() *gateway.SessionStore                { return r.store }
func (r *onbAuthorityRunner) SessionDB(context.Context) *state.SessionDB         { return r.db }
func (r *onbAuthorityRunner) EvictCachedAgent(string)                            {}
func (r *onbAuthorityRunner) CachedAgentFor(string) any                          { return nil }
func (r *onbAuthorityRunner) PeekSessionState(string) gateway.PeekedSessionState { return nil }
func (r *onbAuthorityRunner) AdapterForSource(context.Context, *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	return nil, nil
}

func (r *onbAuthorityRunner) MultiplexProfiles() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.multiplex
}

func (r *onbAuthorityRunner) SetSessionAuthority(a *gateway.SessionAuthority) {
	r.mu.Lock()
	r.authority = a
	r.mu.Unlock()
}

func (r *onbAuthorityRunner) SessionAuthority() *gateway.SessionAuthority {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.authority
}

func (r *onbAuthorityRunner) SessionAuthorities() *gateway.SessionAuthorities {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registry
}

func (r *onbAuthorityRunner) HandleMessage(_ context.Context, event *gateway.MessageEvent) (any, error) {
	if !r.answers {
		return nil, &gateway.PyError{Kind: "AttributeError", Msg: "'types.SimpleNamespace' object has no attribute '_handle_message'"}
	}
	r.mu.Lock()
	r.turns = append(r.turns, event)
	r.mu.Unlock()
	return "ok", nil
}

// Turns the runner answered so far
func (r *onbAuthorityRunner) answered() []*gateway.MessageEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*gateway.MessageEvent(nil), r.turns...)
}

// The local Desktop connected to the launch authority over a native ticket
type onbDesktop struct {
	ctx       context.Context
	tmp       string
	root      string
	authority *gateway.SessionAuthority
	conn      *gateway.AuthorityConnection
	sock      *wsTestSocket
	transport *tuigateway.WSTransport
	db        *state.SessionDB
	runner    *onbAuthorityRunner
}

// Desktop over a fresh openrouter home whose gateway answers every turn ok
func newOnbDesktop(t *testing.T) *onbDesktop {
	t.Helper()
	root := onboardingSessionRoot(t)
	prevFacts := tools.ReadOnboardingFacts
	tools.ReadOnboardingFacts = func(*tools.Env, string, string) tools.OnboardingFacts { return onbAuthorityFacts }
	t.Cleanup(func() { tools.ReadOnboardingFacts = prevFacts })
	writeFile(t, filepath.Join(root, "config.yaml"), "model:\n  provider: openrouter\n  default: anthropic/claude-sonnet-4\n")
	d := authorityDesktop(t, root, true)
	d.connect(t, true, true)
	return d
}

// The launch authority of the root on its gateway store, the runner answering turns when answers
func authorityDesktop(t *testing.T, root string, answers bool) *onbDesktop {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store := gateway.NewSessionStore(ctx, filepath.Join(root, "sessions"), gateway.DefaultGatewayConfig(ctx), nil)
	opened, err := store.DB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	db, ok := opened.(*state.SessionDB)
	if !ok || db == nil {
		t.Fatalf("store db %T", opened)
	}
	runner := &onbAuthorityRunner{store: store, db: db, answers: answers,
		adapters: pydict.New[gateway.Platform, gateway.PlatformAdapter](), profiles: map[string]*pydict.Dict[gateway.Platform, gateway.PlatformAdapter]{}}
	authority, err := gateway.InitializeSessionAuthority(ctx, runner, root, "owner", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	d := &onbDesktop{ctx: ctx, tmp: filepath.Dir(root), root: root, authority: authority, db: db, runner: runner}
	t.Cleanup(func() {
		if d.conn != nil {
			_ = d.conn.Close(ctx)
		}
	})
	return d
}

// The desktop's next connection, on a new Desktop socket when fresh, bound to the native bootstrap when native
func (d *onbDesktop) connect(t *testing.T, fresh, native bool) {
	t.Helper()
	identity := map[string]any{"user_id": "uid:1", "provider": "local", "profile_id": d.authority.ProfileID, "instance_id": "owner"}
	if native {
		identity["native_bootstrap"] = true
	}
	if fresh {
		d.sock = newWSTestSocket()
		d.transport = tuigateway.NewWSTransport(d.sock, "test", nil)
		d.transport.DesktopLocal = true
	}
	conn, err := gateway.NewAuthorityConnection(d.authority, d.transport, identity, false)
	if err != nil {
		t.Fatal(err)
	}
	d.conn = conn
	if native {
		gateway.BindNativeTransport(d.authority, conn.Actor(), identity)
	}
}

// One request through the ws dispatch, the reply read from the socket when the dispatch answers later
func (d *onbDesktop) request(t *testing.T, rid int64, method string, params *pyjson.Object) map[string]any {
	t.Helper()
	req := pyjson.ObjectOf("jsonrpc", "2.0", "id", rid, "method", method, "params", params)
	reply, err := wsDispatchRequest(d.ctx, d.conn, req, method, d.transport)
	if err != nil {
		t.Fatal(err)
	}
	if reply != nil {
		plain, _ := pyjson.Plain(reply).(map[string]any)
		return plain
	}
	var written map[string]any
	found := waitFor(func() bool {
		for _, frame := range d.sock.frames() {
			if pyrepr.Equal(frame["id"], rid) {
				written = frame
				return true
			}
		}
		return false
	}, 20*time.Second)
	if !found {
		t.Fatalf("no reply to %s %d", method, rid)
	}
	return written
}

// The stored id an onboarding.start answered with
func (d *onbDesktop) start(t *testing.T, rid int64, params *pyjson.Object) string {
	t.Helper()
	started := d.request(t, rid, "onboarding.start", params)
	result, ok := started["result"].(map[string]any)
	if !ok {
		t.Fatalf("start answered %v", started)
	}
	stored, _ := result["stored_session_id"].(string)
	return stored
}

// Waits for the first turn the gateway answered
func (d *onbDesktop) firstTurn(t *testing.T) *gateway.MessageEvent {
	t.Helper()
	if !waitFor(func() bool { return len(d.runner.answered()) > 0 }, 10*time.Second) {
		t.Fatal("no turn reached the gateway")
	}
	return d.runner.answered()[0]
}

// Waits until the opening turn's lease is released and a delete is not refused as a write in flight
func (d *onbDesktop) settled(t *testing.T, sessionID string) {
	t.Helper()
	if !waitFor(func() bool {
		guarded, err := d.db.GuardedIDs(d.ctx, []string{sessionID})
		return err == nil && len(guarded) == 0
	}, 10*time.Second) {
		t.Fatalf("%s never settled", sessionID)
	}
}

// Whether the authority holds the live session
func (d *onbDesktop) live(sessionID string) bool {
	_, ok := d.authority.Live(sessionID)
	return ok
}

func TestTheWindowResumesTheChatOnboardingStartOpened(t *testing.T) {
	for _, row := range []struct {
		id    string
		start *pyjson.Object
	}{
		{"rerun", pyjson.ObjectOf("facts_source", "local", "locale", "en", "rerun", true)},
		{"launch", pyjson.ObjectOf("facts_source", "local", "locale", "en", "fresh_launch", true)},
	} {
		t.Run(row.id, func(t *testing.T) {
			d := newOnbDesktop(t)
			stored := d.start(t, 1, row.start)
			resumed := d.request(t, 2, "session.resume", pyjson.ObjectOf("session_id", stored))
			if _, failed := resumed["error"]; failed {
				t.Fatalf("resume answered %v", resumed)
			}
			result, _ := resumed["result"].(map[string]any)
			if result["session_id"] != stored {
				t.Fatalf("resumed %v, want %s", result["session_id"], stored)
			}
			if !strings.HasPrefix(stored, "local-") || !d.live(stored) {
				t.Fatalf("stored %q live %v", stored, d.live(stored))
			}
			turn := d.firstTurn(t)
			if !strings.Contains(turn.Text, "first-run onboarding") || !strings.Contains(turn.Text, "setup_beat") {
				t.Fatalf("opening turn %q", turn.Text)
			}
		})
	}
}

// Phase of the run record of a chat, nil when it holds none
func (d *onbDesktop) phase(sessionID string) any {
	return tools.LoadOnboardingRecord(d.db, sessionID)["phase"]
}

// A setup_beat card asked in the chat on its own goroutine, its answer delivered on the channel
func askSetupCard(t *testing.T, live *gateway.LiveSession, sessionID string, params *pyjson.Object, timeout float64) <-chan *pyjson.Object {
	t.Helper()
	answer := make(chan *pyjson.Object, 1)
	go func() {
		answer <- gateway.AskViewerResult(live.EventStream, sessionID, "setup_beat", params, nil, &timeout, nil)
	}()
	if !waitFor(func() bool { return gateway.ServerRequestWaiting(live.EventStream, sessionID, "setup_beat") }, 5*time.Second) {
		t.Fatal("the setup_beat card never waited")
	}
	return answer
}

// The answer a card received, failing when none arrives in time
func cardAnswer(t *testing.T, answer <-chan *pyjson.Object, within time.Duration) *pyjson.Object {
	t.Helper()
	select {
	case got := <-answer:
		return got
	case <-time.After(within):
		t.Fatal("the card never returned")
		return nil
	}
}

func TestARerunReplacesTheOpenRunAndSkipEndsTheNewOne(t *testing.T) {
	d := newOnbDesktop(t)
	first := d.start(t, 1, pyjson.ObjectOf("rerun", true))
	second := d.start(t, 2, pyjson.ObjectOf("rerun", true))
	if second == first {
		t.Fatalf("the rerun reopened %s", first)
	}
	if d.phase(first) == "onboarding" || d.phase(second) != "onboarding" {
		t.Fatalf("phases %v %v", d.phase(first), d.phase(second))
	}
	again := d.request(t, 3, "onboarding.start", pyjson.ObjectOf())
	result, _ := again["result"].(map[string]any)
	if result["stored_session_id"] != second || result["resumed"] != true {
		t.Fatalf("plain start answered %v", again)
	}
	live, ok := d.authority.Live(second)
	if !ok {
		t.Fatalf("%s is not live", second)
	}
	answer := askSetupCard(t, live, second, pyjson.ObjectOf("beat", "name"), 10)
	skipped := d.request(t, 4, "onboarding.skip", pyjson.ObjectOf("session_id", second))
	if !reflect.DeepEqual(skipped["result"], map[string]any{"skipped": true}) {
		t.Fatalf("skip answered %v", skipped)
	}
	if got := cardAnswer(t, answer, 10*time.Second); got != nil {
		t.Fatalf("the card was answered %v", got)
	}
	if d.phase(second) != "skipped" {
		t.Fatalf("phase %v", d.phase(second))
	}
}

func TestARerunWithNoModelOpensNoChat(t *testing.T) {
	d := newOnbDesktop(t)
	writeFile(t, filepath.Join(d.root, "config.yaml"), "{}\n")
	writeFile(t, filepath.Join(d.root, ".env"), "")
	ids := func() map[any]bool {
		rows, err := d.db.SearchSessions(d.ctx, state.SearchSessionsOptions{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		out := map[any]bool{}
		for _, row := range rows {
			out[row.GetOr("id", nil)] = true
		}
		return out
	}
	before := ids()
	refused := d.request(t, 1, "onboarding.start", pyjson.ObjectOf("rerun", true))
	failure, _ := refused["error"].(map[string]any)
	if !pyrepr.Equal(failure["code"], 5080) {
		t.Fatalf("start answered %v", refused)
	}
	if after := ids(); !reflect.DeepEqual(after, before) {
		t.Fatalf("rows %v, before %v", after, before)
	}
}

func TestStartFromASecondaryProfileClaimsInTheDefaultHome(t *testing.T) {
	d := newOnbDesktop(t)
	work := filepath.Join(d.root, "profiles", "work")
	mkdirAll(t, work)
	writeFile(t, filepath.Join(work, "config.yaml"), "{}\n")
	var claimsMu sync.Mutex
	var claims []string
	prevClaim := provider.ClaimDesktopFreeTier
	provider.ClaimDesktopFreeTier = func(env provider.PoolEnv, _, _ bool) (map[string]any, error) {
		claimsMu.Lock()
		claims = append(claims, home.Dir(env.Ctx))
		claimsMu.Unlock()
		return nil, nil
	}
	t.Cleanup(func() { provider.ClaimDesktopFreeTier = prevClaim })
	d.runner.mu.Lock()
	d.runner.multiplex = true
	d.runner.mu.Unlock()
	workDB, err := state.NewSessionDB(d.ctx, filepath.Join(work, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workDB.Close() })
	secondary, err := gateway.InitializeSessionAuthority(d.ctx, d.runner, work, "owner", workDB, false)
	if err != nil {
		t.Fatal(err)
	}
	registry := gateway.NewSessionAuthorities(d.root, true)
	defaultName, workName := "default", "work"
	if err := registry.Add(d.root, d.authority, &defaultName); err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(work, secondary, &workName); err != nil {
		t.Fatal(err)
	}
	d.runner.mu.Lock()
	d.runner.registry = registry
	d.runner.mu.Unlock()
	conn, err := gateway.NewAuthorityConnection(secondary, d.transport, map[string]any{"user_id": "uid:1", "provider": "local",
		"profile_id": work, "instance_id": "owner", "profile_scope": "host"}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(d.ctx) })
	reply, err := conn.Dispatch(tools.WithServingLocalDesktop(d.ctx, true), pyjson.ObjectOf("jsonrpc", "2.0", "id", int64(1),
		"method", "onboarding.start", "params", pyjson.ObjectOf()))
	if err != nil {
		t.Fatal(err)
	}
	started, _ := pyjson.Plain(reply).(map[string]any)
	result, ok := started["result"].(map[string]any)
	if !ok {
		t.Fatalf("start answered %v", started)
	}
	claimsMu.Lock()
	got := append([]string(nil), claims...)
	claimsMu.Unlock()
	if !reflect.DeepEqual(got, []string{d.root}) {
		t.Fatalf("claims %v, want [%s]", got, d.root)
	}
	if stored, _ := result["stored_session_id"].(string); !d.live(stored) {
		t.Fatalf("%v is not live on the default authority", result["stored_session_id"])
	}
}

func TestSkipAcceptsTheProfileTheDesktopNames(t *testing.T) {
	d := newOnbDesktop(t)
	stored := d.start(t, 1, pyjson.ObjectOf("rerun", true))
	skipped := d.request(t, 2, "onboarding.skip", pyjson.ObjectOf("session_id", stored, "profile", "default"))
	if !reflect.DeepEqual(skipped["result"], map[string]any{"skipped": true}) {
		t.Fatalf("skip answered %v", skipped)
	}
}

// One revision fenced session.mutate, resent on a revision conflict as the Desktop does
func (d *onbDesktop) mutate(t *testing.T, rid int64, stored, operation string, requestID func(attempt int) string) map[string]any {
	t.Helper()
	var reply map[string]any
	for attempt := 0; attempt < 5; attempt++ {
		row, err := d.db.GetSession(d.ctx, stored)
		if err != nil || row == nil {
			t.Fatalf("row of %s: %v %v", stored, row, err)
		}
		reply = d.request(t, rid*10+int64(attempt), "session.mutate", pyjson.ObjectOf("session_id", stored,
			"request_id", requestID(attempt), "operation", operation,
			"expected_revision", row.GetOr("runtime_revision", nil), "expected_generation", row.GetOr("runtime_generation", nil),
			"payload", pyjson.ObjectOf()))
		failure, _ := reply["error"].(map[string]any)
		data, _ := failure["data"].(map[string]any)
		if _, ok := reply["result"]; ok || data["reason"] != "revision_conflict" {
			return reply
		}
	}
	return reply
}

func TestDeletingTheOnboardingChatFromTheSidebarEndsTheRun(t *testing.T) {
	d := newOnbDesktop(t)
	var endedMu sync.Mutex
	var ended []map[string]any
	var listening atomic.Bool
	listening.Store(true)
	tools.OnOnboardingEnded(func(e map[string]any) {
		if listening.Load() {
			endedMu.Lock()
			ended = append(ended, e)
			endedMu.Unlock()
		}
	})
	t.Cleanup(func() { listening.Store(false) })
	stored := d.start(t, 1, pyjson.ObjectOf("rerun", true))
	d.firstTurn(t)
	d.settled(t, stored)
	var replies []map[string]any
	for range 5 {
		row, err := d.db.GetSession(d.ctx, stored)
		if err != nil || row == nil {
			t.Fatalf("row of %s: %v %v", stored, row, err)
		}
		reply := d.request(t, int64(2+len(replies)), "session.mutate", pyjson.ObjectOf("session_id", stored,
			"request_id", "delete-onboarding-"+strconv.Itoa(len(replies)),
			"expected_revision", row.GetOr("runtime_revision", nil), "expected_generation", row.GetOr("runtime_generation", nil),
			"operation", "delete", "payload", pyjson.ObjectOf()))
		replies = append(replies, reply)
		failure, _ := reply["error"].(map[string]any)
		data, _ := failure["data"].(map[string]any)
		if _, ok := reply["result"]; ok || data["reason"] != "revision_conflict" {
			break
		}
	}
	if _, ok := replies[len(replies)-1]["result"]; !ok {
		t.Fatalf("delete answered %v", replies)
	}
	endedMu.Lock()
	var outcomes []any
	for _, e := range ended {
		if e["stored_session_id"] == stored {
			outcomes = append(outcomes, e["outcome"])
		}
	}
	endedMu.Unlock()
	if !reflect.DeepEqual(outcomes, []any{"deleted"}) {
		t.Fatalf("ended outcomes %v", outcomes)
	}
	if run := onboardingSection(t, d.root)["run"]; run != false {
		t.Fatalf("onboarding.run %v", run)
	}
}

func TestAFreshLaunchDeletesTheChatTheLastLaunchLeftWithNoViewer(t *testing.T) {
	d := newOnbDesktop(t)
	first := d.start(t, 1, pyjson.ObjectOf("rerun", true))
	d.firstTurn(t)
	d.settled(t, first)
	_ = d.conn.Close(d.ctx)
	if !d.live(first) {
		t.Fatalf("%s left the authority when the app quit", first)
	}
	d.connect(t, false, false)
	second := d.start(t, 2, pyjson.ObjectOf("fresh_launch", true))
	if second == first {
		t.Fatalf("the fresh launch reopened %s", first)
	}
	if row, err := d.db.GetSession(d.ctx, first); err != nil || row != nil {
		t.Fatalf("the old chat stayed %v %v", row, err)
	}
	if d.live(first) {
		t.Fatalf("%s is still live", first)
	}
}

// The old chat with the user's answer to the name card, the app quit and relaunched on the same socket
func (d *onbDesktop) relaunchOverAnsweredChat(t *testing.T) (string, []*pyjson.Object, *gateway.AuthorityConnection) {
	t.Helper()
	first := d.start(t, 1, pyjson.ObjectOf("rerun", true))
	d.firstTurn(t)
	d.settled(t, first)
	if _, err := d.db.AppendMessage(d.ctx, first, map[string]any{"role": "user", "content": "Call me Sid."}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	messages, err := d.db.GetMessages(d.ctx, first, state.GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = d.conn.Close(d.ctx)
	d.connect(t, false, false)
	identity := map[string]any{"user_id": "uid:1", "provider": "local", "profile_id": d.authority.ProfileID, "instance_id": "owner"}
	other, err := gateway.NewAuthorityConnection(d.authority, tuigateway.NewWSTransport(newWSTestSocket(), "test", nil), identity, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close(d.ctx) })
	return first, messages, other
}

// The old chat kept with its messages and its run ended as replaced
func (d *onbDesktop) keptAsReplaced(t *testing.T, first string, messages []*pyjson.Object) {
	t.Helper()
	now, err := d.db.GetMessages(d.ctx, first, state.GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pyjson.Plain(now), pyjson.Plain(messages)) {
		t.Fatalf("messages %v, before %v", pyjson.Plain(now), pyjson.Plain(messages))
	}
	if endedBy := tools.LoadOnboardingRecord(d.db, first)["ended_by"]; endedBy != "replaced" {
		t.Fatalf("ended_by %v", endedBy)
	}
	if !d.live(first) {
		t.Fatalf("%s left the authority", first)
	}
}

func TestAWindowThatReopensTheOldChatWhileAFreshLaunchStartsKeepsIt(t *testing.T) {
	d := newOnbDesktop(t)
	first, messages, other := d.relaunchOverAnsweredChat(t)
	var resumed []map[string]any
	recordNewRun := tools.StartOnboarding
	tools.StartOnboarding = func(ctx context.Context, db *state.DB, stored string, facts tools.OnboardingFacts) (map[string]any, error) {
		record, err := recordNewRun(ctx, db, stored, facts)
		done := make(chan map[string]any, 1)
		go func() {
			reply, dispatchErr := other.Dispatch(d.ctx, pyjson.ObjectOf("jsonrpc", "2.0", "id", int64(9), "method", "session.resume",
				"params", pyjson.ObjectOf("session_id", first)))
			if dispatchErr != nil {
				done <- map[string]any{"dispatch_error": dispatchErr.Error()}
				return
			}
			plain, _ := pyjson.Plain(reply).(map[string]any)
			done <- plain
		}()
		select {
		case reply := <-done:
			resumed = append(resumed, reply)
		case <-time.After(20 * time.Second):
			t.Error("the other window's resume never finished")
		}
		return record, err
	}
	t.Cleanup(func() { tools.StartOnboarding = recordNewRun })
	second := d.request(t, 2, "onboarding.start", pyjson.ObjectOf("fresh_launch", true))
	result, _ := second["result"].(map[string]any)
	if result["stored_session_id"] == first || result["stored_session_id"] == nil {
		t.Fatalf("start answered %v", second)
	}
	if len(resumed) == 0 {
		t.Fatal("no resume ran")
	}
	if _, ok := resumed[0]["result"]; !ok {
		t.Fatalf("resume answered %v", resumed)
	}
	d.keptAsReplaced(t, first, messages)
}

func TestAWindowWhoseResumeReadsHistoryWhileAFreshLaunchClaimsTheChatKeepsIt(t *testing.T) {
	d := newOnbDesktop(t)
	first, messages, other := d.relaunchOverAnsweredChat(t)
	reading, release, resumeDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var readingOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	readRows := gateway.DisplayRows
	gateway.DisplayRows = func(ctx context.Context, db *state.SessionDB, target string) (map[int64]bool, []map[string]any, bool, error) {
		first := false
		readingOnce.Do(func() { first = true })
		if first {
			close(reading)
			select {
			case <-release:
			case <-time.After(20 * time.Second):
				t.Error("the history read was never released")
			}
		}
		return readRows(ctx, db, target)
	}
	t.Cleanup(func() { gateway.DisplayRows = readRows })
	var resume map[string]any
	go func() {
		defer close(resumeDone)
		reply, err := other.Dispatch(d.ctx, pyjson.ObjectOf("jsonrpc", "2.0", "id", int64(9), "method", "session.resume",
			"params", pyjson.ObjectOf("session_id", first)))
		if err != nil {
			resume = map[string]any{"dispatch_error": err.Error()}
			return
		}
		resume, _ = pyjson.Plain(reply).(map[string]any)
	}()
	select {
	case <-reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the resume never read the history")
	}
	recordNewRun := tools.StartOnboarding
	tools.StartOnboarding = func(ctx context.Context, db *state.DB, stored string, facts tools.OnboardingFacts) (map[string]any, error) {
		record, err := recordNewRun(ctx, db, stored, facts)
		releaseOnce.Do(func() { close(release) })
		select {
		case <-resumeDone:
		case <-time.After(20 * time.Second):
			t.Error("the resume never finished")
		}
		return record, err
	}
	t.Cleanup(func() { tools.StartOnboarding = recordNewRun })
	second := d.request(t, 2, "onboarding.start", pyjson.ObjectOf("fresh_launch", true))
	result, _ := second["result"].(map[string]any)
	if result["stored_session_id"] == first || result["stored_session_id"] == nil {
		t.Fatalf("start answered %v", second)
	}
	<-resumeDone
	if _, ok := resume["result"]; !ok {
		t.Fatalf("resume answered %v", resume)
	}
	d.keptAsReplaced(t, first, messages)
}

func TestSkipAfterNewInTheOnboardingChatEndsTheRun(t *testing.T) {
	d := newOnbDesktop(t)
	stored := d.start(t, 1, pyjson.ObjectOf("rerun", true))
	d.firstTurn(t)
	d.settled(t, stored)
	if !waitFor(func() bool {
		pending, err := state.ListSessionAdmissions(d.ctx, d.db, stored, true)
		return err == nil && len(pending) == 0
	}, 10*time.Second) {
		t.Fatal("the opening turn's admission never ended")
	}
	reset := d.mutate(t, 2, stored, "reset", func(attempt int) string { return "reset-2-" + strconv.Itoa(attempt) })
	if _, ok := reset["result"]; !ok {
		t.Fatalf("reset answered %v", reset)
	}
	skipped := d.request(t, 3, "onboarding.skip", pyjson.ObjectOf("session_id", stored))
	if !reflect.DeepEqual(skipped["result"], map[string]any{"skipped": true}) {
		t.Fatalf("skip answered %v", skipped)
	}
	if open := tools.OpenOnboardingSessionIDs(d.db); len(open) != 0 {
		t.Fatalf("open runs %v", open)
	}
	if run := onboardingSection(t, d.root)["run"]; run != false {
		t.Fatalf("onboarding.run %v", run)
	}
}

func TestAResumedViewerGetsTheWaitingSetupCardAndItsAnswerUnblocksTheTurn(t *testing.T) {
	d := newOnbDesktop(t)
	stored := d.start(t, 1, pyjson.ObjectOf("rerun", true))
	if _, ok := d.request(t, 2, "session.resume", pyjson.ObjectOf("session_id", stored))["result"]; !ok {
		t.Fatal("the first resume failed")
	}
	live, ok := d.authority.Live(stored)
	if !ok {
		t.Fatalf("%s is not live", stored)
	}
	params := pyjson.ObjectOf("beat", "name", "tool_call_id", "call-1", "card", pyjson.ObjectOf("suggested", "Sid"))
	answer := askSetupCard(t, live, stored, params, 20)
	_ = d.conn.Close(d.ctx)
	d.connect(t, true, true)
	resumed := d.request(t, 3, "session.resume", pyjson.ObjectOf("session_id", stored))
	result, ok := resumed["result"].(map[string]any)
	if !ok {
		t.Fatalf("resume answered %v", resumed)
	}
	entries, _ := result["open_requests"].([]any)
	if len(entries) != 1 {
		t.Fatalf("open requests %v", result["open_requests"])
	}
	entry, _ := entries[0].(map[string]any)
	want := map[string]any{"session_id": stored, "beat": "name", "tool_call_id": "call-1", "card": map[string]any{"suggested": "Sid"}}
	if entry["method"] != "setup_beat" || !reflect.DeepEqual(entry["params"], want) {
		t.Fatalf("entry %v", entry)
	}
	reply, err := d.conn.Dispatch(d.ctx, pyjson.ObjectOf("jsonrpc", "2.0", "id", entry["id"],
		"result", pyjson.ObjectOf("answer", pyjson.ObjectOf("name", "Sid"))))
	if err != nil || reply != nil {
		t.Fatalf("the answer frame got %v %v", reply, err)
	}
	got := cardAnswer(t, answer, 10*time.Second)
	if !reflect.DeepEqual(pyjson.Plain(got), map[string]any{"answer": map[string]any{"name": "Sid"}}) {
		t.Fatalf("the card got %v", pyjson.Plain(got))
	}
	if gateway.ServerRequestWaiting(live.EventStream, stored, "setup_beat") {
		t.Fatal("the card still waits")
	}
}
