package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One send an adapter took with its chat, content, reply anchor and the metadata it carried
type contractSend struct {
	chatID, content, replyTo string
	metadata                 map[string]any
}

// Adapter on the base pipeline recording sends, its send result, connection and reactions fixed per case
type contractAdapter struct {
	*gateway.BasePlatformAdapter
	mu        sync.Mutex
	sends     []contractSend
	result    gateway.SendResult
	onSend    func()
	connected bool
	reactions []string
	sent      chan contractSend
}

func newContractAdapter(platform gateway.Platform) *contractAdapter {
	a := &contractAdapter{result: gateway.SendResult{Success: true}, connected: true, sent: make(chan contractSend, 32)}
	a.BasePlatformAdapter = contractBase(a, platform)
	return a
}

// Enabled base of the platform whose self is the outer adapter, as a BasePlatformAdapter subclass builds it
func contractBase(self gateway.PlatformAdapter, platform gateway.Platform) *gateway.BasePlatformAdapter {
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	cfg.Token = "t"
	return gateway.NewBasePlatformAdapter(cfg, platform, self)
}

func (a *contractAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *contractAdapter) Disconnect(context.Context) error            { return nil }
func (a *contractAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID, "type": "dm"}, nil
}

func (a *contractAdapter) Send(_ context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	sent := contractSend{chatID: chatID, content: content, replyTo: replyTo, metadata: metadata}
	a.sends = append(a.sends, sent)
	result, hook := a.result, a.onSend
	a.onSend = nil
	if result.Success {
		result.MessageID = fmt.Sprintf("m%d", len(a.sends))
	}
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	a.sent <- sent
	return result, nil
}

func (a *contractAdapter) SendRetryIsFinal(gateway.SendResult) bool { return true }
func (a *contractAdapter) IsConnected() bool                        { return a.connected }
func (a *contractAdapter) PlatformValue() string                    { return string(a.Base().Platform) }

func (a *contractAdapter) SetReaction(_ context.Context, chatID, messageID, emoji string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reactions = append(a.reactions, chatID+" "+messageID+" "+emoji)
	return true, nil
}

func (a *contractAdapter) recorded() []contractSend {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]contractSend(nil), a.sends...)
}

// Next send the adapter takes from a background task, failing the case when none arrives within five seconds
func (a *contractAdapter) awaitSend(t *testing.T) contractSend {
	t.Helper()
	select {
	case sent := <-a.sent:
		return sent
	case <-time.After(5 * time.Second):
		t.Fatal("no send reached the adapter")
	}
	return contractSend{}
}

type contractEditingAdapter struct{ *contractAdapter }

func (a *contractEditingAdapter) EditMessage(_ context.Context, chatID, messageID, content string, _ bool) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sends = append(a.sends, contractSend{chatID: chatID, content: "edit " + messageID + " " + content})
	return gateway.SendResult{Success: true, MessageID: messageID}, nil
}

type contractRaisingEditAdapter struct{ *contractAdapter }

func (a *contractRaisingEditAdapter) EditMessage(context.Context, string, string, string, bool) (gateway.SendResult, error) {
	return gateway.SendResult{}, fmt.Errorf("malformed edit response")
}

type contractFailedEditAdapter struct{ *contractAdapter }

func (a *contractFailedEditAdapter) EditMessage(context.Context, string, string, string, bool) (gateway.SendResult, error) {
	return gateway.SendResult{Success: false}, nil
}

// Installs the gateway plugin services over a tool env session binding and points the process at the runner
func installRunnerPluginServices(t *testing.T, r *GatewayRunner) {
	t.Helper()
	previous := plugins.InstalledServices()
	s := &plugins.Services{BoundSessionKey: func(ctx context.Context) string { return tools.BoundSessionKey(tools.ToolEnvFrom(ctx)) }}
	gateway.InstallPluginServices(s)
	plugins.InstallServices(s)
	gateway.SetGatewayRunnerRef(r)
	t.Cleanup(func() {
		plugins.InstallServices(previous)
		gateway.SetGatewayRunnerRef(nil)
	})
}

// Context of a tool handler working for the session key, the Go form of the scoped_session_key binding
func inSessionContext(sessionKey string) context.Context {
	env := &tools.Env{}
	tools.ScopedSessionKey(env, sessionKey)
	return tools.WithToolEnv(context.Background(), env)
}

// Running runner with a live loop, the adapter as its telegram bot and one stored telegram DM session
func pluginPostRunner(t *testing.T, adapter gateway.PlatformAdapter) (*GatewayRunner, *plugins.Context, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	r := injectionRunner(t)
	r.adapters = adapterDict(gateway.PlatformTelegram, adapter)
	r.profileAdapters = profileAdapterDict()
	loop := gateway.NewCallbackLoop()
	t.Cleanup(loop.Close)
	r.gatewayLoop = loop
	userID, userName := "42", "u"
	entry, err := r.sessionStore.GetOrCreateSession(ctx, &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "42",
		ChatType: "dm", UserID: &userID, UserName: &userName}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	installRunnerPluginServices(t, r)
	pctx := plugins.NewContext(&plugins.Manifest{Name: "progress", Key: "progress", Source: "user"}, plugins.NewManager(h))
	return r, pctx, entry.SessionKey
}

// test_plugin_post_message.py test_post_then_edit_targets_the_calling_session for all four adapters, the post and the edit both driven through a real gateway runner
func TestPostThenEditTargetsTheCallingSession(t *testing.T) {
	for _, c := range []struct {
		name   string
		make   func() (gateway.PlatformAdapter, *contractAdapter)
		edited bool
	}{
		{"editing", func() (gateway.PlatformAdapter, *contractAdapter) {
			a := &contractEditingAdapter{newContractAdapter(gateway.PlatformTelegram)}
			a.BasePlatformAdapter = contractBase(a, gateway.PlatformTelegram)
			return a, a.contractAdapter
		}, true},
		{"plain", func() (gateway.PlatformAdapter, *contractAdapter) {
			a := newContractAdapter(gateway.PlatformTelegram)
			return a, a
		}, false},
		{"raising", func() (gateway.PlatformAdapter, *contractAdapter) {
			a := &contractRaisingEditAdapter{newContractAdapter(gateway.PlatformTelegram)}
			a.BasePlatformAdapter = contractBase(a, gateway.PlatformTelegram)
			return a, a.contractAdapter
		}, false},
		{"failed", func() (gateway.PlatformAdapter, *contractAdapter) {
			a := &contractFailedEditAdapter{newContractAdapter(gateway.PlatformTelegram)}
			a.BasePlatformAdapter = contractBase(a, gateway.PlatformTelegram)
			return a, a.contractAdapter
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			adapter, recorder := c.make()
			_, pctx, sessionKey := pluginPostRunner(t, adapter)
			handle := pctx.PostMessage(inSessionContext(sessionKey), "step 1/2")
			if !handle.OK() || handle.MessageID() != "m1" {
				t.Fatalf("post %v", handle.Result())
			}
			result := handle.Edit(inSessionContext(""), "step 2/2")
			if result["ok"] != true || result["edited"] != c.edited {
				t.Fatalf("edit %v", result)
			}
			want := []string{"42 step 1/2", "42 step 2/2"}
			if c.edited {
				want[1] = "42 edit m1 step 2/2"
			} else if result["fallback"] != true || handle.MessageID() != "m2" {
				t.Fatalf("fallback %v %v", result, handle.MessageID())
			}
			var got []string
			for _, sent := range recorder.recorded() {
				got = append(got, sent.chatID+" "+sent.content)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("calls %v", got)
			}
		})
	}
}

// test_plugin_post_message.py test_no_bound_session_is_refused_without_sending, the refusal and the adapter that never sent anything both checked through a real runner
func TestNoBoundSessionIsRefusedWithoutSending(t *testing.T) {
	adapter := &contractEditingAdapter{newContractAdapter(gateway.PlatformTelegram)}
	adapter.BasePlatformAdapter = contractBase(adapter, gateway.PlatformTelegram)
	_, pctx, _ := pluginPostRunner(t, adapter)
	handle := pctx.PostMessage(context.Background(), "from a timer")
	edit := handle.Edit(context.Background(), "still nothing")
	if handle.OK() || handle.ErrorCode() != "no_conversation" || edit["ok"] != false {
		t.Fatalf("%v %v", handle.Result(), edit)
	}
	if len(adapter.recorded()) != 0 {
		t.Fatalf("%v", adapter.recorded())
	}
}

// Gateway loop state of a real runner as the plugin message facade reads it, running flag and loop both required
func TestGatewayLoopFollowsTheRunnerLoopState(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	r := injectionRunner(t)
	gateway.SetGatewayRunnerRef(r)
	t.Cleanup(func() { gateway.SetGatewayRunnerRef(nil) })
	if runner, _ := gateway.GatewayLoop(ctx); runner != nil {
		t.Fatal("a runner without a loop is not live")
	}
	loop := gateway.NewCallbackLoop()
	r.gatewayLoop = loop
	if runner, on := gateway.GatewayLoop(ctx); runner != any(r) || on {
		t.Fatalf("%v %v", runner, on)
	}
	task := spawnGatewayTask(ctx, "on-loop", func(taskCtx context.Context) (any, error) {
		_, on := gateway.GatewayLoop(taskCtx)
		return on, nil
	})
	if on, err := task.wait(ctx); err != nil || on != true {
		t.Fatalf("a gateway task is on the loop: %v %v", on, err)
	}
	r.running.Store(false)
	if runner, _ := gateway.GatewayLoop(ctx); runner != nil {
		t.Fatal("a stopped runner is not live")
	}
	r.running.Store(true)
	loop.Close()
	if runner, _ := gateway.GatewayLoop(ctx); runner != nil {
		t.Fatal("a closed loop is not live")
	}
}

// Multiplexing runner whose default profile holds the default adapter and the given profile adapter maps
func platformActionRunner(t *testing.T, defaultAdapter gateway.PlatformAdapter, profileMaps ...any) (*GatewayRunner, context.Context) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	root := filepath.Join(tmp, ".icebath")
	t.Setenv("ICEBATH_HOME", root)
	profileHome := filepath.Join(root, "profiles", "team-b")
	if err := os.MkdirAll(profileHome, 0o700); err != nil {
		t.Fatal(err)
	}
	grant := "plugins:\n  entries:\n    p:\n      granted_capabilities: [gateway.platform_actions]\n"
	if err := os.WriteFile(filepath.Join(profileHome, "config.yaml"), []byte(grant), 0o600); err != nil {
		t.Fatal(err)
	}
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(context.Background())
	r.adapters = adapterDict(gateway.PlatformTelegram, defaultAdapter)
	r.profileAdapters = profileAdapterDict(profileMaps...)
	r.primaryProfileName = "default"
	installRunnerPluginServices(t, r)
	return r, home.WithOverride(context.Background(), profileHome)
}

// Platform action facade of plugin p from its plugin context, the object a plugin register call receives
func platformActionsOf(t *testing.T) *plugins.PlatformActions {
	t.Helper()
	pctx := plugins.NewContext(&plugins.Manifest{Name: "p", Key: "p", Source: "user"}, plugins.NewManager(t.TempDir()))
	facade, err := pctx.PlatformActions()
	if err != nil {
		t.Fatal(err)
	}
	return facade.(*plugins.PlatformActions)
}

// test_platform_actions.py TestMultiplexProfileRouting test_secondary_profile_routes_to_its_own_adapter_not_default, the fail closed profile ladder of a real gateway runner deciding which bot acts
func TestSecondaryProfileRoutesToItsOwnAdapterNotDefault(t *testing.T) {
	defaultAdapter, teamB := newContractAdapter(gateway.PlatformTelegram), newContractAdapter(gateway.PlatformTelegram)
	_, ctx := platformActionRunner(t, defaultAdapter, "team-b", adapterDict(gateway.PlatformTelegram, teamB))
	result := platformActionsOf(t).AddReaction(ctx, "telegram", "1", "2", "x")
	if result["ok"] != true {
		t.Fatalf("%v", result)
	}
	if !reflect.DeepEqual(teamB.reactions, []string{"1 2 x"}) || len(defaultAdapter.reactions) != 0 {
		t.Fatalf("team-b %v default %v", teamB.reactions, defaultAdapter.reactions)
	}
}

// test_platform_actions.py TestMultiplexProfileRouting test_unresolvable_profile_fails_closed_never_default_bot no-registry-entry, the fail closed profile ladder of a real gateway runner deciding which bot acts
func TestUnresolvableProfileFailsClosedNeverDefaultBot(t *testing.T) {
	defaultAdapter := newContractAdapter(gateway.PlatformTelegram)
	_, ctx := platformActionRunner(t, defaultAdapter)
	result := platformActionsOf(t).AddReaction(ctx, "telegram", "1", "2", "x")
	if result["error"] != "adapter_not_registered" {
		t.Fatalf("%v", result)
	}
	if len(defaultAdapter.reactions) != 0 {
		t.Fatalf("default bot acted %v", defaultAdapter.reactions)
	}
	if refused := platformActionsOf(t).AddReaction(ctx, "no-such-platform", "1", "2", "x"); refused["error"] != "unknown_platform" {
		t.Fatalf("%v", refused)
	}
}

// Slack channel event for chat C1 with inbound id msg-42, answered through the ledger bracket of the adapter
func ledgerEvent() *gateway.MessageEvent {
	event := gateway.NewMessageEvent("hello agent")
	messageID := "msg-42"
	event.MessageID = &messageID
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformSlack, ChatID: "C1", ChatType: "channel"}
	return event
}

// test_delivery_ledger_producer.py test_late_transient_failure_signals_reconnected_runner, the real runner sweeping the failed row and redelivering it through the replacement adapter at once
func TestLateTransientFailureSignalsReconnectedRunner(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	r := injectionRunner(t)
	adapter, replacement := newContractAdapter(gateway.PlatformSlack), newContractAdapter(gateway.PlatformSlack)
	adapter.SetOwnerProfile("reviewer")
	replacement.SetOwnerProfile("reviewer")
	adapter.GatewayRunner = r
	r.adapters = adapterDict(gateway.PlatformSlack, adapter)
	r.profileAdapters = profileAdapterDict()
	adapter.result = gateway.SendResult{Success: false, Error: "send_path_degraded", Retryable: true}
	adapter.onSend = func() {
		r.adapters = adapterDict(gateway.PlatformSlack, replacement)
		r.profileAdapters = profileAdapterDict("reviewer", adapterDict(gateway.PlatformSlack, replacement))
	}
	result, sentBy, err := adapter.SendFinalLedgered(ctx, ledgerEvent(), "agent:main:slack:channel:C1", "final answer", nil, nil, false)
	if err != nil || result.Success || sentBy != gateway.PlatformAdapter(adapter) {
		t.Fatalf("%+v %v %v", result, sentBy, err)
	}
	if redelivered := replacement.recorded(); len(redelivered) != 1 || redelivered[0].chatID != "C1" || redelivered[0].content != gateway.ReconnectedMarker+"final answer" {
		t.Fatalf("the reconnected runner did not redeliver through the replacement: %v", redelivered)
	}
	reviewer := "reviewer"
	if again, err := gateway.SweepFailedForRuntime(ctx, "slack", nil, &reviewer); err != nil || len(again) != 0 {
		t.Fatalf("the redelivered row is still failed: %v %v", again, err)
	}
}

// A refused final arms the runner flood worker of its bot identity and stays failed in the ledger
func TestFloodRefusedFinalArmsTheRunnerFloodWorker(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	r := injectionRunner(t)
	adapter := newContractAdapter(gateway.PlatformTelegram)
	adapter.GatewayRunner = r
	r.adapters = adapterDict(gateway.PlatformTelegram, adapter)
	adapter.result = gateway.SendResult{Success: false, Error: "flood_control:185.0"}
	event := ledgerEvent()
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "42", ChatType: "dm"}
	if _, _, err := adapter.SendFinalLedgered(ctx, event, "agent:main:telegram:dm:42", "final answer", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	waiting, err := gateway.PendingRetries(ctx, nil)
	if err != nil || len(waiting) != 1 || waiting[0].Platform != "telegram" || waiting[0].Profile != "default" {
		t.Fatalf("%+v %v", waiting, err)
	}
	flood := &r.floodRedelivery
	flood.mu.Lock()
	task, armed := flood.tasks[[2]string{"telegram", "default"}]
	wake := flood.wakes[[2]string{"telegram", "default"}]
	flood.mu.Unlock()
	if !armed {
		t.Fatal("no flood worker for the bot identity")
	}
	r.running.Store(false)
	wake.Set()
	if _, err := task.wait(ctx); err != nil {
		t.Fatal(err)
	}
}

// Multiplexing runner on a slack bot whose route sends chat C1 to beta, beta served or parked
func routedSlackRunner(t *testing.T, betaServed bool) (*GatewayRunner, *contractAdapter) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	root := filepath.Join(tmp, ".icebath")
	t.Setenv("ICEBATH_HOME", root)
	r := injectionRunner(t)
	on := true
	r.config.MultiplexProfiles = &on
	r.config.ProfileRoutes = routesOf(t, map[string]any{"platform": "slack", "chat_id": "C1", "profile": "beta"})
	served := []profiles.ServedProfile{{Name: "default", Home: root}}
	if betaServed {
		served = append(served, profiles.ServedProfile{Name: "beta", Home: filepath.Join(root, "profiles", "beta")})
	}
	r.config.SetRuntimeProfileHomes(served)
	r.sessionRuntimeDescriptor.store(map[string]any{"parked_profiles": map[string]any{"beta": "test park"}})
	adapter := newContractAdapter(gateway.PlatformSlack)
	adapter.GatewayRunner = r
	r.adapters = adapterDict(gateway.PlatformSlack, adapter)
	r.profileAdapters = profileAdapterDict()
	r.primaryProfileName = "default"
	return r, adapter
}

// Inbound slack thread reply in chat C1 from user U1 as the adapter builds its source through the runner
func routedSlackSource(ctx context.Context, adapter *contractAdapter) *gateway.SessionSource {
	thread, message, user := "t1", "m9", "U1"
	return adapter.BuildSource(ctx, "C1", gateway.SourceFields{ChatType: "channel", ThreadID: &thread, MessageID: &message, UserID: &user})
}

// Source profile resolution of a real runner, a route to a served profile stamping that profile on the source
func TestBuildSourceTakesTheRunnerRoutedProfile(t *testing.T) {
	_, adapter := routedSlackRunner(t, true)
	source := routedSlackSource(context.Background(), adapter)
	if source.Profile == nil || *source.Profile != "beta" || source.ProfileRouteRejected {
		t.Fatalf("%v %v", source.Profile, source.ProfileRouteRejected)
	}
}

// A route to a parked profile is refused in the sender thread with the runner thread metadata
func TestParkedRouteRefusalRepliesInTheSenderThreadThroughTheRunner(t *testing.T) {
	ctx := context.Background()
	_, adapter := routedSlackRunner(t, false)
	source := routedSlackSource(ctx, adapter)
	if !source.ProfileRouteRejected || source.Profile != nil {
		t.Fatalf("%v %v", source.ProfileRouteRejected, source.Profile)
	}
	gateway.DropRejectedRoute(ctx, adapter, source)
	sent := adapter.awaitSend(t)
	want := map[string]any{"thread_id": "t1", "message_id": "m9", "user_id": "U1", "_interim_send": true}
	if sent.chatID != "C1" || sent.content != gateway.ParkedRouteNotice || sent.replyTo != "m9" || !reflect.DeepEqual(sent.metadata, want) {
		t.Fatalf("%+v", sent)
	}
}

// The real runner as the cron manual run reads it, a shared bot satellite granted only its routed targets
func TestCronGatewayRunnerGrantsASharedBotSatelliteOnlyItsRoutedTargets(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	root := filepath.Join(tmp, ".icebath")
	t.Setenv("ICEBATH_HOME", root)
	ctx := context.Background()
	r := injectionRunner(t)
	on := true
	r.config.MultiplexProfiles = &on
	routes := routesOf(t, map[string]any{"platform": "telegram", "chat_id": "routed", "profile": "sat"})
	r.config.ProfileRoutes = routes
	r.config.SetRuntimeProfileHomes([]profiles.ServedProfile{{Name: "default", Home: root}, {Name: "sat", Home: filepath.Join(root, "profiles", "sat")}})
	primary := newContractAdapter(gateway.PlatformTelegram)
	r.adapters = adapterDict(gateway.PlatformTelegram, primary)
	r.profileAdapters = profileAdapterDict("sat", adapterDict())
	r.primaryProfileName = "default"
	loop := gateway.NewCallbackLoop()
	t.Cleanup(loop.Close)
	r.gatewayLoop = loop
	var runner cron.GatewayRunner = r
	if !runner.IsSharedBotSatellite(ctx, "sat", "") || runner.IsSharedBotSatellite(ctx, "default", "") {
		t.Fatal("satellite detection")
	}
	sat := "sat"
	shared := cron.NewSharedRouteAdapters(runner.AdaptersForProfile(&sat), routes)
	if !shared.PyBool() {
		t.Fatal("the satellite borrowed nothing from the primary")
	}
	if got := shared.Get(ctx, "telegram", map[string]any{"chat_id": "routed"}, nil); got != any(primary) {
		t.Fatalf("routed target %v", got)
	}
	if got := shared.Get(ctx, "telegram", map[string]any{"chat_id": "elsewhere"}, nil); got != nil {
		t.Fatalf("unrouted target borrowed the primary %v", got)
	}
	if runner.GatewayLoop() != gateway.EventLoop(loop) {
		t.Fatal("gateway loop")
	}
}

// Ledger row ob-1 for slack chat C1 as the hermes case records it, then failed with a reconnect only error
func recordFailedSlackObligation(t *testing.T, sessionKey string, adapterProfile *string) {
	t.Helper()
	ctx := context.Background()
	thread := "171.001"
	if err := gateway.RecordObligation(ctx, gateway.DeliveryObligation{ObligationID: "ob-1", SessionKey: sessionKey, Platform: "slack",
		ChatID: "C1", ThreadID: &thread, Content: "the final answer", AdapterProfile: adapterProfile}); err != nil {
		t.Fatal(err)
	}
	if err := gateway.MarkFailed(ctx, "ob-1", "send_path_degraded"); err != nil {
		t.Fatal(err)
	}
}

// State, attempts and last error of a ledger row read straight from the process state.db like the hermes helper
func ledgerRowOf(t *testing.T, obligationID string) (string, int, *string) {
	t.Helper()
	ctx := context.Background()
	conn, err := state.OpenDB(ctx, filepath.Join(home.Process(), "state.db"), "state.db (delivery_ledger)")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var rowState string
	var attempts int
	var lastError *string
	if err := conn.QueryRowContext(ctx, "SELECT state, attempts, last_error FROM delivery_obligations WHERE obligation_id=?",
		obligationID).Scan(&rowState, &attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	return rowState, attempts, lastError
}

// test_delivery_ledger.py TestGatewayRedeliverySweep test_runtime_failed_redelivery_clears_resume_before_send, driven on the real runner with its real session store and ledger
func TestRuntimeFailedRedeliveryClearsResumeBeforeSend(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	r := injectionRunner(t)
	adapter := newContractAdapter(gateway.PlatformSlack)
	r.adapters = adapterDict(gateway.PlatformSlack, adapter)
	r.profileAdapters = profileAdapterDict()
	r.primaryProfileName = "default"
	entry, err := r.sessionStore.GetOrCreateSession(ctx, &gateway.SessionSource{Platform: gateway.PlatformSlack, ChatID: "C1", ChatType: "channel"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if marked, err := r.sessionStore.MarkResumePending(ctx, entry.SessionKey, "restart_timeout"); err != nil || !marked {
		t.Fatalf("%v %v", marked, err)
	}
	recordFailedSlackObligation(t, entry.SessionKey, nil)
	if n := r.RedeliverFailedObligationsForPlatform(ctx, gateway.PlatformSlack, nil); n != 1 {
		t.Fatalf("redelivered %d", n)
	}
	if after := r.sessionStore.LookupBySessionKey(ctx, entry.SessionKey); after == nil || after.ResumePending {
		t.Fatalf("resume_pending was not cleared %+v", after)
	}
	sent := adapter.recorded()
	if len(sent) != 1 || sent[0].content != gateway.ReconnectedMarker+"the final answer" {
		t.Fatalf("%+v", sent)
	}
	if rowState, _, _ := ledgerRowOf(t, "ob-1"); rowState != "delivered" {
		t.Fatalf("row %s", rowState)
	}
}

// test_delivery_ledger.py TestGatewayRedeliverySweep test_runtime_profile_redelivery_uses_matching_bot_adapter, the reviewer bot sending once and the default bot staying silent throughout
func TestRuntimeProfileRedeliveryUsesMatchingBotAdapter(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	r := injectionRunner(t)
	defaultAdapter, reviewerAdapter := newContractAdapter(gateway.PlatformSlack), newContractAdapter(gateway.PlatformSlack)
	r.adapters = adapterDict(gateway.PlatformSlack, defaultAdapter)
	r.profileAdapters = profileAdapterDict("reviewer", adapterDict(gateway.PlatformSlack, reviewerAdapter))
	r.primaryProfileName = "default"
	reviewer := "reviewer"
	recordFailedSlackObligation(t, "agent:reviewer:slack:channel:C1", &reviewer)
	if n := r.RedeliverFailedObligationsForPlatform(ctx, gateway.PlatformSlack, &reviewer); n != 1 {
		t.Fatalf("redelivered %d", n)
	}
	if len(defaultAdapter.recorded()) != 0 || len(reviewerAdapter.recorded()) != 1 {
		t.Fatalf("default %v reviewer %v", defaultAdapter.recorded(), reviewerAdapter.recorded())
	}
}

// test_delivery_ledger.py TestGatewayRedeliverySweep test_runtime_missing_adapter_releases_unsent_claim, the claimed row released back to failed with its retry budget left intact
func TestRuntimeMissingAdapterReleasesUnsentClaim(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	r := injectionRunner(t)
	r.adapters = adapterDict()
	r.profileAdapters = profileAdapterDict()
	r.primaryProfileName = "default"
	recordFailedSlackObligation(t, "agent:main:slack:channel:C1", nil)
	if n := r.RedeliverFailedObligationsForPlatform(ctx, gateway.PlatformSlack, nil); n != 0 {
		t.Fatalf("redelivered %d", n)
	}
	rowState, attempts, lastError := ledgerRowOf(t, "ob-1")
	if rowState != "failed" || attempts != 0 || lastError == nil || *lastError != "send_path_degraded" {
		t.Fatalf("%s %d %v", rowState, attempts, lastError)
	}
}
