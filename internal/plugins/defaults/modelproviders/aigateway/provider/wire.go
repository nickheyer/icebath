package provider

import "github.com/nickheyer/icebath/internal/provider"

// Host of Vercel's gateway, which takes reasoning for every model it serves
const gatewayHost = "ai-gateway.vercel.sh"

// Reasoning passed through as asked, on at medium when unasked; nothing at an address whose models don't take it
func shape(call *provider.Call) (map[string]any, map[string]any) {
	if !call.Thinking.Takes && !provider.BaseHostMatches(call.Route.BaseURL, gatewayHost) {
		return nil, nil
	}
	reasoning := map[string]any{"enabled": true, "effort": "medium"}
	if call.Ask.Set {
		reasoning = call.Ask.Object()
	}
	return map[string]any{"reasoning": reasoning}, nil
}
