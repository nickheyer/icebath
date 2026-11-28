package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config, tools func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Tools",
		About: "Turn tools on or off and limit what they return",
		Group: config.GroupAbilities,
		Order: 10,
		Settings: []config.Setting{
			{Key: Disabled, Title: "Tools that are off", About: "Tools that the agent cannot use.", Default: []string{}, Options: tools, Custom: true},
			{Key: InlineDiffs, Title: "Show diffs", About: "Show the changes to a file as a diff in the chat.", Default: true},
			{Key: InlineImages, Title: "Images in chat", About: "How images from tools are drawn in the chat.", Default: ImagesAuto,
				Options: config.Options(
					config.Option{Value: ImagesAuto, Title: "Automatic", About: "The best way that this terminal supports"},
					config.Option{Value: ImagesKitty, Title: "Graphics", About: "Full images, in terminals that support them"},
					config.Option{Value: ImagesBlock, Title: "Blocks", About: "Colored text blocks, which work everywhere"},
					config.Option{Value: ImagesOff, Title: "Off", About: "Links in place of images"})},
			{Key: RedactSecrets, Title: "Hide secrets", About: "Hide API keys and tokens in what tools show the agent, such as files and search results.",
				Default: true, Env: []string{"ICEBATH_REDACT_SECRETS"}},
			{Key: TrustedWorkspaces, Title: "Trusted folders", About: "Folders where code checkers may run the project's own programs.",
				Help: "Such as npx tsc or rustfmt. The git worktree you work in is always trusted.", Default: []string{}, Entry: &config.Entry{Noun: "folder"},
				Path: &config.Path{Kind: config.PathFolder}},
			{Key: OutputMaxBytes, Title: "Command output size", About: "How much output of one command the agent reads.",
				Help: "Longer output keeps its start and end, and the middle is cut.", Default: 50000, Counts: "characters", Advanced: true},
			{Key: OutputMaxLines, Title: "Lines per file read", About: "The most lines of a file that one read returns.", Default: 2000, Counts: "lines",
				Least: config.Num(1), Advanced: true},
			{Key: OutputMaxLineLength, Title: "Line length", About: "How long a line of a file read may be before the rest is cut.", Default: 2000,
				Counts: "characters", Least: config.Num(1), Advanced: true},
			{Key: BudgetMCPResult, Title: "MCP result size", About: "How long an MCP tool result may be before it is saved to a file.", Default: 50000,
				Zero: "50,000 characters", Counts: "characters", Advanced: true},
		},
	}})
}
