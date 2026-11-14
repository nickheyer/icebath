package providers

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type miniMaxProfile struct {
	base
}

func newMiniMax(s Spec) Profile {
	return &miniMaxProfile{base{spec: s}}
}

func minimaxSpec() Spec {
	return Spec{Name: "minimax", Aliases: []string{"mini-max"}, Mode: wire.ModeAnthropic, EnvVars: []string{"MINIMAX_API_KEY"},
		BaseURL: "https://api.minimax.io/anthropic", AuthType: authTypeAPIKey, DefaultAuxModel: "MiniMax-M3"}
}

func minimaxCNSpec() Spec {
	return Spec{Name: "minimax-cn", Aliases: []string{"minimax-china", "minimax_cn"}, Mode: wire.ModeAnthropic,
		EnvVars: []string{"MINIMAX_CN_API_KEY"}, BaseURL: "https://api.minimaxi.com/anthropic", AuthType: authTypeAPIKey,
		DefaultAuxModel: "MiniMax-M3"}
}

func minimaxOAuthSpec() Spec {
	return Spec{Name: "minimax-oauth", Aliases: []string{"minimax_oauth", "minimax-portal", "minimax-global", "minimax-oauth-io"},
		Mode: wire.ModeAnthropic, DisplayName: "MiniMax (OAuth)", Description: "MiniMax via OAuth browser flow — no API key required",
		SignupURL: "https://api.minimax.io/", BaseURL: "https://api.minimax.io/anthropic", AuthType: authTypeOAuthExternal,
		DefaultAuxModel: "MiniMax-M2.7"}
}

func isMiniMaxGlobalOpenAIBaseURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	return err == nil && strings.ToLower(u.Hostname()) == "api.minimax.io" && strings.ToLower(strings.TrimRight(u.Path, "/")) == "/v1"
}

func (p *miniMaxProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	model := strings.ToLower(strings.TrimSpace(f.Model))
	if !isMiniMaxGlobalOpenAIBaseURL(f.BaseURL) || (model != "minimax-m3" && model != "minimax/minimax-m3") {
		return Extras{}, nil
	}
	body := map[string]json.RawMessage{"reasoning_split": wire.Bool(true)}
	switch {
	case f.Reasoning != nil && !f.Reasoning.Enabled:
		body["thinking"] = json.RawMessage(`{"type":"disabled"}`)
	case f.Reasoning != nil:
		body["thinking"] = json.RawMessage(`{"type":"adaptive"}`)
	}
	return Extras{Body: body}, nil
}
