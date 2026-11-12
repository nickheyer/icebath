package agent

import (
	"context"
	"fmt"
	"maps"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// Puts the agent on a primary runtime shaped snapshot with its client, caching, compressor, reasoning and prompt identity
func reinstallRuntimeSnapshot(ctx context.Context, a *AIAgent, rt map[string]any) error {
	if err := applyPrimaryRuntimeFields(a, rt); err != nil {
		return err
	}
	resetCodexReasoningReplay(a)
	restoreRuntimeCapabilities(a, rt)
	caching, err := runtimeField(rt, "use_prompt_caching")
	if err != nil {
		return err
	}
	a.usePromptCaching = pyTruthy(caching)
	if native, ok := rt["use_native_cache_layout"]; ok {
		a.useNativeCacheLayout = pyTruthy(native)
	} else {
		a.useNativeCacheLayout = a.apiMode == "anthropic_messages" && a.provider == "anthropic"
	}
	if a.cacheDisabled {
		a.usePromptCaching, a.useNativeCacheLayout = false, false
	}
	if err := rebuildPrimaryClient(ctx, a, rt, "restore_primary"); err != nil {
		return err
	}
	if err := reinstallCompressorModel(a, rt); err != nil {
		return err
	}
	if a.compressionFeasibilityChecked {
		revalidateCompressionFeasibility(ctx, a)
	}
	if saved, ok := rt["reasoning_config"].(map[string]any); ok && saved != nil {
		a.reasoningConfig = maps.Clone(saved)
	}
	a.RetrySession().ResetStaleStreak()
	model, _ := rt["model"].(string)
	providerName, _ := rt["provider"].(string)
	a.rewritePromptModelIdentity(model, providerName)
	return nil
}

// Points the context compressor at the snapshot compressor model, window, endpoint and credential
func reinstallCompressorModel(a *AIAgent, rt map[string]any) error {
	if a.contextCompressor == nil {
		return fmt.Errorf("AttributeError: 'NoneType' object has no attribute 'update_model'")
	}
	var fields [5]any
	for i, key := range []string{"compressor_model", "compressor_context_length", "compressor_base_url", "compressor_api_key", "compressor_provider"} {
		v, err := runtimeField(rt, key)
		if err != nil {
			return err
		}
		fields[i] = v
	}
	var contextLength int
	switch n := fields[1].(type) {
	case float64:
		contextLength = int(n)
	default:
		big, ok := pyint.Instance(n)
		if !ok {
			return fmt.Errorf("TypeError: compressor_context_length %T is not an int", n)
		}
		contextLength = int(big.Int64())
	}
	model, _ := fields[0].(string)
	baseURL, _ := fields[2].(string)
	apiKey, _ := fields[3].(string)
	providerName, _ := fields[4].(string)
	apiMode, _ := rt["compressor_api_mode"].(string)
	a.contextCompressor.UpdateModel(model, contextLength, baseURL, apiKey, providerName, apiMode)
	return nil
}

// Copies the snapshot runtime capabilities, the legacy capabilities key when the current key is absent
func restoreRuntimeCapabilities(a *AIAgent, rt map[string]any) {
	raw, present := rt["runtime_capabilities"]
	if !present {
		raw = rt["capabilities"]
	}
	switch caps := raw.(type) {
	case map[string]any:
		restored := make(map[string]bool, len(caps))
		for name, v := range caps {
			restored[name] = pyTruthy(v)
		}
		a.runtimeCapabilities = restored
	case map[string]bool:
		a.runtimeCapabilities = maps.Clone(caps)
		if a.runtimeCapabilities == nil {
			a.runtimeCapabilities = map[string]bool{}
		}
	default:
		if present {
			agentRuntimeHelpersLog().Warn("Ignoring malformed runtime capabilities snapshot")
		}
	}
}
