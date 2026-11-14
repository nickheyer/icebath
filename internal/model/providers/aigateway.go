package providers

import (
	"context"
	"encoding/json"
)

type aiGatewayProfile struct {
	base
}

func newAIGateway(d Deps) Profile {
	return &aiGatewayProfile{base{spec: Spec{
		Name: "ai-gateway", Aliases: []string{"vercel", "vercel-ai-gateway", "ai_gateway", "aigateway"},
		EnvVars: []string{"AI_GATEWAY_API_KEY"}, BaseURL: "https://ai-gateway.vercel.sh/v1",
		DefaultHeaders:  map[string]string{"HTTP-Referer": "https://icebath.app", "X-Title": "icebath"},
		DefaultAuxModel: "google/gemini-3-flash",
	}}}
}

func (p *aiGatewayProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	if !f.SupportsReasoning {
		return Extras{}, nil
	}
	reasoning := json.RawMessage(`{"enabled":true,"effort":"medium"}`)
	if f.Reasoning != nil {
		reasoning = reasoningObject(f.Reasoning)
	}
	return Extras{Body: map[string]json.RawMessage{"reasoning": reasoning}}, nil
}
