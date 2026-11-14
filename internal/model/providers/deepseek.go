package providers

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type deepSeekProfile struct {
	base
}

func newDeepSeek() Profile {
	return &deepSeekProfile{base{spec: Spec{
		Name: "deepseek", Aliases: []string{"deepseek-chat", "deep-seek"}, EnvVars: []string{"DEEPSEEK_API_KEY"},
		DisplayName: "DeepSeek", Description: "DeepSeek — native DeepSeek API", SignupURL: "https://platform.deepseek.com/",
		FallbackModels: []string{"deepseek-v4-pro", "deepseek-flash"}, BaseURL: "https://api.deepseek.com/v1",
		DefaultAuxModel: "deepseek-flash", UnsupportedResponseFormats: []string{"json_schema"},
	}}}
}

func (p *deepSeekProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	m := strings.ToLower(strings.TrimSpace(f.Model))
	v4Plus := strings.HasPrefix(m, "deepseek-v") && !strings.HasPrefix(m, "deepseek-v3")
	if !v4Plus && m != "deepseek-flash" {
		return Extras{}, nil
	}
	body, top := wire.ThinkingToggleExtras(f.Reasoning, wire.EffortsDeepSeekV4.Efforts(), true)
	if r := f.Reasoning; r != nil {
		switch reasoningEffortRaw(r) {
		case "none", "false", "disabled":
			return Extras{Body: map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"disabled"}`)}}, nil
		}
	}
	return Extras{Body: body, TopLevel: top}, nil
}
