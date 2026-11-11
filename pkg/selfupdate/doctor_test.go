package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Answers every request of the default transport from a function and records the requested URLs
func doctorFakeDefaultTransport(t *testing.T, status func(target string) int) *[]string {
	t.Helper()
	var mu sync.Mutex
	calls := &[]string{}
	saved := http.DefaultTransport
	http.DefaultTransport = doctorRoundTrip(func(r *http.Request) (*http.Response, error) {
		target := r.URL.String()
		mu.Lock()
		*calls = append(*calls, target)
		mu.Unlock()
		return doctorHTTPResponse(r, status(target), ""), nil
	})
	t.Cleanup(func() { http.DefaultTransport = saved })
	return calls
}

// Config of the connectivity walks, IPv6 dialling off so the route probe never resolves a public host
const doctorConnectivityConfig = "memory: {}\nnetwork:\n  force_ipv4: true\n"

var doctorConnectivityEntries = []doctorCheckEntry{{"Configuration Files", checkEnvFile}, {"API Connectivity", checkAPIConnectivity}}

func doctorContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestRunDoctorKimiCnEnvIsDetectedAndProbeIsNullSafe(t *testing.T) {
	d, buf := doctorRawHome(t, doctorConnectivityConfig, "KIMI_CN_API_KEY=sk-test\n")
	t.Setenv("KIMI_CN_API_KEY", "sk-test")
	calls := doctorFakeDefaultTransport(t, func(string) int { return 200 })
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConnectivityEntries...)
	if !strings.Contains(out, "API key or custom endpoint configured") || !strings.Contains(out, "Kimi / Moonshot (China)") ||
		strings.Contains(out, "str expected, not NoneType") {
		t.Fatalf("out = %q", out)
	}
	if !doctorContains(*calls, "https://api.moonshot.cn/v1/models") {
		t.Fatalf("calls = %q", *calls)
	}
}

func TestRunDoctorDashscopeRetriesChinaEndpointAfterIntlUnauthorized(t *testing.T) {
	d, buf := doctorRawHome(t, doctorConnectivityConfig, "DASHSCOPE_API_KEY=sk-test\n")
	t.Setenv("DASHSCOPE_API_KEY", "sk-test")
	calls := doctorFakeDefaultTransport(t, func(target string) int {
		if strings.Contains(target, "dashscope.aliyuncs.com") {
			return 200
		}
		return 401
	})
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConnectivityEntries...)
	if !strings.Contains(out, "Alibaba/DashScope") || strings.Contains(out, "invalid API key") {
		t.Fatalf("out = %q", out)
	}
	if !doctorContains(*calls, "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/models") ||
		!doctorContains(*calls, "https://dashscope.aliyuncs.com/compatible-mode/v1/models") {
		t.Fatalf("calls = %q", *calls)
	}
}

func TestRunDoctorOpencodeGoSkipsInvalidModelsProbe(t *testing.T) {
	for _, baseURL := range []string{"", "https://opencode.ai/zen/go/v1"} {
		t.Run("base "+baseURL, func(t *testing.T) {
			d, buf := doctorRawHome(t, doctorConnectivityConfig, "OPENCODE_GO_API_KEY=sk-test\n")
			t.Setenv("OPENCODE_GO_API_KEY", "sk-test")
			if baseURL != "" {
				t.Setenv("OPENCODE_GO_BASE_URL", baseURL)
			}
			calls := doctorFakeDefaultTransport(t, func(string) int { return 200 })
			out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConnectivityEntries...)
			found := false
			for _, line := range strings.Split(out, "\n") {
				found = found || (strings.Contains(line, "OpenCode Go") && strings.Contains(line, "(key configured)"))
			}
			if !found {
				t.Fatalf("out = %q", out)
			}
			for _, target := range *calls {
				low := strings.ToLower(target)
				if target == "https://opencode.ai/zen/go/v1/models" || (strings.Contains(low, "opencode") && strings.Contains(low, "models")) {
					t.Fatalf("calls = %q", *calls)
				}
			}
		})
	}
}

func TestGmiDoctorRunDoctorChecksGmiModelsEndpoint(t *testing.T) {
	d, buf := doctorRawHome(t, doctorConnectivityConfig, "GMI_API_KEY=***\n")
	t.Setenv("GMI_API_KEY", "gmi-test-key")
	calls := doctorFakeDefaultTransport(t, func(string) int { return 200 })
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConnectivityEntries...)
	if !strings.Contains(out, "GMI Cloud") || !doctorContains(*calls, "https://api.gmi-serving.com/v1/models") {
		t.Fatalf("calls = %q out = %q", *calls, out)
	}
}

// Stores an OAuth login of one provider in the auth store of a home
func doctorSeedOAuth(t *testing.T, h, providerID string, state map[string]any) {
	t.Helper()
	if _, err := provider.NewStore(h).SaveActiveProviderState(providerID, state); err != nil {
		t.Fatal(err)
	}
}

func TestRunDoctorIgnoresInvalidDirectKeysWhenOauthFallbackIsHealthy(t *testing.T) {
	cases := []struct {
		envKey, badKey, failingHost, oauthProvider string
		oauthState                                 map[string]any
		unexpectedIssue                            string
	}{
		{"MINIMAX_API_KEY", "bad-minimax-key", "minimax.io", "minimax-oauth",
			map[string]any{"access_token": "minimax-access", "region": "global"}, "Check MINIMAX_API_KEY in .env"},
		{"XAI_API_KEY", "bad-xai-key", "api.x.ai", "xai-oauth",
			map[string]any{"tokens": map[string]any{"access_token": "xai-access", "refresh_token": "xai-refresh"}, "auth_mode": "oauth_pkce"},
			"Check XAI_API_KEY in .env"},
	}
	for _, c := range cases {
		t.Run(c.envKey, func(t *testing.T) {
			d, buf := doctorRawHome(t, "model:\n  provider: nous\n  default: moonshotai/kimi-k2.6\nnetwork:\n  force_ipv4: true\n", "")
			t.Setenv(c.envKey, c.badKey)
			doctorSeedOAuth(t, d.Home, c.oauthProvider, c.oauthState)
			doctorFakeDefaultTransport(t, func(target string) int {
				if strings.Contains(target, c.failingHost) {
					return 401
				}
				return 200
			})
			out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorCheckEntry{"API Connectivity", checkAPIConnectivity})
			if !strings.Contains(out, "invalid API key") || strings.Contains(out, c.unexpectedIssue) {
				t.Fatalf("out = %q", out)
			}
		})
	}
}

var doctorAuthEntry = doctorCheckEntry{"Auth Providers", checkAuthProviders}

func TestDoctorXaiOAuthStatusLoggedInDoesNotEmitNotLoggedInOnXaiLine(t *testing.T) {
	d, buf := doctorRawHome(t, "memory: {}\n", "")
	doctorSeedOAuth(t, d.Home, "xai-oauth", map[string]any{
		"tokens": map[string]any{"access_token": "xai-access", "refresh_token": "xai-refresh"}, "auth_mode": "oauth_pkce"})
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorAuthEntry)
	xaiLine := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "xAI OAuth") {
			xaiLine = line
		}
	}
	if !strings.Contains(xaiLine, "(logged in)") || strings.Contains(xaiLine, "(not logged in)") {
		t.Fatalf("xAI line = %q in %q", xaiLine, out)
	}
}

// Home whose auth store is not readable JSON, so every status reader takes its failure path
func doctorDamagedAuthHome(t *testing.T) (*Doctor, *bytes.Buffer) {
	t.Helper()
	d, buf := doctorRawHome(t, "memory: {}\n", "")
	if err := os.WriteFile(provider.NewStore(d.Home).Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	return d, buf
}

func TestDoctorXaiOAuthStatusImportFailureDoesNotAffectOtherProviders(t *testing.T) {
	d, buf := doctorDamagedAuthHome(t)
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorAuthEntry)
	for _, want := range []string{"Nous Portal auth", "OpenAI Codex auth", "MiniMax OAuth", "logged in"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q missing from %q", want, out)
		}
	}
}

func TestDoctorXaiOAuthStatusFunctionRaisesDoesNotCrashDoctor(t *testing.T) {
	d, buf := doctorDamagedAuthHome(t)
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorAuthEntry)
	if !strings.Contains(out, "Auth Providers") || !strings.Contains(out, "xAI OAuth") {
		t.Fatalf("out = %q", out)
	}
}

func TestDoctorCommandReportsRemainingFindings(t *testing.T) {
	cases := []struct {
		issues, manual []string
		fixed          int
		fix            bool
		expected       int
	}{
		{[]string{"needs repair"}, nil, 0, false, 1},
		{nil, []string{"manual repair"}, 0, false, 1},
		{nil, nil, 0, false, 0},
		{nil, nil, 1, true, 0},
		{nil, []string{"remaining repair"}, 1, true, 1},
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			d, buf := doctorRawHome(t, "", "")
			check := func(_ context.Context, _ *Doctor, shouldFix bool) (*Finding, error) {
				if shouldFix != c.fix {
					t.Errorf("should fix %v, want %v", shouldFix, c.fix)
				}
				return &Finding{Issues: c.issues, ManualIssues: c.manual, Fixed: c.fixed}, nil
			}
			out, result := doctorWalk(t, d, buf, map[string]any{"fix": c.fix, "ack": nil, "live": false}, doctorCheckEntry{"", check})
			if result != c.expected {
				t.Fatalf("result = %d, want %d", result, c.expected)
			}
			for _, issue := range append(append([]string{}, c.issues...), c.manual...) {
				if !strings.Contains(out, issue) {
					t.Fatalf("%q missing from %q", issue, out)
				}
			}
		})
	}
}

func TestDoctorCliProcessStatusMatchesSummary(t *testing.T) {
	for _, unresolved := range []bool{false, true} {
		t.Run(fmt.Sprint(unresolved), func(t *testing.T) {
			d, buf := doctorRawHome(t, "", "")
			var issues []string
			if unresolved {
				issues = []string{"fixture unresolved problem"}
			}
			check := func(context.Context, *Doctor, bool) (*Finding, error) { return &Finding{Issues: issues}, nil }
			out, code := doctorWalk(t, d, buf, map[string]any{}, doctorCheckEntry{"", check})
			want, text := 0, "All checks passed"
			if unresolved {
				want, text = 1, "fixture unresolved problem"
			}
			if code != want || !strings.Contains(out, text) {
				t.Fatalf("code = %d out = %q", code, out)
			}
		})
	}
}

// Records catalog installs through the plugin manager hooks, every one failing at the catalog repo
func doctorFailingInstalls(t *testing.T) *[]string {
	t.Helper()
	calls := &[]string{}
	base := provider.PluginManagerHooks{
		CatalogEntry: func(name string) (string, bool) { return name, true },
		Install: func(context.Context, string, bool) (map[string]any, error) {
			return nil, errors.New("doctor tests install no plugins")
		},
		LoadError:       func(string) string { return "" },
		DependencyHomes: func() ([]string, error) { return nil, nil },
	}
	hooks := base
	hooks.Install = func(_ context.Context, name string, consent bool) (map[string]any, error) {
		*calls = append(*calls, fmt.Sprintf("%s:%v", name, consent))
		return map[string]any{"ok": false, "error": "fatal: unable to access the catalog repo"}, nil
	}
	provider.InstallPluginManagerHooks(hooks)
	t.Cleanup(func() { provider.InstallPluginManagerHooks(base) })
	return calls
}

const doctorCopilotAuxConfig = "model:\n  provider: nous\nauxiliary:\n  vision:\n    provider: copilot\n    model: gpt-4.1\n"

func TestDoctorInstallsNothingAndNamesTheInstall(t *testing.T) {
	d, buf := doctorRawHome(t, doctorCopilotAuxConfig, "")
	installs := doctorFailingInstalls(t)
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false, "ack": nil, "live": false}, doctorConfigFileEntry)
	if len(*installs) != 0 {
		t.Fatalf("installs = %q", *installs)
	}
	if _, err := os.Stat(filepath.Join(d.Home, "plugins", "github-copilot")); err == nil {
		t.Fatal("the plugin was installed by a diagnostic")
	}
	if !strings.Contains(out, "icebath plugins install github-copilot") {
		t.Fatalf("out = %q", out)
	}
}

func TestDoctorInATerminalFrontEndLeavesKeptNoticesForTheNextAgent(t *testing.T) {
	d, buf := doctorRawHome(t, doctorCopilotAuxConfig, "")
	doctorFailingInstalls(t)
	ctx := context.Background()
	provider.DeferStartupNotice(ctx, "kept by the gateway's start")
	prev := provider.SwapNoticesOnStderrForTests(false)
	t.Cleanup(func() { provider.SwapNoticesOnStderrForTests(prev) })
	provider.ServeNoticesOnStderr()
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false, "ack": nil, "live": false}, doctorConfigFileEntry)
	if strings.Contains(out, "kept by the gateway's start") {
		t.Fatalf("the kept notice was printed: %q", out)
	}
	kept := provider.ClaimKeptNotices(ctx)
	if len(kept) != 1 || kept[0] != "kept by the gateway's start" {
		t.Fatalf("kept = %q", kept)
	}
	config.ResetCache()
}

func TestNpmAuditOnlyFindingDoesNotOfferDoctorFix(t *testing.T) {
	out := &bytes.Buffer{}
	doctorPrintSummary(out, false, &Finding{ManualIssues: []string{"web workspace has 1 npm vulnerability"}})
	if !strings.Contains(out.String(), "web workspace has 1 npm vulnerability") || strings.Contains(out.String(), "icebath doctor --fix") {
		t.Fatalf("out = %q", out.String())
	}
	out.Reset()
	doctorPrintSummary(out, false, &Finding{Issues: []string{"fixable"}})
	if !strings.Contains(out.String(), "Tip: run 'icebath doctor --fix' to auto-fix what's possible.") {
		t.Fatalf("out = %q", out.String())
	}
}
