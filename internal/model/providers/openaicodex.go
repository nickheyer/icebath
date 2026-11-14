package providers

import (
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

func newOpenAICodex() Profile {
	return &base{spec: Spec{
		Name: "openai-codex", Aliases: []string{"codex", "openai_codex"}, Mode: wire.ModeResponses,
		BaseURL: auth.DefaultCodexBaseURL, AuthType: authTypeOAuthExternal,
	}}
}
