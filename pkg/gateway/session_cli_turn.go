package gateway

import (
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Most characters of an API side text one CLI turn may commit
const apiTextLimit = 1_000_000

// Per turn fields the classic CLI sends with a prompt.submit request
var cliTurnSubmitFields = []string{"turn_author", "api_text", "model_override", "clarify_batch"}

// Raw per turn CLI fields present on a prompt.submit request, forwarded to admission
func submitCLITurnFields(params map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range cliTurnSubmitFields {
		if value, ok := params[key]; ok {
			out[key] = value
		}
	}
	return out
}

// Committed CLI turn fields of a submission, empty when it carries none, malformed values refused
func admitCLITurn(params map[string]any) (*pyjson.Object, error) {
	committed := pyjson.NewObject()
	if raw := params["turn_author"]; raw != nil {
		author := provider.ParseTurnAuthor(raw)
		if author == nil {
			return nil, runtimeRefusal("invalid_params")
		}
		committed.Set("turn_author", author)
	}
	if raw := params["api_text"]; raw != nil {
		text, ok := raw.(string)
		if !ok || pystr.Len(text) > apiTextLimit {
			return nil, runtimeRefusal("invalid_params")
		}
		if text != "" {
			committed.Set("api_text", text)
		}
	}
	if raw := params["model_override"]; raw != nil {
		override, ok := attachmentItem(raw)
		if !ok {
			return nil, runtimeRefusal("invalid_params")
		}
		for key := range override {
			if key != "model" && key != "provider" && key != "reasoning" {
				return nil, runtimeRefusal("invalid_params")
			}
		}
		model, isModel := override["model"].(string)
		routed, hasProvider := override["provider"]
		providerName, isProvider := routed.(string)
		reasoning, hasReasoning := override["reasoning"]
		effort, isEffort := reasoning.(string)
		if !isModel || pystr.Strip(model) == "" || (hasProvider && !isProvider) ||
			(hasReasoning && (!isEffort || config.ParseReasoningEffort(effort) == nil)) {
			return nil, runtimeRefusal("invalid_params")
		}
		kept := pyjson.ObjectOf("model", pystr.Strip(model))
		if providerName != "" {
			kept.Set("provider", providerName)
		}
		if hasReasoning {
			kept.Set("reasoning", effort)
		}
		committed.Set("model_override", kept)
	}
	batch, ok := optionalBool(params["clarify_batch"])
	if !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	if batch {
		committed.Set("clarify_batch", true)
	}
	if committed.Len() == 0 {
		return pyjson.NewObject(), nil
	}
	return pyjson.ObjectOf("cli_turn_v1", committed), nil
}
