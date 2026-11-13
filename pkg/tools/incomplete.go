package tools

// Every name below is a hermes module not yet ported, the build fails here until each one lands
func init() {
	registerFileTools()
	registerWebTools()
	registerVisionTools()
	registerXSearchTool()
	registerTodoTool()
	registerClarifyTool()
	registerSessionSearchTool()
	registerDesktopUITools()
	registerDiscordTools()
	registerProjectTools()
	registerSetupBeatTool()
	registerCheckpointManager()
	registerRemoteTerminalBackends()
}
