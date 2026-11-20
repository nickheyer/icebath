package tools

// Registers the core tool families of this package in a fixed order
func init() {
	registerFileTools()
	registerWebTools()
	registerVisionTools()
	registerTodoTool()
	registerClarifyTool()
	registerSessionSearchTool()
	registerProjectTools()
	registerCheckpointManager()
}
