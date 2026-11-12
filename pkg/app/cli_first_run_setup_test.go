package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/provider"
)

// A compact one turn CLI over a temp home, its console reading stdin and the given TTY answer
func firstRunShell(t *testing.T, stdin string, tty bool, cliConfig map[string]any) (*CLI, *bytes.Buffer) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("LLM_MODEL", "")
	t.Setenv("ICEBATH_MAX_ITERATIONS", "")
	out := &bytes.Buffer{}
	t.Cleanup(SetStreams(&cliui.Console{In: strings.NewReader(stdin), Out: out, StdinTTY: func() bool { return tty }, StdoutTTY: func() bool { return false }}, &bytes.Buffer{}))
	if cliConfig == nil {
		cliConfig = map[string]any{}
	}
	for key, value := range map[string]any{"model": map[string]any{"default": "anthropic/claude-opus-4.6", "provider": "auto"},
		"display": map[string]any{"compact": true}, "agent": map[string]any{}, "terminal": map[string]any{"env_type": "local"}} {
		if _, ok := cliConfig[key]; !ok {
			cliConfig[key] = value
		}
	}
	SetCLIConfig(cliConfig)
	t.Cleanup(func() { SetCLIConfig(nil) })
	compact, turns := true, 1
	c, err := NewCLI(home.WithOverride(context.Background(), h), CLIOptions{Compact: &compact, MaxTurns: &turns})
	if err != nil {
		t.Fatalf("NewCLI: %v", err)
	}
	t.Cleanup(func() {
		if c.sessionDB != nil {
			c.sessionDB.Close()
		}
	})
	out.Reset()
	return c, out
}

// Replaces the runtime resolver for the test with one answering fn
func swapRuntimeResolver(t *testing.T, fn func() (*provider.Runtime, error)) {
	t.Helper()
	swapSeam(t, &resolveRuntimeProviderFn, func(context.Context, string, string, string, string, string) (*provider.Runtime, error) { return fn() })
}

// Replaces the provider picker, counting the runs
func swapFirstRunPicker(t *testing.T, fn func(ctx context.Context) error) *int {
	t.Helper()
	calls := new(int)
	swapSeam(t, &firstRunProviderPicker, func(ctx context.Context, _ map[string]any) error {
		*calls++
		return fn(ctx)
	})
	return calls
}

var noProviderConfigured = &provider.AuthError{Message: "No inference provider configured.", Code: "no_provider_configured"}

func TestCredentialsReadyFalseWhenNoProvider(t *testing.T) {
	c, _ := firstRunShell(t, "", false, nil)
	swapRuntimeResolver(t, func() (*provider.Runtime, error) { return nil, noProviderConfigured })
	if c.runtimeCredentialsReady() {
		t.Fatal("ready without a provider")
	}
}

func TestCredentialsReadyFalseOnEmptyOpenrouterKey(t *testing.T) {
	c, _ := firstRunShell(t, "", false, nil)
	swapRuntimeResolver(t, func() (*provider.Runtime, error) {
		return &provider.Runtime{Provider: "openrouter", APIKey: "", BaseURL: "https://openrouter.ai/api/v1", Source: "env/config"}, nil
	})
	if c.runtimeCredentialsReady() {
		t.Fatal("ready with an empty OpenRouter key")
	}
}

func TestCredentialsReadyTrueWithKey(t *testing.T) {
	c, _ := firstRunShell(t, "", false, nil)
	swapRuntimeResolver(t, func() (*provider.Runtime, error) {
		return &provider.Runtime{Provider: "openrouter", APIKey: "sk-test", BaseURL: "https://openrouter.ai/api/v1", Source: "env/config"}, nil
	})
	if !c.runtimeCredentialsReady() {
		t.Fatal("not ready with a key")
	}
}

func TestCredentialsReadyTrueForKeylessLocalEndpoint(t *testing.T) {
	c, _ := firstRunShell(t, "", false, nil)
	swapRuntimeResolver(t, func() (*provider.Runtime, error) {
		return &provider.Runtime{Provider: "custom", APIKey: "", BaseURL: "http://localhost:11434/v1", Source: "custom_provider"}, nil
	})
	if !c.runtimeCredentialsReady() {
		t.Fatal("keyless local endpoint not ready")
	}
}

func TestCredentialsReadyTrueForCallableBearerProvider(t *testing.T) {
	c, _ := firstRunShell(t, "", false, nil)
	swapRuntimeResolver(t, func() (*provider.Runtime, error) {
		return &provider.Runtime{Provider: "azure-foundry", TokenSource: func(context.Context) (string, error) { return "tok", nil },
			BaseURL: "https://foundry.example/v1", Source: "entra"}, nil
	})
	if !c.runtimeCredentialsReady() {
		t.Fatal("callable bearer not ready")
	}
}

func TestCredentialsReadyNeverPrints(t *testing.T) {
	c, out := firstRunShell(t, "", false, nil)
	swapRuntimeResolver(t, func() (*provider.Runtime, error) { return nil, noProviderConfigured })
	c.runtimeCredentialsReady()
	if out.Len() != 0 {
		t.Fatalf("probe printed %q", out.String())
	}
}

// Writes the model section the picker saves
func writePickedModel(t *testing.T, c *CLI, providerName, model string) {
	t.Helper()
	h := home.Dir(c.ctx)
	cfg, err := config.LoadContext(c.ctx, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg["model"] = map[string]any{"provider": providerName, "default": model}
	if err := config.SaveConfig(c.ctx, h, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestOfferFirstRunSetupRoutesIntoSharedPicker(t *testing.T) {
	c, _ := firstRunShell(t, "y\n", true, nil)
	calls := swapFirstRunPicker(t, func(context.Context) error {
		writePickedModel(t, c, "nous", "hermes-4-405b")
		return nil
	})
	swapRuntimeResolver(t, func() (*provider.Runtime, error) {
		return &provider.Runtime{Provider: "nous", APIKey: "portal-token", BaseURL: "https://inference-api.nousresearch.com/v1", Source: "oauth"}, nil
	})
	if !c.offerFirstRunSetup() || *calls != 1 || c.requestedProvider != "nous" || c.model != "hermes-4-405b" || c.agent != nil {
		t.Fatalf("calls %d provider %q model %q agent %v", *calls, c.requestedProvider, c.model, c.agent)
	}
}

func TestOfferFirstRunSetupReResolvesReasoningForPickedModel(t *testing.T) {
	c, _ := firstRunShell(t, "y\n", true, map[string]any{"agent": map[string]any{"reasoning_effort": "medium",
		"reasoning_overrides": map[string]any{"hermes-4-405b": "high"}}})
	if c.reasoningConfig["effort"] != "medium" {
		t.Fatalf("launch reasoning %v", c.reasoningConfig)
	}
	swapFirstRunPicker(t, func(context.Context) error {
		writePickedModel(t, c, "nous", "hermes-4-405b")
		return nil
	})
	swapRuntimeResolver(t, func() (*provider.Runtime, error) {
		return &provider.Runtime{Provider: "nous", APIKey: "portal-token", BaseURL: "https://inference-api.nousresearch.com/v1"}, nil
	})
	if !c.offerFirstRunSetup() || c.model != "hermes-4-405b" || c.reasoningConfig["effort"] != "high" {
		t.Fatalf("model %q reasoning %v", c.model, c.reasoningConfig)
	}
}

func TestOfferFirstRunSetupDeclined(t *testing.T) {
	c, _ := firstRunShell(t, "n\n", true, nil)
	calls := swapFirstRunPicker(t, func(context.Context) error {
		t.Error("picker must not run when declined")
		return nil
	})
	if c.offerFirstRunSetup() || *calls != 0 {
		t.Fatalf("declined offer configured, picker ran %d times", *calls)
	}
}

func TestOfferFirstRunSetupPickerCancelIsGraceful(t *testing.T) {
	c, _ := firstRunShell(t, "\n", true, nil)
	calls := swapFirstRunPicker(t, func(context.Context) error { return cliui.ErrInterrupt })
	if c.offerFirstRunSetup() || *calls != 1 {
		t.Fatalf("cancelled picker reported configured, ran %d times", *calls)
	}
}

func TestEmptyKeyErrorNamesActualProvider(t *testing.T) {
	c, out := firstRunShell(t, "", false, nil)
	swapRuntimeResolver(t, func() (*provider.Runtime, error) {
		return &provider.Runtime{Provider: "fireworks", APIKey: "", BaseURL: "https://openrouter.ai/api/v1", Source: "env/config"}, nil
	})
	if c.ensureRuntimeCredentials() {
		t.Fatal("empty key resolved as ready")
	}
	text := out.String()
	if !strings.Contains(text, "fireworks") || strings.Contains(text, "OPENROUTER_API_KEY") ||
		!(strings.Contains(text, "icebath model") || strings.Contains(text, "icebath setup")) {
		t.Fatalf("screen %q", text)
	}
}

// Pool double whose only credential is benched, like the monkeypatched CredentialPool of the hermes case
type benchedPool struct{ entry provider.PooledCredential }

func (benchedPool) HasCredentials() bool     { return true }
func (benchedPool) HasAvailable(string) bool { return false }
func (benchedPool) NextAvailableAt(string) (time.Time, bool) {
	return time.Now().Add(55 * time.Second), true
}
func (p benchedPool) Entries() []provider.PooledCredential {
	return []provider.PooledCredential{p.entry}
}

// Installs the benched Nous pool the startup gate reads
func benchNousPool(t *testing.T, code int, reason string) {
	t.Helper()
	entry := provider.PooledCredential{ID: "e1", Provider: "nous", AuthType: "oauth", AccessToken: "x", RefreshToken: "r", Label: "portal",
		Source: "manual:device_code", LastStatus: provider.StatusExhausted, LastStatusAt: float64(time.Now().Add(-5 * time.Second).Unix()),
		LastErrorCode: code, LastErrorReason: reason}
	swapSeam(t, &startupLoadPool, func(*CLI, string) startupCredentialPool { return benchedPool{entry} })
}

// Fails the test when the first run wizard is offered or its picker runs
func forbidWizard(t *testing.T) {
	t.Helper()
	swapFirstRunPicker(t, func(context.Context) error { t.Error("wizard must not run"); return nil })
}

func TestBenchedCredentialPrintsCooldownInsteadOfWizard(t *testing.T) {
	c, out := firstRunShell(t, "", true, nil)
	c.requestedProvider = "nous"
	swapRuntimeResolver(t, func() (*provider.Runtime, error) {
		return nil, &provider.AuthError{Message: "Icebath is not logged into Nous Portal.", Provider: "nous", Code: "nous_auth_missing", ReloginRequired: true}
	})
	benchNousPool(t, 429, "rate_limited")
	forbidWizard(t)
	c.maybeOfferFirstRunSetup()
	text := out.String()
	ctx := c.ctx
	if strings.Contains(text, i18n.T(ctx, "cli.startup.first_run_no_provider", "", nil)) {
		t.Fatalf("wizard offered %q", text)
	}
	headline := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			headline = line
			break
		}
	}
	want := i18n.T(ctx, "cli.startup.credential_cooling_down", "", map[string]any{"provider": "nous",
		"cause": i18n.T(ctx, "cli.startup.cooldown_cause_rate_limit", "", nil), "minutes": 1})
	if !strings.HasSuffix(headline, want) || strings.Contains(text, i18n.T(ctx, "cli.startup.cooldown_cause_token_refresh", "", nil)) ||
		!strings.Contains(text, "not logged into Nous Portal") || strings.Contains(text, "re-authenticate") || strings.Contains(text, "icebath model") {
		t.Fatalf("headline %q want suffix %q screen %q", headline, want, text)
	}
}

func TestAuthJSONOnlyLoginExplainsInsteadOfWizard(t *testing.T) {
	c, out := firstRunShell(t, "", true, map[string]any{"model": map[string]any{"default": "some-model"}})
	for _, kv := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(kv, "")
	}
	c.requestedProvider = "auto"
	c.explicitAPIKey = ""
	c.explicitBaseURL = ""
	nousFailure := &provider.AuthError{Message: "Icebath is not logged into Nous Portal.", Provider: "nous", Code: "nous_auth_missing", ReloginRequired: true}
	swapRuntimeResolver(t, func() (*provider.Runtime, error) {
		return &provider.Runtime{Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Source: "default", AuthError: nousFailure}, nil
	})
	forbidWizard(t)
	c.maybeOfferFirstRunSetup()
	text := out.String()
	if !strings.Contains(text, "not logged into Nous Portal") || strings.Contains(text, i18n.T(c.ctx, "cli.startup.first_run_no_provider", "", nil)) {
		t.Fatalf("screen %q", text)
	}
	out.Reset()
	swapRuntimeResolver(t, func() (*provider.Runtime, error) {
		return nil, &provider.AuthError{Message: "Icebath is not connected to any AI provider yet.", Code: "no_provider_configured"}
	})
	offered := swapFirstRunPicker(t, func(context.Context) error { return nil })
	c.console.In = strings.NewReader("y\n")
	c.maybeOfferFirstRunSetup()
	if *offered != 1 || !strings.Contains(out.String(), i18n.T(c.ctx, "cli.startup.first_run_no_provider", "", nil)) || strings.Contains(out.String(), "not logged into") {
		t.Fatalf("control offered %d screen %q", *offered, out.String())
	}
}
