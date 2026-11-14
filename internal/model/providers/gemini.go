package providers

import (
	"context"
	"encoding/json"

	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/gemini"
)

type geminiProfile struct {
	base
}

func newGemini() Profile {
	return &geminiProfile{base{spec: Spec{
		Name: "gemini", Aliases: []string{"google", "google-gemini", "google-ai-studio"}, Mode: wire.ModeChat,
		EnvVars: []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"}, BaseURL: "https://generativelanguage.googleapis.com/v1beta",
		AuthType: authTypeAPIKey, DefaultAuxModel: "gemini-3.6-flash",
	}}}
}

func googleThinkingBody(thinking json.RawMessage) map[string]json.RawMessage {
	google := (&wire.Dict{}).Set("thinking_config", thinking).JSON()
	return map[string]json.RawMessage{"extra_body": (&wire.Dict{}).Set("google", google).JSON()}
}

func (p *geminiProfile) ExtraBody(ctx context.Context, f RouteFacts) (map[string]json.RawMessage, error) {
	raw := gemini.ThinkingConfig(f.Model, f.Reasoning)
	if len(raw) == 0 {
		return nil, nil
	}
	baseURL := f.BaseURL
	if baseURL == "" {
		baseURL = p.spec.BaseURL
	}
	if gemini.IsOpenAICompatBaseURL(baseURL) {
		snake := gemini.SnakeThinkingConfig(raw)
		if len(snake) == 0 {
			return nil, nil
		}
		return googleThinkingBody(snake), nil
	}
	body := map[string]json.RawMessage{"thinking_config": raw}
	if budget, ok := gemini.LongDecodeMaxOutputTokens(f.Model, f.Reasoning); ok && budget > 0 && gemini.IsNativeBaseURL(baseURL) {
		body["max_output_tokens"] = wire.Int(int64(budget))
	}
	return body, nil
}
