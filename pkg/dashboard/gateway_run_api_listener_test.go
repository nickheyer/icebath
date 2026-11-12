package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Home a re-executed test binary serves a gateway listener for, the probe script of the hermes subprocess
const sessionListenerProbeEnv = "ICEBATH_TEST_SESSION_LISTENER_HOME"

// Running listener of a runner on loopback, started the way the gateway starts its session API
type testGatewayAPI struct {
	origin   string
	port     int
	server   gateway.GatewayListenerServer
	listener net.Listener
	served   chan error
}

// Binds a loopback socket, loads the runtime API, mounts the dashboard only when gated or loaded, then serves
func startTestGatewayAPI(ctx context.Context, runner gateway.GatewayAPIRunner) (*testGatewayAPI, error) {
	host := "127.0.0.1"
	trusted := gateway.DashboardPublicHosts(ctx)
	boundary := &gateway.ListenerState{BoundHost: host, AuthRequired: gateway.ListenerAuthRequired(host, trusted), TrustedPublicHosts: trusted}
	server, err := BuildGatewayListener(ctx, runner, boundary, nil, host, 0)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", host+":0")
	if err != nil {
		return nil, err
	}
	boundary.BoundPort = listener.Addr().(*net.TCPAddr).Port
	fail := func(err error) (*testGatewayAPI, error) {
		listener.Close()
		return nil, errors.Join(err, server.Mount().Close(ctx))
	}
	if err := server.Load(ctx); err != nil {
		return fail(err)
	}
	if boundary.AuthRequired || server.DashboardLoaded() {
		if _, err := server.Mount().Ensure(ctx); err != nil {
			return fail(err)
		}
	}
	if err := server.Startup(ctx, listener); err != nil {
		return fail(err)
	}
	if !server.Started() || server.ShouldExit() {
		return fail(errors.New("gateway API lifespan startup failed"))
	}
	api := &testGatewayAPI{origin: fmt.Sprintf("http://%s:%d", host, boundary.BoundPort), port: boundary.BoundPort,
		server: server, listener: listener, served: make(chan error, 1)}
	go func() {
		err := server.MainLoop(ctx)
		err = errors.Join(err, server.Shutdown(ctx, listener))
		listener.Close()
		api.served <- errors.Join(err, server.Mount().Close(ctx))
	}()
	return api, nil
}

// Asks the listener to drain and waits for its serve task
func (a *testGatewayAPI) stop() error {
	a.server.RequestExit()
	return <-a.served
}

// Status of a raw upgrade to the session socket carrying the protocols and a Host header
func rawUpgradeStatus(port int, host string, protocols []string) (int, error) {
	conn, err := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(port), 10*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	request := "GET /api/ws HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\n" +
		"Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: " + strings.Join(protocols, ", ") + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		return 0, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return 0, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("status line %q", line)
	}
	return strconv.Atoi(fields[1])
}

// Serves a listener for the home, dials its session socket and dashboard routes and reports what it saw
func sessionListenerProbe(ctx context.Context, h string) (map[string]any, error) {
	previous := sessionHandleWS
	defer func() { sessionHandleWS = previous }()
	sessionHandleWS = func(ctx context.Context, ws tuiserver.ServedSocket, _ map[string]any, _ *string, _ bool) error {
		protocol := gateway.CanonicalGatewayProtocol
		if err := ws.Accept(ctx, &protocol); err != nil {
			return err
		}
		frame, err := json.Marshal(map[string]any{"native": true, "dashboard_loaded": webServerAppLoaded()})
		if err != nil {
			return err
		}
		if err := ws.SendText(ctx, string(frame)); err != nil {
			return err
		}
		return ws.Close(1000)
	}
	tickets, err := gateway.NewTicketStore("owner", map[string]bool{h: true})
	if err != nil {
		return nil, err
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	runner := &fakeAPIRunner{descriptor: map[string]any{"state": "ready"}, tickets: tickets,
		authority: &gateway.SessionAuthority{DB: db, ProfileID: h}}
	out := map[string]any{}
	api, err := startTestGatewayAPI(ctx, runner)
	if err != nil {
		return nil, err
	}
	probe := func() error {
		out["after_bind"] = webServerAppLoaded()
		protocols := func() ([]string, error) {
			ticket, err := tickets.Mint(h, "human", "interactive", "", false)
			return []string{gateway.CanonicalGatewayProtocol, gateway.GatewayWSTicketPrefix + ticket}, err
		}
		offered, err := protocols()
		if err != nil {
			return err
		}
		dialer := websocket.Dialer{Subprotocols: offered, HandshakeTimeout: 10 * time.Second}
		ws, _, err := dialer.DialContext(ctx, strings.Replace(api.origin, "http:", "ws:", 1)+"/api/ws", nil)
		if err != nil {
			return err
		}
		_, frame, err := ws.ReadMessage()
		ws.Close()
		if err != nil {
			return err
		}
		var native map[string]any
		if err := json.Unmarshal(frame, &native); err != nil {
			return err
		}
		out["native"] = native
		if offered, err = protocols(); err != nil {
			return err
		}
		if out["rebind"], err = rawUpgradeStatus(api.port, "attacker.invalid", offered); err != nil {
			return err
		}
		out["before_http"] = webServerAppLoaded()
		client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: nil}}
		status, err := client.Get(api.origin + "/api/status")
		if err != nil {
			return err
		}
		status.Body.Close()
		out["status"] = status.StatusCode
		foreign, err := http.NewRequestWithContext(ctx, http.MethodGet, api.origin+"/api/status", nil)
		if err != nil {
			return err
		}
		foreign.Host = "attacker.invalid"
		rebound, err := client.Do(foreign)
		if err != nil {
			return err
		}
		rebound.Body.Close()
		out["rebind_http"] = rebound.StatusCode
		out["after_http"] = webServerAppLoaded()
		app, err := WebServerApp()
		if err != nil {
			return err
		}
		out["runner_bound"] = app.State.Get("gateway_runner") == any(runner)
		out["token_withheld"] = app.State.Bool("withhold_session_token")
		return nil
	}
	err = probe()
	if stopErr := api.stop(); err == nil {
		err = stopErr
	}
	if err != nil {
		return nil, err
	}
	app, err := WebServerApp()
	if err != nil {
		return nil, err
	}
	out["unbound"] = app.State.Get("gateway_runner") == nil
	return out, nil
}

// Runs the listener probe in this process and prints its result line, the exit status of the probe child
func runSessionListenerProbe(h string) int {
	out, err := sessionListenerProbe(context.Background(), h)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe failed:", err)
		return 1
	}
	line, err := json.Marshal(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe failed:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "RESULT "+string(line))
	return 0
}

func TestSessionListenerServesNativeWsBeforeImportingTheDashboard(t *testing.T) {
	tmp := t.TempDir()
	h, user := filepath.Join(tmp, "state"), filepath.Join(tmp, "user")
	for _, dir := range []string{h, user} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0])
	for _, name := range []string{"PATH", "LANG", "TZ", "ICEBATH_RUNTIME_DIR", "TMPDIR"} {
		if value, set := os.LookupEnv(name); set {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+user, "ICEBATH_HOME="+h, sessionListenerProbeEnv+"="+h)
	output, err := cmd.CombinedOutput()
	var result string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "RESULT ") {
			result = strings.TrimPrefix(line, "RESULT ")
		}
	}
	if err != nil || result == "" {
		tail := string(output)
		if len(tail) > 3000 {
			tail = tail[len(tail)-3000:]
		}
		t.Fatalf("probe exit = %v\n%s", err, tail)
	}
	var out struct {
		AfterBind bool `json:"after_bind"`
		Native    struct {
			Native          bool `json:"native"`
			DashboardLoaded bool `json:"dashboard_loaded"`
		} `json:"native"`
		Rebind        int  `json:"rebind"`
		BeforeHTTP    bool `json:"before_http"`
		Status        int  `json:"status"`
		RebindHTTP    int  `json:"rebind_http"`
		AfterHTTP     bool `json:"after_http"`
		RunnerBound   bool `json:"runner_bound"`
		TokenWithheld bool `json:"token_withheld"`
		Unbound       bool `json:"unbound"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatal(err)
	}
	if out.AfterBind {
		t.Error("listener start built the dashboard application")
	}
	if !out.Native.Native || out.Native.DashboardLoaded {
		t.Errorf("native = %+v", out.Native)
	}
	if out.Rebind != 403 {
		t.Errorf("rebind = %d", out.Rebind)
	}
	if out.BeforeHTTP {
		t.Error("the dashboard was built before the first dashboard request")
	}
	if out.Status != 200 || out.RebindHTTP != 400 {
		t.Errorf("status = %d, rebind_http = %d", out.Status, out.RebindHTTP)
	}
	if !out.AfterHTTP || !out.RunnerBound || !out.TokenWithheld || !out.Unbound {
		t.Errorf("after_http = %v, runner_bound = %v, token_withheld = %v, unbound = %v", out.AfterHTTP, out.RunnerBound, out.TokenWithheld, out.Unbound)
	}
}
