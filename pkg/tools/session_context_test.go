package tools

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

func TestSetCurrentSessionIDSyncsBoundVarsAndProcessEnv(t *testing.T) {
	t.Setenv("ICEBATH_SESSION_ID", "before")
	env := &Env{Values: &sync.Map{}}
	BindSessionContext(env, map[string]string{"ICEBATH_SESSION_PLATFORM": "telegram"})
	SetCurrentSessionID(context.Background(), env, "sess-parent")
	if got := SessionEnv(env, "ICEBATH_SESSION_ID"); got != "sess-parent" {
		t.Fatalf("bound session id %q", got)
	}
	if got := SessionEnv(env, "ICEBATH_SESSION_PLATFORM"); got != "telegram" {
		t.Fatalf("other bound var lost %q", got)
	}
	if got := os.Getenv("ICEBATH_SESSION_ID"); got != "sess-parent" {
		t.Fatalf("process env %q", got)
	}
	child := &Env{Values: &sync.Map{}}
	SetCurrentSessionID(WithDelegatedChildContext(context.Background(), true), child, "sess-child")
	if got := SessionEnv(child, "ICEBATH_SESSION_ID"); got != "sess-child" {
		t.Fatalf("child bound session id %q", got)
	}
	if got := os.Getenv("ICEBATH_SESSION_ID"); got != "sess-parent" {
		t.Fatalf("delegated child clobbered the parent id with %q", got)
	}
	if _, engaged := sessionContextState(child); engaged {
		t.Fatal("setting the session id engaged the session context")
	}
}

// Clean session context slate with the process latch restored after the test
func isolateSessionContext(t *testing.T, engaged bool) {
	t.Helper()
	prev := sessionContextLatch.Load()
	sessionContextLatch.Store(engaged)
	t.Cleanup(func() { sessionContextLatch.Store(prev) })
	for _, name := range SessionContextEnvVars {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// Context copy a spawned task inherits, sharing nothing with the parent Env afterwards
func copySessionContext(src *Env) *Env {
	dst := *src
	dst.Values = &sync.Map{}
	src.Values.Range(func(k, v any) bool {
		dst.Values.Store(k, v)
		return true
	})
	return &dst
}

func TestSetSessionVarsBindsContextWithoutTouchingTheProcessEnv(t *testing.T) {
	isolateSessionContext(t, false)
	env := newTestToolEnv(t)
	SetSessionVars(env, SessionVars{Platform: "telegram", ChatID: "-1001", ChatName: "Group", ChatType: "group", UserID: "123456", UserName: "alice", ThreadID: "17585"})
	for name, want := range map[string]string{
		"ICEBATH_SESSION_PLATFORM": "telegram", "ICEBATH_SESSION_SOURCE": "", "ICEBATH_SESSION_CHAT_ID": "-1001",
		"ICEBATH_SESSION_CHAT_NAME": "Group", "ICEBATH_SESSION_CHAT_TYPE": "group", "ICEBATH_SESSION_USER_ID": "123456",
		"ICEBATH_SESSION_USER_NAME": "alice", "ICEBATH_SESSION_THREAD_ID": "17585",
	} {
		if got := GetSessionEnv(env, name, "dflt"); got != want {
			t.Fatalf("%s = %q want %q", name, got, want)
		}
		if _, ok := os.LookupEnv(name); ok {
			t.Fatalf("%s leaked into the process env", name)
		}
	}
	if !SessionContextEngaged() {
		t.Fatal("binding a session did not latch the process session context")
	}
	ClearSessionVars(env)
	for _, name := range sessionVarNames {
		if got := GetSessionEnv(env, name, "dflt"); got != "" {
			t.Fatalf("%s after clear = %q", name, got)
		}
	}
}

func TestGetSessionEnvFallsBackToProcessEnv(t *testing.T) {
	isolateSessionContext(t, false)
	for _, c := range []struct{ name, process, bound string }{
		{"ICEBATH_SESSION_PLATFORM", "discord", "telegram"},
		{"ICEBATH_SESSION_KEY", "env-session-123", "ctx-session-456"},
	} {
		env := newTestToolEnv(t)
		t.Setenv(c.name, c.process)
		if got := GetSessionEnv(env, c.name, ""); got != c.process {
			t.Fatalf("%s unbound = %q", c.name, got)
		}
		v := SessionVars{Platform: c.bound}
		if c.name == "ICEBATH_SESSION_KEY" {
			v = SessionVars{SessionKey: c.bound}
		}
		SetSessionVars(env, v)
		if got := GetSessionEnv(env, c.name, ""); got != c.bound {
			t.Fatalf("%s bound = %q", c.name, got)
		}
		ClearSessionVars(env)
		if got := GetSessionEnv(env, c.name, ""); got != "" {
			t.Fatalf("%s cleared = %q", c.name, got)
		}
	}
	env := newTestToolEnv(t)
	if got := GetSessionEnv(env, "ICEBATH_SESSION_UNKNOWN_NAME", "fallback"); got != "fallback" {
		t.Fatalf("default %q", got)
	}
	updateSessionVars(env, func(vars map[string]string) { vars["ICEBATH_NOT_A_SESSION_VAR"] = "bound" })
	t.Setenv("ICEBATH_NOT_A_SESSION_VAR", "process")
	if got := GetSessionEnv(env, "ICEBATH_NOT_A_SESSION_VAR", ""); got != "process" {
		t.Fatalf("name outside the var map read the bound context %q", got)
	}
}

func TestSessionKeyIsolatedAcrossConcurrentTasks(t *testing.T) {
	isolateSessionContext(t, false)
	parent := newTestToolEnv(t)
	results := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	handler := func(key string, delay time.Duration) {
		defer wg.Done()
		env := copySessionContext(parent)
		SetSessionVars(env, SessionVars{SessionKey: key})
		defer ClearSessionVars(env)
		time.Sleep(delay)
		mu.Lock()
		results[key] = GetSessionEnv(env, "ICEBATH_SESSION_KEY", "")
		mu.Unlock()
	}
	wg.Add(2)
	go handler("session-A", 150*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	go handler("session-B", 50*time.Millisecond)
	wg.Wait()
	if results["session-A"] != "session-A" || results["session-B"] != "session-B" {
		t.Fatalf("race between session keys %v", results)
	}
}

func TestCronSessionTriState(t *testing.T) {
	isolateSessionContext(t, false)
	t.Setenv("ICEBATH_CRON_SESSION", "1")
	env := newTestToolEnv(t)
	if got := GetSessionEnv(env, "ICEBATH_CRON_SESSION", ""); got != "1" {
		t.Fatalf("legacy env fallback %q", got)
	}
	SetSessionVars(env, SessionVars{Platform: "api_server", CronSession: strPtr("")})
	if got := GetSessionEnv(env, "ICEBATH_CRON_SESSION", ""); got != "" {
		t.Fatalf("explicit blank did not mask the leaked env %q", got)
	}
	ClearSessionVars(env)
	if got := GetSessionEnv(env, "ICEBATH_CRON_SESSION", ""); got != "" {
		t.Fatalf("cleared cron marker %q", got)
	}
	SetSessionVars(env, SessionVars{CronSession: strPtr("1")})
	if got := GetSessionEnv(env, "ICEBATH_CRON_SESSION", ""); got != "1" {
		t.Fatalf("cron marker %q", got)
	}
	ClearSessionVars(env)
	if got := GetSessionEnv(env, "ICEBATH_CRON_SESSION", ""); got != "" {
		t.Fatalf("cleared cron marker %q", got)
	}
	ResetSessionVars(env)
	if got := GetSessionEnv(env, "ICEBATH_CRON_SESSION", ""); got != "1" {
		t.Fatalf("reset did not restore the env fallback %q", got)
	}
	SetSessionVars(env, SessionVars{})
	if got := GetSessionEnv(env, "ICEBATH_CRON_SESSION", ""); got != "1" {
		t.Fatalf("unset cron marker masked the env %q", got)
	}
}

func TestSetSessionVarsBindsTheTurnSessionID(t *testing.T) {
	isolateSessionContext(t, false)
	t.Setenv("ICEBATH_SESSION_ID", "stale-process-env")
	env := newTestToolEnv(t)
	SetSessionVars(env, SessionVars{Platform: "local", ChatID: "local-abc", ChatType: "dm", UserID: "uid:1", SessionKey: "agent:main:local:dm:local-abc", SessionID: "20261006_x", Cwd: " /work/app "})
	if got := GetSessionEnv(env, "ICEBATH_SESSION_ID", ""); got != "20261006_x" {
		t.Fatalf("turn session id %q", got)
	}
	if got := ScopedSessionCwd(env); got != "/work/app" {
		t.Fatalf("session cwd %q", got)
	}
	ClearSessionVars(env)
	if got := ScopedSessionCwd(env); got != "" {
		t.Fatalf("cleared session cwd %q", got)
	}
}

func TestAsyncDeliveryCapability(t *testing.T) {
	isolateSessionContext(t, false)
	env := newTestToolEnv(t)
	if !SessionAsyncDeliverySupported(env) {
		t.Fatal("unbound context must support async delivery")
	}
	SetSessionVars(env, SessionVars{Platform: "api_server", ChatID: "sess1", SessionKey: "sess1", NoAsyncDelivery: true})
	if SessionAsyncDeliverySupported(env) || GetSessionEnv(env, "ICEBATH_SESSION_PLATFORM", "") != "api_server" {
		t.Fatal("stateless bind still supports async delivery")
	}
	ClearSessionVars(env)
	if !SessionAsyncDeliverySupported(env) {
		t.Fatal("cleared context must default to supported")
	}
	sessionContextLatch.Store(false)
	other := newTestToolEnv(t)
	ResetSessionVars(other)
	DeclareStatelessChannel(other)
	if SessionContextEngaged() {
		t.Fatal("declaring a stateless channel latched the session context")
	}
	if _, engaged := sessionContextState(other); engaged {
		t.Fatal("declaring a stateless channel engaged the subprocess bridge")
	}
	if SessionAsyncDeliverySupported(other) {
		t.Fatal("declared stateless channel still supports async delivery")
	}
	ResetSessionVars(other)
	if !SessionAsyncDeliverySupported(other) {
		t.Fatal("reset did not drop the stateless declaration")
	}
}

func TestResetSessionVarsClosesInheritanceLeak(t *testing.T) {
	isolateSessionContext(t, true)
	mine := SessionVars{SessionKey: "agent:main:discord:thread:MINE:MINE", Platform: "discord", ChatID: "MINE_CHAT", ThreadID: "MINE_THREAD", UserID: "MINE_USER", ChatName: "mine", MessageID: "MINE_MSG"}
	foreign := SessionVars{SessionKey: "agent:main:discord:thread:FOREIGN:FOREIGN", Platform: "discord", ChatID: "FOREIGN_CHAT", ThreadID: "FOREIGN_THREAD", UserID: "FOREIGN_USER", ChatName: "foreign", MessageID: "FOREIGN_MSG"}
	parent := newTestToolEnv(t)
	SetSessionVars(parent, mine)
	child := copySessionContext(parent)
	ResetSessionVars(child)
	window := mustRunEnv(t, child)
	for _, name := range []string{"ICEBATH_SESSION_CHAT_ID", "ICEBATH_SESSION_THREAD_ID", "ICEBATH_SESSION_KEY"} {
		if v, ok := window[name]; ok {
			t.Fatalf("%s leaked the parent session after reset %q", name, v)
		}
	}
	SetSessionVars(child, foreign)
	bound := mustRunEnv(t, child)
	if bound["ICEBATH_SESSION_CHAT_ID"] != "FOREIGN_CHAT" || bound["ICEBATH_SESSION_KEY"] != foreign.SessionKey {
		t.Fatalf("own bind not visible %q %q", bound["ICEBATH_SESSION_CHAT_ID"], bound["ICEBATH_SESSION_KEY"])
	}
	if got := GetSessionEnv(parent, "ICEBATH_SESSION_CHAT_ID", ""); got != "MINE_CHAT" {
		t.Fatalf("child reset reached the parent context %q", got)
	}
}

func TestResetSessionVarsClosesAsyncDeliveryLeak(t *testing.T) {
	isolateSessionContext(t, true)
	parent := newTestToolEnv(t)
	SetSessionVars(parent, SessionVars{SessionKey: "agent:main:discord:thread:FOREIGN:FOREIGN", Platform: "discord", ChatID: "FOREIGN_CHAT", NoAsyncDelivery: true})
	child := copySessionContext(parent)
	ResetSessionVars(child)
	if !SessionAsyncDeliverySupported(child) {
		t.Fatal("after reset async delivery must default to supported")
	}
}

func TestEngagedProcessStripsTheProcessEnvMirrorForAnUnboundContext(t *testing.T) {
	isolateSessionContext(t, false)
	bound := newTestToolEnv(t)
	SetSessionVars(bound, SessionVars{SessionKey: "agent:main:telegram:dm:1", Platform: "telegram"})
	SetCurrentSessionID(context.Background(), bound, "sess-bound")
	unbound := newTestToolEnv(t)
	if v, ok := mustRunEnv(t, unbound)["ICEBATH_SESSION_ID"]; ok {
		t.Fatalf("unbound context inherited the mirrored session id %q", v)
	}
	if got := mustRunEnv(t, bound)["ICEBATH_SESSION_ID"]; got != "sess-bound" {
		t.Fatalf("bound context session id %q", got)
	}
}

func TestScopedSessionIDAndKeyRestorePriorValues(t *testing.T) {
	isolateSessionContext(t, false)
	t.Setenv("ICEBATH_SESSION_ID", "process-id")
	t.Setenv("ICEBATH_SESSION_KEY", "process-key")
	env := newTestToolEnv(t)
	restore := ScopedCurrentSessionID(env, strPtr("scoped-id"))
	if got := GetSessionEnv(env, "ICEBATH_SESSION_ID", ""); got != "scoped-id" || os.Getenv("ICEBATH_SESSION_ID") != "process-id" {
		t.Fatalf("scoped id %q process %q", got, os.Getenv("ICEBATH_SESSION_ID"))
	}
	inner := ScopedCurrentSessionID(env, nil)
	SetCurrentSessionID(WithDelegatedChildContext(context.Background(), true), env, "inner-id")
	inner()
	if got := GetSessionEnv(env, "ICEBATH_SESSION_ID", ""); got != "scoped-id" {
		t.Fatalf("save only boundary did not restore %q", got)
	}
	restore()
	if _, had := boundSessionVar(env, "ICEBATH_SESSION_ID"); had {
		t.Fatal("restore left a never bound id bound")
	}
	if got := BoundSessionKey(env); got != "" {
		t.Fatalf("bound key read the process env %q", got)
	}
	restoreKey := ScopedSessionKey(env, "hook-key")
	if BoundSessionKey(env) != "hook-key" || GetSessionEnv(env, "ICEBATH_SESSION_KEY", "") != "hook-key" {
		t.Fatal("scoped key not bound")
	}
	nested := ScopedSessionKey(env, "nested-key")
	nested()
	if BoundSessionKey(env) != "hook-key" {
		t.Fatalf("nested restore %q", BoundSessionKey(env))
	}
	restoreKey()
	if BoundSessionKey(env) != "" || GetSessionEnv(env, "ICEBATH_SESSION_KEY", "") != "process-key" {
		t.Fatal("scoped key not restored to unset")
	}
}

type routeSource struct{ scope, parent string }

func (s routeSource) RouteScopeID() string      { return s.scope }
func (s routeSource) RouteParentChatID() string { return s.parent }

func TestSourceRouteMetadataKeepsInboundAnchors(t *testing.T) {
	meta := map[string]any{"thread_id": "9", "scope_id": "stale"}
	got := SourceRouteMetadata(routeSource{scope: "guild-1", parent: "chan-2"}, meta)
	if got["scope_id"] != "guild-1" || got["parent_chat_id"] != "chan-2" || got["thread_id"] != "9" || meta["scope_id"] != "stale" {
		t.Fatalf("merged %v original %v", got, meta)
	}
	if got := SourceRouteMetadata(routeSource{scope: "guild-1"}, nil); len(got) != 1 || got["scope_id"] != "guild-1" {
		t.Fatalf("nil metadata %v", got)
	}
	if got := SourceRouteMetadata(routeSource{}, meta); got["scope_id"] != "stale" || len(got) != 2 {
		t.Fatalf("anchorless source changed metadata %v", got)
	}
	if got := SourceRouteMetadata(nil, nil); got != nil {
		t.Fatalf("nil source %v", got)
	}
}

func TestSessionIsMessagingSurface(t *testing.T) {
	isolateSessionContext(t, false)
	t.Setenv("ICEBATH_PLATFORM", "")
	for _, c := range []struct {
		platform, source, override string
		want                       bool
	}{
		{"", "", "", false},
		{"telegram", "", "", true},
		{" API_Server ", "", "", false},
		{"", "tui", "", false},
		{"cli", "matrix", "", true},
		{"local", "desktop", "", false},
		{"cli", "", "discord", true},
		{"telegram", "", "cli", false},
	} {
		env := newTestToolEnv(t)
		t.Setenv("ICEBATH_PLATFORM", c.override)
		SetSessionVars(env, SessionVars{Platform: c.platform, Source: c.source})
		if got := SessionIsMessagingSurface(env); got != c.want {
			t.Fatalf("%+v got %v", c, got)
		}
	}
}

func TestSessionHistoryDeliveryFailsClosed(t *testing.T) {
	isolateSessionContext(t, false)
	env := newTestToolEnv(t)
	if SessionHistoryDeliverySupported(env) {
		t.Fatal("undeclared history delivery granted")
	}
	for _, c := range []struct {
		decl *string
		want bool
	}{{nil, false}, {strPtr(""), false}, {strPtr("1"), true}, {strPtr("true"), false}} {
		SetSessionVars(env, SessionVars{ChatID: "c", SessionHistoryDelivery: c.decl})
		if got := SessionHistoryDeliverySupported(env); got != c.want {
			t.Fatalf("declaration %v got %v", c.decl, got)
		}
	}
	ClearSessionVars(env)
	if SessionHistoryDeliverySupported(env) {
		t.Fatal("cleared context kept the wake capability")
	}
	t.Setenv("ICEBATH_SESSION_HISTORY_DELIVERY", "1")
	if SessionHistoryDeliverySupported(env) {
		t.Fatal("capability borrowed from the environment")
	}
}

// One variable set and restored by the exported wrappers, the never set state coming back as absent
func TestBoundSessionVarAndRestoreSessionVarRoundTripOneVariable(t *testing.T) {
	isolateSessionContext(t, false)
	env := &Env{Values: &sync.Map{}}
	if _, had := BoundSessionVar(env, "ICEBATH_CRON_SESSION"); had {
		t.Fatal("fresh context reports a bound variable")
	}
	RestoreSessionVar(env, "ICEBATH_CRON_SESSION", "1", true)
	if v, had := BoundSessionVar(env, "ICEBATH_CRON_SESSION"); !had || v != "1" {
		t.Fatalf("set variable read back %q %v", v, had)
	}
	t.Setenv("ICEBATH_CRON_SESSION", "ambient")
	RestoreSessionVar(env, "ICEBATH_CRON_SESSION", "", false)
	if _, had := BoundSessionVar(env, "ICEBATH_CRON_SESSION"); had {
		t.Fatal("restore to never set left the variable bound")
	}
	if got := GetSessionEnv(env, "ICEBATH_CRON_SESSION", ""); got != "ambient" {
		t.Fatalf("never set variable read %q instead of the process env", got)
	}
}
