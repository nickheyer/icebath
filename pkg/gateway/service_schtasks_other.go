//go:build !windows

package gateway

// The Windows only RuntimeError schtasks raises on every other host
func execSchtasksOnHost(args []string) (int, string, string, error) {
	return 0, "", "", errGatewayWindowsOnly
}

// Nil off Windows where no console mode fact exists
func stdinConsoleModeOK() *bool { return nil }

// The missing windll a ShellExecuteW call meets on every other host
func shellExecuteRunas(file, params, cwd string) (uintptr, error) { return 0, errGatewayWindowsOnly }
