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
