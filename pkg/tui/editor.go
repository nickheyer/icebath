package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Editors tried on PATH in order when neither VISUAL nor EDITOR names one
var editorFallbacks = []string{"editor", "nano", "pico", "vi", "emacs"}

// Draft returned by the external editor, ok false when the editor exited non zero or did not start, err for a temp file failure
type editorDoneMsg struct {
	text string
	ok   bool
	err  error
}

// Editor command line, VISUAL then EDITOR split on whitespace, notepad on Windows, else the first fallback on PATH or vi
func resolveEditor(lookup func(string) (string, bool), platform string) []string {
	explicit, set := lookup("VISUAL")
	if !set {
		explicit, _ = lookup("EDITOR")
	}
	if fields := strings.FieldsFunc(explicit, isJSSpace); len(fields) > 0 {
		return fields
	}
	if platform == "windows" {
		return []string{"notepad.exe"}
	}
	path, _ := lookup("PATH")
	for _, name := range editorFallbacks {
		for _, dir := range filepath.SplitList(path) {
			if dir == "" {
				continue
			}
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return []string{candidate}
			}
		}
	}
	return []string{"vi"}
}

// Command suspending the screen while the user edits text in their editor, the edited text arriving as editorDoneMsg
func openInEditor(initial, suffix string) tea.Cmd {
	dir, err := os.MkdirTemp("", "hermes-edit-")
	if err != nil {
		return func() tea.Msg { return editorDoneMsg{err: err} }
	}
	file := filepath.Join(dir, "edit"+suffix)
	if err := os.WriteFile(file, []byte(initial), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return func() tea.Msg { return editorDoneMsg{err: err} }
	}
	argv := resolveEditor(os.LookupEnv, runtime.GOOS)
	cmd := exec.Command(argv[0], append(argv[1:], file)...)
	return tea.ExecProcess(cmd, func(runErr error) tea.Msg {
		defer os.RemoveAll(dir)
		if runErr != nil {
			return editorDoneMsg{}
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return editorDoneMsg{err: err}
		}
		return editorDoneMsg{text: string(raw), ok: true}
	})
}
