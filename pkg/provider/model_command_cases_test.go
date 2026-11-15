package provider

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Menu console claiming a terminal that reads the scripted keys and records everything the lists draw
func pickerKeysConsole(t *testing.T, keys string) *bytes.Buffer {
	t.Helper()
	out := &bytes.Buffer{}
	env := map[string]string{"TERM": "xterm-256color", "LANG": "C.UTF-8"}
	console := &cliui.Console{In: strings.NewReader(keys), Out: out,
		Getenv:   func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		StdinTTY: func() bool { return true }, StdoutTTY: func() bool { return true },
		TermSize: func() (int, int, error) { return 24, 100, nil }}
	swapFlowSeam(t, &pickerConsoleFn, func() *cliui.Console { return console })
	return out
}

func TestCmdModelForwardsNousLoginTlsOptions(t *testing.T) {
	h, ctx, _ := pickerCaseHome(t)
	writePickerConfig(t, h, "model:\n  default: gpt-5\n  provider: nous\n")
	swapFlowSeam(t, &pickerResolveAutoProviderFn, func(PoolEnv) (string, error) { return "nous", nil })
	swapFlowSeam(t, &pickerPromptProviderChoiceFn, func(io.Writer, []string, int, string) (int, bool) { return 0, true })
	swapFlowSeam(t, &setupPromptProviderChoiceFn, func(io.Writer, []string, int, string) (int, bool) { return 0, true })
	var captured *NousLoginArgs
	swapFlowSeam(t, &flowLoginNousFn, func(_ PoolEnv, _ io.Writer, args NousLoginArgs) error {
		captured = &args
		return nil
	})
	ns := map[string]any{"portal_url": "https://portal.nousresearch.com", "inference_url": "https://inference.nousresearch.com/v1",
		"client_id": "icebath-local", "scope": "openid profile", "no_browser": true, "timeout": 7.5,
		"ca_bundle": "/tmp/local-ca.pem", "insecure": true, "refresh": false}
	if _, err := CmdModel(ctx, ns, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := NousLoginArgs{PortalURL: "https://portal.nousresearch.com", InferenceURL: "https://inference.nousresearch.com/v1",
		ClientID: "icebath-local", Scope: "openid profile", NoBrowser: true, Timeout: 7500 * time.Millisecond,
		CABundle: "/tmp/local-ca.pem", Insecure: true}
	if captured == nil || *captured != want {
		t.Fatalf("login args %+v", captured)
	}
}

func TestStandaloneModelFlowOffersBackAndReopensProvider(t *testing.T) {
	_, ctx, _ := pickerCaseHome(t)
	drawn := pickerKeysConsole(t, "\r\x1b[D\r\r")
	var resolved []int
	swapFlowSeam(t, &selectProviderAndModelFn, func(context.Context, map[string]any) error {
		idx, err := cursesRadiolist("Select provider:", plainPickerRows([]string{"OpenAI"}), PickerRadiolistOptions{CancelReturns: -1})
		if err != nil {
			return err
		}
		resolved = append(resolved, idx)
		idx, err = cursesRadiolist("Select default model:", plainPickerRows([]string{"gpt-5.6-sol"}),
			PickerRadiolistOptions{CancelReturns: -1, Searchable: true, SearchLabels: []string{"gpt-5.6-sol"}})
		if err != nil {
			return err
		}
		resolved = append(resolved, idx)
		return nil
	})
	var out bytes.Buffer
	if _, err := CmdModel(ctx, map[string]any{"refresh": false}, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved, []int{0, 0, 0}) {
		t.Fatalf("resolved %v", resolved)
	}
	screen := ansi.Strip(drawn.String())
	provider, model := strings.Index(screen, "Select provider"), strings.Index(screen, "Select default model")
	if provider < 0 || model < provider || strings.Contains(screen[:model], "← back") || !strings.Contains(screen[model:], "← back") {
		t.Fatalf("the model list must offer the back step and the first provider list must not:\n%q", screen)
	}
	if strings.Count(screen, "› Select provider  OpenAI") != 2 || strings.Count(screen, "› Select default model  gpt-5.6-sol") != 1 {
		t.Fatalf("answers left on the terminal:\n%q", screen)
	}
}

func TestCmdModelRefreshClearsThePickerCache(t *testing.T) {
	_, ctx, _ := pickerCaseHome(t)
	cleared := 0
	swapFlowSeam(t, &clearProviderModelsCacheFn, func(context.Context) { cleared++ })
	swapFlowSeam(t, &selectProviderAndModelFn, func(context.Context, map[string]any) error { return nil })
	var out bytes.Buffer
	if _, err := CmdModel(ctx, map[string]any{"refresh": true}, &out); err != nil {
		t.Fatal(err)
	}
	if cleared != 1 || out.String() != "  Cleared model picker cache.\n" {
		t.Fatalf("cleared %d out %q", cleared, out.String())
	}
}

// Executable that exits zero on PATH, the hermes _fake_binary helper
func fakePickerBinary(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestHermesModelRoutesRegisteredPluginProfilesToTheGenericFlow(t *testing.T) {
	h, ctx, _ := pickerCaseHome(t)
	registerPluginForTest(t, Profile{Name: "example-acp", AuthType: AuthTypeExternalProcess, BaseURL: "acp://example",
		ProcessCommand: "example-acp-bin", FallbackModels: []string{"acp-a", "acp-b"}})
	registerPluginForTest(t, Profile{Name: "example-oauth", AuthType: AuthTypeOAuthExternal, BaseURL: "https://example.invalid/v1",
		FallbackModels: []string{"oauth-a"}})
	registerPluginForTest(t, Profile{Name: "example-key", AuthType: AuthTypeAPIKey, KeyEnv: []string{"EXAMPLE_API_KEY"},
		BaseURL: "https://example.invalid/v1", FallbackModels: []string{"key-a"}})
	fakePickerBinary(t, "example-acp-bin")
	addOAuthPoolEntry(t, modelsPoolEnv(ctx), "example-oauth", PooledCredential{AccessToken: "tok-1", RefreshToken: "rt-1"})
	swapFlowSeam(t, &offerReasoningAfterPickFn, func(*ModelFlowRun, string, int) error { return nil })
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(_ PoolEnv, _ io.Writer, p ModelSelectionPrompt) (string, bool) {
		return p.ModelIDs[len(p.ModelIDs)-1], true
	})
	var apiKeyFlowCalls []string
	swapFlowSeam(t, &modelFlowAPIKeyProviderFn, func(_ *ModelFlowRun, providerID string) error {
		apiKeyFlowCalls = append(apiKeyFlowCalls, providerID)
		return nil
	})
	pick := func(slug string) {
		swapFlowSeam(t, &pickProviderFn, func(*ModelFlowRun, map[string]any, string, map[string]string, *customProviderMap) (string, bool, error) {
			return slug, true, nil
		})
	}
	modelSection := func() map[string]any {
		cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
		if err != nil {
			t.Fatal(err)
		}
		m, _ := cfg["model"].(map[string]any)
		return m
	}
	for _, tc := range []struct{ slug, model, base string }{
		{"example-acp", "acp-b", "acp://example"},
		{"example-oauth", "oauth-a", "https://example.invalid/v1"},
	} {
		pick(tc.slug)
		if err := SelectProviderAndModel(ctx, nil); err != nil {
			t.Fatal(err)
		}
		m := modelSection()
		if m["provider"] != tc.slug || m["default"] != tc.model || m["base_url"] != tc.base {
			t.Fatalf("%s model %v", tc.slug, m)
		}
	}
	pick("example-key")
	if err := SelectProviderAndModel(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(apiKeyFlowCalls, []string{"example-key"}) {
		t.Fatalf("api key flow calls %v", apiKeyFlowCalls)
	}
	pick("no-such-provider")
	if err := SelectProviderAndModel(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if m := modelSection(); m["provider"] != "example-oauth" {
		t.Fatalf("model %v", m)
	}
}

// Flow run over the persistence fixture home, config.model starting as a plain string
func persistenceFlowRun(t *testing.T) (string, *ModelFlowRun) {
	t.Helper()
	h, ctx, out := pickerCaseHome(t)
	writePickerConfig(t, h, "model: some-old-model\n")
	if err := os.WriteFile(home.EnvPath(h), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := pickerCaseRun(t, ctx, h, out)
	r.CurrentModel = "old-model"
	return h, r
}

func TestProviderPersistsAfterModelSaveApiKeyProviderSavedWhenModelWasString(t *testing.T) {
	h, r := persistenceFlowRun(t)
	t.Setenv("KIMI_API_KEY", "sk-kimi-test-key")
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(PoolEnv, io.Writer, ModelSelectionPrompt) (string, bool) { return "kimi-k2.5", true })
	swapFlowSeam(t, &flowLineInputFn, func(string) (string, error) { return "", nil })
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return "", nil })
	if err := modelFlowAPIKeyProvider(r, "kimi-coding"); err != nil {
		t.Fatal(err)
	}
	model, ok := readConfig(t, h)["model"].(map[string]any)
	if !ok || model["provider"] != "kimi-coding" || model["default"] != "kimi-k2.5" {
		t.Fatalf("model %v", readConfig(t, h)["model"])
	}
}

func TestBaseUrlValidationEmptyBaseUrlKeepsDefault(t *testing.T) {
	_, r := persistenceFlowRun(t)
	t.Setenv("MINIMAX_API_KEY", "test-key")
	t.Setenv("MINIMAX_BASE_URL", "")
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(PoolEnv, io.Writer, ModelSelectionPrompt) (string, bool) { return "MiniMax-M2", true })
	swapFlowSeam(t, &flowLineInputFn, func(string) (string, error) { return "", nil })
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return "", nil })
	if err := modelFlowAPIKeyProvider(r, "minimax"); err != nil {
		t.Fatal(err)
	}
	if saved, _, _ := config.GetEnvValue(r.ctx(), r.home(), "MINIMAX_BASE_URL"); saved != "" {
		t.Fatalf("Empty input should not save a base URL, got %q", saved)
	}
}

func TestZaiEndpointPickerCustomProxyRejectsInvalidUrl(t *testing.T) {
	h, r := persistenceFlowRun(t)
	t.Setenv("GLM_API_KEY", "test-key")
	t.Setenv("GLM_BASE_URL", "")
	customIdx := len(ZaiEndpoints)
	swapFlowSeam(t, &setupPromptProviderChoiceFn, func(io.Writer, []string, int, string) (int, bool) { return customIdx, true })
	swapFlowSeam(t, &flowPromptModelSelectionFn, func(PoolEnv, io.Writer, ModelSelectionPrompt) (string, bool) { return "glm-5", true })
	swapFlowSeam(t, &flowLineInputFn, func(string) (string, error) { return "not-a-url", nil })
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return "not-a-url", nil })
	if err := modelFlowAPIKeyProvider(r, "zai"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	if model := cfg["model"].(map[string]any); model["base_url"] == "not-a-url" {
		t.Fatalf("model %v", model)
	}
}

func TestZaiEndpointPickerCurrentEndpointIsDefaultChoice(t *testing.T) {
	_, r := persistenceFlowRun(t)
	codingURL := ZaiEndpoints[2].BaseURL
	var capturedDefault = -1
	swapFlowSeam(t, &setupPromptProviderChoiceFn, func(_ io.Writer, choices []string, def int, title string) (int, bool) {
		capturedDefault = def
		return def, true
	})
	if got := selectZaiEndpoint(r, codingURL); capturedDefault != 2 || got != codingURL {
		t.Fatalf("default %d result %q", capturedDefault, got)
	}
}
