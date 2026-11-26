package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config, providers, risks func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Agent",
		About: "Model, reasoning and approvals",
		Settings: []config.Setting{
			{Key: Provider, Title: "Provider", About: "Provider of this profile new sessions use", Default: "", Options: func() []config.Option {
				return append([]config.Option{{Title: "First connected"}}, providers()...)
			}},
			{Key: Model, Title: "Model", About: "Model for new sessions; empty uses the provider's default", Default: ""},
			{Key: ReasoningEffort, Title: "Reasoning effort", About: "How much the model reasons before answering", Default: "", Options: func() []config.Option {
				options := []config.Option{{Title: "Model default"}}
				for _, effort := range Efforts {
					options = append(options, config.Option{Value: effort, Title: effort})
				}
				return options
			}},
			{Key: Thinking, Title: "Reasoning display", About: "How model reasoning is shown in chats", Default: ThinkingFolded, Options: config.Options(
				config.Option{Value: ThinkingHidden, Title: "Hidden"},
				config.Option{Value: ThinkingFolded, Title: "Collapsed", About: "One line, click to expand"},
				config.Option{Value: ThinkingOpen, Title: "Expanded"},
			)},
			{Key: ApprovalsMode, Title: "Approvals", About: "Ask before running risky commands", Default: ApprovalsManual, Options: config.Options(
				config.Option{Value: ApprovalsManual, Title: "Ask"},
				config.Option{Value: ApprovalsOff, Title: "Don't ask"},
			)},
			{Key: ApprovalsTimeout, Title: "Approval timeout", About: "Seconds to wait for an answer before denying", Default: 300},
			{Key: ApprovalsAllowlist, Title: "Allowlist", About: "Kinds of risky action that run without asking", Default: []string{}, Options: risks},
			{Key: MaxTurns, Title: "Max steps", About: "Max model calls per reply; 0 for no limit", Default: 0},
			{Key: APIMaxRetries, Title: "Retries", About: "Times to retry a failed provider request", Default: 3},
			{Key: ContextFileMaxChars, Title: "AGENTS.md limit", About: "Max characters read from a project's AGENTS.md", Default: 20000},
		},
	}})
}
