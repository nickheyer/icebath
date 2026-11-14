package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Curses stand in replaying scripted key codes and recording every text write, the hermes FakeStdscr
type pickerFakeScreen struct {
	keys   []int
	writes []string
}

func (f *pickerFakeScreen) Getch() (int, error) {
	if len(f.keys) == 0 {
		return 0, errors.New("menu requested a key after the scripted keys")
	}
	k := f.keys[0]
	f.keys = f.keys[1:]
	return k, nil
}
func (f *pickerFakeScreen) Timeout(int)          {}
func (f *pickerFakeScreen) Clear()               {}
func (f *pickerFakeScreen) GetMaxYX() (int, int) { return 24, 100 }
func (f *pickerFakeScreen) AddNStr(_, _ int, text string, n int, _ cliui.Attr) error {
	if rs := []rune(text); len(rs) > n {
		text = string(rs[:max(n, 0)])
	}
	f.writes = append(f.writes, text)
	return nil
}
func (f *pickerFakeScreen) Refresh() error              { return nil }
func (f *pickerFakeScreen) HasColors() bool             { return false }
func (f *pickerFakeScreen) InitColors(bool) error       { return nil }
func (f *pickerFakeScreen) InitPairs(map[int]int) error { return nil }
func (f *pickerFakeScreen) Colors() int                 { return 0 }

// Menu console claiming a terminal whose full screen opens pop the scripted screens in order
func pickerScreensConsole(t *testing.T, screens *[]*pickerFakeScreen) {
	t.Helper()
	console := &cliui.Console{In: strings.NewReader(""), Out: io.Discard,
		Getenv:   func(string) (string, bool) { return "", false },
		StdinTTY: func() bool { return true }, StdoutTTY: func() bool { return true },
		OpenScreen: func() (cliui.Screen, func(), error) {
			if len(*screens) == 0 {
				return nil, nil, errors.New("no scripted screen left")
			}
			next := (*screens)[0]
			*screens = (*screens)[1:]
			return next, func() {}, nil
		}}
	swapFlowSeam(t, &pickerConsoleFn, func() *cliui.Console { return console })
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

func TestStandaloneModelFlowRendersPreviousAndReopensProvider(t *testing.T) {
	_, ctx, _ := pickerCaseHome(t)
	providerScreen := &pickerFakeScreen{keys: []int{13}}
	modelBackScreen := &pickerFakeScreen{keys: []int{cliui.KeyLeft}}
	providerReselectScreen := &pickerFakeScreen{keys: []int{13}}
	modelSelectScreen := &pickerFakeScreen{keys: []int{13}}
	screens := []*pickerFakeScreen{providerScreen, modelBackScreen, providerReselectScreen, modelSelectScreen}
	pickerScreensConsole(t, &screens)
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
	if !reflect.DeepEqual(resolved, []int{0, 0, 0}) || len(screens) != 0 {
		t.Fatalf("resolved %v screens left %d", resolved, len(screens))
	}
	previous := false
	for _, w := range modelBackScreen.writes {
		if strings.Contains(w, "← previous") {
			previous = true
		}
	}
	if !previous {
		t.Fatalf("model screen writes %q", modelBackScreen.writes)
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
