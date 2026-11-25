package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	consoleutil "github.com/nickheyer/icebath/pkg/utils/console"
)

// Route a resumed session runs on, false when the stored model is absent or already current
func storedSessionRoute(ctx context.Context, sessionMeta map[string]any, currentModel, currentProvider string) (model, prov, baseURL, apiMode string, providerChanged, ok bool) {
	stored := strings.TrimSpace(cliStr(sessionMeta["model"], ""))
	if stored == "" {
		return "", "", "", "", false, false
	}
	runtime := state.SessionGatewayRuntime(objectFromPlain(sessionMeta))
	baseURL = cliStr(runtime.ValueOr("base_url", ""), "")
	apiMode = cliStr(runtime.ValueOr("api_mode", ""), "")
	storedProvider := cliStr(runtime.ValueOr("provider", ""), "")
	prov = healBareCustomProvider(ctx, storedProvider, baseURL, stored)
	if prov != storedProvider {
		baseURL = ""
		if prov == "" {
			apiMode = ""
		}
	}
	providerChanged = prov != "" && prov != currentProvider
	if stored == currentModel && !providerChanged {
		return "", "", "", "", false, false
	}
	if provider.IsForeignProviderEndpoint(prov, baseURL) {
		baseURL, apiMode = "", ""
	}
	if derived, ok := provider.ModelDerivedAPIMode(ctx, prov, stored, ""); ok {
		apiMode = derived
		baseURL = provider.NormalizeOpenCodeBaseURL(prov, provider.APIMode(apiMode), baseURL)
	}
	return stored, prov, baseURL, apiMode, providerChanged, true
}

// Durable custom identity of a bare custom provider, empty when nothing heals it
func healBareCustomProvider(ctx context.Context, prov, baseURL, model string) (healed string) {
	if strings.ToLower(strings.TrimSpace(prov)) != "custom" {
		return prov
	}
	defer func() {
		if recover() != nil {
			healed = ""
		}
	}()
	return provider.CanonicalCustomIdentity(ctx, baseURL, "", model, "")
}

// Restores the model and provider recorded on the session row unless the launch named an explicit model
func (c *CLI) restoreSessionModel(sessionMeta map[string]any, quiet bool) {
	if !anyutil.IsTrue(sessionMeta["model"]) || c.explicitModelOverride {
		return
	}
	storedModel, storedProvider, storedBaseURL, storedAPIMode, providerChanged, ok := storedSessionRoute(c.ctx, sessionMeta, c.model, c.provider)
	if !ok {
		return
	}
	managed := provider.LlamacppAliases[strings.ToLower(strings.TrimSpace(storedProvider))]
	c.model = storedModel
	if storedProvider != "" {
		c.provider = storedProvider
		c.requestedProvider = storedProvider
		if storedBaseURL != "" && !managed {
			c.baseURL = storedBaseURL
		}
		if storedAPIMode != "" {
			c.apiMode = storedAPIMode
		}
	}
	switch {
	case managed && !(c.explicitBaseURL != "" && !providerChanged):
		c.explicitAPIKey, c.explicitBaseURL = "", ""
		rt, err := provider.ResolveRuntimeProvider(c.ctx, home.Dir(c.ctx), storedProvider, c.model, "", "")
		if err != nil {
			if storedBaseURL != "" {
				c.baseURL = storedBaseURL
			}
			pkgLog().Debug(fmt.Sprintf("Credential re-resolution for resumed session provider %s failed; keeping ambient credentials", storedProvider))
			break
		}
		if rt.APIKey != "" {
			c.apiKey, c.credentialPool = rt.APIKey, rt.CredentialPool
		}
		if rt.BaseURL != "" {
			c.baseURL = rt.BaseURL
		}
		if storedAPIMode == "" && rt.APIMode != "" {
			c.apiMode = string(rt.APIMode)
		}
	case providerChanged:
		c.explicitAPIKey, c.explicitBaseURL = "", storedBaseURL
		rt, err := provider.ResolveRuntimeProvider(c.ctx, home.Dir(c.ctx), storedProvider, c.model, "", "")
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("Credential re-resolution for resumed session provider %s failed; keeping ambient credentials", storedProvider))
			break
		}
		if rt.APIKey != "" {
			c.apiKey, c.credentialPool = rt.APIKey, rt.CredentialPool
		}
		if storedBaseURL == "" && rt.BaseURL != "" {
			c.baseURL = rt.BaseURL
		}
		if storedAPIMode == "" && rt.APIMode != "" {
			c.apiMode = string(rt.APIMode)
		}
	}
	c.resolveCLIReasoning()
	if c.agentRef() != nil {
		if _, err := c.agentRef().SwitchModel(c.model, c.provider); err != nil {
			pkgLog().Debug("In-place agent model swap on resume failed")
		}
	}
	msg := c.t("cli.model.restored_from_session", map[string]any{"model": storedModel})
	if storedProvider != "" {
		msg += " (" + storedProvider + ")"
	}
	if quiet {
		fmt.Fprintln(stderrWriter(), msg)
		return
	}
	c.consolePrint("[dim]" + consoleutil.Escape(msg) + "[/dim]")
}
