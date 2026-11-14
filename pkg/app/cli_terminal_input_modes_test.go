package app

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Clears every variable the Ctrl Enter probe reads and hides the kernel release files
func localLinuxCtrlEnterEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "WT_SESSION", "GHOSTTY_RESOURCES_DIR", "GHOSTTY_BIN_DIR", "TERM", "TERM_PROGRAM", "WSL_DISTRO_NAME"} {
		t.Setenv(name, env[name])
	}
	prevRead, prevOS := ctrlEnterReadFile, ctrlEnterGOOS
	ctrlEnterReadFile = func(string) ([]byte, error) { return nil, errors.New("no /proc") }
	ctrlEnterGOOS = "linux"
	t.Cleanup(func() { ctrlEnterReadFile, ctrlEnterGOOS = prevRead, prevOS })
}

// Key sequences the submit binder registers on a local Linux terminal
func submitKeysForLocalLinux(t *testing.T, multiline bool) [][]string {
	t.Helper()
	localLinuxCtrlEnterEnv(t, nil)
	kb := &ptKeyBindings{}
	bindPromptSubmitKeys(kb, func(*ptEvent) {}, &multiline)
	var bound [][]string
	for _, b := range kb.bindings {
		bound = append(bound, b.keys)
	}
	return bound
}

func hasKeys(bound [][]string, keys ...string) bool {
	for _, b := range bound {
		if reflect.DeepEqual(b, keys) {
			return true
		}
	}
	return false
}

func TestNativeWindowsPreservesNewline(t *testing.T) {
	localLinuxCtrlEnterEnv(t, nil)
	ctrlEnterGOOS = "windows"
	if !preserveCtrlEnterNewline() {
		t.Fatal("Windows did not keep Ctrl J a newline")
	}
}

func TestSshTtyAlonePreservesNewline(t *testing.T) {
	localLinuxCtrlEnterEnv(t, map[string]string{"SSH_TTY": "/dev/pts/0"})
	if !preserveCtrlEnterNewline() {
		t.Fatal("SSH_TTY did not keep Ctrl J a newline")
	}
}

func TestWindowsTerminalSessionPreservesNewline(t *testing.T) {
	localLinuxCtrlEnterEnv(t, map[string]string{"WT_SESSION": "abc-def"})
	if !preserveCtrlEnterNewline() {
		t.Fatal("WT_SESSION did not keep Ctrl J a newline")
	}
}

func TestGhosttyTmuxSessionPreservesCtrlJNewline(t *testing.T) {
	localLinuxCtrlEnterEnv(t, map[string]string{"TERM": "tmux-256color", "TERM_PROGRAM": "tmux", "GHOSTTY_RESOURCES_DIR": "/usr/share/ghostty"})
	if !preserveCtrlEnterNewline() {
		t.Fatal("Ghostty under tmux did not keep Ctrl J a newline")
	}
}

func TestCliMultilineShortcutsCanBeDisabled(t *testing.T) {
	if cliMultilineShortcutsEnabled(map[string]any{"display": map[string]any{"cli_multiline_shortcuts": false}}) {
		t.Fatal("setting ignored")
	}
}

func TestCtrlJIsNotSubmitWhenMultilineShortcutsEnabled(t *testing.T) {
	bound := submitKeysForLocalLinux(t, true)
	if !hasKeys(bound, "c-m") || hasKeys(bound, "c-j") {
		t.Fatalf("bound %q", bound)
	}
}

func TestCtrlJLegacySubmitWhenMultilineShortcutsDisabled(t *testing.T) {
	bound := submitKeysForLocalLinux(t, false)
	if !hasKeys(bound, "c-m") || !hasKeys(bound, "c-j") {
		t.Fatalf("bound %q", bound)
	}
}

func TestBackslashEnterContinuationReplacesMarkerWithNewline(t *testing.T) {
	for _, text := range []string{"first line\\", "first line\\   "} {
		if got := applyBackslashLineContinuation(text); got != "first line\n" {
			t.Fatalf("%q continued as %q", text, got)
		}
	}
}

// Environment lookup over a fixed map
func envOf(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func TestUnknownTerminalDoesNotEnableExtendedEnterKeys(t *testing.T) {
	if terminalSupportsExtendedEnterKeys(envOf(map[string]string{"TERM_PROGRAM": "unknown"})) {
		t.Fatal("unknown terminal allowlisted")
	}
}

// Bytes the extended key request wrote for an environment
func extendedKeysWritten(t *testing.T, env map[string]string) string {
	t.Helper()
	out := &fakeTUIApp{}
	if !enableExtendedEnterKeys(out, envOf(env)) {
		t.Fatal("request reported no write")
	}
	return strings.Join(out.rawWrites(), "")
}

func TestGhosttyUsesModifyOtherKeysOnly(t *testing.T) {
	written := extendedKeysWritten(t, map[string]string{"TERM_PROGRAM": "ghostty", "TERM": "xterm-ghostty"})
	if !strings.Contains(written, "\x1b[>4;2m") || strings.Contains(written, "\x1b[>1u") {
		t.Fatalf("wrote %q", written)
	}
}

func TestGhosttyViaTermVarUsesModifyOtherKeysOnly(t *testing.T) {
	written := extendedKeysWritten(t, map[string]string{"TERM": "xterm-ghostty"})
	if !strings.Contains(written, "\x1b[>4;2m") || strings.Contains(written, "\x1b[>1u") {
		t.Fatalf("wrote %q", written)
	}
}

func TestNonGhosttyTerminalsStillPushKittyProtocol(t *testing.T) {
	written := extendedKeysWritten(t, map[string]string{"TERM_PROGRAM": "iTerm.app", "TERM": "xterm-256color"})
	if !strings.Contains(written, "\x1b[>1u") || !strings.Contains(written, "\x1b[>4;2m") {
		t.Fatalf("wrote %q", written)
	}
}

func TestProcVersionMicrosoftMarkerPreservesNewline(t *testing.T) {
	localLinuxCtrlEnterEnv(t, nil)
	ctrlEnterReadFile = func(path string) ([]byte, error) {
		if strings.Contains(path, "/proc/version") || strings.Contains(path, "/proc/sys/kernel/osrelease") {
			return []byte("Linux version 5.15.167.4-microsoft-standard-WSL2"), nil
		}
		return nil, errors.New("unexpected read")
	}
	if !preserveCtrlEnterNewline() {
		t.Fatal("WSL kernel marker ignored")
	}
}

func TestIsGhosttyTerminalDetectionPaths(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{"TERM_PROGRAM": "ghostty"}, true},
		{map[string]string{"TERM": "xterm-ghostty"}, true},
		{map[string]string{"TERM": "XTERM-GHOSTTY"}, true},
		{map[string]string{"TERM_PROGRAM": "iTerm.app"}, false},
		{map[string]string{}, false},
	} {
		if got := isGhosttyTerminal(envOf(tc.env)); got != tc.want {
			t.Fatalf("env %v detected %v", tc.env, got)
		}
	}
}
