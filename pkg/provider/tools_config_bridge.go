package provider

import (
	"context"
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
		_, err := readXAIOAuthTokens(env, true)
		return err
	}
	tools.ProcessAux = AuxAdapter{}
	tools.ModelNotFoundPatterns = func() []string { return append([]string(nil), modelNotFoundPatterns...) }
	tools.DelegationFallbackChain = GetFallbackChain
	installToolsConfigHooks()
}

// Installs the Nous, auth and setup prompt facts the tools picker reads
func installToolsConfigHooks() {
	hooks := &tools.ToolsConfigHooks
	hooks.NousFeatures = func(ctx context.Context, h string, cfg map[string]any, forceFresh bool) tools.NousToolFeatures {
		return nousToolFeatures(GetNousSubscriptionFeatures(credentialLifecycleEnv(ctx, h), cfg, forceFresh))
	}
	hooks.NousEntitlementMessage = func(account *tools.NousToolAccount, capability string) string {
		var info *NousPortalAccountInfo
		if account != nil {
			info, _ = account.Info.(*NousPortalAccountInfo)
		}
		return FormatNousPortalEntitlementMessage(info, capability, false)
	}
	hooks.EnsureNousPortalAccess = func(ctx context.Context, h string, out io.Writer, capability, coverageCategory string) bool {
		return EnsureNousPortalAccess(credentialLifecycleEnv(ctx, h), out, capability, coverageCategory)
	}
	hooks.ManagedCoverageCategory = func(feature string) (string, bool) {
		category, ok := ManagedFeatureCoverageCategory[feature]
		return category, ok
	}
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
	hooks.SaveCodexTokens = func(ctx context.Context, h string, tokens map[string]any, lastRefresh string, setActive bool) error {
		return saveCodexTokens(credentialLifecycleEnv(ctx, h), tokens, lastRefresh, "", setActive, false)
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

// Tools picker view of the subscription features, the account record kept for the entitlement message
func nousToolFeatures(f NousSubscriptionFeatures) tools.NousToolFeatures {
	out := tools.NousToolFeatures{NousAuthPresent: f.NousAuthPresent, Features: map[string]tools.NousToolFeature{}}
	for key, state := range f.Features {
		out.Features[key] = tools.NousToolFeature{Available: state.Available, ManagedByNous: state.ManagedByNous}
	}
	if info := f.AccountInfo; info != nil {
		out.Account = &tools.NousToolAccount{
			LoggedIn: info.LoggedIn, PaidServiceAccess: info.PaidServiceAccess, ToolGatewayEntitled: info.ToolGatewayEntitled(),
			EntitledFor: info.ToolGatewayEntitledFor, Info: info,
		}
	}
	return out
}

// Bool like config value through the tools_config parser
func parseEnabledFlag(v any, def bool) bool { return tools.ParseEnabledFlag(v, def) }
