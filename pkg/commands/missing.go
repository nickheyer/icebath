package commands

// Breaks the build until every unwritten piece of the port exists
var _ = []any{
	NewCompleterFromCommandsCompletionPy,
	RenderCLIHelpFromCliInfoMixinShowHelp,
	GatewayHelpExecutorFromSlashExecPy,
	GatewayCommandsPageExecutorFromSlashExecPy,
	ParseCLIArgvArgparseCompatibleFromParserPy,
	CLITreeFromHermesCliSubcommands,
	ShellCompletionScriptsBashZshFishPowershell,
}
