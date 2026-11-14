package provider

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// E2E gate mirroring HERMES_RUN_E2E, the real binary is downloaded and curl and openssl must be present
func ironE2EHome(t *testing.T) IronProxy {
	t.Helper()
	if os.Getenv("ICEBATH_RUN_E2E") != "1" {
		t.Skip("E2E proxy test — set ICEBATH_RUN_E2E=1 to run (requires network + curl + openssl)")
	}
	for _, tool := range []string{"curl", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	ResetIronProxyForTests()
	t.Cleanup(ResetIronProxyForTests)
	return IronProxy{Home: newTestHome(t)}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// Upstream recording one request header of every incoming request
type captureUpstream struct {
	mu       sync.Mutex
	header   string
	captured *string
	server   *httptest.Server
}

func startCaptureUpstream(t *testing.T, header string) *captureUpstream {
	t.Helper()
	c := &captureUpstream{header: header}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := r.Header.Get(c.header)
		c.mu.Lock()
		c.captured = &value
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok": true}`))
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *captureUpstream) port() int {
	return c.server.Listener.Addr().(*net.TCPAddr).Port
}

func (c *captureUpstream) value() *string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captured
}

func (c *captureUpstream) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = nil
}

// Installs the binary and the CA, writes the config for one mapping and starts the daemon
func ironE2EStart(t *testing.T, p IronProxy, mapping TokenMapping, tunnelPort int) IronProxyStatus {
	t.Helper()
	binary, err := p.InstallIronProxy()
	if err != nil || !fileExists(binary) {
		t.Fatalf("install %q %v", binary, err)
	}
	caCert, caKey, err := p.EnsureCACert(false)
	if err != nil || !fileExists(caCert) {
		t.Fatalf("ca %q %v", caCert, err)
	}
	ironE2EWriteConfig(t, p, mapping, caCert, caKey, tunnelPort)
	status, err := p.StartProxy(IronProxyStartOptions{})
	if err != nil {
		t.Skipf("iron-proxy could not start in this environment: %v", err)
	}
	if status.PID == 0 {
		t.Fatal("start reported no pid")
	}
	t.Cleanup(func() { p.StopProxy() })
	for range 50 {
		if portListening("127.0.0.1", tunnelPort) {
			return status
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("iron-proxy never started listening on the tunnel port")
	return status
}

// Writes proxy.yaml and mappings.json for one mapping pinned to loopback with the deny list cleared
func ironE2EWriteConfig(t *testing.T, p IronProxy, mapping TokenMapping, caCert, caKey string, tunnelPort int) {
	t.Helper()
	cfg := p.BuildProxyConfig(IronProxyConfigOptions{Mappings: []TokenMapping{mapping}, CACert: caCert, CAKey: caKey, TunnelPort: tunnelPort,
		AllowedHosts: []string{"127.0.0.1"}, DenyCIDRsSet: true, HTTPListen: []string{"127.0.0.1:" + strconv.Itoa(tunnelPort)}})
	if _, err := p.WriteProxyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteMappings([]TokenMapping{mapping}); err != nil {
		t.Fatal(err)
	}
}

// Sends one request through the plain forward listener with a header carrying the proxy token
func curlThroughProxy(t *testing.T, tunnelPort, upstreamPort int, header string) {
	t.Helper()
	out, err := exec.Command("curl", "--silent", "--max-time", "10",
		"-x", "http://127.0.0.1:"+strconv.Itoa(tunnelPort+1),
		"-H", header,
		"http://127.0.0.1:"+strconv.Itoa(upstreamPort)+"/").CombinedOutput()
	if err != nil {
		t.Fatalf("curl failed: %v %s", err, out)
	}
}

func TestIronProxySwapsAuthorizationHeaderEndToEnd(t *testing.T) {
	p := ironE2EHome(t)
	upstream := startCaptureUpstream(t, "Authorization")
	realSecret := "sk-real-upstream-value-deadbeef"
	t.Setenv("TEST_UPSTREAM_KEY", realSecret)
	proxyToken := MintProxyToken("test")
	tunnelPort := freeLoopbackPort(t)
	ironE2EStart(t, p, TokenMapping{ProxyToken: proxyToken, RealEnvName: "TEST_UPSTREAM_KEY", UpstreamHosts: []string{"127.0.0.1"}, MatchHeaders: []string{"Authorization"}}, tunnelPort)
	curlThroughProxy(t, tunnelPort, upstream.port(), "Authorization: Bearer "+proxyToken)
	captured := upstream.value()
	if captured == nil {
		t.Fatal("upstream never received the request")
	}
	if !contains(*captured, realSecret) {
		t.Fatalf("Authorization header was not swapped — upstream saw: %q", *captured)
	}
	if contains(*captured, proxyToken) {
		t.Fatalf("Proxy token leaked through to upstream: %q", *captured)
	}
}

func TestIronProxySwapsXApiKeyHeaderEndToEnd(t *testing.T) {
	p := ironE2EHome(t)
	upstream := startCaptureUpstream(t, "x-api-key")
	realSecret := "sk-ant-real-value-cafebabe"
	t.Setenv("TEST_XAPI_KEY", realSecret)
	proxyToken := MintProxyToken("anthropic")
	tunnelPort := freeLoopbackPort(t)
	ironE2EStart(t, p, TokenMapping{ProxyToken: proxyToken, RealEnvName: "TEST_XAPI_KEY", UpstreamHosts: []string{"127.0.0.1"}, MatchHeaders: []string{"x-api-key", "Authorization"}}, tunnelPort)
	curlThroughProxy(t, tunnelPort, upstream.port(), "x-api-key: "+proxyToken)
	captured := upstream.value()
	if captured == nil {
		t.Fatal("upstream never received the request")
	}
	if !contains(*captured, realSecret) {
		t.Fatalf("x-api-key header was not swapped — upstream saw: %q", *captured)
	}
	if contains(*captured, proxyToken) {
		t.Fatalf("Proxy token leaked through to upstream: %q", *captured)
	}
}

func TestIronProxyManagementReloadEndToEnd(t *testing.T) {
	p := ironE2EHome(t)
	upstream := startCaptureUpstream(t, "Authorization")
	realSecret := "sk-real-reload-value-0badf00d"
	t.Setenv("TEST_RELOAD_KEY", realSecret)
	tokenV1 := MintProxyToken("v1")
	tunnelPort := freeLoopbackPort(t)
	status := ironE2EStart(t, p, TokenMapping{ProxyToken: tokenV1, RealEnvName: "TEST_RELOAD_KEY", UpstreamHosts: []string{"127.0.0.1"}, MatchHeaders: []string{"Authorization"}}, tunnelPort)
	pidBefore := status.PID
	tokenV2 := MintProxyToken("v2")
	state := ironStateDir(t, p)
	ironE2EWriteConfig(t, p, TokenMapping{ProxyToken: tokenV2, RealEnvName: "TEST_RELOAD_KEY", UpstreamHosts: []string{"127.0.0.1"}, MatchHeaders: []string{"Authorization"}},
		filepath.Join(state, "ca.crt"), filepath.Join(state, "ca.key"), tunnelPort)
	if err := p.ReloadProxy(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if p.GetStatus().PID != pidBefore {
		t.Fatal("the reload restarted the daemon")
	}
	upstream.reset()
	curlThroughProxy(t, tunnelPort, upstream.port(), "Authorization: Bearer "+tokenV2)
	captured := upstream.value()
	if captured == nil || !contains(*captured, realSecret) {
		t.Fatalf("post-reload token was not swapped — upstream saw: %v", captured)
	}
}
