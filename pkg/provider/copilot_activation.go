package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Provider ids that select the Copilot API profile
var copilotProviderIDs = map[string]bool{"copilot": true, "github-copilot": true, "github-models": true, "github-model": true, "github": true}

// Context key marking a Copilot flow the user started
type copilotUserFlowKey struct{}

// Context of a Copilot flow the user started, so GitHub calls made under it count as requested
func withCopilotUserFlow(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, copilotUserFlowKey{}, true)
}

// Reports a context inside a Copilot flow the user started
func inCopilotUserFlow(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(copilotUserFlowKey{}).(bool)
	return marked
}

// Copy of a model flow run whose context and pool env carry the user started Copilot flow marker
func copilotUserFlowRun(r *ModelFlowRun) *ModelFlowRun {
	flowCtx := withCopilotUserFlow(r.ctx())
	run := *r
	run.Ctx = flowCtx
	run.Env.Ctx = flowCtx
	return &run
}

// Reports a config slot whose provider names the Copilot API profile
func copilotSelects(slot any) bool {
	m, ok := slot.(map[string]any)
	if !ok {
		return false
	}
	return copilotProviderIDs[strings.ToLower(strings.TrimSpace(cpOrStr(m["provider"])))]
}

// A list value as is, any other value as a one item list
func copilotChain(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return []any{value}
}

// Every configured slot that can route a request to a provider, each read through its setting
func copilotConfigSlots(ctx context.Context) []any {
	slot := func(provider string) any { return map[string]any{"provider": provider} }
	slots := []any{slot(settings.ModelProvider.Get(ctx)), slot(settings.DelegationProvider.Get(ctx))}
	slots = append(slots, settings.FallbackModels.Get(ctx)...)
	slots = append(slots, settings.DelegationFallbackProviders.Get(ctx)...)
	for _, e := range settings.All() {
		key := e.Info().Key
		if s, ok := e.(*settings.String); ok && strings.HasPrefix(key, "auxiliary.") && strings.HasSuffix(key, ".provider") {
			slots = append(slots, slot(s.Get(ctx)))
		}
	}
	for _, preset := range cpValues(settings.MoaPresets.Get(ctx)) {
		b, ok := preset.(map[string]any)
		if !ok {
			continue
		}
		refs := b["reference_models"]
		if !cpTruthy(refs) {
			refs = []any{}
		}
		slots = append(slots, copilotChain(refs)...)
		slots = append(slots, b["aggregator"])
	}
	return slots
}

// Reports a configured slot of ctx's home naming Copilot
func copilotConfigSelects(ctx context.Context) bool {
	for _, slot := range copilotConfigSlots(ctx) {
		if copilotSelects(slot) {
			return true
		}
	}
	return false
}

// Reports a Copilot pool row from icebath auth add copilot
func copilotPoolHasExplicitRow(h string) (bool, error) {
	rows, err := NewStore(h).ReadCredentialPool("copilot")
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if isManualSource(strings.TrimSpace(cpOrStr(row["source"]))) {
			return true, nil
		}
	}
	return false, nil
}

// Reports COPILOT_GITHUB_TOKEN set in the secret scope
func copilotEnvTokenSet(ctx context.Context) (bool, error) {
	v, _, err := GetSecret(ctx, "COPILOT_GITHUB_TOKEN")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(v) != "", nil
}

// True when the home chose Copilot by COPILOT_GITHUB_TOKEN, a config slot or an explicit pool row
func copilotExplicitlyConfigured(ctx context.Context) bool {
	h := home.Dir(ctx)
	checks := []struct {
		name string
		fn   func() (bool, error)
	}{
		{"_copilot_env_token_set", func() (bool, error) { return copilotEnvTokenSet(ctx) }},
		{"_config_selects_copilot", func() (bool, error) { return copilotConfigSelects(ctx), nil }},
		{"_pool_has_explicit_row", func() (bool, error) { return copilotPoolHasExplicitRow(h) }},
	}
	for _, check := range checks {
		ok, err := check.fn()
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("copilot explicit-config check %s failed: %v", check.name, err))
			continue
		}
		if ok {
			return true
		}
	}
	return false
}

// Whether a token may go to the GitHub exchange or the Copilot catalog right now
func githubCallsAllowed(ctx context.Context) bool {
	return inCopilotUserFlow(ctx) || copilotExplicitlyConfigured(ctx)
}
