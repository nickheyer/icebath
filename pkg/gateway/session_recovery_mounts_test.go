package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestLocalSessionAdapterAuthorizesOnlyTheRegisteredSource(t *testing.T) {
	ctx, a, _ := newRecoverAuthority(t)
	ref, _ := seedRecoverSession(t, ctx, a, recoverActor(), recoverTUIParams(t.TempDir()))
	adapter, err := localAdapter(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	local := adapter.(*LocalSessionAdapter)
	live, _ := a.live(ref.SessionID)
	if !local.authorizeSource(live.Source) {
		t.Fatal("the admitted source is not authorized")
	}
	forged := *live.Source
	if local.authorizeSource(&forged) {
		t.Fatal("a copy of the admitted source is authorized")
	}
	if kept := local.keptSource(&forged); kept != live.Source {
		t.Fatal("an equal source did not come back as the admitted object")
	}
	other := "someone-else"
	changed := *live.Source
	changed.UserID = &other
	if kept := local.keptSource(&changed); kept != &changed {
		t.Fatal("a different source came back as the admitted object")
	}
	original := live.Source.UserID
	live.Source.UserID = &other
	if local.authorizeSource(live.Source) {
		t.Fatal("a source whose identity changed is authorized")
	}
	live.Source.UserID = original
	if local.policy(ref.SessionID) == nil {
		t.Fatal("the frozen policy is not held")
	}
	if err := forgetLiveSession(ctx, a, ref.SessionID, live); err != nil {
		t.Fatal(err)
	}
	if local.authorizeSource(live.Source) || local.policy(ref.SessionID) != nil || local.savedSource(ref.SessionID) != nil {
		t.Fatal("a forgotten session kept its source or policy")
	}
	verdict, err := authorizeLocalSource(a.Runner, live.Source)
	if err != nil || verdict == nil || *verdict {
		t.Fatalf("forgotten local source verdict %v, %v", verdict, err)
	}
	foreign := &SessionSource{Platform: PlatformTelegram, ChatID: "1", ChatType: "dm"}
	if verdict, err := authorizeLocalSource(a.Runner, foreign); err != nil || verdict != nil {
		t.Fatalf("nonlocal source verdict %v, %v", verdict, err)
	}
}

func TestLocalSessionAdapterDeliveryStaysInTheEventStream(t *testing.T) {
	ctx, a, _ := newRecoverAuthority(t)
	local := NewLocalSessionAdapter(a)
	if ok, err := local.Connect(ctx, false); err != nil || !ok {
		t.Fatalf("connect %v %v", ok, err)
	}
	sent, err := local.Send(ctx, "chat", "text", "", nil)
	if id, _ := sent.MessageID.(string); err != nil || !sent.Success || len(id) != 32 {
		t.Fatalf("send %+v %v", sent, err)
	}
	edited, err := local.EditMessage(ctx, "chat", "m1", "text", true)
	if err != nil || !edited.Success || edited.MessageID != "m1" {
		t.Fatalf("edit %+v %v", edited, err)
	}
	approval, err := local.SendExecApproval(ctx, "chat", "ls", "key", nil, nil, true, true, false, nil)
	if err != nil || !approval.Success {
		t.Fatalf("approval %+v %v", approval, err)
	}
	if info, err := local.GetChatInfo(ctx, "chat"); err != nil || info["id"] != "chat" || info["type"] != "dm" {
		t.Fatalf("chat info %v %v", info, err)
	}
	if id := local.RecordDeliveryObligation(ctx, &MessageEvent{Text: "reply"}, "key", "reply", local, false); id != nil {
		t.Fatalf("a delivery obligation %q was ledgered", *id)
	}
	if err := local.SendTyping(ctx, "chat", nil); err != nil {
		t.Fatal(err)
	}
	local.setPolicy("room", &LocalSessionPolicy{Source: "bot_room", RequestJSON: "{}", ConfigJSON: "{}"})
	declined, err := local.SendClarify(ctx, "room", "which?", []string{"a", "b"}, "c1", "key", nil)
	raw, _ := declined.RawResponse.(map[string]any)
	if err != nil || declined.Success || declined.Error != "Clarification is unavailable in hosted rooms" ||
		declined.ErrorKind == nil || *declined.ErrorKind != "forbidden" || raw["code"] != EgressDeclineCode {
		t.Fatalf("clarify %+v %v", declined, err)
	}
	local.registerSource(&SessionSource{Platform: PlatformLocal, ChatID: "chat", ChatType: "dm"})
	if err := local.Disconnect(ctx); err != nil || local.savedSource("chat") != nil {
		t.Fatalf("disconnect kept a source, %v", err)
	}
}

// Installs the secret name policy the process wiring installs from the code execution sandbox
func installClientEnvSecretPolicy(t *testing.T) {
	t.Helper()
	substrings := []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN",
		"WEBHOOK", "CREDS", "BEARER", "APIKEY", "COOKIE"}
	prevName, prevSubs := state.SecretEnvName, state.SecretEnvSubstrings
	state.SecretEnvSubstrings = substrings
	state.SecretEnvName = func(name string) bool {
		upper := strings.ToUpper(name)
		for _, s := range substrings {
			if strings.Contains(upper, s) {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { state.SecretEnvName, state.SecretEnvSubstrings = prevName, prevSubs })
}

func TestProjectPluginsWarning(t *testing.T) {
	ctx, _, _ := newRecoverAuthority(t)
	installClientEnvSecretPolicy(t)
	repo, plain := filepath.Join(t.TempDir(), "repo"), filepath.Join(t.TempDir(), "plain")
	folder := filepath.Join(repo, ".icebath", "plugins")
	if err := os.MkdirAll(filepath.Join(folder, "mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(plain, ".icebath", "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	warn := func(cwd string, env map[string]any, clientLocal bool) *string {
		t.Helper()
		params := map[string]any{"request_id": "r", "source": "cli", "cwd": cwd, "model": "m", "toolsets": []any{}}
		if env != nil {
			params["client_env"] = env
		}
		policy, err := buildPolicy(ctx, params, map[string]any{}, buildPolicyOptions{PrivateSecrets: &RecoveredSecrets{}, WorkspacePosture: true, ClientLocal: clientLocal})
		if err != nil {
			t.Fatal(err)
		}
		return projectPluginsWarning(policy)
	}
	opted := warn(repo, map[string]any{"ICEBATH_ENABLE_PROJECT_PLUGINS": "true"}, true)
	if opted == nil || !strings.Contains(*opted, "not loaded") || !strings.Contains(*opted, folder) {
		t.Fatalf("opted in warning %v", opted)
	}
	if got := warn(repo, map[string]any{"ICEBATH_ENABLE_PROJECT_PLUGINS": "0"}, true); got != nil {
		t.Fatalf("switched off client warned %q", *got)
	}
	if got := warn(repo, map[string]any{}, true); got != nil {
		t.Fatalf("client without the switch warned %q", *got)
	}
	if got := warn(plain, map[string]any{"ICEBATH_ENABLE_PROJECT_PLUGINS": "1"}, true); got != nil {
		t.Fatalf("an empty plugins folder warned %q", *got)
	}
	if got := warn(repo, nil, true); got != nil {
		t.Fatalf("a client that sends no environment warned %q", *got)
	}
	if got := warn(repo, map[string]any{"ICEBATH_ENABLE_PROJECT_PLUGINS": "true"}, false); got != nil {
		t.Fatalf("a client not proven local warned %q", *got)
	}
	if projectPluginsWarning(nil) != nil {
		t.Fatal("no policy warned")
	}
	if err := os.Chmod(folder, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(folder, 0o755) })
	if got := warn(repo, map[string]any{"ICEBATH_ENABLE_PROJECT_PLUGINS": "true"}, true); got != nil {
		t.Fatalf("an unlistable plugins folder warned %q", *got)
	}
}

func TestMountHistoryIsReadOnlyForItsReader(t *testing.T) {
	ctx, a, _ := newAPIOwner(t)
	apiCreate(t, a, "old-cron", "cron")
	apiCreate(t, a, "old-cli", "cli")
	operator := Principal{Subject: "op", ProfileID: "default", TransportID: "t1", Capabilities: map[string]bool{"session:operator": true, "session:read": true}}
	stranger := Principal{Subject: "who", ProfileID: "default", TransportID: "t2", Capabilities: map[string]bool{"session:read": true}}
	if mounted, err := mountHistory(ctx, a, stranger, "old-cron"); err != nil || mounted {
		t.Fatalf("a stranger mounted the row, %v", err)
	}
	if mounted, err := mountHistory(ctx, a, operator, "old-cli"); err != nil || mounted {
		t.Fatalf("a legacy local row was mounted here, %v", err)
	}
	mounted, err := mountHistory(ctx, a, operator, "old-cron")
	if err != nil || !mounted {
		t.Fatalf("mount %v %v", mounted, err)
	}
	live, ok := a.live("old-cron")
	if !ok || !isHistoryMount(live) || live.Route != "history:old-cron" {
		t.Fatalf("live %+v", live)
	}
	if err := requireHistoryAccess(ctx, a, operator, "old-cron", "session:read"); err != nil {
		t.Fatal(err)
	}
	recoverRefused(t, requireHistoryAccess(ctx, a, operator, "old-cron", "session:submit"), "session_read_only")
	recoverRefused(t, requireHistoryAccess(ctx, a, stranger, "old-cron", "session:read"), "not_found")
	a.mu.Lock()
	a.nativeLegacyTransports = map[string]Principal{"t2": stranger}
	a.mu.Unlock()
	if err := requireHistoryAccess(ctx, a, stranger, "old-cron", "session:read"); err != nil {
		t.Fatalf("the native legacy peer was refused: %v", err)
	}
	if err := requireHistoryLeaf(ctx, a, "old-cron"); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.CreateSession(state.NewSession{ID: "child", Source: "cron", ParentSessionID: "old-cron"}); err != nil {
		t.Fatal(err)
	}
	recoverRefused(t, requireHistoryLeaf(ctx, a, "old-cron"), "session_has_children")
	apiCreate(t, a, "bound", "api_server")
	apiBind(t, ctx, a, "bound")
	a.mu.Lock()
	delete(a.Sessions, "bound")
	a.mu.Unlock()
	if mounted, err := mountHistory(ctx, a, operator, "bound"); err != nil || mounted {
		t.Fatalf("a bound API row was mounted read only, %v", err)
	}
	if err := a.Authorize(ctx, operator, SessionRef{ProfileID: "default", SessionID: "bound"}, "session:read"); err != nil {
		t.Fatal(err)
	}
	if restored, ok := a.live("bound"); !ok || isHistoryMount(restored) {
		t.Fatal("authorize did not restore the bound API session")
	}
}

func TestRestoreRetiredBringsAnApiSessionBackAndForgetsADeletedOne(t *testing.T) {
	ctx, a, runner := newAPIOwner(t)
	apiCreate(t, a, "api-1", "api_server")
	apiBind(t, ctx, a, "api-1")
	live, _ := a.live("api-1")
	if pinned(ctx, a, "api-1", live) {
		t.Fatal("an idle API session is pinned")
	}
	a.mu.Lock()
	live.Subscribers = map[string]Principal{"sub": recoverActor()}
	a.mu.Unlock()
	if !pinned(ctx, a, "api-1", live) {
		t.Fatal("a watched session is not pinned")
	}
	a.mu.Lock()
	live.Subscribers = nil
	a.mu.Unlock()
	if !pinned(withDrainTask(ctx, NewLiveSession(nil, "x")), a, "api-1", NewLiveSession(live.Source, live.Route)) {
		t.Fatal("a live session the authority does not hold is not pinned")
	}
	route := live.Route
	a.mu.Lock()
	delete(a.Sessions, "api-1")
	a.retiredRoutes["api-1"] = route
	a.mu.Unlock()
	if err := restoreRetired(ctx, a, "api-1"); err != nil {
		t.Fatal(err)
	}
	restored, ok := a.live("api-1")
	if !ok || restored.Route != route {
		t.Fatalf("restored %+v", restored)
	}
	a.mu.Lock()
	timer := restored.RetireTimer
	a.mu.Unlock()
	if timer == nil {
		t.Fatal("the restored session has no re-retirement check armed")
	}
	timer.Stop()
	if err := restoreRetired(ctx, a, "never-bound"); err != nil || recoverLive(a, "never-bound") {
		t.Fatalf("an unbound session came back, %v", err)
	}
	a.mu.Lock()
	delete(a.Sessions, "api-1")
	a.mu.Unlock()
	if err := forgetRetired(ctx, a, "api-1"); err != nil {
		t.Fatal(err)
	}
	if len(runner.evicted) != 1 || runner.evicted[0] != route {
		t.Fatalf("evicted %v", runner.evicted)
	}
	a.mu.Lock()
	_, kept := a.retiredRoutes["api-1"]
	a.mu.Unlock()
	if kept {
		t.Fatal("a forgotten session kept its retired route")
	}
}

func TestRestoreRoutedSessionMountsAMessagingRowAndDetachesASupersededOne(t *testing.T) {
	ctx, a, runner := newAPIOwner(t)
	store := runner.store
	user := "u1"
	source := &SessionSource{Platform: PlatformTelegram, ChatID: "42", ChatType: "dm", UserID: &user}
	route, err := store.generateSessionKey(ctx, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := pyjson.Dumps(source.ToDict())
	if err != nil {
		t.Fatal(err)
	}
	chat, kind := "42", "dm"
	for _, sid := range []string{"tg-old", "tg-new"} {
		apiCreate(t, a, sid, "telegram")
		if err := a.DB.RecordGatewaySessionPeer(ctx, sid, state.GatewayPeer{Source: "telegram", SessionKey: &route, ChatID: &chat,
			ChatType: &kind, UserID: &user, OriginJSON: &origin}); err != nil {
			t.Fatal(err)
		}
	}
	apiCreate(t, a, "plain-cli", "cli")
	if can, err := mountable(ctx, a, "plain-cli"); err != nil || can {
		t.Fatalf("a row with no route is mountable, %v", err)
	}
	now := sessionNow()
	current := NewSessionEntry(route, "tg-new", now, now)
	current.Origin, current.Platform = source, PlatformTelegram
	store.mu.Lock()
	store.ensureLoadedLocked(ctx)
	store.entries.set(route, current)
	store.mu.Unlock()
	if can, err := mountable(ctx, a, "tg-new"); err != nil || !can {
		t.Fatalf("the routed row is not mountable, %v", err)
	}
	ref, err := restoreRoutedSession(ctx, a, "tg-new")
	if err != nil || ref == nil || ref.SessionID != "tg-new" {
		t.Fatalf("routed restore %v %v", ref, err)
	}
	routed, _ := a.live("tg-new")
	if routed.Route != route || routed.Source != source || liveDetached(routed) {
		t.Fatalf("routed live %+v", routed)
	}
	ref, err = restoreRoutedSession(ctx, a, "tg-old")
	if err != nil || ref == nil {
		t.Fatalf("superseded restore %v %v", ref, err)
	}
	old, _ := a.live("tg-old")
	if !liveDetached(old) || old.Route != "agent:main:detached:tg-old" || *old.Source.DetachedRoute != old.Route || old.Source.ChatID != "42" {
		t.Fatalf("superseded live route %q source %+v", old.Route, old.Source)
	}
	adapter, err := localAdapter(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if !adapter.(*LocalSessionAdapter).authorizeSource(old.Source) {
		t.Fatal("the detached source is not admitted by the local adapter")
	}
	superseded, err := supersededHandOff(ctx, a, *ref)
	if err != nil || !superseded {
		t.Fatalf("superseded %v %v", superseded, err)
	}
	owner, err := messagingOwnerRef(ctx, a, SessionRef{ProfileID: "default", SessionID: "tg-new"})
	if err != nil || owner.SessionID != "tg-new" {
		t.Fatalf("owner ref %v %v", owner, err)
	}
	runner.draining = true
	a.mu.Lock()
	delete(a.Sessions, "tg-new")
	a.mu.Unlock()
	_, err = restoreRoutedSession(ctx, a, "tg-new")
	recoverRefused(t, err, "runtime_draining")
}

func TestStageLocalSessionExactRetryAnswersTheExistingSession(t *testing.T) {
	ctx, a, _ := newRecoverAuthority(t)
	installClientEnvSecretPolicy(t)
	actor := recoverActor()
	cwd := t.TempDir()
	params := func() map[string]any {
		return map[string]any{"request_id": "retry", "source": "gui", "cwd": cwd, "model": "frozen", "toolsets": []any{}}
	}
	ref, _ := seedRecoverSession(t, ctx, a, actor, params())
	again, staged, err := stageLocalSession(ctx, a, actor, params(), localCreateOptions{})
	if err != nil || staged != nil || again == nil || *again != ref {
		t.Fatalf("exact retry answered %v %v %v", again, staged, err)
	}
	created, err := createLocalSession(ctx, a, actor, params(), localCreateOptions{})
	if err != nil || created != ref {
		t.Fatalf("exact create retry answered %v %v", created, err)
	}
	follows, err := stagedCreationFollows(ctx, a, actor, "retry")
	if err != nil || follows {
		t.Fatalf("a committed request without follow_profile_config follows: %v %v", follows, err)
	}
	if follows, err := stagedCreationFollows(ctx, a, actor, "never-staged"); err != nil || !follows {
		t.Fatalf("an unstaged request may not follow: %v %v", follows, err)
	}
	changed := params()
	changed["source"] = "tui"
	_, _, err = stageLocalSession(ctx, a, actor, changed, localCreateOptions{})
	recoverRefused(t, err, "invalid_params")
	keyed := params()
	keyed["api_key"] = "sk-offered"
	_, _, err = stageLocalSession(ctx, a, actor, keyed, localCreateOptions{})
	if err == nil {
		t.Fatal("a retry offering a key to a keyless session was accepted")
	}
	foreign := Principal{Subject: "owner", ProfileID: "elsewhere", Capabilities: actor.Capabilities}
	_, _, err = stageLocalSession(ctx, a, foreign, params(), localCreateOptions{})
	recoverRefused(t, err, "profile_mismatch")
	reader := Principal{Subject: "owner", ProfileID: "fixture", Capabilities: map[string]bool{"session:read": true}}
	_, _, err = stageLocalSession(ctx, a, reader, params(), localCreateOptions{})
	recoverRefused(t, err, "permission_denied")
	long := params()
	long["request_id"] = strings.Repeat("r", 257)
	_, _, err = stageLocalSession(ctx, a, actor, long, localCreateOptions{})
	recoverRefused(t, err, "invalid_params")
	bypass := map[string]any{"request_id": "bypass", "source": "cli", "cwd": cwd, "toolsets": []any{}, "safe_mode": true}
	_, _, err = stageLocalSession(ctx, a, actor, bypass, localCreateOptions{})
	recoverRefused(t, err, "invalid_params")
	if err := publishLocalPolicy(ctx, a, ref.SessionID, true); err != nil {
		t.Fatal(err)
	}
	if evicted := a.Runner.(*recoverTestRunner).evicted; len(evicted) != 1 {
		t.Fatalf("evicted %v", evicted)
	}
}
