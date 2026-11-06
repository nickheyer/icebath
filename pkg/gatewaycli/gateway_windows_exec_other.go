//go:build !windows

package gatewaycli

// The Windows only RuntimeError schtasks raises on every other host
func ExecSchtasks(args []string) (int, string, string, error) {
	return 0, "", "", assertWindows()
}
