//go:build !windows

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

type execFix struct {
	env      *tools.Env
	dir      string
	keys     []string
	attached [][2]string
}

// Isolated exec state with the session layer, supervisor, provider lookup, interpreter and code runner replaced
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
	f.runner(t, func(execRun) execProc { return execProc{stdout: "ok\n"} })
	pin(t, &execPython, func(context.Context, *tools.Env) (string, error) { return "/usr/bin/python3", nil })
	pin(t, &execNativeVision, func(context.Context, *tools.Env) bool { return false })
	pin(t, &execRouteIsSidecar, map[string]bool{})
	pin(t, &realProfileFallbackNotes, map[string]string{})
	pin(t, &realProfileNoteDelivered, map[string]bool{})
	return f
}

// Replaces the code runner, which then sees everything one call runs with
func (f *execFix) runner(t *testing.T, fn func(run execRun) execProc) {
	t.Helper()
	pin(t, &execRunCode, func(_ context.Context, _ *tools.Env, run execRun) execProc { return fn(run) })
}

// Makes the code runner answer with one route field, default when the route leaves it unset
func (f *execFix) echo(t *testing.T, field string) {
	t.Helper()
	f.runner(t, func(run execRun) execProc {
		if v := run.route[field]; v != "" {
			return execProc{stdout: v}
		}
		return execProc{stdout: "default"}
	})
}

// Code opening each URL in turn
func nav(urls ...string) string {
	lines := make([]string, len(urls))
	for i, u := range urls {
		lines[i] = fmt.Sprintf("new_tab(%q)", u)
	}
	return strings.Join(lines, "\n")
}

const pageInfo = "print(page_info())"

// The interpreter lookup the package ships with, before any fixture replaces it
var realExecPython = execPython

func (f *execFix) raw(t *testing.T, code string, session string, timeout any, task string, local bool) string {
	t.Helper()
	out, err := browserExec(context.Background(), f.env, code, session, timeout, task, local, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *execFix) run(t *testing.T, code string, session string, task string) map[string]any {
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

func TestVaultSupervisorAttachExecAttachesSupervisorToTheBrowserItDrives(t *testing.T) {
	f := newExecFix(t, nil)
	result := f.run(t, pageInfo, "", "t-vault")
	if result["success"] != true {
		t.Fatalf("result = %v", result)
	}
	want := [][2]string{{"t-vault", "ws://127.0.0.1:47000/devtools/browser/t-vault"}}
	if !slices.Equal(f.attached, want) {
		t.Fatalf("attached = %v", f.attached)
	}
}

func TestBackendSwapRetargetsEverySessionOnTheNewEndpoint(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execCDPOverride, func(_ context.Context, env *tools.Env) string { return cfgOf(env).cdpOverrideRaw() })
	f.echo(t, "BU_CDP_URL")
	drive := func(session string) string { return f.run(t, pageInfo, session, "t-swap")["output"].(string) }
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

func TestVaultEgressRedactionExecRedactsRegisteredVaultSecretFromOutputAndStderr(t *testing.T) {
	f := newExecFix(t, nil)
	secret := "vault-filled-password-112693"
	f.runner(t, func(execRun) execProc {
		return execProc{returnCode: 1, stdout: "output=" + secret, stderr: "failure=" + secret}
	})
	tools.RegisterVaultRedactionValue(secret)
	out := f.raw(t, pageInfo, "", execDefaultTimeoutS, "", false)
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
	f.runner(t, func(run execRun) execProc {
		return execProc{stdout: "bu:" + run.route["BU_NAME"] + " ws:" + run.route["BU_CDP_WS"]}
	})
	result := f.run(t, pageInfo, "r7k2", "")
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

func TestBackendCdpResolutionNamedSessionDirectAPIBUCloudGetsAProviderSession(t *testing.T) {
	f := newExecFix(t, map[string]any{"cloud_provider": "browser-use"})
	pin(t, &execCloudProvider, providerOf("browser-use"))
	pin(t, &execSessionInfo, func(_ context.Context, _ *tools.Env, key string) (map[string]any, error) {
		f.keys = append(f.keys, key)
		return map[string]any{"cdp_url": "wss://cloud.example/" + key}, nil
	})
	t.Setenv("BROWSER_USE_API_KEY", "bu-key")
	penv := map[string]string{}
	if err := resolveFor(f, penv, "t1", "r7k2"); err != "" || penv["BU_CDP_WS"] != "wss://cloud.example/bu-named-r7k2" ||
		penv[cloudBrowserSentinel] != activateAlways || penv[privateBrowserSentinel] != "1" || !slices.Equal(f.keys, []string{"bu-named-r7k2"}) {
		t.Fatalf("%q %v %v", err, penv, f.keys)
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

// Whether a call on the given browser pins its named session to a tab of its own
func ownTabRun(t *testing.T, session string, provider bool, sharedCDP string) bool {
	t.Helper()
	f := newExecFix(t, nil)
	pin(t, &execCDPOverride, cdpOverrideOf(sharedCDP))
	if provider {
		pin(t, &execCloudProvider, providerOf("scripted"))
		pin(t, &execSessionInfo, func(_ context.Context, _ *tools.Env, key string) (map[string]any, error) {
			return map[string]any{"cdp_url": "wss://browser.example/cdp/" + key}, nil
		})
	}
	own := false
	f.runner(t, func(run execRun) execProc {
		own = run.ownTab
		return execProc{}
	})
	if result := f.run(t, pageInfo, session, ""); result["success"] != true {
		t.Fatalf("%v", result)
	}
	return own
}

func TestOwnTabOnlyANamedSessionOnASharedBrowserGetsItsOwnTab(t *testing.T) {
	for _, c := range []struct {
		session   string
		provider  bool
		sharedCDP string
		own       bool
	}{{"r7k2", false, "http://127.0.0.1:9222", true}, {"r7k2", false, "", false}, {"", false, "", false}, {"r7k2", true, "", false}} {
		if got := ownTabRun(t, c.session, c.provider, c.sharedCDP); got != c.own {
			t.Errorf("%+v pinned %v", c, got)
		}
	}
}

func shotFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeScreenshotsVisionModelGetsMultimodalEnvelope(t *testing.T) {
	f := newExecFix(t, nil)
	shot := shotFile(t, f.dir, "shot.png")
	f.runner(t, func(execRun) execProc { return execProc{stdout: shot + "\n"} })
	pin(t, &execNativeVision, func(context.Context, *tools.Env) bool { return true })
	pin(t, &execResizeForVision, func(*tools.Env, string, string, tools.ResizeOptions) (string, error) {
		return "data:image/png;base64,QUJD", nil
	})
	result := decodeExecResult(t, f.raw(t, pageInfo, "", execDefaultTimeoutS, "", false))
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
	f.runner(t, func(execRun) execProc { return execProc{stdout: "saved " + shot + " and /no/such/old.png\n"} })
	if got := f.run(t, pageInfo, "", "")["screenshot_path"]; got != shot {
		t.Fatalf("screenshot_path = %v", got)
	}
}

func TestNativeScreenshotsNoScreenshotKeepsStringResult(t *testing.T) {
	f := newExecFix(t, nil)
	if _, ok := f.run(t, pageInfo, "", "")["screenshot_path"]; ok {
		t.Fatal("screenshot_path present")
	}
}

func TestBrowserExecRejectsCallsWithoutCode(t *testing.T) {
	f := newExecFix(t, nil)
	for _, code := range []string{"", "   ", "\n\t"} {
		if msg, _ := f.run(t, code, "", "")["error"].(string); !strings.HasPrefix(msg, "No code provided. Pass Python that uses the pre-imported helpers") {
			t.Fatalf("code %q gave %q", code, msg)
		}
	}
}

func TestBrowserExecHandsTheCodeToTheRunnerWithItsInterpreterAndEnvironment(t *testing.T) {
	f := newExecFix(t, nil)
	var seen []execRun
	f.runner(t, func(run execRun) execProc {
		seen = append(seen, run)
		return execProc{stdout: "{'url': 'about:blank'}\n"}
	})
	result := f.run(t, pageInfo, "", "")
	_, hasSession := result["session"]
	_, hasStderr := result["stderr"]
	if result["success"] != true || result["exit_code"] != 0.0 || result["output"] != "{'url': 'about:blank'}\n" || hasSession || hasStderr || result["workspace"] == "" {
		t.Fatalf("%v", result)
	}
	run := seen[0]
	if len(seen) != 1 || run.code != pageInfo || run.python != "/usr/bin/python3" || run.timeout != execDefaultTimeoutS ||
		run.childEnv["BH_AGENT_WORKSPACE"] != result["workspace"] || run.childEnv["BU_CDP_WS"] != run.route["BU_CDP_WS"] || run.childEnv["PYTHONPATH"] != "" {
		t.Fatalf("%+v", seen)
	}
	for k := range run.childEnv {
		if strings.HasPrefix(k, "_ICEBATH_BU_") {
			t.Fatalf("route sentinel %s reached the interpreter", k)
		}
	}
}

func TestBrowserExecWithoutPythonOnPathSaysSo(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execPython, realExecPython)
	t.Setenv("PATH", t.TempDir())
	if msg, _ := f.run(t, pageInfo, "", "")["error"].(string); msg != "browser_exec runs Python code and found no Python 3.8 or newer interpreter on PATH" {
		t.Fatalf("error = %q", msg)
	}
}

func TestBrowserExecSessionNamesTheRoute(t *testing.T) {
	f := newExecFix(t, nil)
	f.echo(t, "BU_NAME")
	if result := f.run(t, pageInfo, "r7k2", ""); result["output"] != "r7k2" || result["session"] != "r7k2" {
		t.Fatalf("%v", result)
	}
}

func TestBrowserExecInvalidSessionNameRejected(t *testing.T) {
	f := newExecFix(t, nil)
	msg, _ := f.run(t, pageInfo, "bad name!", "")["error"].(string)
	if !strings.Contains(strings.ToLower(msg), "session") {
		t.Fatalf("error = %q", msg)
	}
}

func TestBrowserExecFailingCodeReportsExitCodeOutputAndStderr(t *testing.T) {
	f := newExecFix(t, nil)
	f.runner(t, func(execRun) execProc {
		return execProc{returnCode: 1, stdout: "T1\n", stderr: "Traceback (most recent call last):\nRuntimeError: boom\n"}
	})
	result := f.run(t, pageInfo, "", "")
	if result["success"] != false || result["exit_code"] != 1.0 || result["output"] != "T1\n" || result["stderr"] != "Traceback (most recent call last):\nRuntimeError: boom" {
		t.Fatalf("%v", result)
	}
	f.runner(t, func(execRun) execProc { return execProc{returnCode: -9, stderr: strings.Repeat("e", 5000)} })
	result = f.run(t, pageInfo, "", "")
	if result["exit_code"] != -9.0 || result["stderr"] != strings.Repeat("e", 4000)+"\n… (stderr truncated)" {
		t.Fatalf("%v", result)
	}
}

func TestBrowserExecTimeoutAndLaunchFailureSaySo(t *testing.T) {
	f := newExecFix(t, nil)
	pin(t, &execMinTimeoutS, 1)
	var timeouts []int
	f.runner(t, func(run execRun) execProc {
		timeouts = append(timeouts, run.timeout)
		return execProc{timedOut: true}
	})
	result := decodeExecResult(t, f.raw(t, pageInfo, "", 1, "", false))
	if msg, _ := result["error"].(string); !strings.HasPrefix(msg, "browser_exec timed out after 1s. The browser may still be working; retry with a larger timeout_s (max 1800)") || !slices.Equal(timeouts, []int{1}) {
		t.Fatalf("%v %v", result, timeouts)
	}
	f.runner(t, func(execRun) execProc {
		return execProc{launchErr: &os.PathError{Op: "fork/exec", Path: "/usr/bin/python3", Err: syscall.EACCES}}
	})
	result = decodeExecResult(t, f.raw(t, pageInfo, "", 30, "", false))
	if msg, _ := result["error"].(string); msg != "Failed to launch Python for browser_exec: [Errno 13] Permission denied: '/usr/bin/python3'" {
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

func TestLightpandaNamedSessionKeepsItsSinglePage(t *testing.T) {
	f, _ := lightpandaSetup(t, true, nil, nil)
	f.echo(t, privateBrowserSentinel)
	if result := f.run(t, pageInfo, "r7k2", ""); result["success"] != true || result["output"] != "1" {
		t.Fatalf("%v", result)
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
	f.echo(t, "BU_NAME")
	return f
}

func TestExecHybridPrivateURLRoutingPrivateURLRunsOnLocalSidecarOrIsRefused(t *testing.T) {
	cases := []struct {
		name                  string
		code                  string
		refusal, first, later string
	}{
		{"lan", nav(hybridLAN), "", "", ""},
		{"mixed", nav("https://example.com/x", hybridLAN), "", "", ""},
		{"rebind-private-then-public", nav("http://rebind.example/login"), "", "192.168.1.5", "93.184.216.34"},
		{"imds", nav("http://169.254.169.254/latest/meta-data/"), "cloud metadata", "", ""},
		{"hybrid-off", nav(hybridLAN), "private or internal", "", ""},
		{"rebind-public-then-private", nav("http://flip.example/x"), "private or internal", "93.184.216.34", "192.168.1.5"},
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

func TestExecHybridPrivateURLRoutingSidecarRouteHasItsOwnSessionAndSticksForURLLessCalls(t *testing.T) {
	f := newHybridFix(t, true)
	calls := []struct {
		code string
		name string
	}{{nav("https://example.com/"), "default"}, {nav(hybridLAN), "_local_default"}, {"print(js('document.title'))", "_local_default"}}
	for _, call := range calls {
		if got := f.run(t, call.code, "", "t")["output"]; got != call.name {
			t.Fatalf("%v ran on %v", call.code, got)
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
