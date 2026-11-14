package providers

import (
	"context"
	"encoding/json"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type qwenProfile struct {
	base
}

func newQwen() Profile {
	return &qwenProfile{base{spec: Spec{
		Name: "qwen-oauth", Aliases: []string{"qwen", "qwen-portal", "qwen-cli"}, EnvVars: []string{"QWEN_API_KEY"},
		BaseURL: "https://portal.qwen.ai/v1", AuthType: authTypeOAuthExternal, DefaultMaxTokens: 65536,
	}}}
}

func (p *qwenProfile) PrepareMessages(ctx context.Context, msgs []wire.Message) ([]wire.Message, error) {
	if len(msgs) == 0 {
		return []wire.Message{}, nil
	}
	out := make([]wire.Message, len(msgs))
	system := -1
	for i := range msgs {
		out[i] = msgs[i]
		if system < 0 && msgs[i].Role == wire.RoleSystem {
			system = i
		}
		if len(msgs[i].Parts) == 0 && msgs[i].Text != "" {
			m := msgs[i].Clone()
			m.Parts = []wire.Part{{Kind: wire.PartText, Text: m.Text}}
			m.Text = ""
			out[i] = m
		}
	}
	if system >= 0 && len(out[system].Parts) > 0 {
		m := out[system].Clone()
		parts := make([]wire.Part, len(m.Parts))
		copy(parts, m.Parts)
		parts[len(parts)-1].Cache = &wire.CacheMarker{}
		m.Parts = parts
		out[system] = m
	}
	return out, nil
}

func (p *qwenProfile) ExtraBody(ctx context.Context, f RouteFacts) (map[string]json.RawMessage, error) {
	return map[string]json.RawMessage{"vl_high_resolution_images": wire.Bool(true)}, nil
}

func (p *qwenProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	if v, err := wire.ParseValue(f.QwenSession); err == nil && v.Truthy() {
		return topLevel("metadata", f.QwenSession), nil
	}
	return Extras{}, nil
}
