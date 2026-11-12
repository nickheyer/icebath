//go:build !windows

package backend

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type sent struct {
	window, event string
	payload       any
}

type fakeHost struct {
	mu      sync.Mutex
	states  map[string]map[string]any
	sent    []sent
	resets  []string
	primary bool
}

func (h *fakeHost) WindowState(id string) (map[string]any, bool) {
	s, ok := h.states[id]
	return s, ok
}
func (h *fakeHost) MainWindowID() string { return "main" }
func (h *fakeHost) Send(w, e string, p any) {
	h.mu.Lock()
	h.sent = append(h.sent, sent{w, e, p})
	h.mu.Unlock()
}
func (h *fakeHost) IsPrimaryInstance() bool           { return h.primary }
func (h *fakeHost) ResetPreviewReach(windowID string) { h.resets = append(h.resets, windowID) }
func (h *fakeHost) events(name string) []sent {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []sent
	for _, s := range h.sent {
		if s.event == name {
			out = append(out, s)
		}
	}
	return out
}

type fakeRoutes struct {
	active        string
	registry      ConnectionRegistry
	revalidate    RevalidateResult
	teardowns     []string
	activeGateway string
	bridge        map[string]bool
}

func (r *fakeRoutes) ActiveProfile() string { return r.active }
func (r *fakeRoutes) ResolveRequest(requested *string, source *WindowConnectionRoute, primaryKey string) (string, string, bool) {
	if requested != nil && strings.TrimSpace(*requested) != "" {
		return "", strings.TrimSpace(*requested), false
	}
	if source != nil && source.profile() != "" {
		if source.RegistryScoped {
			return source.connectionID(), source.profile(), false
		}
		return "", source.profile(), false
	}
	return "", primaryKey, true
}
func (r *fakeRoutes) ResolvedConnectionID(conn Connection) string {
	if conn["mode"] == "local" {
		return LocalConnectionID
	}
	return ""
}
func (r *fakeRoutes) EnsureRegistryBackend(id, profile string) (Connection, error) {
	return Connection{"baseUrl": "https://" + id + ".invalid", "mode": "remote", "profile": profile}, nil
}
func (r *fakeRoutes) ProfileRoute(key string) ProfileBackendRoute {
	if key == r.active || (r.active == "" && key == "default") {
		return ProfileBackendRoute{Backend: "primary"}
	}
	return ProfileBackendRoute{Backend: "pool"}
}
func (r *fakeRoutes) AssertCanStart(string) error { return nil }
func (r *fakeRoutes) ResolveRemote(string, bool, bool) (Connection, error) {
	return nil, nil
}
func (r *fakeRoutes) ConnectRemote(remote Connection, profile string, boot *PrimaryRemoteBoot) (Connection, error) {
	return remote, nil
}
func (r *fakeRoutes) WaitForFirstRunDecision(ResolvedBackend) (string, error) {
	return "continue-local", nil
}
func (r *fakeRoutes) RevalidatePrimary(*Future[Connection], func()) (RevalidateResult, error) {
	return r.revalidate, nil
}
func (r *fakeRoutes) RevalidatePool() error { return nil }
func (r *fakeRoutes) RemoteWsURL(conn Connection) (string, error) {
	return conn.str("wsUrl"), nil
}
func (r *fakeRoutes) IsAuthRejection(err error) bool         { return strings.Contains(err.Error(), "401") }
func (r *fakeRoutes) SetActiveGatewayProfile(profile string) { r.activeGateway = profile }
func (r *fakeRoutes) SetBridgeProfileState(profile string, local bool) {
	if r.bridge == nil {
		r.bridge = map[string]bool{}
	}
	r.bridge[profile] = local
}
func (r *fakeRoutes) TeardownSSH(scope string) error {
	r.teardowns = append(r.teardowns, scope)
	return nil
}

type fakeRuntime struct{ hold any }

func (fakeRuntime) EnsureRuntime(b ResolvedBackend, assert func() error) (ResolvedBackend, error) {
	if err := assert(); err != nil {
		return b, err
	}
	return b, nil
}
func (fakeRuntime) WaitForUpdate() error                        { return nil }
func (fakeRuntime) WaitForPoolUpdate(string, func() bool) error { return nil }
func (r fakeRuntime) UpdateHold() any                           { return r.hold }

// fakeEnsureCLI is an icebath stand in that answers gateway ensure with a ready endpoint for its profile home
func fakeEnsureCLI(t *testing.T, home string) string {
	endpoint := testEndpoint(home, "owner")
	payload, _ := json.Marshal(map[string]any{"state": "ready", "endpoint": endpoint})
	calls := filepath.Join(t.TempDir(), "calls")
	return writeScript(t, t.TempDir(), "icebath", fmt.Sprintf("echo \"$*\" >> %q\nprintf '%%s' '%s'\n", calls, string(payload)))
}

func newTestService(t *testing.T, routes *fakeRoutes, cli string) (*Service, *fakeHost) {
	home := t.TempDir()
	host := &fakeHost{primary: true, states: map[string]map[string]any{"main": {"isFullscreen": false}, "w2": {"isFullscreen": true}}}
	svc := NewService(host, routes, fakeRuntime{hold: map[string]any{"holdId": "h1"}}, ServiceOptions{
		IcebathHome: home,
		Cwd:         t.TempDir(),
		Env:         func() map[string]string { return map[string]string{"PATH": os.Getenv("PATH")} },
		Resolve: func(args []string) (ResolvedBackend, error) {
			if cli == "" {
				return ResolvedBackend{Kind: "bootstrap-needed", Bootstrap: true}, nil
			}
			return ResolvedBackend{BackendCommand: BackendCommand{Command: cli, Args: args}, Kind: "command", Label: "test cli"}, nil
		},
	})
	return svc, host
}

func TestGetConnectionDialsTheLocalPrimaryAndCarriesTheCallerLiveWindowState(t *testing.T) {
	routes := &fakeRoutes{registry: ConnectionRegistry{Primary: "local", Connections: []RegistrySource{{"local", "local"}}}}
	svc, host := newTestService(t, routes, fakeEnsureCLI(t, "/h"))
	conn, err := svc.GetConnection("w2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if conn["baseUrl"] != "http://127.0.0.1:1234" || conn["mode"] != "local" || conn["authMode"] != "native" || conn["profile"] != "default" ||
		conn["connectionId"] != "local" || conn["isFullscreen"] != true {
		t.Fatalf("descriptor %v", conn)
	}
	if routes.activeGateway != "default" || !reflect.DeepEqual(routes.bridge, map[string]bool{"default": true}) {
		t.Fatalf("wsl bridge %q %v", routes.activeGateway, routes.bridge)
	}
	again, err := svc.GetConnection("main", nil)
	if err != nil || again["isFullscreen"] != false || again.Endpoint().InstanceID != "owner" {
		t.Fatalf("second dial %v %v", again, err)
	}
	progress := host.events("boot-progress")
	last := progress[len(progress)-1].payload.(map[string]any)
	if last["phase"] != "backend.ready" || last["progress"] != 94 {
		t.Fatalf("boot progress %v", last)
	}
}

func TestARegistryScopedWindowRouteDialsItsConnectionAndIsMarkedRegistryScoped(t *testing.T) {
	svc, _ := newTestService(t, &fakeRoutes{}, "")
	svc.SetActiveConnectionRoute("w2", map[string]any{"connectionId": "macmini", "profile": "work", "registryScoped": true})
	conn, err := svc.GetConnection("w2", nil)
	if err != nil || conn["connectionId"] != "macmini" || conn["registryScoped"] != true || conn["profile"] != "work" || conn["isFullscreen"] != true {
		t.Fatalf("%v %v", conn, err)
	}
}

func TestANamedNonPrimaryProfileDialsItsOwnPooledGateway(t *testing.T) {
	routes := &fakeRoutes{}
	svc, _ := newTestService(t, routes, fakeEnsureCLI(t, "/h/profiles/work"))
	if _, err := svc.GetConnection("main", "work"); err == nil || !strings.Contains(err.Error(), `Profile "work" no longer exists.`) {
		t.Fatalf("missing profile %v", err)
	}
	os.MkdirAll(filepath.Join(svc.opts.IcebathHome, "profiles", "work"), 0o700)
	conn, err := svc.GetConnection("main", "work")
	if err != nil || conn["profile"] != "work" || conn.Endpoint().ProfileID != "/h/profiles/work" {
		t.Fatalf("%v %v", conn, err)
	}
	if svc.pool["work"] == nil {
		t.Fatal("pool entry missing")
	}
}

func TestAPrimaryBootFailureIsReportedOnBootProgressAndTheNextDialRetries(t *testing.T) {
	cli := writeScript(t, t.TempDir(), "icebath", `printf '%s' '{"state":"incompatible","reason_code":"runtime_protocol","endpoint":null}'; exit 3`)
	svc, host := newTestService(t, &fakeRoutes{}, cli)
	_, err := svc.GetConnection("main", nil)
	if err == nil || !strings.Contains(err.Error(), "Gateway incompatible (runtime_protocol)") {
		t.Fatalf("%v", err)
	}
	progress := host.events("boot-progress")
	last := progress[len(progress)-1].payload.(map[string]any)
	if last["phase"] != "backend.error" || last["running"] != false || !strings.HasPrefix(last["message"].(string), "Desktop boot failed: Gateway incompatible") {
		t.Fatalf("progress %v", last)
	}
	if svc.state.Promise() != nil || svc.pin.Booted() != "" {
		t.Fatal("failed attempt left its promise or pin")
	}
}

func TestANonPrimaryInstanceNeverStartsTheBackend(t *testing.T) {
	svc, host := newTestService(t, &fakeRoutes{}, fakeEnsureCLI(t, "/h"))
	host.primary = false
	if _, err := svc.GetConnection("main", nil); err == nil || err.Error() != nonPrimaryInstanceBootFailed {
		t.Fatalf("%v", err)
	}
}

func TestBootProgressNeverMovesBackwardsUnlessAllowedAndOnlyThePrimaryWindowSeesTheHold(t *testing.T) {
	svc, _ := newTestService(t, &fakeRoutes{}, "")
	svc.AdvanceBootProgress("backend.runtime", "Resolving Icebath runtime", 28)
	svc.AdvanceBootProgress("backend.resolve", "Resolving Icebath backend", 8)
	if got := svc.GetBootProgress("main"); got["progress"] != 28 || got["phase"] != "backend.resolve" || !reflect.DeepEqual(got["updateHold"], map[string]any{"holdId": "h1"}) {
		t.Fatalf("%v", got)
	}
	svc.UpdateBootProgress(map[string]any{"progress": 4.4, "error": "boom"}, true)
	got := svc.GetBootProgress("w2")
	if got["progress"] != 4 || got["updateHold"] != nil || got["retryable"] != false || got["error"] != "boom" {
		t.Fatalf("%v", got)
	}
	svc.UpdateBootProgress(map[string]any{"progress": 400.0, "retryable": true}, false)
	if got := svc.GetBootProgress("main"); got["progress"] != 100 || got["retryable"] != true || got["error"] != "boom" {
		t.Fatalf("%v", got)
	}
}

func TestBackendExitIsWithheldDuringASoftRehomeAndConnectionAppliedReachesTheMainWindow(t *testing.T) {
	svc, host := newTestService(t, &fakeRoutes{}, "")
	svc.SendBackendExit(map[string]any{"code": 1})
	svc.mu.Lock()
	svc.softRehome = true
	svc.mu.Unlock()
	svc.SendBackendExit(map[string]any{"code": 2})
	svc.SendConnectionApplied()
	if exits := host.events("backend-exit"); len(exits) != 1 || exits[0].window != "main" {
		t.Fatalf("%v", exits)
	}
	if applied := host.events("connection:applied"); len(applied) != 1 || applied[0].window != "main" {
		t.Fatalf("%v", applied)
	}
}

func TestAWindowRouteChangeResetsOnlyThatWindowPreviewReach(t *testing.T) {
	svc, host := newTestService(t, &fakeRoutes{}, "")
	svc.SetActiveConnectionRoute("w2", map[string]any{"profile": "work"})
	svc.SetActiveConnectionRoute("w2", map[string]any{"profile": "work"})
	svc.SetActiveConnectionRoute("w2", map[string]any{"profile": "work", "connectionId": "a", "registryScoped": true})
	svc.WindowClosed("w2")
	if !reflect.DeepEqual(host.resets, []string{"w2", "w2", "w2"}) || svc.windowRoutes.Get("w2") != nil {
		t.Fatalf("%v", host.resets)
	}
}

func TestProbeLocalBackendReportsAMissingRuntimeWithoutStartingAnything(t *testing.T) {
	svc, host := newTestService(t, &fakeRoutes{}, "")
	probe, err := svc.ProbeLocalBackend()
	if err != nil || !probe.BootstrapNeeded || len(host.sent) != 0 {
		t.Fatalf("%+v %v %v", probe, err, host.sent)
	}
	installed, _ := newTestService(t, &fakeRoutes{}, "/usr/bin/true")
	if probe, _ := installed.ProbeLocalBackend(); probe.BootstrapNeeded {
		t.Fatal("installed runtime reported missing")
	}
}

func TestRecyclingThePrimaryTearsItDownSoftlyAndAnnouncesTheAppliedConnection(t *testing.T) {
	routes := &fakeRoutes{}
	svc, host := newTestService(t, routes, fakeEnsureCLI(t, "/h"))
	if _, err := svc.GetConnection("main", nil); err != nil {
		t.Fatal(err)
	}
	exitsBefore := len(host.events("backend-exit"))
	got, err := svc.RecycleBackend("")
	if err != nil || !reflect.DeepEqual(got, map[string]bool{"ok": true}) {
		t.Fatalf("%v %v", got, err)
	}
	if svc.state.Promise() != nil || len(host.events("connection:applied")) != 1 || len(host.events("backend-exit")) != exitsBefore || !reflect.DeepEqual(routes.teardowns, []string{""}) {
		t.Fatalf("promise %v applied %v teardowns %v", svc.state.Promise(), host.events("connection:applied"), routes.teardowns)
	}
}

func TestRevalidationWithoutAPrimaryOnlySweepsThePool(t *testing.T) {
	svc, _ := newTestService(t, &fakeRoutes{revalidate: RevalidateResult{OK: true, Rebuilt: true}}, "")
	if got, err := svc.RevalidateConnection(); err != nil || got != (RevalidateResult{OK: true}) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestTheGatewaySocketURLOfANativePrimaryCarriesAOneUseGrantBoundToTheWindow(t *testing.T) {
	home := shortSocketTmpDir(t, "desktop-ws-")
	controlServer(t, filepath.Join(home, "gateway.sock"), func(map[string]any, int) any { return grantReply(home, "owner", "ws-grant") })
	svc, _ := newTestService(t, &fakeRoutes{}, fakeEnsureCLI(t, home))
	result := svc.GetGatewayWsURL("w2", nil)
	if !result.OK {
		t.Fatalf("%+v", result)
	}
	u, err := url.Parse(result.WsURL)
	if err != nil || u.Scheme != "ws" || u.Path != "/api/ws" || u.Query().Get("ticket") != "ws-grant" || u.Query().Get("native_dial") == "" {
		t.Fatalf("%s", result.WsURL)
	}
	q := u.Query()
	q.Del("ticket")
	u.RawQuery = q.Encode()
	headers := svc.DialHeaders(DialRequest{URL: u.String(), WindowID: "w2", ResourceType: "webSocket", RequestHeaders: map[string]string{"Origin": "wails://localhost"}})
	if headers == nil || headers["Origin"] != "" {
		t.Fatalf("%v", headers)
	}
	failing, _ := newTestService(t, &fakeRoutes{}, writeScript(t, t.TempDir(), "icebath", "echo 'HTTP 401' >&2; exit 1"))
	if got := failing.GetGatewayWsURL("w2", nil); got.OK || !got.NeedsOauthLogin || !strings.Contains(got.Error, "401") {
		t.Fatalf("%+v", got)
	}
}

func TestAQuittingDesktopRefusesNewBackendStartsAndAbortsTheOneInFlight(t *testing.T) {
	cli := writeScript(t, t.TempDir(), "icebath", "exec sleep 5\n")
	svc, _ := newTestService(t, &fakeRoutes{}, cli)
	if svc.HasPending() {
		t.Fatal("pending before any start")
	}
	if err := svc.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetConnection("main", nil); err == nil || err.Error() != "Icebath Desktop is quitting." {
		t.Fatalf("%v", err)
	}
	if _, err := svc.GetConnection("main", "work"); err == nil || err.Error() != "Icebath Desktop is quitting." {
		t.Fatalf("pool %v", err)
	}
}
