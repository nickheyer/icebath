package selfupdate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/spf13/cobra"
)

// Temp home for a status run with every status seam restored after the test
func stHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	savedCfg, savedReq, savedRes, savedLabel, savedEnv := statusLoadConfig, statusResolveRequestedProvider, statusResolveProvider,
		statusProviderLabel, statusGetEnvValue
	savedSnap, savedGwCfg, savedEntries, savedList := statusGatewaySnapshot, statusLoadGatewayConfig, statusPluginEntries, statusListProviders
	savedNous, savedInfo, savedProbe := statusNousAuthStatusLocal, statusNousAccountInfo, statusProbeLMStudio
	savedGetters := map[string]func(provider.PoolEnv) map[string]any{}
	for k, v := range statusOAuthGetters {
		savedGetters[k] = v
	}
	t.Cleanup(func() {
		statusLoadConfig, statusResolveRequestedProvider, statusResolveProvider, statusProviderLabel, statusGetEnvValue =
			savedCfg, savedReq, savedRes, savedLabel, savedEnv
		statusGatewaySnapshot, statusLoadGatewayConfig, statusPluginEntries, statusListProviders = savedSnap, savedGwCfg, savedEntries, savedList
		statusNousAuthStatusLocal, statusNousAccountInfo, statusProbeLMStudio = savedNous, savedInfo, savedProbe
		statusOAuthGetters = savedGetters
	})
	statusOAuthGetters = map[string]func(provider.PoolEnv) map[string]any{}
	for k, v := range savedGetters {
		statusOAuthGetters[k] = v
	}
	return h
}

func stEmptyStatus(provider.PoolEnv) map[string]any { return map[string]any{} }

// Pins the loaded config and the requested, resolved and labelled provider
func stPinProvider(cfg map[string]any, requested, resolved, label string) {
	if cfg != nil {
		statusLoadConfig = func(context.Context, string) (map[string]any, error) { return cfg, nil }
	}
	statusResolveRequestedProvider = func(context.Context, string) (string, error) { return requested, nil }
	statusResolveProvider = func(context.Context, string, string) (string, error) { return resolved, nil }
	if label != "" {
		statusProviderLabel = func(string) string { return label }
	}
}

// The provider and empty auth pins the xAI OAuth cases share
func stBaseXAIMocks(t *testing.T) string {
	t.Helper()
	h := stHome(t)
	stPinProvider(map[string]any{"model": "gpt-5.4"}, "openai-codex", "openai-codex", "OpenAI Codex")
	statusNousAuthStatusLocal = stEmptyStatus
	for _, k := range []string{"get_codex_auth_status", "get_qwen_auth_status", "get_minimax_oauth_auth_status"} {
		statusOAuthGetters[k] = stEmptyStatus
	}
	return h
}

func stShow(t *testing.T, h string, ns map[string]any) string {
	t.Helper()
	var out bytes.Buffer
	if _, err := ShowStatus(context.Background(), h, ns, &out); err != nil {
		t.Fatalf("show status: %v\n%s", err, out.String())
	}
	return out.String()
}

var stFull = map[string]any{"full": true, "deep": false}

func TestShowStatusAllDoesNotPrintKeenableKeyValue(t *testing.T) {
	h := stHome(t)
	sentinel := "NONSECRET_SENTINEL_VALUE_DO_NOT_PRINT_123456"
	t.Setenv("KEENABLE_API_KEY", sentinel)
	out := stShow(t, h, stFull)
	if !strings.Contains(out, "Keenable") || strings.Contains(out, sentinel) {
		t.Fatal(out)
	}
}

func TestShowStatusAllDoesNotPrintTavilyKeyValue(t *testing.T) {
	h := stHome(t)
	sentinel := "NONSECRET_SENTINEL_VALUE_DO_NOT_PRINT_TAVILY_123456"
	t.Setenv("TAVILY_API_KEY", sentinel)
	out := stShow(t, h, stFull)
	if !strings.Contains(out, "Tavily") || strings.Contains(out, sentinel) {
		t.Fatal(out)
	}
}

func TestShowStatusTermuxGatewaySectionSkipsSystemctl(t *testing.T) {
	t.Setenv("TERMUX_VERSION", "0.118.3")
	t.Setenv("PREFIX", "/data/data/com.termux/files/usr")
	h := stHome(t)
	stPinProvider(map[string]any{"model": "gpt-5.4"}, "openai-codex", "openai-codex", "OpenAI Codex")
	statusNousAuthStatusLocal = stEmptyStatus
	statusOAuthGetters["get_codex_auth_status"] = stEmptyStatus
	statusOAuthGetters["get_xai_oauth_auth_status"] = stEmptyStatus
	out := stShow(t, h, stFull)
	if strings.Contains(out, "systemd (user)") || !strings.Contains(out, "Termux / manual process") {
		t.Fatal(out)
	}
}

func TestShowStatusReportsVercelBackendContract(t *testing.T) {
	h := stHome(t)
	t.Setenv("TERMINAL_ENV", "vercel_sandbox")
	t.Setenv("TERMINAL_VERCEL_RUNTIME", "python3.13")
	t.Setenv("TERMINAL_CONTAINER_PERSISTENT", "true")
	t.Setenv("VERCEL_OIDC_TOKEN", "oidc-token")
	statusLoadConfig = func(context.Context, string) (map[string]any, error) {
		return map[string]any{"terminal": map[string]any{"backend": "vercel_sandbox"}}, nil
	}
	for _, k := range []string{"get_codex_auth_status", "get_qwen_auth_status", "get_xai_oauth_auth_status"} {
		statusOAuthGetters[k] = stEmptyStatus
	}
	out := stShow(t, h, stFull)
	for _, want := range []string{"vercel_sandbox", "python3.13", "VERCEL_OIDC_TOKEN", "SDK:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "oidc-token") {
		t.Fatal(out)
	}
}

func TestShowStatusXaiOAuthLoggedInShowsAuthStore(t *testing.T) {
	h := stBaseXAIMocks(t)
	statusOAuthGetters["get_xai_oauth_auth_status"] = func(provider.PoolEnv) map[string]any {
		return map[string]any{"logged_in": true, "auth_store": "/home/u/.icebath/auth.json"}
	}
	if out := stShow(t, h, stFull); !strings.Contains(out, "Auth file:  /home/u/.icebath/auth.json") {
		t.Fatal(out)
	}
}

func TestShowStatusXaiOAuthImportFailureDoesNotBreakOtherOauthProviders(t *testing.T) {
	h := stBaseXAIMocks(t)
	statusNousAuthStatusLocal = func(provider.PoolEnv) map[string]any { return map[string]any{"logged_in": true} }
	delete(statusOAuthGetters, "get_xai_oauth_auth_status")
	out := stShow(t, h, stFull)
	if !strings.Contains(out, "Nous Portal") || !strings.Contains(out, "MiniMax OAuth") {
		t.Fatal(out)
	}
}

func TestShowStatusXaiOAuthStatusFunctionExceptionDoesNotCrash(t *testing.T) {
	h := stBaseXAIMocks(t)
	statusOAuthGetters["get_xai_oauth_auth_status"] = func(provider.PoolEnv) map[string]any { panic("backend unreachable") }
	if out := stShow(t, h, stFull); !strings.Contains(out, "◆ Auth Providers") {
		t.Fatal(out)
	}
}

func TestShowStatusXaiOAuthStatusFunctionReturnsNoneDoesNotCrash(t *testing.T) {
	h := stBaseXAIMocks(t)
	statusOAuthGetters["get_xai_oauth_auth_status"] = func(provider.PoolEnv) map[string]any { return nil }
	if out := stShow(t, h, stFull); !strings.Contains(out, "xAI OAuth") {
		t.Fatal(out)
	}
}

func TestShowStatusReportsGatewaySessionLastActivity(t *testing.T) {
	h := stHome(t)
	stPinProvider(map[string]any{"model": "gpt-5.4"}, "openai-codex", "openai-codex", "OpenAI Codex")
	for _, k := range []string{"get_codex_auth_status", "get_qwen_auth_status", "get_xai_oauth_auth_status"} {
		statusOAuthGetters[k] = stEmptyStatus
	}
	path := filepath.Join(h, "state.db")
	db, err := state.Open(path, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"gw-old", "gw-new"} {
		if err := db.CreateSession(state.NewSession{ID: sid, Source: "telegram"}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	for sid, age := range map[string]float64{"gw-old": 7200, "gw-new": 90} {
		if _, err := conn.Exec("UPDATE sessions SET session_key=?, started_at=?, last_activity_at=? WHERE id=?", sid, now-age, now-age, sid); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := stShow(t, h, stFull)
	for _, want := range []string{"Active:       2 session(s)", "Last activity:", "1m ago"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Fatal("status wrote to state.db")
	}
}

// Argparse namespace of a status command line
func stStatusArgs(t *testing.T, argv ...string) map[string]any {
	t.Helper()
	c := commands.NewCLI()
	var ns map[string]any
	c.Bind("status", func(cmd *cobra.Command, args []string) error {
		ns = c.Namespace(cmd, args)
		return nil
	})
	if err := c.Execute(append([]string{"status"}, argv...)); err != nil {
		t.Fatal(err)
	}
	return ns
}

func TestStatusDefaultsToSummaryAndFullOrAllPrintsEverySection(t *testing.T) {
	h := stHome(t)
	statusListProviders = func(context.Context, provider.AuthenticatedProvidersOptions) ([]map[string]any, error) {
		return []map[string]any{{"name": "Acme AI"}}, nil
	}
	summary := stShow(t, h, stStatusArgs(t))
	if !strings.Contains(summary, "Providers:    Acme AI") || strings.Contains(summary, "◆ Environment") {
		t.Fatal(summary)
	}
	for _, flag := range []string{"--full", "--all"} {
		if out := stShow(t, h, stStatusArgs(t, flag)); !strings.Contains(out, "◆ Environment") {
			t.Fatalf("%s\n%s", flag, out)
		}
	}
}

func TestPlatformRowsFollowTheGatewayVerdictNotCheckFn(t *testing.T) {
	h := stHome(t)
	entries := []*gateway.PlatformEntry{}
	for _, n := range []string{"telegram", "discord"} {
		entries = append(entries, &gateway.PlatformEntry{Name: n, Label: strings.ToUpper(n[:1]) + n[1:], CheckFn: func() bool { return true }})
	}
	statusPluginEntries = func(context.Context) []*gateway.PlatformEntry { return entries }
	statusLoadGatewayConfig = func(context.Context) (*statusGatewayConfig, error) {
		return &statusGatewayConfig{connected: []gateway.Platform{"telegram"}, homes: map[string]*gateway.HomeChannel{}}, nil
	}
	out := stShow(t, h, stFull)
	rows := map[string][]bool{}
	for _, line := range strings.Split(out, "\n") {
		for _, name := range []string{"Telegram", "Discord"} {
			if strings.HasPrefix(strings.TrimSpace(line), name) {
				rows[name] = append(rows[name], strings.Contains(line, "not configured"))
			}
		}
	}
	if len(rows["Discord"]) != 1 || !rows["Discord"][0] || len(rows["Telegram"]) != 1 || rows["Telegram"][0] {
		t.Fatalf("%v\n%s", rows, out)
	}
	if summary := stShow(t, h, map[string]any{}); !strings.Contains(summary, "Platforms:    Telegram\n") {
		t.Fatal(summary)
	}
}

// Pins the env reads to an empty value except OPENAI_BASE_URL
func stPatchCommonStatusDeps(t *testing.T, openaiBaseURL string) string {
	t.Helper()
	h := stHome(t)
	statusGetEnvValue = func(_ context.Context, _ string, name string) (string, error) {
		if name == "OPENAI_BASE_URL" {
			return openaiBaseURL, nil
		}
		return "", nil
	}
	statusNousAuthStatusLocal = stEmptyStatus
	statusOAuthGetters["get_codex_auth_status"] = stEmptyStatus
	return h
}

func TestShowStatusDisplaysConfiguredDictModelAndProviderLabel(t *testing.T) {
	h := stPatchCommonStatusDeps(t, "")
	stPinProvider(map[string]any{"model": map[string]any{"default": "anthropic/claude-sonnet-4", "provider": "anthropic"}},
		"anthropic", "anthropic", "Anthropic")
	out := stShow(t, h, stFull)
	if !strings.Contains(out, "Model:        anthropic/claude-sonnet-4") || !strings.Contains(out, "Provider:     Anthropic") {
		t.Fatal(out)
	}
}

func TestShowStatusReportsEmptyLmstudioListingAsReachable(t *testing.T) {
	h := stPatchCommonStatusDeps(t, "")
	stPinProvider(map[string]any{"model": map[string]any{"default": "qwen/qwen3-coder-30b", "provider": "lmstudio",
		"base_url": "http://127.0.0.1:1234/v1"}}, "lmstudio", "lmstudio", "LM Studio")
	statusProbeLMStudio = func(context.Context, any, string, time.Duration) ([]string, bool, error) {
		return []string{}, true, nil
	}
	out := stShow(t, h, stFull)
	if !strings.Contains(out, "LM Studio") || !strings.Contains(out, "reachable (0 model(s)) at http://127.0.0.1:1234/v1") {
		t.Fatal(out)
	}
}

func TestSessionStatusDoesNotCreateDatabase(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "legacy"}[legacy], func(t *testing.T) {
			h := stHome(t)
			if legacy {
				dir := filepath.Join(h, "sessions")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(map[string]any{"fixture": map[string]any{}})
				if err := os.WriteFile(filepath.Join(dir, "sessions.json"), raw, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			s := &statusRun{ctx: context.Background(), h: h, w: &out, c: &cliui.Console{Out: &out}, config: map[string]any{}}
			if err := s.renderSessions(); err != nil {
				t.Fatal(err)
			}
			if legacy && !strings.Contains(out.String(), "1 session(s)") || !legacy && !strings.Contains(out.String(), "Active:") {
				t.Fatal(out.String())
			}
			if _, err := os.Stat(filepath.Join(h, "state.db")); err == nil {
				t.Fatal("status created state.db")
			}
		})
	}
}

// Effective provider label with the given config and OPENAI_BASE_URL behind an auto request resolving to OpenRouter
func stLabelWith(t *testing.T, cfg map[string]any, envBaseURL string) string {
	t.Helper()
	h := stHome(t)
	stPinProvider(cfg, "auto", "openrouter", "")
	statusGetEnvValue = func(_ context.Context, _ string, name string) (string, error) {
		if name == "OPENAI_BASE_URL" {
			return envBaseURL, nil
		}
		return "", nil
	}
	s := &statusRun{ctx: context.Background(), h: h}
	label, err := s.effectiveProviderLabel()
	if err != nil {
		t.Fatal(err)
	}
	return label
}

func TestConfigBaseURLLabelsCustom(t *testing.T) {
	if label := stLabelWith(t, map[string]any{"model": map[string]any{"base_url": "http://localhost:8080/v1"}}, ""); strings.Contains(label, "OpenRouter") {
		t.Fatal(label)
	}
}

func TestBlankBaseURLStaysOpenrouter(t *testing.T) {
	if label := stLabelWith(t, map[string]any{"model": map[string]any{"base_url": "   "}}, ""); !strings.Contains(label, "OpenRouter") {
		t.Fatal(label)
	}
}

func TestNonOpenrouterProviderUntouched(t *testing.T) {
	h := stHome(t)
	stPinProvider(nil, "anthropic", "anthropic", "")
	s := &statusRun{ctx: context.Background(), h: h}
	label, err := s.effectiveProviderLabel()
	if err != nil || strings.Contains(label, "OpenRouter") {
		t.Fatal(label, err)
	}
}

// Writes a jobs file with a UTF-8 BOM holding the given enabled flags
func stBOMJobs(t *testing.T, h string, enabled ...bool) {
	t.Helper()
	jobs := []map[string]any{}
	for i, on := range enabled {
		jobs = append(jobs, map[string]any{"id": "j" + string(rune('1'+i)), "enabled": on})
	}
	raw, _ := json.Marshal(map[string]any{"jobs": jobs})
	if err := os.MkdirAll(filepath.Join(h, "cron"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "cron", "jobs.json"), append([]byte("\xef\xbb\xbf"), raw...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDumpCronSummaryAcceptsUtf8Bom(t *testing.T) {
	h := t.TempDir()
	stBOMJobs(t, h, true, false)
	if got := dumpCronSummary(h); got != "1 active / 2 total" {
		t.Fatal(got)
	}
}

func TestStatusScheduledJobsAcceptsUtf8Bom(t *testing.T) {
	h := stHome(t)
	stBOMJobs(t, h, true, true)
	stPinProvider(map[string]any{"model": "gpt-5.4"}, "openai-codex", "openai-codex", "OpenAI Codex")
	statusOAuthGetters["get_codex_auth_status"] = stEmptyStatus
	statusOAuthGetters["get_xai_oauth_auth_status"] = stEmptyStatus
	out := stShow(t, h, stFull)
	if strings.Contains(out, "(error reading jobs file)") || !strings.Contains(out, "2 active, 2 total") {
		t.Fatal(out)
	}
}

func TestStatusLineWhenPaused(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	if line := statusEstopLine(ctx); line != "" {
		t.Fatal(line)
	}
	tools.Engage(ctx, "ops")
	line := statusEstopLine(ctx)
	if !strings.Contains(strings.ToLower(line), "paused") || !strings.Contains(line, "ops") {
		t.Fatal(line)
	}
	tools.Disengage(ctx)
	if line := statusEstopLine(ctx); line != "" {
		t.Fatal(line)
	}
}
