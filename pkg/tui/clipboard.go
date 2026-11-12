package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"unicode/utf16"
)

const clipboardMaxBuffer = 4 * 1024 * 1024

var powershellReadArgs = []string{
	"-NoProfile", "-NonInteractive", "-Command",
	"[Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes((Get-Clipboard -Raw)))",
}

var errClipboardOverflow = errors.New("clipboard output exceeds the buffer")

// One clipboard read command, base64 when its stdout holds the text encoded
type clipboardReadCmd struct {
	cmd    string
	args   []string
	base64 bool
}

// One clipboard write command, fed the text on stdin or through a PowerShell script argument
type clipboardWriteCmd struct {
	cmd   string
	args  []string
	stdin bool
}

// Runs a command and returns its stdout, the clipboard read seam
type clipboardRun func(ctx context.Context, cmd string, args []string) (string, error)

// Runs a command with optional stdin and reports a zero exit, the clipboard write seam
type clipboardStart func(ctx context.Context, cmd string, args []string, stdin *string) bool

// Writer that fails once more than its limit arrives
type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errClipboardOverflow
	}
	return b.Buffer.Write(p)
}

// Stdout of a command capped at the clipboard buffer size
func runClipboardCommand(ctx context.Context, cmd string, args []string) (string, error) {
	c := exec.CommandContext(ctx, cmd, args...)
	out := &cappedBuffer{limit: clipboardMaxBuffer}
	c.Stdout = out
	if err := c.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Runs a clipboard writer, piping the text to its stdin when given, true on a zero exit
func startClipboardCommand(ctx context.Context, cmd string, args []string, stdin *string) bool {
	c := exec.CommandContext(ctx, cmd, args...)
	if stdin != nil {
		c.Stdin = strings.NewReader(*stdin)
	}
	return c.Run() == nil
}

// Clipboard text that looks like real text, not empty, whitespace or binary residue
func isUsableClipboardText(text string) bool {
	if !strings.ContainsFunc(text, func(r rune) bool { return !isJSSpace(r) }) {
		return false
	}
	if strings.ContainsRune(text, 0) {
		return false
	}
	suspicious, units := 0, 0
	for _, r := range text {
		units += max(1, utf16.RuneLen(r))
		if (r < 0x20 && r != '\n' && r != '\r' && r != '\t') || r == '\ufffd' {
			suspicious++
		}
	}
	return suspicious <= max(2, units*2/100)
}

// Read commands to try in order for a platform and environment
func readClipboardCommands(platform string, env func(string) string) []clipboardReadCmd {
	switch platform {
	case "darwin":
		return []clipboardReadCmd{{cmd: "pbpaste"}}
	case "windows":
		return []clipboardReadCmd{{cmd: "powershell", args: powershellReadArgs, base64: true}}
	}
	var attempts []clipboardReadCmd
	if env("WSL_INTEROP") != "" || env("WSL_DISTRO_NAME") != "" {
		attempts = append(attempts, clipboardReadCmd{cmd: "powershell.exe", args: powershellReadArgs, base64: true})
	}
	if env("WAYLAND_DISPLAY") != "" {
		attempts = append(attempts, clipboardReadCmd{cmd: "wl-paste", args: []string{"--type", "text"}})
	}
	return append(attempts, clipboardReadCmd{cmd: "xclip", args: []string{"-selection", "clipboard", "-out"}})
}

// Clipboard text from the first backend that answers, false when every backend failed
func readClipboardTextWith(ctx context.Context, platform string, run clipboardRun, env func(string) string) (string, bool) {
	for _, attempt := range readClipboardCommands(platform, env) {
		out, err := run(ctx, attempt.cmd, append([]string(nil), attempt.args...))
		if err != nil {
			continue
		}
		if attempt.base64 {
			decoded, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
			return strings.ToValidUTF8(string(decoded), "\ufffd"), true
		}
		return out, true
	}
	return "", false
}

// Clipboard text of the host platform
func readClipboardText(ctx context.Context) (string, bool) {
	return readClipboardTextWith(ctx, runtime.GOOS, runClipboardCommand, getenv)
}

// PowerShell command that sets the clipboard from base64 text
func powershellWriteScript(b64 string) string {
	return "Set-Clipboard -Value ([System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String('" + b64 + "')))"
}

// Write commands to try in order for a platform and environment
func writeClipboardCommands(platform string, env func(string) string) []clipboardWriteCmd {
	switch platform {
	case "darwin":
		return []clipboardWriteCmd{{cmd: "pbcopy", stdin: true}}
	case "windows":
		return []clipboardWriteCmd{{cmd: "powershell", args: []string{"-NoProfile", "-NonInteractive"}}}
	}
	var attempts []clipboardWriteCmd
	if env("WSL_INTEROP") != "" || env("WSL_DISTRO_NAME") != "" {
		attempts = append(attempts, clipboardWriteCmd{cmd: "powershell.exe", args: []string{"-NoProfile", "-NonInteractive"}})
	}
	if env("WAYLAND_DISPLAY") != "" {
		attempts = append(attempts, clipboardWriteCmd{cmd: "wl-copy", args: []string{"--type", "text/plain"}, stdin: true})
	}
	return append(attempts,
		clipboardWriteCmd{cmd: "xclip", args: []string{"-selection", "clipboard", "-in"}, stdin: true},
		clipboardWriteCmd{cmd: "xsel", args: []string{"--clipboard", "--input"}, stdin: true})
}

// Writes text through the first clipboard backend that succeeds, false when none did
func writeClipboardTextWith(ctx context.Context, text, platform string, start clipboardStart, env func(string) string) bool {
	for _, c := range writeClipboardCommands(platform, env) {
		args := append([]string(nil), c.args...)
		if c.stdin {
			if start(ctx, c.cmd, args, &text) {
				return true
			}
			continue
		}
		script := powershellWriteScript(base64.StdEncoding.EncodeToString([]byte(text)))
		if start(ctx, c.cmd, append(args, "-Command", script), nil) {
			return true
		}
	}
	return false
}

// Writes text to the host clipboard
func writeClipboardText(ctx context.Context, text string) bool {
	return writeClipboardTextWith(ctx, text, runtime.GOOS, startClipboardCommand, getenv)
}

// Whitespace as a JavaScript regular expression class matches it
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}
