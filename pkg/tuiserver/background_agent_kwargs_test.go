package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
)

// Parent agent stand in exposing only the attributes the fake sets
type fakeBackgroundParent struct {
	model    string
	provider string
	disabled []string
	chain    []map[string]any
}

func (f fakeBackgroundParent) BaseURL() string                        { return "" }
func (f fakeBackgroundParent) APIKey() string                         { return "" }
func (f fakeBackgroundParent) Provider() string                       { return f.provider }
func (f fakeBackgroundParent) APIMode() string                        { return "" }
func (f fakeBackgroundParent) ACPCommand() string                     { return "" }
func (f fakeBackgroundParent) ACPArgs() []string                      { return nil }
func (f fakeBackgroundParent) EphemeralSystemPrompt() string          { return "" }
func (f fakeBackgroundParent) ProviderRouting() agent.ProviderRouting { return agent.ProviderRouting{} }
func (f fakeBackgroundParent) Model() string                          { return f.model }
func (f fakeBackgroundParent) EnabledToolsets() []string              { return nil }
func (f fakeBackgroundParent) DisabledToolsets() []string             { return f.disabled }
func (f fakeBackgroundParent) ReasoningConfig() map[string]any        { return nil }
func (f fakeBackgroundParent) ServiceTier() string                    { return "" }
func (f fakeBackgroundParent) RequestOverrides() map[string]any       { return map[string]any{} }
func (f fakeBackgroundParent) SessionStore() agent.SessionStore       { return nil }
func (f fakeBackgroundParent) FallbackChain() []map[string]any        { return f.chain }

// Home whose config.yaml holds the given text, the profile the background kwargs read
func backgroundCfgHome(t *testing.T, cfg string) context.Context {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	useSharedDB(t, nil)
	return context.Background()
}

// Parent stand in holding a dedicated store, the SimpleNamespace with session db
type fakeStoreParent struct {
	fakeBackgroundParent
	store agent.SessionStore
}

func (f fakeStoreParent) SessionStore() agent.SessionStore { return f.store }
