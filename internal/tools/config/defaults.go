package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config, tools func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Tools",
		About: "Tool access and output limits",
		Settings: []config.Setting{
			{Key: Disabled, Title: "Disabled tools", About: "Tools the agent can't use", Default: []string{}, Options: tools},
			{Key: OutputMaxBytes, Title: "Max output size", About: "Max bytes of tool output sent to the model", Default: 50000},
			{Key: OutputMaxLineLength, Title: "Max line length", About: "Characters per line of tool output before the rest is cut", Default: 2000},
			{Key: OutputMaxLines, Title: "Max output lines", About: "Most lines of a file one read returns, whatever limit the agent asks for", Default: 2000},
			{Key: RedactSecrets, Title: "Hide secrets", About: "Mask API keys and tokens in what tools hand the model, such as files read and search hits", Default: true, Env: []string{"ICEBATH_REDACT_SECRETS"}},
			{Key: TrustedWorkspaces, Title: "Trusted folders", About: "Folders where linters and code checkers may run the project's own toolchain (npx tsc, rustfmt); the git worktree you work in is always trusted", Default: []string{}},
			{Key: InlineDiffs, Title: "Inline diffs", About: "Show file changes as a diff right in the transcript", Default: true},
			{Key: InlineImages, Title: "Show images inline", About: "Draw pictures tools make inside the transcript instead of as links", Default: ImagesAuto,
				Options: config.Options(
					config.Option{Value: ImagesAuto, Title: "Automatic", About: "Best method this terminal supports"},
					config.Option{Value: ImagesKitty, Title: "Graphics", About: "Full pictures in terminals that support them"},
					config.Option{Value: ImagesBlock, Title: "Blocks", About: "Coloured text blocks, works everywhere"},
					config.Option{Value: ImagesOff, Title: "Off", About: "No pictures"})},
			{Key: BudgetMCPResult, Title: "MCP result size", About: "Largest MCP tool result kept in the chat before it is saved to a file, in characters", Default: 50000},
		},
	}})
}
