package app

import (
	"os"
	"os/exec"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
)

// Editors tried after VISUAL and EDITOR
var fallbackEditors = []string{"/usr/bin/editor", "/usr/bin/nano", "/usr/bin/pico", "/usr/bin/vi", "/usr/bin/emacs"}

// Runs an editor command on a file with the terminal attached, swapped by tests
var runEditorCommand = func(argv []string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Opens the file in the first editor that starts, true when it exited cleanly
func openFileInEditor(filename string) bool {
	for _, editor := range append([]string{os.Getenv("VISUAL"), os.Getenv("EDITOR")}, fallbackEditors...) {
		if editor == "" {
			continue
		}
		argv, err := pyshlex.SplitPOSIX(editor, false)
		if err != nil || len(argv) == 0 {
			continue
		}
		code, err := runEditorCommand(append(argv, filename))
		if err != nil {
			continue
		}
		return code == 0
	}
	return false
}

// Edits the buffer text in an external editor, done running after a clean save replaced the text
func (b *ptBuffer) openInEditor(app *ptApplication, done func()) error {
	if b.isReadOnly() {
		return errEditReadOnlyBuffer
	}
	f, err := os.CreateTemp("", "*"+b.tempfileSuffix)
	if err != nil {
		return err
	}
	filename := f.Name()
	_, werr := f.WriteString(b.text())
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(filename)
		return werr
	}
	app.RunInTerminal(func() {
		defer os.Remove(filename)
		if !openFileInEditor(filename) {
			return
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return
		}
		text := strings.TrimSuffix(string(data), "\n")
		_ = b.setDocument(text, len([]rune(text)), false)
		if done != nil {
			done()
		}
	})
	return nil
}

// Opens the active input buffer in an external editor, a clean save submitting the draft
func (c *CLI) openExternalEditor(buffer *ptBuffer) bool {
	dim := func(key string, kwargs map[string]any) { cprint(dimANSI + c.t(key, kwargs) + resetANSI) }
	app := c.tui.app
	if app == nil {
		dim("cli.editor.only_interactive", nil)
		return false
	}
	c.uiMu.Lock()
	running := c.commandRunning
	prompting := c.sudoState != nil || c.secretState != nil || c.approvalState != nil || c.slashConfirmState != nil ||
		c.clarifyState != nil || c.connectionState != nil
	c.uiMu.Unlock()
	if running {
		dim("cli.editor.wait_for_command", nil)
		return false
	}
	if prompting {
		dim("cli.editor.finish_active_prompt", nil)
		return false
	}
	if buffer == nil {
		buffer = app.buffer
	}
	if buffer == nil {
		dim("cli.editor.no_input_buffer", nil)
		return false
	}
	c.inlinePastes(buffer)
	c.tui.skipPasteCollapse = true
	if err := buffer.openInEditor(app, func() { c.submitEditorBuffer(buffer) }); err != nil {
		dim("cli.editor.open_failed", map[string]any{"error": err})
		return false
	}
	return true
}

// Submits the draft an external editor left the way Enter does, bang and slash lines dispatched, the rest queued
func (c *CLI) submitEditorBuffer(buffer *ptBuffer) {
	text := strings.TrimSpace(buffer.text())
	if text == "" {
		return
	}
	c.uiMu.Lock()
	app := c.app
	c.uiMu.Unlock()
	done := func() {
		c.resetInputBuffer(buffer)
		if app != nil {
			_ = app.Invalidate()
		}
	}
	failed := func(key string, r any) {
		cprint("  " + dimANSI + c.t(key, map[string]any{"error": r}) + resetANSI)
	}
	handled, panicked := false, false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				failed("cli.editor.shell_command_failed", r)
			}
		}()
		handled = c.handleBangShell(text)
	}()
	if handled || panicked {
		done()
		return
	}
	if commands.LooksLikeSlashCommand(text) {
		func() {
			defer done()
			defer func() {
				if r := recover(); r != nil {
					failed("cli.editor.command_failed", r)
				}
			}()
			if !c.processCommand(text) {
				c.setShouldExit(false)
				if app != nil && app.IsRunning() {
					app.Exit()
				}
			}
		}()
		return
	}
	c.uiMu.Lock()
	running := c.agentRunning
	c.uiMu.Unlock()
	if running {
		if c.busyInputMode == "interrupt" {
			c.interruptQueue.Put(text)
		} else {
			c.pendingInput.Put(text)
		}
		cprint("  " + c.t("cli.editor.queued_next_turn", map[string]any{"preview": preview80(text)}))
	} else {
		c.pendingInput.Put(text)
	}
	done()
}
