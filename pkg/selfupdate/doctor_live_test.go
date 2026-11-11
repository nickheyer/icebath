package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/config"
)

const doctorClashNoProxy = "127.0.0.1,localhost,::1,[::1]"

var doctorProxyEnvKeys = []string{"NO_PROXY", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"}

// Removes the variables for the length of a test
func doctorUnsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// Sets the environment Clash Verge exports, a bracketed IPv6 entry in both no proxy variables
func doctorClashEnv(t *testing.T) {
	t.Helper()
	doctorUnsetEnv(t, doctorProxyEnvKeys...)
	t.Setenv("NO_PROXY", doctorClashNoProxy)
	t.Setenv("no_proxy", doctorClashNoProxy)
}

// Live probe seams over a given config with no backend credential set and no browser available
func doctorLiveTestEnv(t *testing.T, cfg map[string]any) (*doctorLiveEnv, *bytes.Buffer) {
	t.Helper()
	for _, v := range []string{"FIRECRAWL_API_KEY", "FAL_KEY", "OPENAI_API_KEY", "ELEVENLABS_API_KEY", "GROQ_API_KEY"} {
		t.Setenv(v, "")
	}
	out := &bytes.Buffer{}
	h := t.TempDir()
	e := newDoctorLiveEnv(context.Background(), &Doctor{W: out, Home: h, ProjectRoot: t.TempDir(), DisplayHome: "~/.icebath"})
	if cfg == nil {
		cfg = map[string]any{}
	}
	e.loadConfig = func() (map[string]any, error) { return cfg, nil }
	e.browserAvailable = func() bool { return false }
	return e, out
}

func doctorLiveByName(results []doctorLiveProbeResult) map[string]doctorLiveProbeResult {
	out := map[string]doctorLiveProbeResult{}
	for _, r := range results {
		out[r.Name] = r
	}
	return out
}

func doctorLiveRun(t *testing.T, e *doctorLiveEnv, issues *[]string) []doctorLiveProbeResult {
	t.Helper()
	if issues == nil {
		issues = &[]string{}
	}
	results, err := e.doctorRunLiveChecks(issues)
	if err != nil {
		t.Fatal(err)
	}
	return results
}

func doctorLiveStatus(code int) func(string, [][2]string, float64) (int, error) {
	return func(string, [][2]string, float64) (int, error) { return code, nil }
}

func TestLiveFlagGatingNoLiveFlagMeansZeroProbes(t *testing.T) {
	e, out := doctorLiveTestEnv(t, nil)
	called := false
	e.loadConfig = func() (map[string]any, error) { called = true; return map[string]any{}, nil }
	if result := e.doctorMaybeRunLiveChecks(map[string]any{"live": false}, &[]string{}); result != nil {
		t.Fatalf("result = %#v", result)
	}
	if called || out.Len() != 0 {
		t.Fatalf("live probes ran without the flag: %q", out.String())
	}
}

func TestLiveFlagGatingLiveCheckCrashNeverPropagates(t *testing.T) {
	e, out := doctorLiveTestEnv(t, nil)
	e.loadConfig = func() (map[string]any, error) { return nil, errors.New("probe subsystem exploded") }
	if result := e.doctorMaybeRunLiveChecks(map[string]any{"live": true}, &[]string{}); result != nil {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(out.String(), "Live backend probes crashed") || !strings.Contains(out.String(), "(probe subsystem exploded)") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestConfiguredOnlySelectionUnconfiguredBackendsDoNotTouchNetwork(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, nil)
	e.httpGet = func(string, [][2]string, float64) (int, error) {
		t.Error("HTTP call made for unconfigured backend")
		return 0, nil
	}
	for _, r := range doctorLiveRun(t, e, nil) {
		if r.Status != "skip" {
			t.Fatalf("%s is %s", r.Name, r.Status)
		}
	}
}

func TestConfiguredOnlySelectionFirecrawlProbedWhenKeyPresent(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, nil)
	t.Setenv("FIRECRAWL_API_KEY", "fc-test")
	var calls []string
	var auth string
	e.httpGet = func(target string, headers [][2]string, _ float64) (int, error) {
		calls = append(calls, target)
		auth = headers[0][0] + ": " + headers[0][1]
		return 200, nil
	}
	if got := doctorLiveByName(doctorLiveRun(t, e, nil))["Firecrawl"]; got.Status != "pass" {
		t.Fatalf("Firecrawl = %#v", got)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "firecrawl") || auth != "Authorization: Bearer fc-test" {
		t.Fatalf("calls = %v auth = %q", calls, auth)
	}
}

func TestConfiguredOnlySelectionFirecrawlInvalidKeyFailsAndAppendsIssue(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, nil)
	t.Setenv("FIRECRAWL_API_KEY", "fc-bad")
	e.httpGet = doctorLiveStatus(401)
	var issues []string
	if got := doctorLiveByName(doctorLiveRun(t, e, &issues))["Firecrawl"]; got.Status != "fail" {
		t.Fatalf("Firecrawl = %#v", got)
	}
	if len(issues) != 1 || issues[0] != "Live probe failed: Firecrawl (HTTP 401 — check FIRECRAWL_API_KEY)" {
		t.Fatalf("issues = %q", issues)
	}
}

func TestConfiguredOnlySelectionMcpServersProbedPerConfiguredServer(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, map[string]any{"mcp_servers": map[string]any{
		"alpha": map[string]any{"url": "https://x"}, "beta": map[string]any{"command": "foo"}}})
	var probed []string
	e.probeMCPServer = func(name string, _ map[string]any, _ float64) (int, error) {
		probed = append(probed, name)
		return 1, nil
	}
	var results []doctorLiveProbeResult
	for _, r := range doctorLiveRun(t, e, nil) {
		if strings.HasPrefix(r.Name, "MCP") {
			results = append(results, r)
		}
	}
	sort.Strings(probed)
	if !reflect.DeepEqual(probed, []string{"alpha", "beta"}) || len(results) != 2 {
		t.Fatalf("probed = %v results = %#v", probed, results)
	}
	for _, r := range results {
		if r.Status != "pass" || r.Detail != "(1 tool(s))" {
			t.Fatalf("result = %#v", r)
		}
	}
}

func TestConfiguredOnlySelectionTtsLocalProviderSkipped(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, map[string]any{"tts": map[string]any{"provider": "edge"}})
	if got := doctorLiveByName(doctorLiveRun(t, e, nil))["TTS"]; got.Status != "skip" {
		t.Fatalf("TTS = %#v", got)
	}
}

func TestConfiguredOnlySelectionTtsOpenaiProbedWithKey(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, map[string]any{"tts": map[string]any{"provider": "openai"}})
	t.Setenv("OPENAI_API_KEY", "sk-test")
	e.httpGet = doctorLiveStatus(200)
	if got := doctorLiveByName(doctorLiveRun(t, e, nil))["TTS"]; got.Status != "pass" || got.Detail != "(openai) (HTTP 200)" {
		t.Fatalf("TTS = %#v", got)
	}
}

func TestConfiguredOnlySelectionSttGroqProbedWithKey(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, map[string]any{"stt": map[string]any{"provider": "groq"}})
	t.Setenv("GROQ_API_KEY", "gsk-test")
	e.httpGet = doctorLiveStatus(200)
	if got := doctorLiveByName(doctorLiveRun(t, e, nil))["STT"]; got.Status != "pass" {
		t.Fatalf("STT = %#v", got)
	}
}

func TestConfiguredOnlySelectionSttProviderConfiguredButKeyMissingWarns(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, map[string]any{"stt": map[string]any{"provider": "groq"}})
	if got := doctorLiveByName(doctorLiveRun(t, e, nil))["STT"]; got.Status != "warn" {
		t.Fatalf("STT = %#v", got)
	}
}

func TestConfiguredOnlySelectionBrowserProbedWhenAvailable(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, nil)
	e.browserAvailable = func() bool { return true }
	e.launchBrowserProbe = func(float64) (bool, string) { return true, "about:blank ok" }
	if got := doctorLiveByName(doctorLiveRun(t, e, nil))["Browser"]; got.Status != "pass" || got.Detail != "(about:blank ok)" {
		t.Fatalf("Browser = %#v", got)
	}
}

// Serves a bare 200 to every metadata GET
func doctorLiveOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(srv.Close)
	return srv
}

func TestHttpGetSanitizesBracketedIpv6NoProxy(t *testing.T) {
	for _, kind := range []string{"NO_PROXY", "no_proxy"} {
		for _, entry := range []string{"[::1]", "[::1]:8080", "::1/128"} {
			t.Run(kind+" "+entry, func(t *testing.T) {
				doctorUnsetEnv(t, doctorProxyEnvKeys...)
				t.Setenv(kind, "127.0.0.1,localhost,::1,"+entry)
				e, _ := doctorLiveTestEnv(t, nil)
				code, err := e.doctorLiveHTTPGet(doctorLiveOrigin(t).URL+"/", nil, 5.0)
				if err != nil || code != 200 {
					t.Fatalf("status %d %v", code, err)
				}
			})
		}
	}
}

func TestRunLiveChecksSanitizesClashEnvAndProbesPass(t *testing.T) {
	doctorClashEnv(t)
	e, _ := doctorLiveTestEnv(t, nil)
	t.Setenv("FIRECRAWL_API_KEY", "fc-test")
	saved := doctorLiveKeyedProbes[0]
	t.Cleanup(func() { doctorLiveKeyedProbes[0] = saved })
	doctorLiveKeyedProbes[0] = doctorLiveKeyedSpec{"Firecrawl", doctorLiveOrigin(t).URL + "/v2/team/credit-usage", "FIRECRAWL_API_KEY", "Bearer"}
	var issues []string
	got := doctorLiveByName(doctorLiveRun(t, e, &issues))["Firecrawl"]
	if got.Status != "pass" || len(issues) != 0 || strings.Contains(got.Detail, "Invalid port") {
		t.Fatalf("Firecrawl = %#v issues = %q", got, issues)
	}
	if strings.Contains(os.Getenv("NO_PROXY"), "[::1]") || os.Getenv("no_proxy") != "127.0.0.1,localhost,::1" {
		t.Fatalf("NO_PROXY = %q no_proxy = %q", os.Getenv("NO_PROXY"), os.Getenv("no_proxy"))
	}
}

func TestRunLiveChecksLeavesACleanEnvironmentUntouched(t *testing.T) {
	doctorUnsetEnv(t, doctorProxyEnvKeys...)
	t.Setenv("NO_PROXY", "example.com,10.0.0.0/8")
	e, _ := doctorLiveTestEnv(t, nil)
	if code, err := e.doctorLiveHTTPGet(doctorLiveOrigin(t).URL+"/", nil, 5.0); err != nil || code != 200 {
		t.Fatalf("status %d %v", code, err)
	}
	if got := os.Getenv("NO_PROXY"); got != "example.com,10.0.0.0/8" {
		t.Fatalf("NO_PROXY = %q", got)
	}
}

func TestBrowserAvailableFollowsChromiumSelection(t *testing.T) {
	for _, found := range []bool{true, false} {
		t.Run(fmt.Sprint(found), func(t *testing.T) {
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			config.ResetCache()
			t.Cleanup(config.ResetCache)
			if found {
				chrome := filepath.Join(t.TempDir(), "chrome")
				if err := os.WriteFile(chrome, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				seed := fmt.Sprintf("_config_version: %d\nbrowser:\n  executable_path: %s\n", config.LatestConfigVersion(), chrome)
				if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(seed), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			e := newDoctorLiveEnv(context.Background(), &Doctor{W: &bytes.Buffer{}, Home: h, DisplayHome: "~/.icebath"})
			want := found || browser.ChromiumExecutable(e.toolsEnv(), true) != ""
			if got := e.doctorLiveBrowserAvailable(); got != want {
				t.Fatalf("available = %v, want %v", got, want)
			}
			if found && browser.ChromiumExecutable(e.toolsEnv(), true) == "" {
				t.Fatal("the configured Chromium was not selected")
			}
		})
	}
}

// Records the launcher calls of a browser probe and answers the launch with a fixed result
func doctorFakeLauncher(e *doctorLiveEnv, launched *browser.LaunchedBrowser, launchErr string) *[]string {
	calls := &[]string{}
	e.launchChromium = func(key string, opts browser.LaunchOptions) (*browser.LaunchedBrowser, string) {
		*calls = append(*calls, fmt.Sprintf("launch %s ephemeral=%v timeout=%v", key, opts.Ephemeral, *opts.Timeout))
		return launched, launchErr
	}
	e.stopBrowser = func(key string) { *calls = append(*calls, "stop "+key) }
	return calls
}

func TestLaunchBrowserProbeSuccessStopsTheBrowser(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, nil)
	calls := doctorFakeLauncher(e, &browser.LaunchedBrowser{CDPURL: "ws://x"}, "")
	ok, detail := e.doctorLaunchBrowserProbe(5.0)
	if !ok || !strings.Contains(detail, "CDP ready") {
		t.Fatalf("ok = %v detail = %q", ok, detail)
	}
	want := []string{fmt.Sprintf("launch doctor-live-probe ephemeral=true timeout=%v", 5*time.Second), "stop doctor-live-probe"}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls = %q", *calls)
	}
}

func TestLaunchBrowserProbeLaunchErrorIsReportedAndStillStopped(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, nil)
	calls := doctorFakeLauncher(e, nil, "Chromium exited: missing libnss3")
	ok, detail := e.doctorLaunchBrowserProbe(5.0)
	if ok || !strings.Contains(detail, "libnss3") {
		t.Fatalf("ok = %v detail = %q", ok, detail)
	}
	if len(*calls) != 2 || !strings.HasPrefix((*calls)[0], "launch ") || !strings.HasPrefix((*calls)[1], "stop ") {
		t.Fatalf("calls = %q", *calls)
	}
}

func TestFailureIsolationOneProbeRaisingDoesNotStopOthers(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, nil)
	t.Setenv("FIRECRAWL_API_KEY", "fc-test")
	t.Setenv("FAL_KEY", "fal-test")
	e.httpGet = func(target string, _ [][2]string, _ float64) (int, error) {
		if strings.Contains(target, "firecrawl") {
			return 0, errors.New("connection reset")
		}
		return 200, nil
	}
	results := doctorLiveByName(doctorLiveRun(t, e, &[]string{}))
	if results["Firecrawl"].Status != "fail" || results["Firecrawl"].Detail != "(connection reset)" || results["FAL"].Status != "pass" {
		t.Fatalf("results = %#v", results)
	}
}

func TestFailureIsolationMcpProbeFailureIsolatedPerServer(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, map[string]any{"mcp_servers": map[string]any{
		"bad": map[string]any{"url": "https://x"}, "good": map[string]any{"url": "https://y"}}})
	e.probeMCPServer = func(name string, _ map[string]any, _ float64) (int, error) {
		if name == "bad" {
			return 0, errors.New("refused")
		}
		return 1, nil
	}
	results := doctorLiveByName(doctorLiveRun(t, e, nil))
	if results["MCP: bad"].Status != "fail" || results["MCP: good"].Status != "pass" {
		t.Fatalf("results = %#v", results)
	}
}

func TestTimeoutHandlingTimeoutReportedAsFail(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, nil)
	t.Setenv("FIRECRAWL_API_KEY", "fc-test")
	e.httpGet = func(string, [][2]string, float64) (int, error) { return 0, context.DeadlineExceeded }
	got := doctorLiveByName(doctorLiveRun(t, e, nil))["Firecrawl"]
	if got.Status != "fail" || !strings.HasPrefix(got.Detail, "(timed out: ") {
		t.Fatalf("Firecrawl = %#v", got)
	}
}

func TestTimeoutHandlingProbeTimeoutBoundedAndConfigurable(t *testing.T) {
	e, _ := doctorLiveTestEnv(t, map[string]any{"doctor": map[string]any{"live_probe_timeout": 3}})
	t.Setenv("FIRECRAWL_API_KEY", "fc-test")
	seen := 0.0
	e.httpGet = func(_ string, _ [][2]string, timeout float64) (int, error) {
		seen = timeout
		return 200, nil
	}
	doctorLiveRun(t, e, nil)
	if seen != 3 {
		t.Fatalf("timeout = %v", seen)
	}
	for raw, want := range map[any]float64{0.2: 1, "7": 7, "soon": 10, nil: 10} {
		if got, err := doctorLiveTimeout(map[string]any{"doctor": map[string]any{"live_probe_timeout": raw}}); err != nil || got != want {
			t.Fatalf("timeout of %v = %v %v, want %v", raw, got, err, want)
		}
	}
}

func TestReadOnlySkipsNeverAppendIssues(t *testing.T) {
	e, out := doctorLiveTestEnv(t, nil)
	var issues []string
	doctorLiveRun(t, e, &issues)
	if len(issues) != 0 {
		t.Fatalf("issues = %q", issues)
	}
	if !strings.Contains(out.String(), "MCP (no servers configured) — skipped") {
		t.Fatalf("out = %q", out.String())
	}
}
