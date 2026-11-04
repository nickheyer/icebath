package browser

// Registers every browser tool into tools.Default once the remaining modules exist
func init() {
	registerPageTools()
	registerCDPTool()
	registerDialogTool()
	registerVaultTool()
}
