package tui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/tty"
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

// Suspends the screen for the user's editor on a file in place, handing done the run failure
func editFile(path string, done func(runErr error) tea.Msg) tea.Cmd {
	argv := resolveEditor(os.LookupEnv, runtime.GOOS)
	return tty.Exec(exec.Command(argv[0], append(argv[1:], path)...), done)
}

// Suspends the screen for the user's editor, handing done the saved text, the run failure and the temp file failure
func editText(initial, suffix string, done func(text string, runErr, fileErr error) tea.Msg) tea.Cmd {
	dir, err := os.MkdirTemp("", "icebath-edit-")
	if err != nil {
		return func() tea.Msg { return done("", nil, err) }
	}
	file := filepath.Join(dir, "edit"+suffix)
	if err := os.WriteFile(file, []byte(initial), 0o644); err != nil {
		err = errors.Join(err, os.RemoveAll(dir))
		return func() tea.Msg { return done("", nil, err) }
	}
	argv := resolveEditor(os.LookupEnv, runtime.GOOS)
	cmd := exec.Command(argv[0], append(argv[1:], file)...)
	return tty.Exec(cmd, func(runErr error) tea.Msg {
		defer os.RemoveAll(dir)
		var exit *exec.ExitError
		if runErr != nil && !errors.As(runErr, &exit) {
			return done("", runErr, nil)
		}
		raw, err := os.ReadFile(file)
		return done(string(raw), runErr, err)
	})
}

// Command suspending the screen while the user edits text in their editor, the edited text arriving as editorDoneMsg
func openInEditor(initial, suffix string) tea.Cmd {
	return editText(initial, suffix, func(text string, runErr, fileErr error) tea.Msg {
		switch {
		case runErr != nil:
			return editorDoneMsg{}
		case fileErr != nil:
			return editorDoneMsg{err: fileErr}
		}
		return editorDoneMsg{text: text, ok: true}
	})
}
