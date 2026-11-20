//go:build windows

package gatewaycli

import "github.com/nickheyer/icebath/pkg/tools"

// Exit code, stdout and stderr of schtasks under its fifteen second timeout
func ExecSchtasks(args []string) (int, string, string, error) {
	code, out, errText := tools.ExecSchtasks(args)
	return code, out, errText, nil
}
