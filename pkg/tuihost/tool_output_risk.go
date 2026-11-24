package tuihost

import (
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Risk badge payload of a tool row from the risk metadata the executor attached, nil without metadata
func OutputRiskPayload(name string, kw map[string]any) map[string]any {
	metadata, ok := kw["risk_metadata"].(map[string]any)
	if !ok {
		return nil
	}
	risk := "low"
	if value, _ := metadata["risk"].(string); value != "" {
		risk = value
	}
	findings := []any{}
	switch list := metadata["findings"].(type) {
	case []string:
		for _, item := range list {
			findings = append(findings, item)
		}
	case []any:
		for _, item := range list {
			findings = append(findings, anyutil.String(item))
		}
	}
	redacted, _ := metadata["redacted"].(bool)
	toolID, _ := kw["tool_call_id"].(string)
	return map[string]any{"tool_id": toolID, "name": name, "risk": risk, "findings": findings, "redacted": redacted}
}
