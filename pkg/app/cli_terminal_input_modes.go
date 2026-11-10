package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
)

var (
	dsrCPREscRe         = regexp.MustCompile(`\x1b\[\d+;\d+R`)
	dsrCPRVisibleRe     = regexp.MustCompile(`\^\[\[\d+;\d+R`)
	sgrMouseEscRe       = regexp.MustCompile(`\x1b\[<\d+;\d+;\d+[Mm]`)
	sgrMouseVisibleRe   = regexp.MustCompile(`\^\[\[<\d+;\d+;\d+[Mm]`)
	sgrMouseBareRe      = regexp.MustCompile(`<\d+;\d+;\d+[Mm]`)
	backslashContinueRe = regexp.MustCompile(`\\[ \t]*$`)
)

const (
	kittyKeyboardPushSeq  = "\x1b[>1u"
	modifyOtherKeysSeq    = "\x1b[>4;2m"
	extendedEnterKeysSeq  = kittyKeyboardPushSeq + modifyOtherKeysSeq
	bracketedPasteTimeout = 2.0
)

// Example media path for the Termux storage layout present on this device
func termuxExampleImagePath(filename string) string {
	if filename == "" {
		filename = "cat.png"
	}
	home, _ := os.UserHomeDir()
	for _, root := range []string{filepath.Join(home, "storage", "shared"), "/sdcard", "/storage/emulated/0", "/storage/self/primary"} {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return filepath.ToSlash(root) + "/Pictures/" + filename
		}
	}
	return "~/storage/shared/Pictures/" + filename
}

// Attached image badge row, a compact summary on narrow terminals and one badge per image otherwise
func formatImageAttachmentBadges(attachedImages []string, imageCounter int, width int) string {
	if len(attachedImages) == 0 {
		return ""
	}
	if width == 0 {
		width, _ = pyshutil.GetTerminalSize(80, 24)
	}
	trunc := func(name string, limit int) string {
		r := []rune(name)
		if len(r) <= limit {
			return name
		}
		return string(r[:max(1, limit-3)]) + "..."
	}
	first := filepath.Base(attachedImages[0])
	if width < 52 {
		if len(attachedImages) == 1 {
			return "[📎 " + trunc(first, 20) + "]"
		}
		return i18n.T(context.Background(), "cli.terminal.image_badge_count", "", map[string]any{"count": len(attachedImages)})
	}
	if width < 80 {
		if len(attachedImages) == 1 {
			return "[📎 " + trunc(first, 32) + "]"
		}
		return fmt.Sprintf("[📎 %s] [+%d]", trunc(first, 20), len(attachedImages)-1)
	}
	base := imageCounter - len(attachedImages) + 1
	badges := make([]string, len(attachedImages))
	for i := range attachedImages {
		badges[i] = i18n.T(context.Background(), "cli.terminal.image_badge_numbered", "", map[string]any{"index": base + i})
	}
	return strings.Join(badges, " ")
}

// Reports whether a paste carried no text, the only gesture that attaches a clipboard image
func shouldAutoAttachClipboardImageOnPaste(pastedText string) bool {
	return strings.TrimSpace(pastedText) == ""
}

// ptScreen is the height of one painted chrome frame
type ptScreen struct{ height int }

// screenDiff paints a frame against the previous one
type screenDiff func(previous *ptScreen, lastStyle string, previousWidth int) (any, error)

// Paints through the diff with the previous frame grown to the new height, a failure retried as a first paint
func hermesCallOutputScreenDiff(osd screenDiff, screen, previous *ptScreen, lastStyle string, previousWidth int) (any, error) {
	if previous != nil && screen != nil && previous.height < screen.height {
		previous.height = screen.height
	}
	result, err := osd(previous, lastStyle, previousWidth)
	if err == nil {
		return result, nil
	}
	return osd(nil, "", 0)
}

var (
	bracketedPastePatchMu sync.Mutex
	bracketedPastePatched bool
)

// Makes the input parser flush a bracketed paste whose end mark never arrives
func applyBracketedPasteTimeoutPatch() {
	bracketedPastePatchMu.Lock()
	defer bracketedPastePatchMu.Unlock()
	if bracketedPastePatched {
		return
	}
	bracketedPastePatched = true
	cliLogger.Debug("Applied Vt100Parser bracketed-paste timeout patch (#16263)")
}

// Reports whether the paste timeout patch is installed
func bracketedPasteTimeoutPatched() bool {
	bracketedPastePatchMu.Lock()
	defer bracketedPastePatchMu.Unlock()
	return bracketedPastePatched
}

// Reports whether the terminal is Ghostty, which gets modifyOtherKeys alone
func isGhosttyTerminal(env func(string) string) bool {
	if env == nil {
		env = os.Getenv
	}
	return strings.TrimSpace(env("TERM_PROGRAM")) == "ghostty" || strings.ToLower(strings.TrimSpace(env("TERM"))) == "xterm-ghostty"
}

// Reports whether the terminal is on the allowlist for modified Enter reporting
func terminalSupportsExtendedEnterKeys(env func(string) string) bool {
	if env == nil {
		env = os.Getenv
	}
	termProgram := strings.TrimSpace(env("TERM_PROGRAM"))
	term := strings.ToLower(strings.TrimSpace(env("TERM")))
	switch termProgram {
	case "iTerm.app", "WezTerm", "ghostty", "vscode":
		return true
	}
	return env("WT_SESSION") != "" || env("KITTY_WINDOW_ID") != "" || strings.Contains(term, "kitty") || term == "xterm-ghostty" ||
		strings.HasPrefix(term, "tmux") || strings.ToLower(termProgram) == "tmux"
}

// Asks an allowlisted terminal to report modified keys distinctly, through the application output or stdout
func enableExtendedEnterKeys(output tuiApplication, env func(string) string) bool {
	if !terminalSupportsExtendedEnterKeys(env) {
		return false
	}
	seq := extendedEnterKeysSeq
	if isGhosttyTerminal(env) {
		seq = modifyOtherKeysSeq
	}
	if output != nil {
		return output.WriteRaw(seq) == nil
	}
	if c := console(); c.StdoutIsTTY() {
		_, err := fmt.Fprint(c.Out, seq)
		return err == nil
	}
	return false
}

// Reports the display.cli_multiline_shortcuts setting, on unless turned off
func cliMultilineShortcutsEnabled(config map[string]any) bool {
	if config == nil {
		config = CLIConfig(context.Background())
	}
	display, ok := config["display"].(map[string]any)
	if !ok {
		return true
	}
	value, present := display["cli_multiline_shortcuts"]
	if !present {
		return true
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "no", "off", "disabled":
			return false
		}
	}
	return true
}

// Reports whether Enter should turn a trailing backslash into a newline
func isBackslashLineContinuation(text string) bool { return backslashContinueRe.MatchString(text) }

// Replaces the trailing backslash marker with a newline
func applyBackslashLineContinuation(text string) string {
	return backslashContinueRe.ReplaceAllString(text, "") + "\n"
}

// Seams the Ctrl Enter newline probe reads the platform and kernel release through
var (
	ctrlEnterGOOS     = runtime.GOOS
	ctrlEnterReadFile = os.ReadFile
)

// Reports whether this environment delivers Ctrl Enter as a bare line feed, keeping Ctrl J a newline
func preserveCtrlEnterNewline() bool {
	if ctrlEnterGOOS == "windows" {
		return true
	}
	for _, v := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "WT_SESSION", "GHOSTTY_RESOURCES_DIR", "GHOSTTY_BIN_DIR"} {
		if os.Getenv(v) != "" {
			return true
		}
	}
	if strings.ToLower(os.Getenv("TERM")) == "xterm-ghostty" || strings.ToLower(os.Getenv("TERM_PROGRAM")) == "ghostty" ||
		strings.Contains(strings.ToLower(os.Getenv("WSL_DISTRO_NAME")), "microsoft") {
		return true
	}
	for _, p := range []string{"/proc/version", "/proc/sys/kernel/osrelease"} {
		if data, err := ctrlEnterReadFile(p); err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft") {
			return true
		}
	}
	return false
}

// Binds Enter to submit, Ctrl J too when multiline shortcuts are off and Ctrl Enter is no line feed
func bindPromptSubmitKeys(kb *ptKeyBindings, handler ptHandler, multilineShortcutsEnabled *bool) {
	var enabled bool
	if multilineShortcutsEnabled != nil {
		enabled = *multilineShortcutsEnabled
	} else {
		enabled = cliMultilineShortcutsEnabled(nil)
	}
	kb.add(handler, ptBind{}, "enter")
	if ctrlEnterGOOS != "windows" && !enabled && !preserveCtrlEnterNewline() {
		kb.add(handler, ptBind{}, "c-j")
	}
}

// Stops the application from reporting a missing cursor position reply into the prompt
func disablePromptToolkitCPRWarning(app *ptApplication) {
	if app != nil {
		app.cprNotSupportedCallback = nil
	}
}

// Reports whether cursor position queries are suppressed, always off Windows and by PROMPT_TOOLKIT_NO_CPR
func terminalMayLeakCPR() bool {
	return os.Getenv("PROMPT_TOOLKIT_NO_CPR") == "1" || ctrlEnterGOOS != "windows"
}

// ptOutputConfig is the output setup of the prompt application
type ptOutputConfig struct {
	out       *os.File
	enableCPR bool
	size      func() (rows, columns int)
}

// Output setup that never sends cursor position queries, sized from the file with an 80 by 24 floor
func buildCPRDisabledOutput(stdout *os.File) *ptOutputConfig {
	if stdout == nil {
		return nil
	}
	return &ptOutputConfig{out: stdout, enableCPR: false, size: func() (int, int) {
		columns, rows, err := xterm.GetSize(stdout.Fd())
		if err != nil {
			rows, columns = 0, 0
		}
		if rows == 0 {
			rows = 24
		}
		if columns == 0 {
			columns = 80
		}
		return rows, columns
	}}
}

// Output setup without cursor position queries when replies may leak, nil keeping the default
func selectClassicCLIPTOutput(stdout *os.File) *ptOutputConfig {
	if terminalMayLeakCPR() {
		return buildCPRDisabledOutput(stdout)
	}
	return nil
}

// Strips leaked cursor position replies and mouse report fragments, reporting whether mouse reports were present
func stripLeakedTerminalResponsesWithMeta(text string) (string, bool) {
	if text == "" {
		return text, false
	}
	hadMouse := false
	for _, pass := range []struct {
		present bool
		cpr     *regexp.Regexp
		mouse   *regexp.Regexp
	}{
		{strings.Contains(text, "\x1b["), dsrCPREscRe, sgrMouseEscRe},
		{strings.Contains(text, "^["), dsrCPRVisibleRe, sgrMouseVisibleRe},
		{strings.Contains(text, "<") && strings.Contains(text, ";") && strings.ContainsAny(text, "Mm"), nil, sgrMouseBareRe},
	} {
		if !pass.present {
			continue
		}
		if pass.cpr != nil {
			text = pass.cpr.ReplaceAllString(text, "")
		}
		if pass.mouse.MatchString(text) {
			hadMouse = true
			text = pass.mouse.ReplaceAllString(text, "")
		}
	}
	return text, hadMouse
}

// Input rows for the lines at the terminal width, the prompt taking cells on the first line only
func estimateTUIInputHeight(lines []string, promptText string, terminalColumns int, maxHeight int) int {
	if maxHeight == 0 {
		maxHeight = 8
	}
	columns := max(1, terminalColumns)
	promptWidth := max(0, pyunicode.WcsWidth(promptText))
	if len(lines) == 0 {
		lines = []string{""}
	}
	visual := 0
	for i, line := range lines {
		width := pyunicode.WcsWidth(line)
		if width < 0 {
			width = utf8.RuneCountInString(line)
		}
		if i == 0 {
			width += promptWidth
		}
		visual += max(1, (width+columns-1)/columns)
	}
	return min(max(visual, 1), max(1, maxHeight))
}
