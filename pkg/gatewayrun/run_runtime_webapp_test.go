package gatewayrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

type fakeDashboardWeb struct{ printed, checked int }

func (w *fakeDashboardWeb) WebDist() (string, bool)     { return "", false }
func (w *fakeDashboardWeb) PrivateLaunch() bool         { return true }
func (w *fakeDashboardWeb) ListenerPublicURL() string   { return "" }
func (w *fakeDashboardWeb) AuthRequired() bool          { return false }
func (w *fakeDashboardWeb) LaunchTicketTTLSeconds() int { return 60 }
func (w *fakeDashboardWeb) MintLaunchTicket() (string, error) {
	w.checked++
	return fmt.Sprintf("checked-%d", w.checked), nil
}
func (w *fakeDashboardWeb) MintPrintedLaunchTicket() (string, error) {
	w.printed++
	return fmt.Sprintf("printed-%d", w.printed), nil
}
func (w *fakeDashboardWeb) RotateBrowserCredential(string) map[string]int {
	return map[string]int{"sockets": 0}
}

type fakeDashboardMount struct {
	mu     sync.Mutex
	web    *fakeDashboardWeb
	closed bool
}

func (m *fakeDashboardMount) Ensure(context.Context) (gateway.GatewayDashboardWeb, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.web == nil {
		m.web = &fakeDashboardWeb{}
	}
	return m.web, nil
}
func (m *fakeDashboardMount) Web() gateway.GatewayDashboardWeb {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.web == nil {
		return nil
	}
	return m.web
}
func (m *fakeDashboardMount) Closed() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.closed }
func (m *fakeDashboardMount) Close(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

type fakeListenerServer struct {
	mount   *fakeDashboardMount
	exit    chan struct{}
	once    sync.Once
	started bool
}

func (s *fakeListenerServer) Backlog() int                         { return 2048 }
func (s *fakeListenerServer) DashboardLoaded() bool                { return false }
func (s *fakeListenerServer) Mount() gateway.GatewayDashboardMount { return s.mount }
func (s *fakeListenerServer) Load(context.Context) error           { return nil }
func (s *fakeListenerServer) Startup(context.Context, net.Listener) error {
	s.started = true
	return nil
}
func (s *fakeListenerServer) Started() bool                                { return s.started }
func (s *fakeListenerServer) ShouldExit() bool                             { return false }
func (s *fakeListenerServer) RequestExit()                                 { s.once.Do(func() { close(s.exit) }) }
func (s *fakeListenerServer) MainLoop(context.Context) error               { <-s.exit; return nil }
func (s *fakeListenerServer) Shutdown(context.Context, net.Listener) error { return nil }

var webappVerbs = []string{"webapp-status", "webapp-launch", "webapp-sign-out"}

// Answer of a control verb asked by peer, the decoded response object
func askVerb(t *testing.T, server *gateway.GatewayControlServer, verb, peer string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	raw, _ := json.Marshal(map[string]any{"protocol": 1, "verb": verb, "id": 1, "params": params})
	var out map[string]any
	if err := json.Unmarshal(server.HandleRequestLine(raw, peer), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Free loopback port the gate leaves to this case
func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// Runtime listener start for config with the dashboard builder stood in, the _start_runtime_api of the hermes cases
func startRuntimeAPI(t *testing.T, config string, busy bool, whileStarting *[]map[string]map[string]any) (*GatewayRunner, string) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	r := allocGatewayRunner()
	r.sessionRuntimeDescriptor.store(map[string]any{})
	server, err := gateway.NewGatewayControlServer(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.sessionControlServer = server
	prev, prevDist := BuildGatewayListener, gateway.WebappRendererBuild.PackagedDist
	t.Cleanup(func() { BuildGatewayListener, gateway.WebappRendererBuild.PackagedDist = prev, prevDist })
	gateway.WebappRendererBuild.PackagedDist = func() string { return "" }
	BuildGatewayListener = func(_ context.Context, _ gateway.GatewayAPIRunner, _ *gateway.ListenerState, webapp *gateway.WebappMount,
		_ string, port int) (gateway.GatewayListenerServer, error) {
		if whileStarting != nil && webapp != nil {
			answers := map[string]map[string]any{}
			for _, verb := range webappVerbs {
				answers[verb] = askVerb(t, server, verb, "same-user-peer", nil)
			}
			*whileStarting = append(*whileStarting, answers)
		}
		return &fakeListenerServer{mount: &fakeDashboardMount{}, exit: make(chan struct{})}, nil
	}
	if busy {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", webappPortOf(config)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
	}
	if err := startGatewayRuntimeAPI(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopGatewayAPI(context.Background(), r.sessionAPI) })
	return r, h
}

// Port a webapp config block names
func webappPortOf(config string) int {
	var port int
	if i := strings.Index(config, "port: "); i >= 0 {
		fmt.Sscanf(config[i+len("port: "):], "%d", &port)
	}
	return port
}

func TestTheWebappVerbsAnswerWhileTheWebappsListenerStarts(t *testing.T) {
	var seen []map[string]map[string]any
	r, h := startRuntimeAPI(t, fmt.Sprintf("webapp:\n  enabled: true\n  port: %d\n", freePort(t)), false, &seen)
	if len(seen) != 1 {
		t.Fatalf("answers %v", seen)
	}
	status := seen[0]["webapp-status"]["result"].(map[string]any)
	if status["state"] != "starting" || status["url"] != nil || status["reason"] != nil {
		t.Fatalf("status %v", status)
	}
	for _, verb := range []string{"webapp-launch", "webapp-sign-out"} {
		answer := seen[0][verb]
		if answer["ok"] != false || !strings.Contains(answer["error"].(string), "still starting") {
			t.Fatalf("%s answered %v", verb, answer)
		}
	}
	if _, err := os.Stat(filepath.Join(h, gateway.WebappTokenPath)); !os.IsNotExist(err) {
		t.Fatalf("sign-out wrote a credential: %v", err)
	}
	if r.webappListenerStatus().State != "serving" {
		t.Fatalf("state %s", r.webappListenerStatus().State)
	}
}

func TestTheLauncherReadsTheWebappStateFromItsGateway(t *testing.T) {
	off, _ := startRuntimeAPI(t, "", false, nil)
	status := askVerb(t, off.sessionControlServer, "webapp-status", "same-user-peer", nil)["result"].(map[string]any)
	if status["state"] != "off" || status["url"] != nil || status["reason"] != nil {
		t.Fatalf("off status %v", status)
	}
	stopGatewayAPI(context.Background(), off.sessionAPI)
	port := freePort(t)
	busy, _ := startRuntimeAPI(t, fmt.Sprintf("webapp:\n  enabled: true\n  port: %d\n", port), true, nil)
	status = askVerb(t, busy.sessionControlServer, "webapp-status", "same-user-peer", nil)["result"].(map[string]any)
	if status["state"] != "unavailable" || !strings.Contains(status["reason"].(string), fmt.Sprintf("127.0.0.1:%d", port)) {
		t.Fatalf("busy status %v", status)
	}
}

func TestAGatewayNotServingTheWebappMintsNoLaunchLink(t *testing.T) {
	off, _ := startRuntimeAPI(t, "", false, nil)
	refused := askVerb(t, off.sessionControlServer, "webapp-launch", "same-user-peer", nil)
	if refused["ok"] != false || !strings.Contains(refused["error"].(string), "webapp.enabled") {
		t.Fatalf("off launch %v", refused)
	}
	stopGatewayAPI(context.Background(), off.sessionAPI)
	port := freePort(t)
	busy, _ := startRuntimeAPI(t, fmt.Sprintf("webapp:\n  enabled: true\n  port: %d\n", port), true, nil)
	refused = askVerb(t, busy.sessionControlServer, "webapp-launch", "same-user-peer", nil)
	if refused["ok"] != false || !strings.Contains(refused["error"].(string), fmt.Sprintf("127.0.0.1:%d", port)) {
		t.Fatalf("busy launch %v", refused)
	}
}

func TestWebappVerbsAnswerOnlyAnAuthenticatedSameUserPeer(t *testing.T) {
	r, _ := startRuntimeAPI(t, fmt.Sprintf("webapp:\n  enabled: true\n  port: %d\n", freePort(t)), false, nil)
	for _, verb := range webappVerbs {
		refused := askVerb(t, r.sessionControlServer, verb, "", nil)
		if refused["ok"] != false || !strings.Contains(refused["error"].(string), "PermissionError") {
			t.Fatalf("%s answered %v", verb, refused)
		}
	}
}

func TestWhereOneUseLinksCannotBeCheckedTheLauncherGetsOneToOpenByHand(t *testing.T) {
	port := freePort(t)
	r, h := startRuntimeAPI(t, fmt.Sprintf("webapp:\n  enabled: true\n  port: %d\n", port), false, nil)
	unchecked := false
	registerControlVerbs(context.Background(), r, h, nil, &unchecked)
	result := askVerb(t, r.sessionControlServer, "webapp-launch", "same-user-peer", nil)["result"].(map[string]any)
	page, fragment, _ := strings.Cut(result["url"].(string), "#")
	if page != fmt.Sprintf("http://127.0.0.1:%d/webapp/launch", port) {
		t.Fatalf("page %s", page)
	}
	if result["private"] != true || result["expires_in_seconds"] != float64(60) {
		t.Fatalf("result %v", result)
	}
	values, _ := url.ParseQuery(fragment)
	if values.Get("ticket") != "printed-1" {
		t.Fatalf("ticket %v", values)
	}
}

func TestALaunchLinkForANamedProfileOpensThatProfile(t *testing.T) {
	port := freePort(t)
	r, h := startRuntimeAPI(t, fmt.Sprintf("webapp:\n  enabled: true\n  port: %d\n", port), false, nil)
	for _, checked := range []bool{true, false} {
		registerControlVerbs(context.Background(), r, h, nil, &checked)
		result := askVerb(t, r.sessionControlServer, "webapp-launch", "same-user-peer", map[string]any{"profile": "work"})["result"].(map[string]any)
		page, fragment, _ := strings.Cut(result["url"].(string), "#")
		values, _ := url.ParseQuery(fragment)
		if page != fmt.Sprintf("http://127.0.0.1:%d/webapp/launch", port) || values.Get("query") != "?profile=work" {
			t.Fatalf("url %s", result["url"])
		}
	}
}

func TestALaunchLinkRefusesAMalformedProfileName(t *testing.T) {
	for _, profile := range []any{"../etc", "a b", "", strings.Repeat("x", 65), 7} {
		r, _ := startRuntimeAPI(t, fmt.Sprintf("webapp:\n  enabled: true\n  port: %d\n", freePort(t)), false, nil)
		refused := askVerb(t, r.sessionControlServer, "webapp-launch", "same-user-peer", map[string]any{"profile": profile})
		if refused["ok"] != false || !strings.Contains(refused["error"].(string), "profile") {
			t.Fatalf("%v answered %v", profile, refused)
		}
		stopGatewayAPI(context.Background(), r.sessionAPI)
	}
}
