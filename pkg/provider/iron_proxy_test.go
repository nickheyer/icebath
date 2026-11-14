package provider

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func ironHome(t *testing.T) (string, IronProxy) {
	t.Helper()
	ResetIronProxyForTests()
	t.Cleanup(ResetIronProxyForTests)
	h := newTestHome(t)
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasSuffix(k, "_API_KEY") {
			t.Setenv(k, "")
		}
	}
	t.Setenv("PATH", t.TempDir())
	return h, IronProxy{Home: h}
}

// State dir of the proxy home, failing the test when it cannot be created
func ironStateDir(t *testing.T, p IronProxy) string {
	t.Helper()
	state, err := p.stateDir()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func sampleMapping(envName string) TokenMapping {
	return TokenMapping{ProxyToken: MintProxyToken("test"), RealEnvName: envName, UpstreamHosts: []string{"openrouter.ai", "*.openrouter.ai"}, MatchHeaders: []string{"Authorization"}}
}

func TestMintProxyTokenHasPrefixAndLength(t *testing.T) {
	token := MintProxyToken("alpha")
	if !strings.HasPrefix(token, "alpha-") || len(token) < len("alpha-")+32 {
		t.Fatalf("got %q", token)
	}
}

func TestManagementTokenPathIsSingleAuthority(t *testing.T) {
	h, p := ironHome(t)
	if p.managementTokenPath() != filepath.Join(p.stateDirRO(), "management.token") || fileExists(filepath.Join(h, "proxy")) {
		t.Fatal("the path never creates state")
	}
	token, err := p.EnsureManagementToken(false)
	if err != nil || !strings.HasPrefix(token, "icebath-mgmt-") || readTextOrEmpty(p.managementTokenPath()) != token {
		t.Fatalf("got %q %v", token, err)
	}
	if again, _ := p.EnsureManagementToken(false); again != token {
		t.Fatal("reuse without minting")
	}
	rotated, _ := p.EnsureManagementToken(true)
	if rotated == token || readTextOrEmpty(p.managementTokenPath()) != rotated {
		t.Fatal("forced rotation mints at the same path")
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(p.managementTokenPath()); info.Mode().Perm() != 0o600 {
			t.Fatalf("management token must be private, got %o", info.Mode().Perm())
		}
	}
}

func TestBuildProxyConfigCustomAllowedHosts(t *testing.T) {
	_, p := ironHome(t)
	cfg := p.BuildProxyConfig(IronProxyConfigOptions{Mappings: []TokenMapping{sampleMapping("OPENAI_API_KEY")}, CACert: "/ca.crt", CAKey: "/ca.key", AllowedHosts: []string{"custom-host.test"}, HTTPListen: []string{"127.0.0.1:9090"}})
	domains := cfg["transforms"].([]any)[0].(map[string]any)["config"].(map[string]any)["domains"].([]any)
	joined := make([]string, 0, len(domains))
	for _, d := range domains {
		joined = append(joined, d.(string))
	}
	if !containsString(joined, "custom-host.test") || !containsString(joined, "openrouter.ai") {
		t.Fatalf("custom hosts plus the mapping hosts expected: %v", joined)
	}
	proxy := cfg["proxy"].(map[string]any)
	if proxy["tunnel_listen"] != "127.0.0.1:9090" || proxy["http_listen"] != "127.0.0.1:9091" || cfg["management"].(map[string]any)["listen"] != "127.0.0.1:9092" {
		t.Fatalf("listeners wrong: %v", cfg)
	}
	deny := proxy["upstream_deny_cidrs"].([]any)
	if len(deny) != len(DefaultIronUpstreamDenyCIDRs) {
		t.Fatal("default SSRF deny list applies")
	}
	empty := p.BuildProxyConfig(IronProxyConfigOptions{CACert: "/ca.crt", CAKey: "/ca.key", DenyCIDRsSet: true, HTTPListen: []string{"127.0.0.1:9090"}})
	if len(empty["proxy"].(map[string]any)["upstream_deny_cidrs"].([]any)) != 0 {
		t.Fatal("an explicit empty list opts out")
	}
}

func TestLoadMappingsHandlesCorruptJSON(t *testing.T) {
	_, p := ironHome(t)
	if err := os.WriteFile(filepath.Join(ironStateDir(t, p), "mappings.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := p.LoadMappings(); err != nil || len(loaded) != 0 {
		t.Fatal("corrupt mappings load as empty")
	}
}

func TestVerifyChecksumsSignatureSkipsWithoutGPG(t *testing.T) {
	_, p := ironHome(t)
	dir := t.TempDir()
	cks := filepath.Join(dir, "checksums.txt")
	_ = os.WriteFile(cks, []byte("abc  iron-proxy.tar.gz\n"), 0o600)
	if ok, err := p.VerifyChecksumsSignature(dir, cks); ok || err != nil {
		t.Fatalf("no gpg degrades to false: %t %v", ok, err)
	}
}

// Spawns a process whose argv0 reads as iron-proxy so the pid probes treat it as ours
func fakeIronProcess(t *testing.T) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("argv0 probing needs /proc")
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	cmd := exec.Command("bash", "-c", "exec -a iron-proxy sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/cmdline"); err == nil && strings.HasPrefix(string(raw), "iron-proxy") {
			return cmd.Process.Pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the fake proxy never renamed itself")
	return 0
}

func TestStartProxyIdempotentWhenAlreadyRunning(t *testing.T) {
	_, p := ironHome(t)
	pid := fakeIronProcess(t)
	state := ironStateDir(t, p)
	_ = os.WriteFile(filepath.Join(state, "iron-proxy.pid"), []byte(strconv.Itoa(pid)), 0o600)
	_ = os.WriteFile(filepath.Join(state, "proxy.yaml"), []byte("proxy: {}\n"), 0o600)
	status, err := p.StartProxy(IronProxyStartOptions{Binary: "/nonexistent/iron-proxy"})
	if err != nil || status.PID != pid {
		t.Fatalf("an already running proxy must not be spawned again: %+v %v", status, err)
	}
}

func TestSubprocessEnvStripsUnrelatedSecrets(t *testing.T) {
	_, p := ironHome(t)
	t.Setenv("MY_PRIVATE_TOKEN", "should-not-leak")
	t.Setenv("DATABASE_URL", "postgres://very-private")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-very-secret")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-real")
	t.Setenv("HTTPS_PROXY", "http://corp:3128")
	if _, err := p.WriteMappings([]TokenMapping{sampleMapping("OPENROUTER_API_KEY")}); err != nil {
		t.Fatal(err)
	}
	env, err := p.buildProxySubprocessEnv(nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"MY_PRIVATE_TOKEN", "DATABASE_URL", "SLACK_BOT_TOKEN", "HTTPS_PROXY"} {
		if _, has := env[k]; has {
			t.Fatalf("%s must not propagate", k)
		}
	}
	if env["OPENROUTER_API_KEY"] != "sk-or-real" || env["NO_COLOR"] != "1" {
		t.Fatalf("mapped secrets propagate: %v", env)
	}
}

func TestMappingsRoundtripPreservesHeadersAndAliases(t *testing.T) {
	_, p := ironHome(t)
	m := TokenMapping{ProxyToken: MintProxyToken("gemini"), RealEnvName: "GEMINI_API_KEY", UpstreamHosts: []string{"generativelanguage.googleapis.com"}, MatchHeaders: []string{"x-goog-api-key"}, AliasEnvNames: []string{"GOOGLE_API_KEY"}}
	if _, err := p.WriteMappings([]TokenMapping{m}); err != nil {
		t.Fatal(err)
	}
	loaded, err := p.LoadMappings()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].MatchHeaders[0] != "x-goog-api-key" || loaded[0].AliasEnvNames[0] != "GOOGLE_API_KEY" || loaded[0].ProxyToken != m.ProxyToken {
		t.Fatalf("got %+v", loaded)
	}
	if info, _ := os.Stat(filepath.Join(ironStateDir(t, p), "mappings.json")); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal("mappings are private")
	}
}

func TestDiscoverAndMergeMappings(t *testing.T) {
	discovered := DiscoverProviderMappings([]string{"OPENROUTER_API_KEY", "GOOGLE_API_KEY", "AWS_ACCESS_KEY_ID"})
	names := map[string]TokenMapping{}
	for _, m := range discovered {
		names[m.RealEnvName] = m
	}
	if len(names) != 2 || names["GEMINI_API_KEY"].AliasEnvNames[0] != "GOOGLE_API_KEY" || !strings.HasPrefix(names["openrouter"+""].ProxyToken+names["OPENROUTER_API_KEY"].ProxyToken, "openrouter-") {
		t.Fatalf("got %+v", discovered)
	}
	if uncovered := DiscoverUncoveredProviders([]string{"AWS_ACCESS_KEY_ID", "OPENAI_API_KEY"}); len(uncovered) != 1 || uncovered[0] != "AWS_ACCESS_KEY_ID" {
		t.Fatalf("got %v", uncovered)
	}
	existing := []TokenMapping{{ProxyToken: "keep-me", RealEnvName: "OPENROUTER_API_KEY", UpstreamHosts: []string{"old.host"}}}
	merged := MergeMappings(existing, discovered, false)
	for _, m := range merged {
		if m.RealEnvName == "OPENROUTER_API_KEY" && (m.ProxyToken != "keep-me" || m.UpstreamHosts[0] != "openrouter.ai") {
			t.Fatalf("existing tokens survive and hosts refresh: %+v", m)
		}
	}
	for _, m := range MergeMappings(existing, discovered, true) {
		if m.ProxyToken == "keep-me" {
			t.Fatal("rotate re-mints")
		}
	}
}

func TestReloadProxyRefusesWhenNotRunning(t *testing.T) {
	_, p := ironHome(t)
	if err := p.ReloadProxy(); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("got %v", err)
	}
}

func TestReloadProxyPostsBearerToManagementEndpoint(t *testing.T) {
	_, p := ironHome(t)
	pid := fakeIronProcess(t)
	captured := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured["path"], captured["method"], captured["auth"] = r.URL.Path, r.Method, r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	t.Cleanup(server.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	state := ironStateDir(t, p)
	_ = os.WriteFile(filepath.Join(state, "iron-proxy.pid"), []byte(strconv.Itoa(pid)), 0o600)
	_ = os.WriteFile(filepath.Join(state, "proxy.yaml"), []byte("management:\n  listen: "+host+":"+port+"\n"), 0o600)
	token, _ := p.EnsureManagementToken(false)
	if err := p.ReloadProxy(); err != nil {
		t.Fatal(err)
	}
	if captured["path"] != "/v1/reload" || captured["method"] != "POST" || captured["auth"] != "Bearer "+token {
		t.Fatalf("got %v", captured)
	}
}

func TestPersistedNonceRoundtrip(t *testing.T) {
	_, p := ironHome(t)
	path := p.persistedNoncePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte("test-nonce-abc123\n"), 0o600)
	if p.readPersistedNonce() != "test-nonce-abc123" {
		t.Fatalf("got %q", p.readPersistedNonce())
	}
}

func TestGetStatusProbesConfiguredBindHost(t *testing.T) {
	_, p := ironHome(t)
	pid := fakeIronProcess(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	state := ironStateDir(t, p)
	_ = os.WriteFile(filepath.Join(state, "proxy.yaml"), []byte("proxy:\n  http_listen: 127.0.0.1:"+strconv.Itoa(port)+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(state, "ca.crt"), []byte("cert"), 0o600)
	_ = os.WriteFile(filepath.Join(state, "iron-proxy.pid"), []byte(strconv.Itoa(pid)), 0o600)
	status := p.GetStatus()
	if status.TunnelPort != port || !status.Listening || status.PID != pid || status.CACertPath == "" || status.ConfigPath == "" || !status.Configured() {
		t.Fatalf("got %+v", status)
	}
}

func TestAuditLogKwargDoesNotInjectAuditPathV039(t *testing.T) {
	_, p := ironHome(t)
	cfg := p.BuildProxyConfig(IronProxyConfigOptions{Mappings: []TokenMapping{sampleMapping("OPENROUTER_API_KEY")}, CACert: "/ca.crt", CAKey: "/ca.key", AuditLog: "/audit.log", HTTPListen: []string{"127.0.0.1:9090"}})
	if _, has := cfg["log"].(map[string]any)["audit_path"]; has {
		t.Fatal("iron-proxy v0.39 has no log.audit_path field; emitting it breaks startup")
	}
}

func TestPartialBitwardenSecretsRaiseWithoutFallback(t *testing.T) {
	_, p := ironHome(t)
	if _, err := p.WriteMappings([]TokenMapping{sampleMapping("OPENROUTER_API_KEY")}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-host")
	t.Setenv("BWS_ACCESS_TOKEN", "tok")
	previous := ironBitwardenModule
	ironBitwardenModule = func() (func(BitwardenFetchOptions) (map[string]string, []string, error), error) {
		return func(BitwardenFetchOptions) (map[string]string, []string, error) { return map[string]string{}, nil, nil }, nil
	}
	t.Cleanup(func() { ironBitwardenModule = previous })
	_, err := p.buildProxySubprocessEnv(nil, true, map[string]any{"project_id": "proj", "access_token_env": "BWS_ACCESS_TOKEN"})
	if err == nil || !strings.Contains(err.Error(), "did not return secrets") {
		t.Fatalf("strict default raises: %v", err)
	}
}

func TestBitwardenImporterrorRaiseWithoutFallback(t *testing.T) {
	_, p := ironHome(t)
	if _, err := p.WriteMappings([]TokenMapping{sampleMapping("OPENROUTER_API_KEY")}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-host")
	t.Setenv("BWS_ACCESS_TOKEN", "tok")
	previous := ironBitwardenModule
	ironBitwardenModule = func() (func(BitwardenFetchOptions) (map[string]string, []string, error), error) {
		return nil, errors.New("bws SDK not installed")
	}
	t.Cleanup(func() { ironBitwardenModule = previous })
	_, err := p.buildProxySubprocessEnv(nil, true, map[string]any{"project_id": "proj", "access_token_env": "BWS_ACCESS_TOKEN"})
	if err == nil || !strings.Contains(err.Error(), "Bitwarden refresh module unavailable") {
		t.Fatalf("a missing refresh module raises without fallback: %v", err)
	}
}

func TestPartialBitwardenSecretsHonorAllowEnvFallback(t *testing.T) {
	_, p := ironHome(t)
	if _, err := p.WriteMappings([]TokenMapping{sampleMapping("OPENROUTER_API_KEY")}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-host-fallback")
	t.Setenv("BWS_ACCESS_TOKEN", "tok")
	fakeBinary(t, "bws", `printf '[]'`)
	env, err := p.buildProxySubprocessEnv(nil, true, map[string]any{"project_id": "proj", "access_token_env": "BWS_ACCESS_TOKEN", "allow_env_fallback": true})
	if err != nil || env["OPENROUTER_API_KEY"] != "sk-host-fallback" {
		t.Fatalf("fallback expected: %v %v", env, err)
	}
	_, err = p.buildProxySubprocessEnv(nil, true, map[string]any{"project_id": "proj", "access_token_env": "BWS_ACCESS_TOKEN"})
	if err == nil || !strings.Contains(err.Error(), "did not return secrets") {
		t.Fatalf("strict default raises: %v", err)
	}
	_, err = p.buildProxySubprocessEnv(nil, true, map[string]any{"project_id": "", "access_token_env": "BWS_ACCESS_TOKEN"})
	if err == nil || !strings.Contains(err.Error(), "project_id is empty") {
		t.Fatalf("empty config raises: %v", err)
	}
}

func TestProcStartTimeParsesCommWithParens(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs")
	}
	if pidProcStartTime(os.Getpid()) == "" {
		t.Fatal("our own start time parses")
	}
	if pidProcStartTime(99999999) != "" {
		t.Fatal("missing pid gives nothing")
	}
}

func TestProxyConfigRoundTripsThroughYAML(t *testing.T) {
	h, p := ironHome(t)
	cfg := p.BuildProxyConfig(IronProxyConfigOptions{Mappings: []TokenMapping{sampleMapping("OPENROUTER_API_KEY")}, CACert: filepath.Join(h, "ca.crt"), CAKey: filepath.Join(h, "ca.key"), HTTPListen: []string{"127.0.0.1:9090"}})
	if _, err := p.WriteProxyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	host, port, ok := p.readManagementListen("")
	if !ok || host != "127.0.0.1" || port != 9092 {
		t.Fatalf("management listen must round trip: %s %d %t", host, port, ok)
	}
	raw, _ := json.Marshal(cfg)
	if !strings.Contains(string(raw), "\"require\":true") {
		t.Fatal("secret rules fail closed")
	}
}

func TestUncreatableStateDirFailsEveryWriterAndStop(t *testing.T) {
	h, p := ironHome(t)
	if err := os.WriteFile(filepath.Join(h, "proxy"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.EnsureCACert(false); err == nil {
		t.Fatal("the CA cert needs the state dir")
	}
	if _, err := p.EnsureManagementToken(false); err == nil {
		t.Fatal("the management token needs the state dir")
	}
	if _, err := p.WriteProxyConfig(map[string]any{"proxy": map[string]any{}}); err == nil {
		t.Fatal("proxy.yaml needs the state dir")
	}
	if _, err := p.WriteMappings([]TokenMapping{sampleMapping("OPENROUTER_API_KEY")}); err == nil {
		t.Fatal("mappings.json needs the state dir")
	}
	if _, err := p.LoadMappings(); err == nil {
		t.Fatal("loading mappings creates the state dir first")
	}
	if _, err := p.StartProxy(IronProxyStartOptions{Binary: "/nonexistent/iron-proxy"}); err == nil {
		t.Fatal("start needs the state dir for its config")
	}
	if stopped, err := p.StopProxy(); err == nil || stopped {
		t.Fatal("stop cannot forget the daemon without the state dir")
	}
	if raw, _ := os.ReadFile(filepath.Join(h, "proxy")); string(raw) != "not a directory" {
		t.Fatal("the blocking file is left alone")
	}
}
