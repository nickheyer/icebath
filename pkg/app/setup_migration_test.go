package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Recorded hermes result of one setup_migration call
type migrationRef struct {
	Value any      `json:"value"`
	Out   string   `json:"out"`
	Err   *string  `json:"err"`
	Calls []string `json:"calls"`
}

func loadMigrationRefs(t *testing.T) map[string]migrationRef {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "setup_migration.json"))
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]migrationRef{}
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	return refs
}

// Unsets every process credential the migration probes could read
func clearCredentialEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key := strings.SplitN(kv, "=", 2)[0]
		if strings.HasSuffix(key, "_API_KEY") || strings.HasSuffix(key, "_TOKEN") || strings.HasSuffix(key, "_SECRET") ||
			strings.HasSuffix(key, "_URL") || strings.HasSuffix(key, "_KEY") || strings.HasPrefix(key, "AWS_") {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
}

// Python value of a Go summary, None for an empty one
func summaryValue(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestModelSectionHasCredentialsMatchesHermes(t *testing.T) {
	refs := loadMigrationRefs(t)
	for _, c := range []struct {
		name string
		cfg  config.Tree
		env  map[string]string
		auth map[string]any
	}{
		{"creds_active", config.Tree{}, nil, map[string]any{"version": 1, "providers": map[string]any{}, "active_provider": "nous"}},
		{"creds_openrouter_config", config.Tree{"model": map[string]any{"provider": "OpenRouter "}}, map[string]string{"OPENROUTER_API_KEY": "sk-or"}, nil},
		{"creds_openai_key_only", config.Tree{}, map[string]string{"OPENAI_API_KEY": "sk"}, nil},
		{"creds_deepseek_config", config.Tree{"model": map[string]any{"provider": "deepseek"}}, map[string]string{"DEEPSEEK_API_KEY": "ds"}, nil},
		{"creds_deepseek_no_config", config.Tree{}, map[string]string{"DEEPSEEK_API_KEY": "ds"}, nil},
		{"creds_claude_oauth_only", config.Tree{}, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "c"}, nil},
		{"creds_gh_token_only", config.Tree{}, map[string]string{"GH_TOKEN": "gh"}, nil},
		{"creds_none", config.Tree{"model": map[string]any{"provider": "anthropic"}}, nil, nil},
		{"creds_bad_provider", config.Tree{"model": map[string]any{"provider": 5}}, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			clearCredentialEnv(t)
			w := newWizardRun(t, "", true)
			w.seed(t, "", c.auth, c.env)
			got, err := provider.ModelSectionHasCredentials(w.ctx, c.cfg)
			ref := refs[c.name]
			if ref.Err != nil {
				if err == nil || err.Error() != *ref.Err {
					t.Fatalf("error %v want %s", err, *ref.Err)
				}
				return
			}
			if err != nil || got != ref.Value {
				t.Fatalf("got %v %v want %v", got, err, ref.Value)
			}
		})
	}
}

func TestSectionSummariesMatchHermes(t *testing.T) {
	refs := loadMigrationRefs(t)
	clearCredentialEnv(t)
	w := newWizardRun(t, "", true)
	w.seed(t, "", nil, map[string]string{"OPENROUTER_API_KEY": "sk-or"})
	for name, cfg := range map[string]config.Tree{
		"model_str": {"model": "  gpt-x  "}, "model_dict": {"model": map[string]any{"default": "m1", "model": "m2"}},
		"model_dict_model": {"model": map[string]any{"model": "m2"}}, "model_empty": {"model": map[string]any{}}, "model_blank": {"model": "  "},
		"model_none": {}, "model_number": {"model": map[string]any{"default": 7}},
	} {
		got, err := setupModelSummary(w.ctx, cfg)
		if err != nil || summaryValue(got) != refs[name].Value {
			t.Fatalf("%s %q %v want %v", name, got, err, refs[name].Value)
		}
	}
	if err := os.Remove(filepath.Join(w.home, ".env")); err != nil {
		t.Fatal(err)
	}
	config.InvalidateEnvCache()
	if got, err := setupModelSummary(w.ctx, config.Tree{"model": "gpt-x"}); err != nil || summaryValue(got) != refs["model_no_creds"].Value {
		t.Fatalf("no creds %q %v", got, err)
	}
	statuses := map[string]string{"Telegram (Bot API)": "configured", "Matrix": "configured + E2EE", "(weird)": "configured", "Slack": "not configured", "X": ""}
	var calls []string
	platforms := []gateway.SetupPlatform{{Label: "Telegram (Bot API)"}, {Label: "Matrix"}, {Label: "(weird)"}, {Label: "Slack"}, {Label: "X"}}
	swapSeam(t, &setupAllPlatforms, func(context.Context) []gateway.SetupPlatform { return platforms })
	swapSeam(t, &setupPlatformStatus, func(_ context.Context, p gateway.SetupPlatform) (string, error) {
		calls = append(calls, p.Label)
		return statuses[p.Label], nil
	})
	if got, err := setupGatewaySummary(w.ctx); err != nil || summaryValue(got) != refs["gateway_summary"].Value || !slices.Equal(calls, refs["gateway_summary"].Calls) {
		t.Fatalf("gateway %q %v calls %v", got, err, calls)
	}
	platforms = []gateway.SetupPlatform{{Label: "Slack"}}
	if got, _ := setupGatewaySummary(w.ctx); summaryValue(got) != refs["gateway_none"].Value {
		t.Fatalf("gateway none %q", got)
	}
	w.seed(t, "", nil, map[string]string{"ELEVENLABS_API_KEY": "el", "FIRECRAWL_API_KEY": "fc"})
	config.InvalidateEnvCache()
	if got, err := setupToolsSummary(w.ctx); err != nil || summaryValue(got) != refs["tools_summary"].Value {
		t.Fatalf("tools %q %v", got, err)
	}
	if err := os.Remove(filepath.Join(w.home, ".env")); err != nil {
		t.Fatal(err)
	}
	config.InvalidateEnvCache()
	if got, _ := setupToolsSummary(w.ctx); summaryValue(got) != refs["tools_none"].Value {
		t.Fatalf("tools none %q", got)
	}
	for name, c := range map[string]struct {
		cfg config.Tree
		key string
	}{
		"cfg_terminal": {config.Tree{"terminal": map[string]any{"backend": "docker"}}, "terminal"}, "cfg_agent_default": {config.Tree{}, "agent"},
		"cfg_agent_none": {config.Tree{"agent": map[string]any{"max_turns": nil}}, "agent"}, "cfg_unknown": {config.Tree{}, "tts"},
	} {
		if got, err := getSectionConfigSummary(w.ctx, c.cfg, c.key); err != nil || summaryValue(got) != refs[name].Value {
			t.Fatalf("%s %q %v", name, got, err)
		}
	}
}

func TestSkipConfiguredSectionMatchesHermes(t *testing.T) {
	refs := loadMigrationRefs(t)
	for _, c := range []struct {
		name, stdin, key, label string
		cfg                     config.Tree
	}{
		{"skip_keep", "\n", "terminal", "Terminal Backend", config.Tree{"terminal": map[string]any{"backend": "ssh"}}},
		{"skip_reconfigure", "y\n", "agent", "Agent Settings", config.Tree{}},
		{"skip_nothing", "", "tools", "Tools", config.Tree{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			clearCredentialEnv(t)
			w := newWizardRun(t, c.stdin, true)
			t.Cleanup(provider.WithSetupConsole(console()))
			skipped, err := skipConfiguredSection(w.ctx, c.cfg, c.key, c.label)
			if err != nil || skipped != refs[c.name].Value || w.out.String() != refs[c.name].Out {
				t.Fatalf("skip %v %v screen %q want %v %q", skipped, err, w.out.String(), refs[c.name].Value, refs[c.name].Out)
			}
		})
	}
}
