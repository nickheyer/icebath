package browser

// Registers browser_exec, browser_cdp, browser_dialog and browser_vault into the tool registry
func init() {
	registerBrowserExecTool()
	registerBrowserCDPTool()
	registerBrowserDialogTool()
	registerBrowserVaultTool()
}
