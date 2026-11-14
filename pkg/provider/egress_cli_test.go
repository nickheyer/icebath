package provider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

// Egress CLI over a temp home with every provider key blanked, the backend bound to the real iron proxy
func newEgressTestCLI(t *testing.T, ns map[string]any) (*egressCLI, *bytes.Buffer) {
	t.Helper()
	h, p := ironHome(t)
	for _, name := range []string{"BWS_ACCESS_TOKEN", "ANTHROPIC_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	var out bytes.Buffer
	console := pyrich.NewConsole(&out)
	console.Width = 200
	base := map[string]any{"force": false, "tunnel_port": nil, "from_bitwarden": false, "no_bitwarden": false, "rotate_tokens": false, "restart": false, "no_restart": false, "show_tokens": false}
	for k, v := range ns {
		base[k] = v
	}
	return &egressCLI{ctx: context.Background(), home: h, console: console, ns: base, ip: newEgressBackend(p)}, &out
}

// Saves cfg as the home config
func saveEgressConfig(t *testing.T, h string, mutate func(cfg map[string]any)) {
	t.Helper()
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		t.Fatal(err)
	}
	mutate(cfg)
	if err := secretsSaveConfig(h, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestCmdInstallFailureReturns1(t *testing.T) {
	cli, _ := newEgressTestCLI(t, nil)
	cli.ip.installIronProxy = func(bool) (string, error) { return "", errors.New("download failed") }
	if rc, err := egressCmdInstall(cli); err != nil || rc != 1 {
		t.Fatalf("install = %d %v", rc, err)
	}
}

func TestCmdSetupFromBitwardenRefusesOnEmptyVault(t *testing.T) {
	cli, _ := newEgressTestCLI(t, map[string]any{"from_bitwarden": true})
	saveEgressConfig(t, cli.home, func(cfg map[string]any) {
		secretsSetdefaultMapping(cfg, "secrets")["bitwarden"] = map[string]any{"enabled": true, "project_id": "test-proj", "access_token_env": "BWS_ACCESS_TOKEN"}
	})
	t.Setenv("BWS_ACCESS_TOKEN", "bwsk-test-token")
	cli.ip.findIronProxy = func(bool) string { return filepath.Join(cli.home, "iron-proxy") }
	cli.ip.ironProxyVersion = func(string) string { return "test" }
	cli.ip.ensureCACert = func(bool) (string, string, error) {
		return filepath.Join(cli.home, "ca.crt"), filepath.Join(cli.home, "ca.key"), nil
	}
	cli.ip.fetchBitwardenSecrets = func(BitwardenFetchOptions) (map[string]string, []string, error) { return map[string]string{}, nil, nil }
	if rc, err := egressCmdSetup(cli); err != nil || rc != 1 {
		t.Fatalf("setup = %d %v", rc, err)
	}
}

func TestCmdStartPassesBitwardenRefreshFlagWhenCredentialSourceIsBitwarden(t *testing.T) {
	cli, _ := newEgressTestCLI(t, nil)
	saveEgressConfig(t, cli.home, func(cfg map[string]any) {
		proxy := secretsSetdefaultMapping(cfg, "proxy")
		proxy["enabled"], proxy["credential_source"] = true, "bitwarden"
		secretsSetdefaultMapping(cfg, "secrets")["bitwarden"] = map[string]any{"enabled": true, "project_id": "test-proj-id", "access_token_env": "BWS_ACCESS_TOKEN"}
	})
	t.Setenv("BWS_ACCESS_TOKEN", "bwsk-test-access-token")
	var captured *IronProxyStartOptions
	cli.ip.startProxy = func(opts IronProxyStartOptions) (IronProxyStatus, error) {
		captured = &opts
		return IronProxyStatus{PID: 4242, Listening: true, TunnelPort: 9090}, nil
	}
	cli.ip.discoverUncoveredProviders = func([]string) []string { return nil }
	if rc, err := egressCmdStart(cli); err != nil || rc != 0 {
		t.Fatalf("start = %d %v", rc, err)
	}
	if captured == nil || !captured.RefreshSecretsFromBitwarden || captured.BitwardenConfig == nil {
		t.Fatalf("start_proxy options %+v", captured)
	}
}

// Pid of a process that already exited, so a stale pidfile names a dead proxy
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestCmdDisableUsesPublicStatusPidNotPrivateReadPid(t *testing.T) {
	cli, out := newEgressTestCLI(t, nil)
	saveEgressConfig(t, cli.home, func(cfg map[string]any) {
		secretsSetdefaultMapping(cfg, "proxy")["enabled"] = true
	})
	state := ironStateDir(t, IronProxy{Home: cli.home})
	if err := os.WriteFile(filepath.Join(state, "iron-proxy.pid"), []byte(strconv.Itoa(deadPID(t))), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, err := egressCmdDisable(cli); err != nil || rc != 0 {
		t.Fatalf("disable = %d %v", rc, err)
	}
	if strings.Contains(out.String(), "still running") {
		t.Fatalf("a stale pidfile read as a running proxy: %s", out.String())
	}
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		t.Fatal(err)
	}
	if enabled := secretsMapping(cfg["proxy"])["enabled"]; enabled != false {
		t.Fatalf("proxy.enabled = %v", enabled)
	}
}

func TestCmdStartRefusesWhenBitwardenModeButDisabled(t *testing.T) {
	cli, _ := newEgressTestCLI(t, nil)
	saveEgressConfig(t, cli.home, func(cfg map[string]any) {
		proxy := secretsSetdefaultMapping(cfg, "proxy")
		proxy["enabled"], proxy["credential_source"] = true, "bitwarden"
		secretsSetdefaultMapping(cfg, "secrets")["bitwarden"] = map[string]any{"enabled": false}
	})
	cli.ip.startProxy = func(IronProxyStartOptions) (IronProxyStatus, error) {
		t.Fatal("start_proxy must not run when bitwarden mode is broken")
		return IronProxyStatus{}, nil
	}
	cli.ip.discoverUncoveredProviders = func([]string) []string { return nil }
	if rc, err := egressCmdStart(cli); err != nil || rc != 1 {
		t.Fatalf("start = %d %v", rc, err)
	}
}

func TestCmdSetupAuditLogFailureIsWarningNotAbort(t *testing.T) {
	cli, _ := newEgressTestCLI(t, nil)
	cli.ip.findIronProxy = func(bool) string { return filepath.Join(cli.home, "iron-proxy") }
	cli.ip.discoverProviderMappings = func([]string) []TokenMapping {
		return []TokenMapping{{ProxyToken: "icebath-proxy-deadbeef", RealEnvName: "OPENROUTER_API_KEY", UpstreamHosts: []string{"openrouter.ai"}, MatchHeaders: []string{"Authorization"}}}
	}
	cli.ip.discoverUncoveredProviders = func([]string) []string { return nil }
	cli.ip.ensureAuditLog = func(string) error { return errors.New("could not pre-create audit log (synthetic)") }
	if rc, err := egressCmdSetup(cli); err != nil || rc != 0 {
		t.Fatalf("setup = %d %v", rc, err)
	}
}
