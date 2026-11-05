//go:build linux

package provider

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCAKeyCreatedWith0600(t *testing.T) {
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

func TestStartProxyInjectsManagementKeyEnv(t *testing.T) {
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
	if info, _ := os.Stat(cfgPath); info.Mode().Perm() != 0o600 {
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
