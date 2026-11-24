package gateway

import (
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Agent reasoning config of model options, structured reasoning or the legacy effort, nil when neither applies
func apiRequestReasoningConfig(modelOptions any) map[string]any {
	options, ok := modelOptions.(*jsonutil.Object)
	if !ok {
		return nil
	}
	var enabled, effort any
	if reasoning, isDict := options.ValueOr("reasoning", nil).(*jsonutil.Object); isDict {
		enabled = reasoning.ValueOr("enabled", nil)
		effort = reasoning.ValueOr("effort", nil)
	}
	effort = options.ValueOr("reasoning_effort", effort)
	effortNorm := ""
	if effort != nil {
		effortNorm = strings.ToLower(strings.TrimSpace(anyutil.String(effort)))
	}
	if enabled == false || effortNorm == "none" {
		return map[string]any{"enabled": false}
	}
	if apiReasoningEfforts[effortNorm] {
		return map[string]any{"enabled": true, "effort": effortNorm}
	}
	if enabled == true {
		return map[string]any{"enabled": true}
	}
	return nil
}

// Per request service tier override, the missing marker when the options name none
func apiRequestServiceTier(modelOptions any) any {
	options, ok := modelOptions.(*jsonutil.Object)
	if !ok {
		return apiRequestOptionMissing
	}
	if rawTier, found := options.Get("service_tier"); found {
		if _, isText := rawTier.(string); isText {
			if cleaned := apiCleanRequestString(rawTier); cleaned != nil {
				return *cleaned
			}
			return nil
		}
		return rawTier
	}
	if fast, found := options.Get("fast"); found {
		if apiCoerceRequestBool(fast, false) {
			return "priority"
		}
		return nil
	}
	return apiRequestOptionMissing
}
