package provider

import (
	"context"
	"errors"
	"io"

	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	tools.LeftCorePlatforms = func() []string {
		var out []string
		for _, f := range LeftCore() {
			if f.Platform != "" {
				out = append(out, f.Platform)
			}
		}
		return out
	}
	tools.XAIOAuthTokensReader = func(ctx context.Context, h string) error {
		env := homePoolEnv(h)
		env.Ctx = ctx
		if !LoadSelectedPool(env, "xai-oauth").HasCredentials() {
			return errors.New("no xAI login")
		}
		return nil
	}
	tools.ProcessAux = AuxAdapter{}
	tools.ModelNotFoundPatterns = func() []string { return append([]string(nil), modelNotFoundPatterns...) }
	tools.DelegationFallbackChain = func(cfg map[string]any) []map[string]any { return FallbackChainOf(cfg["fallback_providers"]) }
	installToolsConfigHooks()
}

// Installs the auth and setup prompt facts the tools picker reads
func installToolsConfigHooks() {
	hooks := &tools.ToolsConfigHooks
	hooks.AuxPickerRows = func(ctx context.Context, currentProvider, currentModel, currentBaseURL string, maxModels int) ([]map[string]any, error) {
		return BuildAuxPickerRows(ctx, currentProvider, currentModel, currentBaseURL, &maxModels)
	}
	hooks.AuxPickerLabels = func(rows []map[string]any, currentProvider, currentBaseURL string) []string {
		var labels []string
		for _, entry := range FormatAuxPickerEntries(rows, currentProvider, currentBaseURL) {
			labels = append(labels, entry.Label)
		}
		return labels
	}
	hooks.XAIOAuthStatus = func(ctx context.Context, h string) map[string]any {
		return GetXAIOAuthAuthStatus(credentialLifecycleEnv(ctx, h))
	}
	hooks.CodexAuthStatus = func(ctx context.Context, h string) map[string]any {
		return GetCodexAuthStatus(credentialLifecycleEnv(ctx, h))
	}
	hooks.CodexDeviceCodeLogin = func(ctx context.Context, h string, out io.Writer) (map[string]any, error) {
		return codexDeviceCodeLogin(credentialLifecycleEnv(ctx, h), out)
	}
	hooks.SaveCodexTokens = func(ctx context.Context, h string, tokens map[string]any, lastRefresh string) error {
		return saveCodexLogin(credentialLifecycleEnv(ctx, h), tokens, lastRefresh)
	}
	hooks.XAIOAuthLoginFromSetup = func(ctx context.Context, h string) bool {
		return runXAIOAuthLoginFromSetup(credentialLifecycleEnv(ctx, h))
	}
	hooks.SetupPromptChoice = func(question string, choices []string, def int) int {
		return PromptChoice(question, choices, def, "")
	}
	hooks.SetupPrompt = SetupPrompt
	hooks.SetupIsNoninteractive = IsNoninteractive
}

// Bool like config value through the tools_config parser
func parseEnabledFlag(v any, def bool) bool { return tools.ParseEnabledFlag(v, def) }

// Stores the tokens of a Codex device code sign in as a new owned login, made the default when the provider has none
func saveCodexLogin(env PoolEnv, tokens map[string]any, lastRefresh string) error {
	previous := currentLoginID(env, "openai-codex")
	c := &authCLI{ctx: env.ctx(), env: env, ns: map[string]any{}, out: io.Discard, errw: io.Discard}
	entry, err := c.storeOAuthCredential("openai-codex", LoadPool(env, "openai-codex"), map[string]any{"tokens": tokens, "last_refresh": lastRefresh})
	if err != nil {
		return err
	}
	_, err = NameNewLogin(env, "openai-codex", entry.ID, "", false, previous)
	return err
}
