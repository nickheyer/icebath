//go:build !windows

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

type execFix struct {
	env      *tools.Env
	dir      string
	keys     []string
	attached [][2]string
}

// Isolated exec state with the session layer, supervisor, provider lookup and CLI discovery replaced
func newExecFix(t *testing.T, section map[string]any) *execFix {
	t.Helper()
	isolateSessions(t)
	env := testEnv(t)
	if section == nil {
		section = map[string]any{}
	}
	env.Config = config.Tree{"browser": section}
	for _, k := range []string{"BU_NAME", "BU_AUTOSPAWN", "BROWSER_USE_API_KEY", "BH_AGENT_WORKSPACE", "BROWSER_CDP_URL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	resetCaches("")
	t.Cleanup(func() { resetCaches("") })
	f := &execFix{env: env, dir: t.TempDir()}
	pin(t, &execSessionInfo, func(_ context.Context, _ *tools.Env, key string) (map[string]any, error) {
		f.keys = append(f.keys, key)
		return map[string]any{"cdp_url": "ws://127.0.0.1:47000/devtools/browser/" + key, "features": map[string]any{"local": true}}, nil
	})
	pin(t, &execAttachSupervisor, func(_ *tools.Env, task, cdp, _ string, _ float64) error {
		f.attached = append(f.attached, [2]string{task, cdp})
		return nil
	})
	pin(t, &execCloudProvider, func(*tools.Env) (BrowserProvider, error) { return nil, nil })
	pin(t, &execCDPOverride, func(context.Context, *tools.Env) string { return "" })
	pin(t, &harnessSiteDir, func() string { return "" })
	pin(t, &findCLI, func(*tools.Env) []string { return nil })
	pin(t, &execNativeVision, func(context.Context, *tools.Env) bool { return false })
	pin(t, &execRouteIsSidecar, map[string]bool{})
	pin(t, &realProfileFallbackNotes, map[string]string{})
	pin(t, &realProfileNoteDelivered, map[string]bool{})
	pin(t, &drivenDaemons, map[string]drivenDaemon{})
	pin(t, &ownedDaemonGroups, map[string]int{})
	return f
}

// Executable fake browser-use CLI the exec runs instead of the harness
func (f *execFix) cli(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(f.dir, "browser-use")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	pin(t, &findCLI, func(*tools.Env) []string { return []string{path} })
	return path
}

func (f *execFix) raw(t *testing.T, code, session any, timeout any, task string, local bool) string {
	t.Helper()
	out, err := browserExec(context.Background(), f.env, code, session, timeout, task, local, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *execFix) run(t *testing.T, code, session string, task string) map[string]any {
	t.Helper()
	return decodeExecResult(t, f.raw(t, code, session, execDefaultTimeoutS, task, false))
}

func decodeExecResult(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	return out
}

func resolveFor(f *execFix, penv map[string]string, task, name string) string {
	return resolveBackendCDP(context.Background(), f.env, penv, task, name, false)
}

func providerOf(name string) func(*tools.Env) (BrowserProvider, error) {
	p := newFakeCloud(name, true)
	return func(*tools.Env) (BrowserProvider, error) { return p, nil }
}

func cdpOverrideOf(url string) func(context.Context, *tools.Env) string {
	return func(context.Context, *tools.Env) string { return url }
}

func sessionInfoOf(info map[string]any, err error, seen *[]string) func(context.Context, *tools.Env, string) (map[string]any, error) {
	return func(_ context.Context, _ *tools.Env, key string) (map[string]any, error) {
		if seen != nil {
			*seen = append(*seen, key)
		}
		return info, err
	}
}

func TestSubprocessEnvironmentBrowserUseTelemetryDefaultsOff(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execBrowserEnv, func(*tools.Env) map[string]string { return map[string]string{} })
	if got := baseSubprocessEnv(context.Background(), f.env)["ANONYMIZED_TELEMETRY"]; got != "false" {
		t.Fatalf("ANONYMIZED_TELEMETRY = %q", got)
	}
}

func TestSubprocessEnvironmentSubprocessEnvReplacesParentPythonImportPaths(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execBrowserEnv, func(*tools.Env) map[string]string {
		return map[string]string{"PYTHONPATH": "/icebath:/icebath/venv/lib/site-packages", "PYTHONHOME": "/icebath/venv", "KEEP_ME": "yes"}
	})
	pin(t, &harnessSiteDir, func() string { return "/harness-site" })
	env := baseSubprocessEnv(context.Background(), f.env)
	if env["PYTHONPATH"] != "/harness-site" || env["KEEP_ME"] != "yes" {
		t.Fatalf("env = %v", env)
	}
	if _, ok := env["PYTHONHOME"]; ok {
		t.Fatal("PYTHONHOME reached the harness child")
	}
}

func TestSubprocessEnvironmentSubprocessEnvFloorsVersionManagerOnlyPath(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execBrowserEnv, func(*tools.Env) map[string]string {
		return map[string]string{"PATH": strings.Repeat("/home/u/.nvm/versions/node/v24.18.0/bin:", 6) + "/home/u/.nvm/versions/node/v24.18.0/bin"}
	})
	parts := strings.Split(baseSubprocessEnv(context.Background(), f.env)["PATH"], ":")
	if !slices.Contains(parts, "/usr/bin") || !slices.Contains(parts, "/bin") {
		t.Fatalf("PATH = %v", parts)
	}
}

func TestSubprocessEnvironmentFloorPreservesExistingEntriesAndOrder(t *testing.T) {
	original := []string{"/opt/toolchain/bin", "/usr/bin", "/snap/bin"}
	merged := strings.Split(floorSubprocessPath(strings.Join(original, ":")), ":")
	positions := []int{}
	for _, p := range original {
		i := slices.Index(merged, p)
		if i < 0 {
			t.Fatalf("%s dropped from %v", p, merged)
		}
		positions = append(positions, i)
	}
	if !slices.IsSorted(positions) {
		t.Fatalf("order changed %v", merged)
	}
}

func TestVaultSupervisorAttachExecAttachesSupervisorToTheBrowserItDrives(t *testing.T) {
	f := newExecFix(t, nil)
	f.cli(t, "cat > /dev/null\necho ok\n")
	result := f.run(t, "print(1)", "", "t-vault")
	if result["success"] != true {
		t.Fatalf("result = %v", result)
	}
	want := [][2]string{{"t-vault", "ws://127.0.0.1:47000/devtools/browser/t-vault"}}
	if !slices.Equal(f.attached, want) {
		t.Fatalf("attached = %v", f.attached)
	}
}

func TestBackendSwapRetargetsDaemonsBrowserSwapRestartsEveryDaemonOnTheNewEndpoint(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execCDPOverride, func(_ context.Context, env *tools.Env) string { return cfgOf(env).cdpOverrideRaw() })
	f.cli(t, `latch="`+f.dir+`/daemon-${BU_NAME:-default}"
if [ "${1:-}" = "--reload" ]; then rm -f "$latch"; exit 0; fi
cat > /dev/null
[ -f "$latch" ] || echo "$BU_CDP_URL" > "$latch"
cat "$latch"
`)
	drive := func(session string) string {
		return strings.TrimSpace(f.run(t, "print(1)", session, "t-swap")["output"].(string))
	}
	t.Setenv("BROWSER_CDP_URL", "http://127.0.0.1:9400")
	if a, b := drive(""), drive("research"); a != "http://127.0.0.1:9400" || b != a {
		t.Fatalf("before swap %q %q", a, b)
	}
	cleanupAllBrowsers(context.Background(), f.env)
	t.Setenv("BROWSER_CDP_URL", "http://127.0.0.1:9401")
	if a, b := drive(""), drive("research"); a != "http://127.0.0.1:9401" || b != a {
		t.Fatalf("after swap %q %q", a, b)
	}
}

func TestVaultEgressRedactionExecRedactsRegisteredVaultSecretFromStdoutAndStderr(t *testing.T) {
	f := newExecFix(t, nil)
	secret := "vault-filled-password-112693"
	f.cli(t, "cat > /dev/null\necho \"stdout="+secret+"\"\necho \"stderr="+secret+"\" >&2\n")
	tools.RegisterVaultRedactionValue(secret)
	out := f.raw(t, "print(1)", "", execDefaultTimeoutS, "", false)
	tools.ClearVaultRedactionValues(context.Background())
	if strings.Contains(out, secret) || strings.Count(out, "«redacted-vault-secret»") != 2 {
		t.Fatalf("result = %s", out)
	}
}

func legacyEnv(t *testing.T, section map[string]any) *tools.Env {
	t.Helper()
	return newExecFix(t, section).env
}

func TestLegacyCloudMigrationDirectAPIConfigMigrates(t *testing.T) {
	env := legacyEnv(t, map[string]any{"cloud_provider": "browser-use"})
	t.Setenv("BROWSER_USE_API_KEY", "bu-key")
	if !isLegacyBrowserUseCloudConfig(env) {
		t.Fatal("direct API config did not migrate")
	}
}

func TestLegacyCloudMigrationGatewayConfigStaysOnLegacyPath(t *testing.T) {
	env := legacyEnv(t, map[string]any{"cloud_provider": "browser-use", "use_gateway": true})
	t.Setenv("BROWSER_USE_API_KEY", "bu-key")
	if isLegacyBrowserUseCloudConfig(env) {
		t.Fatal("gateway config migrated")
	}
}

func TestLegacyCloudMigrationNoAPIKeyStaysOnLegacyPath(t *testing.T) {
	if isLegacyBrowserUseCloudConfig(legacyEnv(t, map[string]any{"cloud_provider": "browser-use"})) {
		t.Fatal("keyless config migrated")
	}
}

func TestLegacyCloudMigrationOtherCloudProviderDoesNotMigrate(t *testing.T) {
	env := legacyEnv(t, map[string]any{"cloud_provider": "browserbase"})
	t.Setenv("BROWSER_USE_API_KEY", "bu-key")
	if isLegacyBrowserUseCloudConfig(env) {
		t.Fatal("browserbase config migrated")
	}
}

func TestLegacyCloudMigrationAutoDetectWithKeyMigrates(t *testing.T) {
	env := legacyEnv(t, nil)
	t.Setenv("BROWSER_USE_API_KEY", "bu-key")
	t.Setenv("BROWSERBASE_API_KEY", "bb-key")
	t.Setenv("BROWSERBASE_PROJECT_ID", "bb-project")
	if !isLegacyBrowserUseCloudConfig(env) {
		t.Fatal("autodetect with key did not migrate")
	}
}

func TestLegacyCloudMigrationMigratedConfigGetsBUAutospawn(t *testing.T) {
	f := newExecFix(t, map[string]any{"cloud_provider": "browser-use"})
	t.Setenv("BROWSER_USE_API_KEY", "bu-key")
	f.cli(t, "cat > /dev/null\necho \"autospawn:$BU_AUTOSPAWN\"\n")
	if out := f.run(t, "print(1)", "", "")["output"].(string); !strings.Contains(out, "autospawn:1") {
		t.Fatalf("output = %q", out)
	}
}

func TestLegacyCloudMigrationNonLegacyConfigDoesNotSetBUAutospawn(t *testing.T) {
	f := newExecFix(t, nil)
	f.cli(t, "cat > /dev/null\necho \"autospawn:[$BU_AUTOSPAWN]\"\n")
	if out := f.run(t, "print(1)", "", "")["output"].(string); !strings.Contains(out, "autospawn:[]") {
		t.Fatalf("output = %q", out)
	}
}

func TestBackendCdpResolutionExistingBUEnvWins(t *testing.T) {
	f := newExecFix(t, nil)
	penv := map[string]string{"BU_CDP_WS": "ws://operator-override:9222"}
	if err := resolveFor(f, penv, "t1", ""); err != "" || penv["BU_CDP_WS"] != "ws://operator-override:9222" {
		t.Fatalf("%q %v", err, penv)
	}
}

func TestBackendCdpResolutionCdpOverrideExported(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execCDPOverride, cdpOverrideOf("http://127.0.0.1:9222"))
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", ""); err != "" || penv["BU_CDP_URL"] != "http://127.0.0.1:9222" {
		t.Fatalf("%q %v", err, penv)
	}
}

func TestBackendCdpResolutionWsOverrideUsesBUCdpWs(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execCDPOverride, cdpOverrideOf("wss://connect.example/x"))
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", ""); err != "" || penv["BU_CDP_WS"] != "wss://connect.example/x" {
		t.Fatalf("%q %v", err, penv)
	}
}

func TestBackendCdpResolutionCloudProviderSessionExported(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execCloudProvider, providerOf("scripted"))
	pin(t, &execSessionInfo, sessionInfoOf(map[string]any{"cdp_url": "wss://browser.example/cdp/abc"}, nil, nil))
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", ""); err != "" || penv["BU_CDP_WS"] != "wss://browser.example/cdp/abc" {
		t.Fatalf("%q %v", err, penv)
	}
}

func TestBackendCdpResolutionNoProviderDrivesPackagedChromiumNotUserChrome(t *testing.T) {
	f := newExecFix(t, nil)
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", ""); err != "" {
		t.Fatal(err)
	}
	if penv["BU_CDP_WS"] != "ws://127.0.0.1:47000/devtools/browser/t1" || penv[privateBrowserSentinel] != "1" || !slices.Equal(f.keys, []string{"t1"}) {
		t.Fatalf("%v %v", penv, f.keys)
	}
	if err := resolveFor(f, map[string]string{}, "t1", "r7k2"); err != "" || f.keys[len(f.keys)-1] != "bu-named-r7k2" {
		t.Fatalf("%q %v", err, f.keys)
	}
}

func TestBackendCdpResolutionPackagedChromiumLaunchFailureIsAnError(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execSessionInfo, sessionInfoOf(nil, errors.New("Chromium browser not installed"), nil))
	penv := map[string]string{}
	err := resolveFor(f, penv, "t1", "")
	if !strings.Contains(err, "Chromium browser not installed") || hasCDPEnv(penv) {
		t.Fatalf("%q %v", err, penv)
	}
}

func TestBackendCdpResolutionProviderFailureReturnsError(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execCloudProvider, providerOf("scripted"))
	pin(t, &execSessionInfo, sessionInfoOf(nil, errors.New("api down"), nil))
	if err := resolveFor(f, map[string]string{}, "t1", ""); !strings.Contains(err, "api down") {
		t.Fatalf("%q", err)
	}
}

func TestBackendCdpResolutionProviderFallbackToLocalDrivesPackagedChromium(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execCloudProvider, providerOf("scripted"))
	pin(t, &execSessionInfo, func(_ context.Context, _ *tools.Env, key string) (map[string]any, error) {
		f.keys = append(f.keys, key)
		return map[string]any{"cdp_url": "ws://127.0.0.1:47000/devtools/browser/" + key, "features": map[string]any{"local": true},
			"fallback_from_cloud": true, "fallback_reason": "429"}, nil
	})
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", ""); err != "" || penv["BU_CDP_WS"] != "ws://127.0.0.1:47000/devtools/browser/t1" || !slices.Equal(f.keys, []string{"t1"}) {
		t.Fatalf("%q %v %v", err, penv, f.keys)
	}
}

func TestBackendCdpResolutionProviderWithoutCdpReturnsError(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execCloudProvider, providerOf("scripted"))
	pin(t, &execSessionInfo, sessionInfoOf(map[string]any{"cdp_url": nil}, nil, nil))
	err := resolveFor(f, map[string]string{}, "t1", "")
	if !strings.Contains(strings.ToLower(err), "no") || !strings.Contains(err, "CDP") {
		t.Fatalf("%q", err)
	}
}

func TestBackendCdpResolutionNamedSessionComposesWithProviderBackend(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execCloudProvider, providerOf("scripted"))
	seen := []string{}
	pin(t, &execSessionInfo, func(_ context.Context, _ *tools.Env, key string) (map[string]any, error) {
		seen = append(seen, key)
		return map[string]any{"cdp_url": "wss://browser.example/cdp/" + key}, nil
	})
	f.cli(t, "cat > /dev/null\necho \"bu:$BU_NAME ws:$BU_CDP_WS\"\n")
	result := f.run(t, "print(1)", "r7k2", "")
	out := result["output"].(string)
	if result["success"] != true || !slices.Equal(seen, []string{"bu-named-r7k2"}) || !strings.Contains(out, "bu:r7k2") ||
		!strings.Contains(out, "ws:wss://browser.example/cdp/bu-named-r7k2") {
		t.Fatalf("%v %v", result, seen)
	}
}

func TestBackendCdpResolutionNamedSessionKeyStableAcrossTasks(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execCloudProvider, providerOf("scripted"))
	seen := []string{}
	pin(t, &execSessionInfo, sessionInfoOf(map[string]any{"cdp_url": "wss://x/cdp/a"}, nil, &seen))
	if resolveFor(f, map[string]string{}, "task-A", "research") != "" || resolveFor(f, map[string]string{}, "task-B", "research") != "" {
		t.Fatal("named resolution failed")
	}
	if !slices.Equal(seen, []string{"bu-named-research", "bu-named-research"}) {
		t.Fatalf("%v", seen)
	}
}

func TestBackendCdpResolutionNamedSessionDirectAPIBUCloudStillSkipsProvider(t *testing.T) {
	f := newExecFix(t, map[string]any{"cloud_provider": "browser-use"})
	pin(t, &execCloudProvider, providerOf("browser-use"))
	pin(t, &execSessionInfo, func(context.Context, *tools.Env, string) (map[string]any, error) {
		t.Fatal("must skip provider")
		return nil, nil
	})
	t.Setenv("BROWSER_USE_API_KEY", "bu-key")
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", "r7k2"); err != "" || hasCDPEnv(penv) {
		t.Fatalf("%q %v", err, penv)
	}
}

func TestBackendCdpResolutionDirectAPIWithoutKeyUsesSessionPathNotChromeHunt(t *testing.T) {
	f := newExecFix(t, map[string]any{"cloud_provider": "browser-use"})
	pin(t, &execCloudProvider, providerOf("browser-use"))
	pin(t, &execSessionInfo, func(_ context.Context, _ *tools.Env, key string) (map[string]any, error) {
		f.keys = append(f.keys, key)
		return map[string]any{"cdp_url": "ws://127.0.0.1:47000/devtools/browser/" + key, "features": map[string]any{"local": true},
			"fallback_from_cloud": true, "fallback_reason": "BROWSER_USE_API_KEY is not set"}, nil
	})
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", ""); err != "" || penv["BU_CDP_WS"] != "ws://127.0.0.1:47000/devtools/browser/t1" || !slices.Equal(f.keys, []string{"t1"}) {
		t.Fatalf("%q %v %v", err, penv, f.keys)
	}
}

func gatewayResolves(t *testing.T, section map[string]any, cdp string) {
	t.Helper()
	f := newExecFix(t, section)
	pin(t, &execCloudProvider, providerOf("browser-use"))
	pin(t, &execSessionInfo, sessionInfoOf(map[string]any{"cdp_url": cdp}, nil, nil))
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", ""); err != "" || penv["BU_CDP_WS"] != cdp {
		t.Fatalf("%q %v", err, penv)
	}
}

func TestBackendCdpResolutionPickerManagedSelectionResolvesGatewayProvider(t *testing.T) {
	gatewayResolves(t, map[string]any{"cloud_provider": "nous"}, "wss://gateway.example/cdp/managed")
}

func TestBackendCdpResolutionNeverConfiguredAutodetectManagedProviderResolvesThroughProvider(t *testing.T) {
	gatewayResolves(t, nil, "wss://gateway.example/cdp/autodetect")
}

func TestBackendCdpResolutionLegacyUseGatewayFlagStillResolvesGatewayProvider(t *testing.T) {
	gatewayResolves(t, map[string]any{"use_gateway": true}, "wss://gateway.example/cdp/legacy")
}

func ownTabRun(t *testing.T, session string, provider bool, sharedCDP string) map[string]any {
	t.Helper()
	f := newExecFix(t, nil)
	pin(t, &execCDPOverride, cdpOverrideOf(sharedCDP))
	if provider {
		pin(t, &execCloudProvider, providerOf("scripted"))
		pin(t, &execSessionInfo, func(_ context.Context, _ *tools.Env, key string) (map[string]any, error) {
			return map[string]any{"cdp_url": "wss://browser.example/cdp/" + key}, nil
		})
	}
	f.cli(t, "cat\n")
	return f.run(t, "print('payload')", session, "")
}

func TestOwnTabPreambleNamedSharedBrowserGetsPreamble(t *testing.T) {
	result := ownTabRun(t, "r7k2", false, "http://127.0.0.1:9222")
	out := result["output"].(string)
	i, j := strings.Index(out, "_icebath_ensure_own_tab"), strings.Index(out, "print('payload')")
	if result["success"] != true || i < 0 || j < 0 || i > j {
		t.Fatalf("%v", result)
	}
}

func TestOwnTabPreambleNamedPackagedChromiumSkipsPreamble(t *testing.T) {
	result := ownTabRun(t, "r7k2", false, "")
	if result["success"] != true || strings.Contains(result["output"].(string), "_icebath_ensure_own_tab") {
		t.Fatalf("%v", result)
	}
}

func TestOwnTabPreambleUnnamedSessionGetsNoPreamble(t *testing.T) {
	result := ownTabRun(t, "", false, "")
	if result["success"] != true || strings.Contains(result["output"].(string), "_icebath_ensure_own_tab") {
		t.Fatalf("%v", result)
	}
}

func TestOwnTabPreambleNamedProviderBrowserSkipsPreamble(t *testing.T) {
	result := ownTabRun(t, "r7k2", true, "")
	if result["success"] != true || strings.Contains(result["output"].(string), "_icebath_ensure_own_tab") {
		t.Fatalf("%v", result)
	}
}

func TestOwnTabPreambleSentinelNeverReachesSubprocessEnv(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execCloudProvider, providerOf("scripted"))
	pin(t, &execSessionInfo, func(_ context.Context, _ *tools.Env, key string) (map[string]any, error) {
		return map[string]any{"cdp_url": "wss://browser.example/cdp/" + key}, nil
	})
	f.cli(t, "cat > /dev/null\necho \"sentinel:${"+privateBrowserSentinel+":-unset}\"\n")
	if out := f.run(t, "print(1)", "r7k2", "")["output"].(string); !strings.Contains(out, "sentinel:unset") {
		t.Fatalf("output = %q", out)
	}
}

func TestOwnTabPreamblePreambleIsValidPython(t *testing.T) {
	for _, src := range []string{ownTabPreamble, ownTabPreamble + "print('x')"} {
		if _, err := pyast.Parse(src); err != nil {
			t.Fatal(err)
		}
	}
}

func shotFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("\x89PNG fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func TestNativeScreenshotsFindScreenshotReturnsLastFreshPath(t *testing.T) {
	dir := t.TempDir()
	a, b := shotFile(t, dir, "shot.png"), shotFile(t, dir, "b.png")
	if got := findScreenshot("step one saved "+a+"\nthen saved "+b+"\n", nowSeconds()-5); got != b {
		t.Fatalf("got %q", got)
	}
}

func TestNativeScreenshotsFindScreenshotRejectsStaleAndMissing(t *testing.T) {
	stale := shotFile(t, t.TempDir(), "shot.png")
	old := time.Now().Add(-900 * time.Second)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if got := findScreenshot(stale+"\n/nonexistent/dir/x.png\n", nowSeconds()); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestNativeScreenshotsVisionModelGetsMultimodalEnvelope(t *testing.T) {
	f := newExecFix(t, nil)
	shot := shotFile(t, f.dir, "shot.png")
	f.cli(t, "cat > /dev/null\necho \""+shot+"\"\n")
	pin(t, &execNativeVision, func(context.Context, *tools.Env) bool { return true })
	pin(t, &execResizeForVision, func(*tools.Env, string, string, tools.ResizeOptions) (string, error) {
		return "data:image/png;base64,QUJD", nil
	})
	result := decodeExecResult(t, f.raw(t, "print(capture_screenshot())", "", execDefaultTimeoutS, "", false))
	content, _ := result["content"].([]any)
	kinds := []string{}
	for _, part := range content {
		kinds = append(kinds, part.(map[string]any)["type"].(string))
	}
	meta, _ := result["meta"].(map[string]any)
	if result["_multimodal"] != true || !slices.Equal(kinds, []string{"text", "image_url"}) || meta["screenshot_path"] != shot ||
		!strings.Contains(result["text_summary"].(string), shot) {
		t.Fatalf("%v", result)
	}
}

func TestNativeScreenshotsTextOnlyModelGetsPlainResultWithPath(t *testing.T) {
	f := newExecFix(t, nil)
	shot := shotFile(t, f.dir, "shot.png")
	f.cli(t, "cat > /dev/null\necho \""+shot+"\"\n")
	if got := f.run(t, "print(capture_screenshot())", "", "")["screenshot_path"]; got != shot {
		t.Fatalf("screenshot_path = %v", got)
	}
}

func TestNativeScreenshotsNoScreenshotKeepsStringResult(t *testing.T) {
	f := newExecFix(t, nil)
	f.cli(t, "cat > /dev/null\necho \"no images here\"\n")
	if _, ok := f.run(t, "print(1)", "", "")["screenshot_path"]; ok {
		t.Fatal("screenshot_path present")
	}
}

func TestBrowserExecMissingCliReturnsInstallHint(t *testing.T) {
	f := newExecFix(t, nil)
	if f.run(t, "print(page_info())", "", "")["error"] == nil {
		t.Fatal("no install hint")
	}
}

func TestBrowserExecEmptyCodeRejected(t *testing.T) {
	f := newExecFix(t, nil)
	if _, ok := f.run(t, "   ", "", "")["error"]; !ok {
		t.Fatal("blank code accepted")
	}
}

func TestBrowserExecCodePipedOnStdin(t *testing.T) {
	f := newExecFix(t, nil)
	f.cli(t, "code=$(cat)\necho \"got:$code\"\n")
	result := f.run(t, `print("hi")`, "", "")
	_, hasSession := result["session"]
	if result["success"] != true || result["exit_code"] != float64(0) || !strings.Contains(result["output"].(string), `got:print("hi")`) || hasSession {
		t.Fatalf("%v", result)
	}
}

func TestBrowserExecSessionSetsBUName(t *testing.T) {
	f := newExecFix(t, nil)
	f.cli(t, "cat > /dev/null\necho \"bu:$BU_NAME\"\n")
	result := f.run(t, "print(1)", "r7k2", "")
	if !strings.Contains(result["output"].(string), "bu:r7k2") || result["session"] != "r7k2" {
		t.Fatalf("%v", result)
	}
}

func TestBrowserExecInvalidSessionNameRejected(t *testing.T) {
	f := newExecFix(t, nil)
	f.cli(t, "cat > /dev/null\n")
	msg, _ := f.run(t, "print(1)", "bad name!", "")["error"].(string)
	if !strings.Contains(strings.ToLower(msg), "session") {
		t.Fatalf("error = %q", msg)
	}
}

func TestBrowserExecNonzeroExitReportsFailureAndStderr(t *testing.T) {
	f := newExecFix(t, nil)
	f.cli(t, "cat > /dev/null\necho \"boom\" >&2\nexit 3\n")
	result := f.run(t, "print(1)", "", "")
	if result["success"] != false || result["exit_code"] != float64(3) || !strings.Contains(result["stderr"].(string), "boom") {
		t.Fatalf("%v", result)
	}
}

// Lightpanda engine with the session layer answering info or failing with boom
func lightpandaSetup(t *testing.T, engine bool, info map[string]any, boom error) (*execFix, *[]string) {
	t.Helper()
	f := newExecFix(t, nil)
	seen := &[]string{}
	if info == nil {
		info = map[string]any{"cdp_url": "http://127.0.0.1:43111"}
	}
	pin(t, &execUsingLightpanda, func(*tools.Env) bool { return engine })
	pin(t, &execSessionInfo, func(_ context.Context, _ *tools.Env, key string) (map[string]any, error) {
		*seen = append(*seen, key)
		if boom != nil {
			return nil, boom
		}
		return info, nil
	})
	return f, seen
}

func TestLightpandaBackendResolutionExportsBUCdpURLAndPrivateSentinel(t *testing.T) {
	f, seen := lightpandaSetup(t, true, nil, nil)
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", ""); err != "" || penv["BU_CDP_URL"] != "http://127.0.0.1:43111" || penv[privateBrowserSentinel] != "1" || !slices.Equal(*seen, []string{"t1"}) {
		t.Fatalf("%q %v %v", err, penv, *seen)
	}
}

func TestLightpandaBackendResolutionNamedSessionKeysItsOwnProcess(t *testing.T) {
	f, seen := lightpandaSetup(t, true, nil, nil)
	if err := resolveFor(f, map[string]string{}, "t1", "r7k2"); err != "" || !slices.Equal(*seen, []string{"bu-named-r7k2"}) {
		t.Fatalf("%q %v", err, *seen)
	}
}

func TestLightpandaBackendResolutionDefaultKeyWithoutTask(t *testing.T) {
	f, seen := lightpandaSetup(t, true, nil, nil)
	if err := resolveFor(f, map[string]string{}, "", ""); err != "" || !slices.Equal(*seen, []string{"browser-exec-default"}) {
		t.Fatalf("%q %v", err, *seen)
	}
}

func TestLightpandaBackendResolutionLaunchFailureReturnsActionableError(t *testing.T) {
	f, _ := lightpandaSetup(t, true, nil, errors.New("no lightpanda binary was found"))
	if err := resolveFor(f, map[string]string{}, "t1", ""); !strings.Contains(err, "no lightpanda binary was found") {
		t.Fatalf("%q", err)
	}
}

func TestLightpandaBackendResolutionMissingCdpReturnsError(t *testing.T) {
	f, _ := lightpandaSetup(t, true, map[string]any{"cdp_url": nil}, nil)
	if err := resolveFor(f, map[string]string{}, "t1", ""); err == "" {
		t.Fatal("missing CDP accepted")
	}
}

func TestLightpandaBackendResolutionEngineAutoFallsThroughToPackagedChromium(t *testing.T) {
	f, seen := lightpandaSetup(t, false, map[string]any{"cdp_url": "ws://127.0.0.1:47000/devtools/browser/t1", "features": map[string]any{"local": true}}, nil)
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", ""); err != "" || !slices.Equal(*seen, []string{"t1"}) || !strings.HasPrefix(penv["BU_CDP_WS"], "ws://") {
		t.Fatalf("%q %v %v", err, penv, *seen)
	}
}

func TestLightpandaBackendResolutionEngineNamesTheFailingLocalBrowser(t *testing.T) {
	f, _ := lightpandaSetup(t, false, nil, errors.New("Chromium browser is missing."))
	err := resolveFor(f, map[string]string{}, "t1", "")
	if !strings.Contains(err, "local browser could not be started") || strings.Contains(err, "Lightpanda") {
		t.Fatalf("%q", err)
	}
}

func TestLightpandaBackendResolutionBUEnvWins(t *testing.T) {
	f, seen := lightpandaSetup(t, true, nil, nil)
	penv := map[string]string{"BU_CDP_WS": "ws://operator:9222"}
	if err := resolveFor(f, penv, "t1", ""); err != "" || penv["BU_CDP_WS"] != "ws://operator:9222" || len(*seen) != 0 {
		t.Fatalf("%q %v %v", err, penv, *seen)
	}
}

func TestLightpandaBackendResolutionCdpOverrideWins(t *testing.T) {
	f, seen := lightpandaSetup(t, true, nil, nil)
	pin(t, &execCDPOverride, cdpOverrideOf("http://127.0.0.1:9222"))
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", ""); err != "" || penv["BU_CDP_URL"] != "http://127.0.0.1:9222" || len(*seen) != 0 {
		t.Fatalf("%q %v %v", err, penv, *seen)
	}
}

func TestLightpandaBackendResolutionCloudProviderWins(t *testing.T) {
	f, seen := lightpandaSetup(t, true, map[string]any{"cdp_url": "wss://cloud.example/x"}, nil)
	pin(t, &execCloudProvider, providerOf("scripted"))
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", ""); err != "" || penv["BU_CDP_WS"] != "wss://cloud.example/x" || !slices.Equal(*seen, []string{"t1"}) {
		t.Fatalf("%q %v %v", err, penv, *seen)
	}
}

func TestLightpandaPreambleLightpandaSessionSkipsOwnTabPreamble(t *testing.T) {
	f, _ := lightpandaSetup(t, true, nil, nil)
	f.cli(t, "cat\n")
	result := f.run(t, "print('payload')", "r7k2", "")
	out := result["output"].(string)
	if result["success"] != true || strings.Contains(out, "_icebath_ensure_own_tab") || !strings.Contains(out, "print('payload')") {
		t.Fatalf("%v", result)
	}
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestTimeoutProcessGroupKillTimeoutKillsGrandchildAndReturnsPromptly(t *testing.T) {
	f := newExecFix(t, nil)
	pidFile := filepath.Join(f.dir, "grandchild.pid")
	f.cli(t, "cat > /dev/null\nsleep 60 &\necho $! > \""+pidFile+"\"\nsleep 60\n")
	pin(t, &execMinTimeoutS, 1)
	start := time.Now()
	result := decodeExecResult(t, f.raw(t, "print(1)", "", 1, "", false))
	if msg, _ := result["error"].(string); !strings.Contains(msg, "timed out") {
		t.Fatalf("%v", result)
	}
	if time.Since(start) >= 30*time.Second {
		t.Fatalf("returned after %s", time.Since(start))
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && pidAlive(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatalf("grandchild %d survived the group kill", pid)
	}
}

func TestTimeoutProcessGroupKillPostKillDrainIsBounded(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "browser-use")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stuck int
	pin(t, &killCLIGroup, func(pid int) { stuck = pid })
	pin(t, &postKillDrain, 200*time.Millisecond)
	t.Cleanup(func() {
		if stuck > 0 {
			syscall.Kill(-stuck, syscall.SIGKILL)
		}
	})
	start := time.Now()
	_, err := runCLIKillingProcessGroup([]string{cli}, "code", map[string]string{"PATH": os.Getenv("PATH")}, time.Second)
	if !errors.Is(err, errCLITimeout) || time.Since(start) > 10*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

const hybridLAN = "http://192.168.0.1:9120/login"

type hybridFix struct {
	*execFix
	cloudKeys, localKeys []string
	lookups              int
	dns                  func(n int) string
}

// Cloud provider with private auto local routing, a fake DNS shared by both URL checks
func newHybridFix(t *testing.T, autoLocal bool) *hybridFix {
	t.Helper()
	f := &hybridFix{execFix: newExecFix(t, map[string]any{"cloud_provider": "hybrid-cloud", "auto_local_for_private_urls": autoLocal})}
	registerClouds(t, newFakeCloud("hybrid-cloud", true))
	pin(t, &execCloudProvider, providerOf("hybrid-cloud"))
	pin(t, &execSessionInfo, func(_ context.Context, _ *tools.Env, key string) (map[string]any, error) {
		if isLocalSidecarKey(key) {
			f.localKeys = append(f.localKeys, key)
			return map[string]any{"cdp_url": "ws://127.0.0.1:47000/devtools/browser/" + key, "features": map[string]any{"local": true}}, nil
		}
		f.cloudKeys = append(f.cloudKeys, key)
		return map[string]any{"cdp_url": "wss://cloud.example/x"}, nil
	})
	resolve := func(host string) string {
		if _, err := netip.ParseAddr(host); err == nil {
			return host
		}
		f.lookups++
		if f.dns != nil {
			return f.dns(f.lookups)
		}
		return "93.184.216.34"
	}
	pin(t, &policyLookupHost, func(host string) ([]string, error) { return []string{resolve(host)}, nil })
	pin(t, &policyIsSafeURL, func(_ *tools.Env, raw string) bool {
		u, err := url.Parse(raw)
		if err != nil {
			return false
		}
		ip := netip.MustParseAddr(resolve(u.Hostname()))
		return !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
	})
	f.cli(t, "cat >/dev/null; printf \"%s\" \"${BU_NAME:-default}\"\n")
	return f
}

func TestExecHybridPrivateURLRoutingPrivateURLRunsOnLocalSidecarOrIsRefused(t *testing.T) {
	cases := []struct{ name, code, refusal, first, later string }{
		{"lan", `new_tab("` + hybridLAN + `")`, "", "", ""},
		{"mixed", "new_tab(\"https://example.com/x\")\nnew_tab(\"" + hybridLAN + "\")", "", "", ""},
		{"rebind-private-then-public", `new_tab("http://rebind.example/login")`, "", "192.168.1.5", "93.184.216.34"},
		{"imds", `new_tab("http://169.254.169.254/latest/meta-data/")`, "cloud metadata", "", ""},
		{"hybrid-off", `new_tab("` + hybridLAN + `")`, "private or internal", "", ""},
		{"rebind-public-then-private", `new_tab("http://flip.example/x")`, "private or internal", "93.184.216.34", "192.168.1.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newHybridFix(t, c.name != "hybrid-off")
			if c.first != "" {
				f.dns = func(n int) string {
					if n == 1 {
						return c.first
					}
					return c.later
				}
			}
			result := f.run(t, c.code, "", "t")
			if len(f.cloudKeys) != 0 {
				t.Fatalf("a call carrying a private URL reached the cloud provider %v", f.cloudKeys)
			}
			if c.refusal != "" {
				if msg, _ := result["error"].(string); !strings.Contains(msg, c.refusal) || len(f.localKeys) != 0 {
					t.Fatalf("%v %v", result, f.localKeys)
				}
				return
			}
			if result["success"] != true || !slices.Equal(f.localKeys, []string{"t::local"}) || result["output"] != "_local_default" {
				t.Fatalf("%v %v", result, f.localKeys)
			}
		})
	}
}

func TestExecHybridPrivateURLRoutingSidecarRouteHasItsOwnDaemonAndSticksForURLLessCalls(t *testing.T) {
	f := newHybridFix(t, true)
	steps := [][2]string{{`new_tab("https://example.com/")`, "default"}, {`new_tab("` + hybridLAN + `")`, "_local_default"},
		{`print(js("document.title"))`, "_local_default"}}
	for _, step := range steps {
		if got := f.run(t, step[0], "", "t")["output"]; got != step[1] {
			t.Fatalf("%s ran on %v", step[0], got)
		}
	}
	if !slices.Equal(f.cloudKeys, []string{"t"}) || !slices.Equal(f.localKeys, []string{"t::local", "t::local"}) {
		t.Fatalf("%v %v", f.cloudKeys, f.localKeys)
	}
}

func TestClampTimeoutFollowsPythonInt(t *testing.T) {
	for _, c := range []struct {
		in   any
		want int
	}{{nil, 300}, {"abc", 300}, {"60", 60}, {2.9, 5}, {99999, 1800}, {true, 5}, {pyjson.ParseInt("120"), 120}} {
		if got := clampTimeout(c.in); got != c.want {
			t.Errorf("clampTimeout(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
