package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Isolated home bound to the context with setup prints captured, the hermes conftest home
func pickerCaseHome(t *testing.T) (string, context.Context, *bytes.Buffer) {
	t.Helper()
	h := newTestHome(t)
	for _, k := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "ICEBATH_INFERENCE_PROVIDER", "ICEBATH_MODEL", "LLM_MODEL"} {
		t.Setenv(k, "")
	}
	out := &bytes.Buffer{}
	restore := WithSetupOutput(out)
	t.Cleanup(restore)
	return h, home.WithOverride(context.Background(), h), out
}

// A flow run over the home with the given output, the config loaded like load_config
func pickerCaseRun(t *testing.T, ctx context.Context, h string, out io.Writer) *ModelFlowRun {
	t.Helper()
	r := &ModelFlowRun{Ctx: ctx, Env: modelsPoolEnv(ctx), Out: out}
	cfg, err := r.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	r.Config = cfg
	return r
}

// Answers the first row whose label starts with the prefix
func choosePrefix(prefix string) func(choices []string) (int, bool) {
	return func(choices []string) (int, bool) {
		for i, c := range choices {
			if strings.HasPrefix(c, prefix) {
				return i, true
			}
		}
		return 0, false
	}
}

// Writes the config document the way yaml.safe_dump writes the hermes fixture
func writePickerConfig(t *testing.T, h, body string) {
	t.Helper()
	if err := os.WriteFile(home.ConfigPath(h), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

func TestSaveAuxChoicePersistsToConfigYaml(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	r := pickerCaseRun(t, ctx, h, out)
	if err := saveAuxChoice(r, "vision", "openrouter", "google/gemini-2.5-flash", "", "", nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := r.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	v := cfg["auxiliary"].(map[string]any)["vision"].(map[string]any)
	if v["provider"] != "openrouter" || v["model"] != "google/gemini-2.5-flash" || v["base_url"] != "" || v["api_key"] != "" {
		t.Fatalf("vision %v", v)
	}
}

func TestSaveDelegationWritesTopLevelSection(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	r := pickerCaseRun(t, ctx, h, out)
	if err := saveAuxChoice(r, delegationTaskKey, "openrouter", "google/gemini-3-flash", "", "", nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := r.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := cfg["delegation"].(map[string]any)
	if d["provider"] != "openrouter" || d["model"] != "google/gemini-3-flash" || d["base_url"] != "" || d["api_key"] != "" {
		t.Fatalf("delegation %v", d)
	}
	if aux, ok := cfg["auxiliary"].(map[string]any); ok {
		if entry, ok := aux["delegation"].(map[string]any); ok && pyTruthy(entry["provider"]) {
			t.Fatal("delegation routing leaked into auxiliary.delegation")
		}
	}
}

func TestSaveDelegationAutoStoresEmptyProvider(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	r := pickerCaseRun(t, ctx, h, out)
	if err := saveAuxChoice(r, delegationTaskKey, "openrouter", "m", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := saveAuxChoice(r, delegationTaskKey, "auto", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := r.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := cfg["delegation"].(map[string]any)
	if d["provider"] != "" || d["model"] != "" || d["base_url"] != "" || d["api_key"] != "" {
		t.Fatalf("delegation %v", d)
	}
}

func TestResetAuxClearsDelegationRoutingPreservesSettings(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	r := pickerCaseRun(t, ctx, h, out)
	cfg, err := r.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := ensureDictSection(cfg, "delegation")
	d["provider"], d["model"], d["max_concurrent_children"] = "openrouter", "x", 7
	if err := config.Save(h, cfg); err != nil {
		t.Fatal(err)
	}
	n, err := resetAuxToAuto(r)
	if err != nil || n < 1 {
		t.Fatalf("reset %d %v", n, err)
	}
	cfg, err = r.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	d = cfg["delegation"].(map[string]any)
	if d["provider"] != "" || d["model"] != "" || d["max_concurrent_children"] != 7 {
		t.Fatalf("delegation %v", d)
	}
}

func TestDelegationCfgAsTaskProjection(t *testing.T) {
	if got := formatAuxCurrent(delegationCfgAsTask(map[string]any{})); got != "auto" {
		t.Fatalf("empty %q", got)
	}
	shaped := delegationCfgAsTask(map[string]any{"delegation": map[string]any{"provider": "nous", "model": "Hermes-4.5"}})
	if got := formatAuxCurrent(shaped); got != "nous · Hermes-4.5" {
		t.Fatalf("shaped %q", got)
	}
	if got := formatAuxCurrent(delegationCfgAsTask(map[string]any{"delegation": "bogus"})); got != "auto" {
		t.Fatalf("bogus %q", got)
	}
}

// Writes provider and model into config.yaml through load and save like the hermes _write_provider helper
func writeProviderConfig(t *testing.T, h, provider, model string) {
	t.Helper()
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	modelCfg, ok := cfg["model"].(map[string]any)
	if !ok {
		modelCfg = map[string]any{}
	}
	modelCfg["provider"], modelCfg["default"] = provider, model
	cfg["model"] = modelCfg
	if err := config.Save(h, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestClearStaleOpenaiBaseUrlClearsWhenProviderIsNamed(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	writeProviderConfig(t, h, "openrouter", "test-model")
	if err := config.SaveEnvValue(h, "OPENAI_BASE_URL", "http://localhost:11434/v1"); err != nil {
		t.Fatal(err)
	}
	if err := clearStaleOpenAIBaseURL(pickerCaseRun(t, ctx, h, out)); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := config.GetEnvValue(ctx, h, "OPENAI_BASE_URL"); v != "" {
		t.Fatalf("Expected OPENAI_BASE_URL to be cleared, got: %q", v)
	}
	if out.String() != "Cleared stale OPENAI_BASE_URL from .env (was: http://localhost:11434/v1)\n" {
		t.Fatalf("out %q", out.String())
	}
}

func TestClearStaleOpenaiBaseUrlPreservesWhenProviderIsCustom(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	writeProviderConfig(t, h, "custom", "test-model")
	if err := config.SaveEnvValue(h, "OPENAI_BASE_URL", "http://localhost:11434/v1"); err != nil {
		t.Fatal(err)
	}
	if err := clearStaleOpenAIBaseURL(pickerCaseRun(t, ctx, h, out)); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := config.GetEnvValue(ctx, h, "OPENAI_BASE_URL"); v != "http://localhost:11434/v1" {
		t.Fatalf("Expected OPENAI_BASE_URL to be preserved, got: %q", v)
	}
}

func TestClearStaleOpenaiBaseUrlTruncatesALongShownValue(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	writeProviderConfig(t, h, "nous", "m")
	long := "http://very-long-hostname.internal.example.com:11434/v1"
	if err := config.SaveEnvValue(h, "OPENAI_BASE_URL", long); err != nil {
		t.Fatal(err)
	}
	if err := clearStaleOpenAIBaseURL(pickerCaseRun(t, ctx, h, out)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Cleared stale OPENAI_BASE_URL from .env (was: "+long[:40]+"...)\n" {
		t.Fatalf("out %q", out.String())
	}
}

func TestReasoningMenuOrdersMinimalBeforeLow(t *testing.T) {
	var items []string
	selected := -1
	swapFlowSeam(t, &flowRadiolistFn, func(title string, rows [][]PickerSegment, opts PickerRadiolistOptions) (int, error) {
		for _, r := range rows {
			items = append(items, r[0].Text)
		}
		selected = opts.Selected
		return opts.Selected, nil
	})
	got, ok := promptReasoningEffortSelection(io.Discard, []string{"low", "minimal", "medium", "high"}, "medium", "")
	if !ok || got != "medium" {
		t.Fatalf("selected %q %v", got, ok)
	}
	var first []string
	for _, item := range items[:4] {
		first = append(first, strings.Fields(item)[0])
	}
	if !reflect.DeepEqual(first, []string{"minimal", "low", "medium", "high"}) || selected != 2 {
		t.Fatalf("items %v selected %d", items, selected)
	}
}

func TestCurrentReasoningEffortReadsDictForm(t *testing.T) {
	if got := currentReasoningEffort(map[string]any{"agent": map[string]any{"reasoning_effort": map[string]any{"enabled": true, "effort": "Thinking"}}}); got != "thinking" {
		t.Fatalf("dict %q", got)
	}
	if got := currentReasoningEffort(map[string]any{"agent": map[string]any{"reasoning_effort": map[string]any{"enabled": false}}}); got != "none" {
		t.Fatalf("disabled %q", got)
	}
	if got := currentReasoningEffort(map[string]any{"agent": map[string]any{"reasoning_effort": "high"}}); got != "high" {
		t.Fatalf("string %q", got)
	}
	if got := currentReasoningEffort(map[string]any{"agent": "bogus"}); got != "" {
		t.Fatalf("non dict agent %q", got)
	}
	cfg := map[string]any{"agent": "bogus"}
	setReasoningEffort(cfg, "low")
	if !reflect.DeepEqual(cfg, map[string]any{"agent": map[string]any{"reasoning_effort": "low"}}) {
		t.Fatalf("set %v", cfg)
	}
}

func TestHermesModelOffersReasoningWheneverAPickIsSaved(t *testing.T) {
	h, ctx, _ := pickerCaseHome(t)
	writeProviderConfig(t, h, "openrouter", "m")
	var prompted [][2]string
	swapFlowSeam(t, &promptMainReasoningEffortFn, func(_ *ModelFlowRun, model, provider string) error {
		prompted = append(prompted, [2]string{model, provider})
		return nil
	})
	swapFlowSeam(t, &clearStaleOpenAIBaseURLFn, func(*ModelFlowRun) error { return nil })
	pickSameModelOnNous := func(r *ModelFlowRun) error {
		if err := saveModelChoice(r.Env, r.CurrentModel); err != nil {
			return err
		}
		writeProviderConfig(t, h, "nous", r.CurrentModel)
		return nil
	}
	noChange := func(r *ModelFlowRun) error {
		fmt.Fprintln(r.Out, "No change.")
		return nil
	}
	for _, tc := range []struct {
		slug     string
		flow     func(r *ModelFlowRun) error
		expected [][2]string
	}{{"nous", pickSameModelOnNous, [][2]string{{"m", "nous"}}}, {"anthropic", noChange, nil}} {
		prompted = nil
		slug := tc.slug
		swapFlowSeam(t, &pickProviderFn, func(*ModelFlowRun, map[string]any, string, map[string]string, *customProviderMap) (string, bool, error) {
			return slug, true, nil
		})
		prev := providerModelFlows[slug]
		providerModelFlows[slug] = tc.flow
		t.Cleanup(func() { providerModelFlows[slug] = prev })
		if err := SelectProviderAndModel(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(prompted, tc.expected) {
			t.Fatalf("%s prompted %v want %v", slug, prompted, tc.expected)
		}
	}
}

// Registry api key provider config of a provider id
func registryPconfig(t *testing.T, ctx context.Context, id string) Profile {
	t.Helper()
	p, ok := registryProfileCtx(ctx, id)
	if !ok {
		t.Fatalf("no registry profile %s", id)
	}
	return p
}

// Answers the raw input with choice and the masked prompt with newKey like the hermes _run_prompt helper
func scriptPromptAPIKey(t *testing.T, choice, newKey string) *[]string {
	t.Helper()
	prompts := &[]string{}
	swapFlowSeam(t, &flowRawInputFn, func(_ io.Writer, prompt string) (string, error) {
		*prompts = append(*prompts, prompt)
		return choice, nil
	})
	swapFlowSeam(t, &flowSecretInputFn, func(string) (string, error) { return newKey, nil })
	return prompts
}

func TestPoolOnlyKeyDoesNotOfferOrExecuteClear(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	if err := os.WriteFile(home.EnvPath(h), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prompts := scriptPromptAPIKey(t, "c", "")
	env := modelsPoolEnv(ctx)
	key, abort, err := PromptAPIKey(env, out, registryPconfig(t, ctx, "deepseek"), "pool-secret", "deepseek", "credential_pool:deepseek")
	if err != nil || key != "pool-secret" || abort {
		t.Fatalf("key %q abort %v err %v", key, abort, err)
	}
	if len(*prompts) == 0 || strings.Contains((*prompts)[0], "[C]lear") || !strings.Contains((*prompts)[0], "[K]eep / [R]eplace") {
		t.Fatalf("prompts %q", *prompts)
	}
	if raw, _ := os.ReadFile(home.EnvPath(h)); len(raw) != 0 {
		t.Fatalf("env written %q", raw)
	}
	if strings.Contains(out.String(), "API key cleared") {
		t.Fatalf("out %q", out.String())
	}
}

func TestClearWipesEnvAndAborts(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("OTHER_VAR", "")
	for k, v := range map[string]string{"DEEPSEEK_API_KEY": "sk-existing", "OTHER_VAR": "keep-me"} {
		if err := config.SaveEnvValue(h, k, v); err != nil {
			t.Fatal(err)
		}
	}
	scriptPromptAPIKey(t, "c", "")
	key, abort, err := PromptAPIKey(modelsPoolEnv(ctx), out, registryPconfig(t, ctx, "deepseek"), "sk-existing", "", "")
	if err != nil || key != "" || !abort {
		t.Fatalf("key %q abort %v err %v", key, abort, err)
	}
	if v, _, _ := config.GetEnvValue(ctx, h, "DEEPSEEK_API_KEY"); v != "" {
		t.Fatalf("DEEPSEEK_API_KEY %q", v)
	}
	if v, _, _ := config.GetEnvValue(ctx, h, "OTHER_VAR"); v != "keep-me" {
		t.Fatalf("OTHER_VAR %q", v)
	}
}

func TestLmstudioFirstTimeEmptyUsesPlaceholder(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	t.Setenv("LM_API_KEY", "")
	scriptPromptAPIKey(t, "", "")
	key, abort, err := PromptAPIKey(modelsPoolEnv(ctx), out, registryPconfig(t, ctx, "lmstudio"), "", "lmstudio", "")
	if err != nil || key != LMStudioNoAuthPlaceholder || abort {
		t.Fatalf("key %q abort %v err %v", key, abort, err)
	}
	if v, _, _ := config.GetEnvValue(ctx, h, "LM_API_KEY"); v != LMStudioNoAuthPlaceholder {
		t.Fatalf("LM_API_KEY %q", v)
	}
}

// Swaps the three Claude Code credential seams the anthropic flow reads
func scriptClaudeSetup(t *testing.T, token string, creds map[string]any, valid bool) {
	t.Helper()
	swapFlowSeam(t, &anthRunOAuthSetupTokenFn, func(PoolEnv) (string, error) { return token, nil })
	swapFlowSeam(t, &anthReadClaudeCodeCredentialsFn, func(PoolEnv) map[string]any { return creds })
	swapFlowSeam(t, &anthIsClaudeCodeTokenValidFn, func(map[string]any) bool { return valid })
}

func TestRunAnthropicOauthFlowPrefersClaudeCodeCredentials(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	t.Setenv("ANTHROPIC_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	scriptClaudeSetup(t, "sk-ant-oat01-from-claude-setup", map[string]any{"accessToken": "cc-access-token", "refreshToken": "cc-refresh-token", "expiresAt": 9999999999999}, true)
	save := func(name, value string) error { return config.SaveEnvValue(h, name, value) }
	if err := save("ANTHROPIC_TOKEN", "stale-env-token"); err != nil {
		t.Fatal(err)
	}
	ok, err := runAnthropicOAuthFlow(ctx, modelsPoolEnv(ctx), out, save)
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	envVars := config.LoadEnv(h)
	if v, has := envVars["ANTHROPIC_TOKEN"]; !has || v != "" {
		t.Fatalf("ANTHROPIC_TOKEN %q %v", v, has)
	}
	if v, has := envVars["ANTHROPIC_API_KEY"]; !has || v != "" {
		t.Fatalf("ANTHROPIC_API_KEY %q %v", v, has)
	}
	if !strings.Contains(out.String(), "Claude Code credentials linked") {
		t.Fatalf("out %q", out.String())
	}
}

func TestRunAnthropicOauthFlowManualTokenStillPersists(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	t.Setenv("ANTHROPIC_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	scriptClaudeSetup(t, "", nil, false)
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return "sk-ant-oat01-manual-token", nil })
	swapFlowSeam(t, &flowSecretInputFn, func(string) (string, error) { return "sk-ant-oat01-manual-token", nil })
	save := func(name, value string) error { return config.SaveEnvValue(h, name, value) }
	ok, err := runAnthropicOAuthFlow(ctx, modelsPoolEnv(ctx), out, save)
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if v := config.LoadEnv(h)["ANTHROPIC_TOKEN"]; v != "sk-ant-oat01-manual-token" {
		t.Fatalf("ANTHROPIC_TOKEN %q", v)
	}
	if !strings.Contains(out.String(), "Setup-token saved") {
		t.Fatalf("out %q", out.String())
	}
}

func TestRunAnthropicOauthFlowGuidesAManualInstallWithoutTheClaudeCLI(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	t.Setenv("ANTHROPIC_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	swapFlowSeam(t, &anthRunOAuthSetupTokenFn, func(PoolEnv) (string, error) { return "", claudeCLIMissingError{} })
	swapFlowSeam(t, &flowSecretInputFn, func(string) (string, error) { return "", nil })
	ok, err := runAnthropicOAuthFlow(ctx, modelsPoolEnv(ctx), out, func(name, value string) error { return config.SaveEnvValue(h, name, value) })
	if err != nil || ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	want := "\n  Running 'claude setup-token' — follow the prompts below.\n  A browser window will open for you to authorize access.\n\n" +
		"\n  The 'claude' CLI is required for OAuth login.\n\n  To install and authenticate:\n\n" +
		"    1. Install Claude Code:  npm install -g @anthropic-ai/claude-code\n    2. Run:                  claude setup-token\n" +
		"    3. Follow the browser prompts to authorize\n    4. Re-run:               icebath model\n\n" +
		"  Or paste an existing setup-token now (sk-ant-oat-...):\n\n  Cancelled — install Claude Code and try again.\n"
	if out.String() != want {
		t.Fatalf("out %q", out.String())
	}
}

func TestSaveCustomProviderUsesProvidedName(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	writePickerConfig(t, h, "{}\n")
	if err := saveCustomProvider(pickerCaseRun(t, ctx, h, out), "http://localhost:11434/v1", "", "", 0, "Ollama", "", ""); err != nil {
		t.Fatal(err)
	}
	entries, _ := readConfig(t, h)["custom_providers"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["name"] != "Ollama" {
		t.Fatalf("entries %v", entries)
	}
	if out.String() != "  💾 Saved to custom providers as \"Ollama\" (edit in config.yaml)\n" {
		t.Fatalf("out %q", out.String())
	}
}

func TestSaveCustomProviderReferencesTheKeyInsteadOfInliningIt(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	writePickerConfig(t, h, "{}\n")
	if err := saveCustomProvider(pickerCaseRun(t, ctx, h, out), "http://localhost:11434/v1", "sk-secret", "", 0, "Ollama", "", "ICEBATH_CUSTOM_LOCALHOST_11434_API_KEY"); err != nil {
		t.Fatal(err)
	}
	entry := readConfig(t, h)["custom_providers"].([]any)[0].(map[string]any)
	if entry["key_env"] != "ICEBATH_CUSTOM_LOCALHOST_11434_API_KEY" {
		t.Fatalf("entry %v", entry)
	}
	if _, has := entry["api_key"]; has {
		t.Fatalf("api_key inlined %v", entry)
	}
	if raw, _ := os.ReadFile(home.ConfigPath(h)); strings.Contains(string(raw), "sk-secret") {
		t.Fatalf("secret in config %q", raw)
	}
}

// Registration entry build_auxiliary_task_entry stores for a plugin task without inherit_from
func pluginAuxEntry(key, name, desc, plugin string) map[string]any {
	return map[string]any{"key": key, "display_name": name, "description": desc,
		"defaults":     map[string]any{"provider": "auto", "model": "", "base_url": "", "api_key": "", "timeout": 60, "extra_body": map[string]any{}},
		"inherit_from": nil, "plugin": plugin, "plugin_key": plugin}
}

func TestAllAuxTasksIncludesPluginRegistered(t *testing.T) {
	withPluginAuxTasks(t, pluginAuxEntry("memory_retain_filter", "Memory retain filter", "hindsight pre-retain dedup/extract", "hindsight"))
	merged := allAuxTasks(context.Background())
	if !reflect.DeepEqual(merged[:len(auxTasks)], auxTasks) {
		t.Fatalf("built ins %v", merged)
	}
	found := false
	for _, task := range merged {
		if task.Key == "memory_retain_filter" {
			found = true
			if task != (auxTaskRow{Key: "memory_retain_filter", Name: "Memory retain filter", Desc: "hindsight pre-retain dedup/extract"}) {
				t.Fatalf("plugin task %+v", task)
			}
		}
	}
	if !found {
		t.Fatalf("plugin task missing %v", merged)
	}
}

func TestResetAuxToAutoResetsPluginTasks(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	withPluginAuxTasks(t, pluginAuxEntry("my_aux", "My Aux", "d", "plug"))
	r := pickerCaseRun(t, ctx, h, out)
	cfg, err := r.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ensureDictSection(cfg, "auxiliary")["my_aux"] = map[string]any{"provider": "openrouter", "model": "gpt-4o", "base_url": "", "api_key": ""}
	if err := config.Save(h, cfg); err != nil {
		t.Fatal(err)
	}
	n, err := resetAuxToAuto(r)
	if err != nil || n < 1 {
		t.Fatalf("reset %d %v", n, err)
	}
	cfg, err = r.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	mine := cfg["auxiliary"].(map[string]any)["my_aux"].(map[string]any)
	if mine["provider"] != "auto" || mine["model"] != "" {
		t.Fatalf("my_aux %v", mine)
	}
}

func TestRemoveCustomProviderFallsBackOnMenuRuntimeError(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	swapFlowSeam(t, &flowRadiolistFn, func(string, [][]PickerSegment, PickerRadiolistOptions) (int, error) {
		return 0, errors.New("Command '['tput', 'clear']' returned non-zero exit status 2.")
	})
	r := pickerCaseRun(t, ctx, h, out)
	cfg := r.Config
	cfg["custom_providers"] = []any{
		map[string]any{"name": "Local A", "base_url": "http://localhost:8001/v1"},
		map[string]any{"name": "Local B", "base_url": "http://localhost:8002/v1"},
	}
	if err := config.Save(h, config.Tree(cfg)); err != nil {
		t.Fatal(err)
	}
	responses := []string{"1"}
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) {
		next := responses[0]
		responses = responses[1:]
		return next, nil
	})
	if err := removeCustomProvider(r); err != nil {
		t.Fatal(err)
	}
	reloaded, err := r.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded["custom_providers"], []any{map[string]any{"name": "Local B", "base_url": "http://localhost:8002/v1"}}) {
		t.Fatalf("custom providers %v", reloaded["custom_providers"])
	}
}

func TestSelectProviderAndModelWarnsIfNamedCustomProviderDisappears(t *testing.T) {
	h, ctx, out := pickerCaseHome(t)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	cfg["custom_providers"] = []any{map[string]any{"name": "Local", "base_url": "http://localhost:8080/v1"}}
	if err := config.Save(h, cfg); err != nil {
		t.Fatal(err)
	}
	swapFlowSeam(t, &pickerPromptProviderChoiceFn, func(_ io.Writer, choices []string, def int, title string) (int, bool) {
		current, err := config.Load(h, config.NewSecrets(h).Lookup())
		if err != nil {
			t.Fatal(err)
		}
		current["custom_providers"] = []any{}
		if err := config.Save(h, current); err != nil {
			t.Fatal(err)
		}
		return choosePrefix("Local (localhost:8080/v1)")(choices)
	})
	swapFlowSeam(t, &pickerResolveAutoProviderFn, func(PoolEnv) (string, error) { return "", nil })
	swapFlowSeam(t, &modelFlowNamedCustomFn, func(*ModelFlowRun, map[string]any) error {
		t.Fatal("named custom flow should not run")
		return nil
	})
	if err := SelectProviderAndModel(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "selected saved custom provider is no longer available") {
		t.Fatalf("out %q", out.String())
	}
}

func TestGmiMainFlowSelectProviderAndModelRoutesGmiToGenericFlow(t *testing.T) {
	_, ctx, _ := pickerCaseHome(t)
	swapFlowSeam(t, &pickerResolveAutoProviderFn, func(PoolEnv) (string, error) { return "", nil })
	swapFlowSeam(t, &pickerPromptProviderChoiceFn, func(_ io.Writer, choices []string, def int, title string) (int, bool) {
		return choosePrefix("GMI Cloud")(choices)
	})
	recorded := ""
	swapFlowSeam(t, &modelFlowAPIKeyProviderFn, func(_ *ModelFlowRun, providerID string) error {
		recorded = providerID
		return nil
	})
	if err := SelectProviderAndModel(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if recorded != "gmi" {
		t.Fatalf("provider %q", recorded)
	}
}

// Provider menu labels of the first picker call, cancelled right after like the hermes capture helper
func capturePickerLabels(t *testing.T, ctx context.Context) []string {
	t.Helper()
	var labels []string
	swapFlowSeam(t, &pickerPromptProviderChoiceFn, func(_ io.Writer, choices []string, def int, title string) (int, bool) {
		if labels == nil {
			labels = append([]string{}, choices...)
		}
		return 0, false
	})
	if err := SelectProviderAndModel(ctx, nil); err != nil {
		t.Fatal(err)
	}
	return labels
}

func TestCliPickerHidesExcludedProvider(t *testing.T) {
	h, ctx, _ := pickerCaseHome(t)
	writePickerConfig(t, h, "custom_providers: []\nmodel: old-model\nmodel_catalog:\n  excluded_providers:\n  - openrouter\n")
	labels := capturePickerLabels(t, ctx)
	if len(labels) == 0 {
		t.Fatal("provider menu was empty")
	}
	for _, l := range labels {
		if strings.Contains(l, "OpenRouter") {
			t.Fatalf("OpenRouter should be hidden by excluded_providers, got: %v", labels)
		}
	}
}

func TestCliPickerHidesExcludedProviderByAlias(t *testing.T) {
	h, ctx, _ := pickerCaseHome(t)
	targetAlias, targetSlug := "glm", "zai"
	if CatalogProviderAliases()[targetAlias] != targetSlug || !IsCanonicalProvider(targetSlug) {
		t.Fatalf("the first hermes alias %s no longer names the canonical %s", targetAlias, targetSlug)
	}
	fragment := labelOr(catalogProviderLabelsCopy(), targetSlug)
	contains := func(labels []string) bool {
		for _, l := range labels {
			if strings.Contains(l, fragment) {
				return true
			}
		}
		return false
	}
	writePickerConfig(t, h, "custom_providers: []\nmodel: old-model\n")
	if baseline := capturePickerLabels(t, ctx); !contains(baseline) {
		t.Fatalf("sanity: %s (%q) should appear by default; labels=%v", targetSlug, fragment, baseline)
	}
	writePickerConfig(t, h, "custom_providers: []\nmodel: old-model\nmodel_catalog:\n  excluded_providers:\n  - "+targetAlias+"\n")
	if excluded := capturePickerLabels(t, ctx); contains(excluded) {
		t.Fatalf("excluding alias %q should hide %s; labels=%v", targetAlias, targetSlug, excluded)
	}
}

func TestCliPickerEmptyExcludedIsNoop(t *testing.T) {
	h, ctx, _ := pickerCaseHome(t)
	writePickerConfig(t, h, "custom_providers: []\nmodel: old-model\nmodel_catalog:\n  excluded_providers: []\n")
	excluded := capturePickerLabels(t, ctx)
	writePickerConfig(t, h, "custom_providers: []\nmodel: old-model\n")
	baseline := capturePickerLabels(t, ctx)
	if !reflect.DeepEqual(excluded, baseline) {
		t.Fatalf("excluded %v baseline %v", excluded, baseline)
	}
}

func TestPromptModelSelectionUsesLineEditorForCustomModel(t *testing.T) {
	scriptPickerTerminal(t, func(_ string, choices [][]PickerSegment, _ PickerRadiolistOptions) (int, error) {
		return len(choices) - 2, nil
	}, nil, editedModelLine, nil)
	if got, ok := promptModelSelection(PoolEnv{}, io.Discard, ModelSelectionPrompt{ModelIDs: []string{"vendor/default-model"}}); !ok || got != "vendor/edited-model" {
		t.Fatalf("got %q %v", got, ok)
	}
}

// Picker row keys for a config with no active provider and no custom providers, the hermes _picker_keys helper
func pickerRowKeys(t *testing.T, cfg map[string]any) []string {
	t.Helper()
	rows, _, err := buildProviderPickerRows(context.Background(), cfg, "", map[string]string{}, newCustomProviderMap())
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i] = row.Key
	}
	return keys
}

func TestLocalModelsRowFollowsTheEnginePinAndExclusions(t *testing.T) {
	pickerCaseHome(t)
	has := func(keys []string) bool {
		for _, k := range keys {
			if k == "llamacpp" {
				return true
			}
		}
		return false
	}
	reason, err := UnavailableReason("cpu", "")
	if has(pickerRowKeys(t, map[string]any{})) != (err == nil && reason == "") {
		t.Fatalf("llamacpp row %v reason %q err %v", pickerRowKeys(t, map[string]any{}), reason, err)
	}
	if has(pickerRowKeys(t, map[string]any{"model_catalog": map[string]any{"excluded_providers": []any{"llama.cpp"}}})) {
		t.Fatal("llama.cpp exclusion kept the local models row")
	}
}

func TestProviderPickerRowsRaiseOnAMalformedModelCatalog(t *testing.T) {
	pickerCaseHome(t)
	for cfg, want := range map[string]map[string]any{
		"AttributeError: 'list' object has no attribute 'get'": {"model_catalog": []any{"openrouter"}},
		"TypeError: 'int' object is not iterable":              {"model_catalog": map[string]any{"excluded_providers": 5}},
	} {
		if _, _, err := buildProviderPickerRows(context.Background(), want, "", map[string]string{}, newCustomProviderMap()); err == nil || err.Error() != cfg {
			t.Fatalf("err %v want %s", err, cfg)
		}
	}
}

type auxHomeKey struct{}

func TestAllAuxTasksListsPluginsOfTheContextHome(t *testing.T) {
	saved := pluginAuxiliaryTasksFn
	t.Cleanup(func() { pluginAuxiliaryTasksFn = saved })
	pluginAuxiliaryTasksFn = func(ctx context.Context) []map[string]any {
		name, _ := ctx.Value(auxHomeKey{}).(string)
		return []map[string]any{pluginAuxEntry(name+"_task", name+" task", "d", name)}
	}
	for _, home := range []string{"alpha", "beta"} {
		ctx := context.WithValue(context.Background(), auxHomeKey{}, home)
		rows := allAuxTasks(ctx)
		last := rows[len(rows)-1]
		if len(rows) != len(auxTasks)+1 || last.Key != home+"_task" || last.Name != home+" task" {
			t.Fatalf("home %s listed %+v", home, last)
		}
		if got := auxTaskDisplayName(ctx, home+"_task"); got != home+" task" {
			t.Fatalf("home %s display name %q", home, got)
		}
	}
}
