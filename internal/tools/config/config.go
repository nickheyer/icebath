package config

const (
	Key                 = "tools"
	Disabled            = Key + ".disabled"
	OutputMaxBytes      = Key + ".output.max_bytes"
	OutputMaxLineLength = Key + ".output.max_line_length"
	OutputMaxLines      = Key + ".output.max_lines"
	// Mask API keys and tokens in what tools hand the model
	RedactSecrets = Key + ".redact_secrets"
	// Folders whose own toolchains linters and code checkers may run, beside the git worktrees the user works in
	TrustedWorkspaces = Key + ".trusted_workspaces"
	// Changes to files shown as a diff in the transcript
	InlineDiffs = Key + ".inline_diffs"
	// Pictures tools made or looked at drawn in the transcript, and how
	InlineImages = Key + ".inline_images"
	// Largest result of an MCP tool kept in the chat before it is saved to a file
	BudgetMCPResult = Key + ".budget.mcp_result_size_chars"
)

// Ways tools.inline_images draws pictures
const (
	ImagesAuto  = "auto"
	ImagesKitty = "kitty"
	ImagesBlock = "unicode"
	ImagesOff   = "off"
)
