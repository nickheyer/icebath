package backend

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func strp(s string) *string { return &s }

func TestNormalizesAnExactRegistryScopedConnectionAndProfile(t *testing.T) {
	got := NormalizeWindowConnectionRoute(map[string]any{"connectionId": "source-b", "profile": "research", "registryScoped": true})
	if !reflect.DeepEqual(got, &WindowConnectionRoute{ConnectionID: strp("source-b"), Profile: strp("research"), RegistryScoped: true}) {
		t.Fatalf("%+v", got)
	}
}

func TestKeepsLegacyProfileOnlyRoutesDistinctFromRegistryIdentities(t *testing.T) {
	got := NormalizeWindowConnectionRoute(map[string]any{"profile": "work"})
	if !reflect.DeepEqual(got, &WindowConnectionRoute{Profile: strp("work")}) {
		t.Fatalf("%+v", got)
	}
}

func TestPreservesARegistryConnectionWhenNoProfileIsSelected(t *testing.T) {
	got := NormalizeWindowConnectionRoute(map[string]any{"connectionId": "source-b", "registryScoped": true})
	if !reflect.DeepEqual(got, &WindowConnectionRoute{ConnectionID: strp("source-b"), RegistryScoped: true}) {
		t.Fatalf("%+v", got)
	}
}

func TestIsolatesActiveRoutesByWebContentsID(t *testing.T) {
	routes := NewWindowConnectionRoutes()
	routes.Set("11", map[string]any{"connectionId": "source-a", "profile": "default", "registryScoped": true})
	routes.Set("22", map[string]any{"connectionId": "source-b", "profile": "worker", "registryScoped": true})
	if routes.Get("11").connectionID() != "source-a" || routes.Get("22").connectionID() != "source-b" {
		t.Fatal("routes mixed")
	}
	routes.Delete("11")
	if routes.Get("11") != nil || routes.Get("22").connectionID() != "source-b" {
		t.Fatal("delete leaked")
	}
}

func TestInvalidPublicationsClearOnlyTheSenderRoute(t *testing.T) {
	routes := NewWindowConnectionRoutes()
	routes.Set("11", map[string]any{"connectionId": "source-a", "profile": "default", "registryScoped": true})
	routes.Set("22", map[string]any{"connectionId": "source-b", "profile": "worker", "registryScoped": true})
	routes.Set("11", nil)
	if routes.Get("11") != nil || routes.Get("22").connectionID() != "source-b" {
		t.Fatal("invalid publication")
	}
}

func TestRoutesANonPrimarySSHConnectionIndependentlyFromAnotherWindow(t *testing.T) {
	registry := ConnectionRegistry{Primary: "source-a", Connections: []RegistrySource{{"source-a", "ssh"}, {"source-b", "ssh"}, {"source-c", "remote"}}}
	routes := NewWindowConnectionRoutes()
	routes.Set("11", map[string]any{"connectionId": "source-b", "profile": "worker", "registryScoped": true})
	routes.Set("22", map[string]any{"connectionId": "source-c", "profile": "default", "registryScoped": true})
	if scope, ok := RegistrySSHScopeForWindowRoute(routes.Get("11"), registry); !ok || scope != "conn:source-b::worker" {
		t.Fatalf("ssh scope %q", scope)
	}
	if _, ok := RegistrySSHScopeForWindowRoute(routes.Get("22"), registry); ok {
		t.Fatal("remote source got an ssh scope")
	}
}

func TestUsesTheCanonicalDefaultProfileScopeWhenARegistrySSHRouteHasNoProfile(t *testing.T) {
	registry := ConnectionRegistry{Primary: "source-a", Connections: []RegistrySource{{"source-a", "ssh"}, {"source-b", "ssh"}}}
	scope, ok := RegistrySSHScopeForWindowRoute(&WindowConnectionRoute{ConnectionID: strp("source-b"), RegistryScoped: true}, registry)
	if !ok || scope != "conn:source-b::default" {
		t.Fatalf("%q", scope)
	}
}

func TestRecoversTheBootstrapPoolKeyARegistrySSHTunnelWasPublishedUnder(t *testing.T) {
	scope, ok := RegistrySSHPoolScopeByConnectionID([]RegistrySSHPoolEntry{{Scope: "", RegistryConnectionID: "source-b", HasSSH: true}}, "source-b")
	if !ok || scope != "" {
		t.Fatalf("%q %v", scope, ok)
	}
}

func TestDoesNotMatchAnotherConnectionAnUnlabelledEntryOrATornDownTunnel(t *testing.T) {
	pool := []RegistrySSHPoolEntry{
		{Scope: "research", RegistryConnectionID: "source-a", HasSSH: true},
		{Scope: "", RegistryConnectionID: "", HasSSH: true},
		{Scope: "worker", RegistryConnectionID: "source-b"},
	}
	for _, id := range []string{"source-b", "source-c"} {
		if _, ok := RegistrySSHPoolScopeByConnectionID(pool, id); ok {
			t.Fatalf("%s matched", id)
		}
	}
}

func TestFallsBackToTheCanonicalProfileWhenTheAppliedWindowHadNoRouteToKeep(t *testing.T) {
	want := WindowConnectionRoute{Profile: strp("default")}
	if got := AppliedPrimaryWindowRoute(ConnectionRegistry{Primary: "local"}, ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if got := AppliedPrimaryWindowRoute(ConnectionRegistry{Primary: ""}, "  "); !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if got := AppliedPrimaryWindowRoute(ConnectionRegistry{Primary: "macmini"}, "work"); !reflect.DeepEqual(got, WindowConnectionRoute{ConnectionID: strp("macmini"), Profile: strp("work"), RegistryScoped: true}) {
		t.Fatalf("remote apply %+v", got)
	}
}

func TestAboutPanelVersionScopeFollowsTheWindowRouteNotThePrimary(t *testing.T) {
	if got := WindowRouteVersionScope(&WindowConnectionRoute{ConnectionID: strp("remote-a"), Profile: strp("work"), RegistryScoped: true}); got != (VersionScope{ConnectionID: "remote-a", Profile: "work"}) {
		t.Fatalf("%+v", got)
	}
	if got := WindowRouteVersionScope(&WindowConnectionRoute{ConnectionID: strp("x"), Profile: strp("work")}); got != (VersionScope{Profile: "work"}) {
		t.Fatalf("%+v", got)
	}
	if got := WindowRouteVersionScope(nil); got != (VersionScope{}) {
		t.Fatalf("%+v", got)
	}
}

func TestAboutPanelScopeUsesTheFocusedWindowRouteAndFallsBackToThePrimary(t *testing.T) {
	routes := NewWindowConnectionRoutes()
	routes.Set("7", map[string]any{"connectionId": "remote-a", "profile": "work", "registryScoped": true})
	if got := routes.VersionScopeFor("7", true); got != (VersionScope{ConnectionID: "remote-a", Profile: "work"}) {
		t.Fatalf("%+v", got)
	}
	for _, c := range []struct {
		id   string
		live bool
	}{{"7", false}, {"8", true}, {"", false}} {
		if got := routes.VersionScopeFor(c.id, c.live); got != (VersionScope{}) {
			t.Fatalf("%v %+v", c, got)
		}
	}
}

func TestLiveWindowStateReadsTheSenderWindowStateNotACachedSnapshot102451(t *testing.T) {
	read := ""
	state := LiveWindowState(func() (map[string]any, bool) { read = "sender"; return map[string]any{"isFullscreen": true}, true }, nil)
	if !reflect.DeepEqual(state, map[string]any{"isFullscreen": true}) || read != "sender" {
		t.Fatalf("%v %s", state, read)
	}
}

func TestLiveWindowStateFallsBackToThePrimaryWindowWhenTheReplyCarriesNoSender(t *testing.T) {
	state := LiveWindowState(nil, func() (map[string]any, bool) { return map[string]any{"isFullscreen": false}, true })
	if !reflect.DeepEqual(state, map[string]any{"isFullscreen": false}) {
		t.Fatalf("%v", state)
	}
}

func TestLiveWindowStateReturnsUndefinedWhenNoUsableWindowExists(t *testing.T) {
	gone := func() (map[string]any, bool) { return nil, false }
	if LiveWindowState(nil, gone) != nil || LiveWindowState(gone, nil) != nil {
		t.Fatal("state from a destroyed window")
	}
}

func TestOverlayWindowStateSpreadsLiveStateOverTheConnectionReply(t *testing.T) {
	got := OverlayWindowState(Connection{"model": "x", "isFullscreen": false}, map[string]any{"isFullscreen": true})
	if !reflect.DeepEqual(got, Connection{"model": "x", "isFullscreen": true}) {
		t.Fatalf("%v", got)
	}
}

func TestOverlayWindowStateReturnsTheConnectionUntouchedWhenNoLiveStateWasComputable(t *testing.T) {
	connection := Connection{"model": "x", "isFullscreen": false}
	got := OverlayWindowState(connection, nil)
	if !reflect.DeepEqual(got, connection) {
		t.Fatalf("%v", got)
	}
}

func TestARendererThatJoinsARemoteBootMidRosterReadAdoptsTheServedProfileItRePinnedTo(t *testing.T) {
	for _, asked := range []*string{nil, strp("windows-only")} {
		pin := &PrimaryProfilePin{}
		pin.Pin("windows-only")
		primary := func() string { return pin.Resolve(func() string { return "windows-only" }) }
		request := NewPrimaryBootRequest(asked, primary(), asked == nil)
		routeFor := func(key string) ProfileBackendRoute {
			return ProfileBackendRoute{Backend: "primary", DescriptorProfile: key, ScopePath: true}
		}
		route := routeFor(request.Key)
		joined, err := JoinPrimaryBoot(request, route, primary, routeFor, func(PrimaryBootRequest) (Connection, error) {
			pin.Pin("default")
			return Connection{"mode": "remote"}, nil
		})
		want := "default"
		if asked != nil {
			want = "windows-only"
		}
		if err != nil || joined.Key != want || joined.Route.DescriptorProfile != want ||
			!reflect.DeepEqual(joined.Descriptor, Connection{"mode": "remote", "profile": want, "sharedPrimary": true}) {
			t.Fatalf("asked %v: %+v %v", asked, joined, err)
		}
	}
}

func TestALivePrimaryKeepsAnsweringForItsBootedProfileAfterThePreferenceMoves(t *testing.T) {
	pin := &PrimaryProfilePin{}
	preference := "default"
	if pin.Pin(preference) != "default" || pin.Resolve(func() string { return preference }) != "default" {
		t.Fatal("pin")
	}
	preference = "claude"
	if pin.Resolve(func() string { return preference }) != "default" {
		t.Fatal("pin moved with the preference")
	}
}

func TestTeardownReleasesThePinSoTheNextStartFollowsThePreference(t *testing.T) {
	pin := &PrimaryProfilePin{}
	pin.Pin("default")
	pin.Clear()
	if pin.Booted() != "" || pin.Resolve(func() string { return "claude" }) != "claude" {
		t.Fatal("clear")
	}
}

func TestOneLaunchDecisionFeedsRoutingArgvAndEnvFromTheSameRead(t *testing.T) {
	if got := ResolveLaunchProfile(func() string { return "beta" }); got != (LaunchProfile{ArgvProfile: "beta", RoutingProfile: "beta"}) {
		t.Fatalf("%+v", got)
	}
	for _, v := range []string{"", "   "} {
		if got := ResolveLaunchProfile(func() string { return v }); got != (LaunchProfile{RoutingProfile: "default"}) {
			t.Fatalf("%q %+v", v, got)
		}
	}
}

type startupRemote struct{ BaseURL, ConnectionID string }
type startupBackend struct{ Kind, Command string }

type startupCalls struct {
	mu                                                                                               sync.Mutex
	resolveRemote, connectRemote, ensureRuntime, prepare, decision, waitLocal, selectPrimary, attach int
	ensured                                                                                          []startupBackend
	connected                                                                                        []startupRemote
}

var bootstrapBackend = startupBackend{Kind: "bootstrap-needed"}

// firstRunGate stands in for the first run setup gate of the connections group
type firstRunGate struct {
	waiting  chan struct{}
	decision chan string
}

func newFirstRunGate() *firstRunGate {
	return &firstRunGate{waiting: make(chan struct{}), decision: make(chan string, 1)}
}

func (g *firstRunGate) wait(startupBackend) (string, error) {
	close(g.waiting)
	return <-g.decision, nil
}

func startupOptions(c *startupCalls) PrimaryStartupOptions[startupBackend, startupRemote, Connection] {
	return PrimaryStartupOptions[startupBackend, startupRemote, Connection]{
		AssertCurrentAttempt: func() error { return nil },
		ConnectRemote: func(r startupRemote) (Connection, error) {
			c.mu.Lock()
			c.connectRemote++
			c.connected = append(c.connected, r)
			c.mu.Unlock()
			return Connection{"baseUrl": r.BaseURL, "mode": "remote"}, nil
		},
		EnsureLocalRuntime: func(b startupBackend) (startupBackend, error) {
			c.mu.Lock()
			c.ensureRuntime++
			c.ensured = append(c.ensured, b)
			c.mu.Unlock()
			b.Command = "icebath"
			return b, nil
		},
		PrepareLocalBackend: func() (startupBackend, error) { c.prepare++; return bootstrapBackend, nil },
		ResolveRemote:       func() (*startupRemote, error) { c.resolveRemote++; return nil, nil },
		WaitForDecision:     func(startupBackend) (string, error) { c.decision++; return "continue-local", nil },
		WaitForLocalStart:   func() error { c.waitLocal++; return nil },
	}
}

func TestRemoteApplyReResolvesTheSavedConnectionWithoutEnsuringALocalRuntime(t *testing.T) {
	c := &startupCalls{}
	gate := newFirstRunGate()
	saved := startupRemote{BaseURL: "https://gateway.example.com/hermes"}
	var configured *startupRemote
	o := startupOptions(c)
	o.ResolveRemote = func() (*startupRemote, error) { c.resolveRemote++; return configured, nil }
	o.WaitForDecision = gate.wait
	done := make(chan PrimaryStartupResult[startupBackend, Connection], 1)
	go func() { r, _ := RunPrimaryBackendStartup(o); done <- r }()
	<-gate.waiting
	configured = &saved
	gate.decision <- "remote-applied"
	r := <-done
	if r.Kind != StartupRemote || !reflect.DeepEqual(r.Connection, Connection{"baseUrl": saved.BaseURL, "mode": "remote"}) {
		t.Fatalf("%+v", r)
	}
	if c.resolveRemote != 2 || !reflect.DeepEqual(c.connected, []startupRemote{saved}) || c.ensureRuntime != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestAnAlreadySavedRemoteBypassesEveryLocalStartupStep(t *testing.T) {
	c := &startupCalls{}
	saved := startupRemote{BaseURL: "https://gateway.example.com/hermes"}
	o := startupOptions(c)
	o.ResolveRemote = func() (*startupRemote, error) { return &saved, nil }
	r, err := RunPrimaryBackendStartup(o)
	if err != nil || r.Kind != StartupRemote || r.Connection["baseUrl"] != saved.BaseURL {
		t.Fatalf("%+v %v", r, err)
	}
	if c.waitLocal+c.prepare+c.decision+c.ensureRuntime != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestRemoteApplyFailsClearlyWhenNoSavedRemoteCanBeResolved(t *testing.T) {
	c := &startupCalls{}
	gate := newFirstRunGate()
	o := startupOptions(c)
	o.WaitForDecision = gate.wait
	gate.decision <- "remote-applied"
	_, err := RunPrimaryBackendStartup(o)
	if err == nil || !strings.Contains(err.Error(), "without a saved remote backend") || c.connectRemote != 0 || c.ensureRuntime != 0 {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestAfterUpdateClearanceARegistryPrimaryIsSelectedBeforeAnyLocalSpawn(t *testing.T) {
	c := &startupCalls{}
	sshPrimary := startupRemote{BaseURL: "http://127.0.0.1:54414", ConnectionID: "100-106-105-2"}
	cleared := false
	o := startupOptions(c)
	o.SelectRegistryPrimary = func() (*startupRemote, error) {
		if !cleared {
			t.Error("registry primary selected before update clearance")
		}
		return &sshPrimary, nil
	}
	o.WaitForLocalStart = func() error { cleared = true; return nil }
	o.AttachHostBackend = func() (*Connection, error) {
		c.attach++
		return &Connection{"baseUrl": "http://127.0.0.1:49900", "mode": "local"}, nil
	}
	r, err := RunPrimaryBackendStartup(o)
	if err != nil || r.Kind != StartupRemote || r.Connection["baseUrl"] != sshPrimary.BaseURL {
		t.Fatalf("%+v %v", r, err)
	}
	if c.prepare+c.ensureRuntime+c.attach != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestANullPostUpdatePrimaryStillSpawnsLocalExactlyOnce(t *testing.T) {
	c := &startupCalls{}
	o := startupOptions(c)
	o.SelectRegistryPrimary = func() (*startupRemote, error) { c.selectPrimary++; return nil, nil }
	r, err := RunPrimaryBackendStartup(o)
	if err != nil || r.Kind != StartupLocal || c.selectPrimary != 1 || c.ensureRuntime != 1 {
		t.Fatalf("%+v %v %+v", r, err, c)
	}
}

func TestContinueLocalWaitsForUpdateExclusionAndEnsuresThePreparedRuntimeExactlyOnce(t *testing.T) {
	c := &startupCalls{}
	gate := newFirstRunGate()
	o := startupOptions(c)
	o.WaitForDecision = gate.wait
	gate.decision <- "continue-local"
	r, err := RunPrimaryBackendStartup(o)
	if err != nil || r.Kind != StartupLocal || r.Backend != (startupBackend{Kind: "bootstrap-needed", Command: "icebath"}) {
		t.Fatalf("%+v %v", r, err)
	}
	if c.waitLocal != 1 || c.prepare != 1 || !reflect.DeepEqual(c.ensured, []startupBackend{bootstrapBackend}) || c.resolveRemote != 1 {
		t.Fatalf("%+v", c)
	}
}

func TestInvalidatingAPendingRuntimeDiscoveryPreventsSetupAndBootstrap(t *testing.T) {
	c := &startupCalls{}
	state := &ConnectionState[int, Connection]{}
	attempt, err := state.StartAttempt()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finish := make(chan startupBackend)
	o := startupOptions(c)
	o.AssertCurrentAttempt = func() error { return state.AssertCurrentAttempt(attempt) }
	o.PrepareLocalBackend = func() (startupBackend, error) { close(started); return <-finish, nil }
	done := make(chan error, 1)
	go func() { _, err := RunPrimaryBackendStartup(o); done <- err }()
	<-started
	state.Invalidate()
	finish <- bootstrapBackend
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "superseded") || c.decision != 0 || c.ensureRuntime != 0 {
			t.Fatalf("%v %+v", err, c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not settle")
	}
}

func TestResetRejectsWithATypedErrorAndNeverEntersEitherBackend(t *testing.T) {
	c := &startupCalls{}
	gate := newFirstRunGate()
	o := startupOptions(c)
	o.WaitForDecision = gate.wait
	gate.decision <- "reset"
	_, err := RunPrimaryBackendStartup(o)
	var reset FirstRunSetupResetError
	if !errors.As(err, &reset) || !reset.FirstRunSetupReset() || c.connectRemote != 0 || c.ensureRuntime != 0 {
		t.Fatalf("%v %+v", err, c)
	}
}
