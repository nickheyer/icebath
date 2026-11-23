package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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
	return copilotProviderIDs[strings.ToLower(pystr.Strip(cpOrStr(m["provider"])))]
}

// A list value as is, any other value as a one item list
func copilotChain(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return []any{value}
}

// Every config slot that can route a request to a provider
func copilotConfigSlots(cfg map[string]any) []any {
	slots := []any{cfg["model"]}
	for _, key := range []string{"fallback_model", "fallback_providers"} {
		slots = append(slots, copilotChain(cfg[key])...)
	}
	if delegation, ok := cfg["delegation"].(map[string]any); ok {
		slots = append(slots, delegation)
		slots = append(slots, copilotChain(delegation["fallback_providers"])...)
	}
	if aux, ok := cfg["auxiliary"].(map[string]any); ok {
		slots = append(slots, cpValues(aux)...)
	}
	if moa, ok := cfg["moa"].(map[string]any); ok {
		blocks := []any{moa}
		if presets, ok := moa["presets"].(map[string]any); ok {
			blocks = append(blocks, cpValues(presets)...)
		}
		for _, block := range blocks {
			b, ok := block.(map[string]any)
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
	}
	return slots
}

// Reports a config.yaml slot of the home naming Copilot
func copilotConfigSelects(h string) (bool, error) {
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return false, err
	}
	for _, slot := range copilotConfigSlots(cfg) {
		if copilotSelects(slot) {
			return true, nil
		}
	}
	return false, nil
}

// Reports a Copilot pool row from icebath auth add copilot
func copilotPoolHasExplicitRow(h string) (bool, error) {
	rows, err := NewStore(h).ReadCredentialPool("copilot")
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if isManualSource(pystr.Strip(cpOrStr(row["source"]))) {
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
	return pystr.Strip(v) != "", nil
}

// True when the home chose Copilot by COPILOT_GITHUB_TOKEN, a config slot or an explicit pool row
func copilotExplicitlyConfigured(ctx context.Context) bool {
	h := home.Dir(ctx)
	checks := []struct {
		name string
		fn   func() (bool, error)
	}{
		{"_copilot_env_token_set", func() (bool, error) { return copilotEnvTokenSet(ctx) }},
		{"_config_selects_copilot", func() (bool, error) { return copilotConfigSelects(h) }},
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
