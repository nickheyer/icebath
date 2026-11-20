// Package clip puts text on the clipboard of the person at the terminal and asks the terminal for what its clipboard holds
package clip

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Longest a clipboard tool or the tmux buffer load may take before a copy goes on without it
const toolTimeout = 2 * time.Second

// Clipboard query of the terminal, answered with the clipboard sequence holding the text
const querySequence = "\x1b]52;c;?\x07"

var (
	envOn  = regexp.MustCompile(`(?i)^(?:1|true|yes|on)$`)
	envOff = regexp.MustCompile(`(?i)^(?:0|false|no|off)$`)
)

// Switches that force the clipboard sequence on or off, the first one set deciding
var sequenceSwitches = []string{"ICEBATH_TUI_FORCE_OSC52", "ICEBATH_TUI_CLIPBOARD_OSC52", "ICEBATH_TUI_COPY_OSC52"}

// Terminals whose own clipboard sequence is reliable, where a clipboard tool beside it races the terminal's write
var sequenceCapable = map[string]bool{"ghostty": true, "kitty": true, "WezTerm": true, "windows-terminal": true, "vscode": true}

// One clipboard write command, fed the text on stdin or through a PowerShell script argument
type WriteCmd struct {
	Cmd   string
	Args  []string
	Stdin bool
}

// Runs a command with optional stdin and reports a zero exit, the clipboard write seam
type Start func(ctx context.Context, cmd string, args []string, stdin *string) bool

// Environment lookup of the shape of os.LookupEnv
type Lookup func(string) (string, bool)

// Runs a clipboard writer, piping the text to its stdin when given, true on a zero exit
func StartCommand(ctx context.Context, cmd string, args []string, stdin *string) bool {
	c := exec.CommandContext(ctx, cmd, args...)
	if stdin != nil {
		c.Stdin = strings.NewReader(*stdin)
	}
	return c.Run() == nil
}

// PowerShell command that sets the clipboard from base64 text
func PowershellWriteScript(b64 string) string {
	return "Set-Clipboard -Value ([System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String('" + b64 + "')))"
}

// Write commands to try in order for a platform and environment
func WriteCommands(platform string, env func(string) string) []WriteCmd {
	switch platform {
	case "darwin":
		return []WriteCmd{{Cmd: "pbcopy", Stdin: true}}
	case "windows":
		return []WriteCmd{{Cmd: "powershell", Args: []string{"-NoProfile", "-NonInteractive"}}}
	}
	var attempts []WriteCmd
	if env("WSL_INTEROP") != "" || env("WSL_DISTRO_NAME") != "" {
		attempts = append(attempts, WriteCmd{Cmd: "powershell.exe", Args: []string{"-NoProfile", "-NonInteractive"}})
	}
	if env("WAYLAND_DISPLAY") != "" {
		attempts = append(attempts, WriteCmd{Cmd: "wl-copy", Args: []string{"--type", "text/plain"}, Stdin: true})
	}
	return append(attempts,
		WriteCmd{Cmd: "xclip", Args: []string{"-selection", "clipboard", "-in"}, Stdin: true},
		WriteCmd{Cmd: "xsel", Args: []string{"--clipboard", "--input"}, Stdin: true})
}

// Writes text through the first clipboard backend that succeeds, false when none did
func WriteTextWith(ctx context.Context, text, platform string, start Start, env func(string) string) bool {
	for _, c := range WriteCommands(platform, env) {
		args := append([]string(nil), c.Args...)
		if c.Stdin {
			if start(ctx, c.Cmd, args, &text) {
				return true
			}
			continue
		}
		script := PowershellWriteScript(base64.StdEncoding.EncodeToString([]byte(text)))
		if start(ctx, c.Cmd, append(args, "-Command", script), nil) {
			return true
		}
	}
	return false
}

// Writes text to the host clipboard
func WriteText(ctx context.Context, text string) bool {
	return WriteTextWith(ctx, text, runtime.GOOS, StartCommand, os.Getenv)
}

// Value of a variable, empty when it is not set
func (l Lookup) get(name string) string {
	v, _ := l(name)
	return v
}

// Name of the terminal the process runs in, empty when nothing names it
func terminalName(env Lookup) string {
	term := env.get("TERM")
	switch {
	case env.get("CURSOR_TRACE_ID") != "":
		return "cursor"
	case term == "xterm-ghostty":
		return "ghostty"
	case strings.Contains(term, "kitty"):
		return "kitty"
	case env.get("TERM_PROGRAM") != "":
		return env.get("TERM_PROGRAM")
	case env.get("TMUX") != "":
		return "tmux"
	case env.get("STY") != "":
		return "screen"
	case env.get("KITTY_WINDOW_ID") != "":
		return "kitty"
	case env.get("WT_SESSION") != "":
		return "windows-terminal"
	}
	return term
}

// Whether a copy writes the terminal's clipboard sequence: always over SSH, never inside a multiplexer on this machine, unless a switch says otherwise
func emitsSequence(env Lookup) bool {
	override := ""
	for _, name := range sequenceSwitches {
		if v, set := env(name); set {
			override = strings.TrimSpace(v)
			break
		}
	}
	switch {
	case envOn.MatchString(override):
		return true
	case envOff.MatchString(override):
		return false
	}
	return env.get("SSH_CONNECTION") != "" || (env.get("TMUX") == "" && env.get("STY") == "")
}

// Whether a copy also runs the clipboard tool of this machine, never over SSH where the tool would fill the far machine's clipboard
func usesTool(env Lookup) bool {
	switch {
	case env.get("SSH_CONNECTION") != "":
		return false
	case env.get("TMUX") != "" || env.get("STY") != "":
		return true
	case !emitsSequence(env):
		return true
	}
	return !sequenceCapable[terminalName(env)]
}

// Whether a clipboard tool can reach a clipboard here, a Linux without a display server having none
func toolReachable(platform string, env Lookup) bool {
	if platform == "darwin" || platform == "windows" {
		return true
	}
	return env.get("DISPLAY") != "" || env.get("WAYLAND_DISPLAY") != "" || env.get("WSL_INTEROP") != "" || env.get("WSL_DISTRO_NAME") != ""
}

// Loads text into the tmux paste buffer, tmux passing it on to the terminal it is attached to where its own clipboard option allows
func tmuxLoad(ctx context.Context, text string, env Lookup, start Start) bool {
	if env.get("TMUX") == "" {
		return false
	}
	args := []string{"load-buffer", "-w", "-"}
	if env.get("LC_TERMINAL") == "iTerm2" {
		args = []string{"load-buffer", "-"}
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	return start(ctx, "tmux", args, &text)
}

// Sequence wrapped for tmux to pass to the terminal under it, escapes doubled
func tmuxPassthrough(seq string) string {
	return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
}

// Sequence wrapped for the multiplexer the process runs in, unchanged outside tmux and screen
func wrapForMultiplexer(seq string, env Lookup) string {
	switch {
	case env.get("TMUX") != "":
		return tmuxPassthrough(seq)
	case env.get("STY") != "":
		return "\x1bP" + seq + "\x1b\\"
	}
	return seq
}

// Runs every path of a copy this environment has and returns the bytes to write to the terminal, empty when the sequence is not sent
func copyWith(ctx context.Context, text, platform string, env Lookup, start Start) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(text))
	if usesTool(env) && toolReachable(platform, env) {
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), toolTimeout)
			defer cancel()
			WriteTextWith(ctx, text, platform, start, env.get)
		}()
	}
	loaded := tmuxLoad(ctx, text, env, start)
	switch {
	case !emitsSequence(env):
		return ""
	case loaded:
		return tmuxPassthrough("\x1b]52;c;" + b64 + "\x07")
	case terminalName(env) == "kitty":
		return "\x1b]52;c;" + b64 + "\x1b\\"
	}
	return "\x1b]52;c;" + b64 + "\x07"
}

// Copies text for the person at the terminal: the clipboard tool of this machine where it is theirs, the tmux buffer inside tmux, and the terminal's clipboard sequence
func Copy(text string) tea.Cmd {
	return func() tea.Msg {
		seq := copyWith(context.Background(), text, runtime.GOOS, os.LookupEnv, StartCommand)
		if seq == "" {
			return nil
		}
		return tea.RawMsg{Msg: seq}
	}
}

// Sends text to the clipboard of the terminal alone, through the multiplexer when the process runs in one
func Send(text string) tea.Cmd {
	return tea.Raw(wrapForMultiplexer("\x1b]52;c;"+base64.StdEncoding.EncodeToString([]byte(text))+"\x07", os.LookupEnv))
}

// Asks the terminal for its clipboard, the answer arriving as a clipboard message
func Query() tea.Cmd {
	return tea.Raw(wrapForMultiplexer(querySequence, os.LookupEnv))
}
