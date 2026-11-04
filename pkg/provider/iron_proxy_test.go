package provider

import (
	"encoding/json"
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

func TestBuildProxyConfigCustomAllowedHostsAndNoAuditPath(t *testing.T) {
	_, p := ironHome(t)
	cfg := p.BuildProxyConfig(IronProxyConfigOptions{Mappings: []TokenMapping{sampleMapping("OPENAI_API_KEY")}, CACert: "/ca.crt", CAKey: "/ca.key", AllowedHosts: []string{"custom-host.test"}, AuditLog: "/audit.log", HTTPListen: []string{"127.0.0.1:9090"}})
	domains := cfg["transforms"].([]any)[0].(map[string]any)["config"].(map[string]any)["domains"].([]any)
	joined := make([]string, 0, len(domains))
	for _, d := range domains {
		joined = append(joined, d.(string))
	}
	if !containsString(joined, "custom-host.test") || !containsString(joined, "openrouter.ai") {
		t.Fatalf("custom hosts plus the mapping hosts expected: %v", joined)
	}
	if _, has := cfg["log"].(map[string]any)["audit_path"]; has {
		t.Fatal("v0.39 rejects log.audit_path")
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
	if err := os.WriteFile(filepath.Join(p.stateDir(), "mappings.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(p.LoadMappings()) != 0 {
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
	state := p.stateDir()
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

func TestCAKeyCreatedWith0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits")
	}
	_, p := ironHome(t)
	fakeBinary(t, "openssl", `if [ "$1" = genrsa ]; then
  out=""; for a in "$@"; do if [ "$prev" = "-out" ]; then out="$a"; fi; prev="$a"; done
  printf -- '-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----\n' > "$out"
else
  out=""; for a in "$@"; do if [ "$prev" = "-out" ]; then out="$a"; fi; prev="$a"; done
  printf -- '-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n' > "$out"
fi`)
	crt, key, err := p.EnsureCACert(false)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(key)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("CA key has perms %o", info.Mode().Perm())
	}
	if crtInfo, _ := os.Stat(crt); crtInfo.Mode().Perm() != 0o644 {
		t.Fatal("the cert is public")
	}
	if dirInfo, _ := os.Stat(p.stateDir()); dirInfo.Mode().Perm() != 0o700 {
		t.Fatal("the state dir is private")
	}
}

func TestEnsureAuditLogCreatesAndTightens(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits")
	}
	audit := filepath.Join(t.TempDir(), "audit.log")
	if err := EnsureAuditLog(audit); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(audit); info.Mode().Perm() != 0o600 {
		t.Fatal("created 0600")
	}
	_ = os.WriteFile(audit, []byte("preexisting content\n"), 0o644)
	_ = os.Chmod(audit, 0o644)
	if err := EnsureAuditLog(audit); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(audit); info.Mode().Perm() != 0o600 {
		t.Fatal("existing perms tightened")
	}
}

func TestMappingsRoundtripPreservesHeadersAndAliases(t *testing.T) {
	_, p := ironHome(t)
	m := TokenMapping{ProxyToken: MintProxyToken("gemini"), RealEnvName: "GEMINI_API_KEY", UpstreamHosts: []string{"generativelanguage.googleapis.com"}, MatchHeaders: []string{"x-goog-api-key"}, AliasEnvNames: []string{"GOOGLE_API_KEY"}}
	if _, err := p.WriteMappings([]TokenMapping{m}); err != nil {
		t.Fatal(err)
	}
	loaded := p.LoadMappings()
	if len(loaded) != 1 || loaded[0].MatchHeaders[0] != "x-goog-api-key" || loaded[0].AliasEnvNames[0] != "GOOGLE_API_KEY" || loaded[0].ProxyToken != m.ProxyToken {
		t.Fatalf("got %+v", loaded)
	}
	if info, _ := os.Stat(filepath.Join(p.stateDir(), "mappings.json")); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
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
	state := p.stateDir()
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

func TestStartProxyInjectsManagementKeyEnv(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	h, p := ironHome(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	cfg := p.BuildProxyConfig(IronProxyConfigOptions{Mappings: []TokenMapping{sampleMapping("OPENROUTER_API_KEY")}, CACert: filepath.Join(h, "ca.crt"), CAKey: filepath.Join(h, "ca.key"), TunnelPort: port, HTTPListen: []string{"127.0.0.1:" + strconv.Itoa(port)}})
	cfgPath, err := p.WriteProxyConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(cfgPath); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal("proxy.yaml holds tokens and is private")
	}
	binDir := filepath.Join(h, "bin")
	_ = os.MkdirAll(binDir, 0o700)
	fakeBin := filepath.Join(binDir, "iron-proxy")
	_ = os.WriteFile(fakeBin, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755)
	status, err := p.StartProxy(IronProxyStartOptions{Binary: fakeBin, ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.StopProxy() })
	if status.PID == 0 || !status.Listening {
		t.Fatalf("started status expected: %+v", status)
	}
	environ, err := os.ReadFile("/proc/" + strconv.Itoa(status.PID) + "/environ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(environ), IronProxyMgmtKeyEnv+"=icebath-mgmt-") || !strings.Contains(string(environ), IronProxyNonceEnv+"=") {
		t.Fatal("the management key and nonce must reach the daemon env")
	}
	if p.readPersistedNonce() == "" || !p.pidAlive(status.PID) {
		t.Fatal("the nonce is persisted beside the pidfile and proves the pid")
	}
	if !p.StopProxy() {
		t.Fatal("stop reports it was running")
	}
	if p.readPID() != 0 || p.GetStatus().PID != 0 {
		t.Fatal("stop forgets the daemon")
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
	state := p.stateDir()
	_ = os.WriteFile(filepath.Join(state, "proxy.yaml"), []byte("proxy:\n  http_listen: 127.0.0.1:"+strconv.Itoa(port)+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(state, "ca.crt"), []byte("cert"), 0o600)
	_ = os.WriteFile(filepath.Join(state, "iron-proxy.pid"), []byte(strconv.Itoa(pid)), 0o600)
	status := p.GetStatus()
	if status.TunnelPort != port || !status.Listening || status.PID != pid || status.CACertPath == "" || status.ConfigPath == "" || !status.Configured() {
		t.Fatalf("got %+v", status)
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

func TestPidAliveReadsAProxyThatExitedAfterTheLivenessProbeAsDead(t *testing.T) {
	_, p := ironHome(t)
	pid := fakeIronProcess(t)
	if !p.pidAlive(pid) {
		t.Fatal("a live proxy is known by argv0")
	}
	proc, _ := os.FindProcess(pid)
	_ = proc.Kill()
	_, _ = proc.Wait()
	if p.pidAlive(pid) {
		t.Fatal("an exited proxy reads as dead")
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
