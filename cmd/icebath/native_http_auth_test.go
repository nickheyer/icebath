package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/dashboard"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
)

// Header a native client presents its private HTTP grant in
const nativeTicketHeader = "X-Icebath-Gateway-Ticket"

// HTTP client of one origin that never routes through a proxy
type originClient struct {
	t      *testing.T
	origin string
	client *http.Client
}

func newOriginClient(t *testing.T, origin string) *originClient {
	return &originClient{t: t, origin: origin, client: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 30 * time.Second}}
}

// Status and body of one request, the header lines sent as given and a Host header naming the request host
func (c *originClient) call(method, path string, header http.Header, body string) (int, string) {
	c.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, c.origin+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	for name, values := range header {
		if name == "Host" {
			request.Host = values[0]
			continue
		}
		request.Header[name] = values
	}
	response, err := c.client.Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(raw)
}

// Fails unless the code is the wanted one, naming the request
func wantStatus(t *testing.T, label string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %d, want %d", label, got, want)
	}
}

// First IPv4 address of a real non loopback interface of this host, sorted as text
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, addr := range addrs {
		prefix, isNet := addr.(*net.IPNet)
		if isNet && prefix.IP.To4() != nil && !prefix.IP.IsLoopback() {
			found = append(found, prefix.IP.String())
		}
	}
	if len(found) == 0 {
		t.Fatal("real non-loopback interface required for this invariant")
	}
	slices.Sort(found)
	return found[0]
}

func TestNativeHTTPPreservesGatedAuthAndActualSocketBoundary(t *testing.T) {
	tmp := t.TempDir()
	h, user := filepath.Join(tmp, "home"), filepath.Join(tmp, "user")
	for _, dir := range []string{h, user} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("HOME", user)
	if err := dashboard.ImportWebServer(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("dashboard:\n  public_url: https://gateway.example.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	provider := newStubAuthProvider()
	if err := gateway.RegisterDashboardAuthProvider(ctx, provider, ""); err != nil {
		t.Fatal(err)
	}
	login, err := provider.StartLogin(ctx, "http://127.0.0.1/callback")
	if err != nil {
		t.Fatal(err)
	}
	pkce, err := url.ParseQuery(strings.ReplaceAll(login.CookiePayload["hermes_session_pkce"], ";", "&"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := provider.CompleteLogin(ctx, "stub_code", pkce.Get("state"), pkce.Get("verifier"), "http://127.0.0.1/callback")
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.ProcessOwnership.Reserve(ctx, []string{h}, ""); err != nil {
		t.Fatal(err)
	}
	runner, err := gatewayrun.NewGatewayRunner(ctx, gateway.DefaultGatewayConfig(ctx))
	if err != nil {
		t.Fatal(err)
	}
	runner.BindGatewayLoop(runningCaseLoop(t))
	var control *gateway.GatewayControlServer
	defer func() {
		if err := runner.Stop(ctx, false, false, false); err != nil {
			t.Error(err)
		}
		if control != nil {
			control.Stop()
		}
		gateway.ProcessOwnership.Close()
		gateway.ClearDashboardAuthProviders()
	}()
	if err := gatewayrun.InitializeGatewayRuntime(ctx, runner); err != nil {
		t.Fatal(err)
	}
	if control, err = gateway.NewGatewayControlServer("", nil); err != nil {
		t.Fatal(err)
	}
	runner.SetSessionControlServer(control)
	if !control.Start() {
		t.Fatal("the control server did not start")
	}
	api, err := gatewayrun.StartGatewayAPI(ctx, runner, "0.0.0.0", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner.SetSessionAPI(api)
	port := api.Socket().Addr().(*net.TCPAddr).Port
	mint := func() http.Header {
		ticket, err := runner.SessionTicketStore().Mint(h, "uid:fixture", "native-http", "", false)
		if err != nil {
			t.Fatal(err)
		}
		return http.Header{nativeTicketHeader: {ticket}}
	}
	with := func(base http.Header, extra map[string]string) http.Header {
		merged := http.Header{}
		for name, values := range base {
			merged[name] = values
		}
		for name, value := range extra {
			merged[name] = []string{value}
		}
		return merged
	}
	client := newOriginClient(t, fmt.Sprintf("http://127.0.0.1:%d", port))
	status, _ := client.call("GET", "/api/config", mint(), "")
	wantStatus(t, "grant before readiness", status, 503)
	if started, err := runner.Start(ctx); err != nil || !started {
		t.Fatalf("start = %v, %v", started, err)
	}
	if err := gatewayrun.PublishGatewayRuntimeReady(runner); err != nil {
		t.Fatal(err)
	}
	if web := api.App().Web(); web == nil || !web.AuthRequired() {
		t.Fatalf("auth_required of the mounted app is not true: %v", web)
	}
	bearer := map[string]string{"Authorization": "Bearer " + session.AccessToken}
	cookie := map[string]string{"Cookie": dashboard.SessionATCookie + "=" + session.AccessToken}
	for _, normal := range []map[string]string{bearer, cookie} {
		status, body := client.call("GET", "/api/config", with(nil, normal), "")
		wantStatus(t, "signed in "+body, status, 200)
		status, _ = client.call("GET", "/api/config", with(http.Header{nativeTicketHeader: {"bad"}}, normal), "")
		wantStatus(t, "signed in with a bad grant", status, 401)
	}
	status, _ = client.call("GET", "/api/config", http.Header{"Authorization": {"Bearer " + dashboard.SessionToken()}}, "")
	wantStatus(t, "session token behind the gate", status, 401)
	status, _ = client.call("GET", "/api/config", mint(), "")
	wantStatus(t, "grant", status, 200)
	status, _ = client.call("GET", "/api/config", with(mint(), map[string]string{"X-Forwarded-For": "203.0.113.9"}), "")
	wantStatus(t, "grant through a proxy header", status, 200)
	for _, origin := range []string{"", "https://gateway.example.test"} {
		extra := map[string]string{"Origin": origin}
		for name, value := range cookie {
			extra[name] = value
		}
		status, _ = client.call("GET", "/api/config", with(mint(), extra), "")
		wantStatus(t, "grant with origin "+origin, status, 401)
	}
	remote := newOriginClient(t, fmt.Sprintf("http://%s:%d", nonLoopbackIPv4(t), port))
	host := fmt.Sprintf("127.0.0.1:%d", port)
	status, _ = remote.call("GET", "/api/config", with(mint(), map[string]string{"X-Forwarded-For": "127.0.0.1", "Host": host}), "")
	wantStatus(t, "grant from a non loopback peer", status, 401)
	status, _ = remote.call("GET", "/api/config", with(nil, map[string]string{"Authorization": bearer["Authorization"], "Host": host}), "")
	wantStatus(t, "bearer from a non loopback peer", status, 200)
	runner.SetRuntimeDescriptorField("state", "draining")
	status, _ = client.call("GET", "/api/config", mint(), "")
	wantStatus(t, "grant while draining", status, 503)
	t.Log(`{"normal_oauth_bearer_cookie": "passed", "native_gated_auth_raw_peer_readiness": "passed"}`)
}
