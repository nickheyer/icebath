//go:build windows

package shell

import (
	"encoding/base64"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// Script run by a POSIX shell so the final directory reaches the directory file however the command ends
const wrapper = `trap 'pwd -P > "$2"' EXIT
eval "$1"`

func (l Local) shell() string {
	if l.Shell != "" {
		return l.Shell
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p
	}
	return "powershell"
}

// Arguments that run line and report the final directory into dirFile for a POSIX shell or PowerShell
func (l Local) args(shell, line, dirFile string) []string {
	name := strings.ToLower(strings.TrimSuffix(filepath.Base(shell), ".exe"))
	if name == "powershell" || name == "pwsh" {
		script := "$ErrorActionPreference = 'Continue'\n" +
			"& { " + line + " }\n" +
			"$code = $LASTEXITCODE\n" +
			"if ($null -eq $code) { if ($?) { $code = 0 } else { $code = 1 } }\n" +
			"Set-Content -LiteralPath '" + strings.ReplaceAll(dirFile, "'", "''") + "' -Value (Get-Location).Path\n" +
			"exit $code\n"
		return []string{"-NoProfile", "-NonInteractive", "-EncodedCommand", encodeUTF16(script)}
	}
	return []string{"-c", wrapper, shell, line, dirFile}
}

// Base64 of the script in little endian UTF-16 as PowerShell expects for an encoded command
func encodeUTF16(s string) string {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(units)*2)
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// Exit status of the finished process, with 137 standing in for a kill
func exitCode(c *exec.Cmd, err error, killed bool) int {
	if err == nil {
		return 0
	}
	if killed {
		return 137
	}
	return c.ProcessState.ExitCode()
}
