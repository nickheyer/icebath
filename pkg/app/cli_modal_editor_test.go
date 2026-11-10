package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// A CLI with an idle prompt application, editor launches recorded and the file left as it is
func editorCLI(t *testing.T, withApp bool) (*CLI, *[][]string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "test-editor --wait")
	var calls [][]string
	prev := runEditorCommand
	runEditorCommand = func(argv []string) (int, error) {
		calls = append(calls, argv)
		return 1, nil
	}
	t.Cleanup(func() { runEditorCommand = prev })
	c := &CLI{ctx: context.Background(), pendingInput: &inputQueue{}, interruptQueue: &inputQueue{}}
	if withApp {
		c.tui.app = newPTApplication(nil, io.Discard)
		c.tui.app.buffer.tempfileSuffix = ".md"
		c.app = c.tui.app
	}
	return c, &calls
}

// Text of a collapsed paste reference to a file holding the content
func pasteReference(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return "[Pasted text #1: 2 lines → " + path + "]"
}

func TestOpenExternalEditorUsesPromptToolkitBufferEditor(t *testing.T) {
	c, calls := editorCLI(t, true)
	if !c.openExternalEditor(nil) {
		t.Fatal("editor refused to open")
	}
	if len(*calls) != 1 || (*calls)[0][0] != "test-editor" || (*calls)[0][1] != "--wait" || !strings.HasSuffix((*calls)[0][2], ".md") {
		t.Fatalf("editor launches %v", *calls)
	}
}

func TestOpenExternalEditorRejectsWhenNoTui(t *testing.T) {
	c, calls := editorCLI(t, false)
	out := &bytes.Buffer{}
	restore := SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out}, out)
	t.Cleanup(restore)
	if c.openExternalEditor(nil) || len(*calls) != 0 {
		t.Fatalf("opened without a prompt, launches %v", *calls)
	}
	if !strings.Contains(strings.ToLower(out.String()), "interactive cli") {
		t.Fatalf("printed %q", out.String())
	}
}

func TestOpenExternalEditorExpandsPastePlaceholdersBeforeOpen(t *testing.T) {
	c, calls := editorCLI(t, true)
	buf := newPTBuffer(nil)
	_ = buf.setText(pasteReference(t, "paste.txt", "alpha\nbeta"))
	if !c.openExternalEditor(buf) {
		t.Fatal("editor refused to open")
	}
	if buf.text() != "alpha\nbeta" || buf.cursor != len("alpha\nbeta") || len(*calls) != 1 {
		t.Fatalf("buffer %q cursor %d launches %d", buf.text(), buf.cursor, len(*calls))
	}
}

func TestInlinePastesStoresFullContent(t *testing.T) {
	c, _ := editorCLI(t, true)
	buf := newPTBuffer(nil)
	_ = buf.setText(pasteReference(t, "paste.txt", "line one\nline two"))
	c.inlinePastes(buf)
	if buf.text() != "line one\nline two" || buf.cursor != len("line one\nline two") || !c.tui.skipPasteCollapse {
		t.Fatalf("buffer %q cursor %d skip %v", buf.text(), buf.cursor, c.tui.skipPasteCollapse)
	}
}

func TestInlinePastesMissingFileKeepsPlaceholder(t *testing.T) {
	c, _ := editorCLI(t, true)
	buf := newPTBuffer(nil)
	reference := pasteReference(t, "gone.txt", "")
	_ = buf.setText(reference)
	c.inlinePastes(buf)
	if buf.text() != reference || c.tui.skipPasteCollapse {
		t.Fatalf("buffer %q skip %v", buf.text(), c.tui.skipPasteCollapse)
	}
}

func TestCleanEditorSaveReplacesTheBufferText(t *testing.T) {
	c, _ := editorCLI(t, true)
	runEditorCommand = func(argv []string) (int, error) {
		return 0, os.WriteFile(argv[len(argv)-1], []byte("rewritten in the editor\n"), 0o644)
	}
	buf := c.tui.app.buffer
	_ = buf.setText("draft")
	saved := false
	if err := buf.openInEditor(c.tui.app, func() { saved = true }); err != nil {
		t.Fatal(err)
	}
	if !saved || buf.text() != "rewritten in the editor" || buf.cursor != len("rewritten in the editor") {
		t.Fatalf("saved %v buffer %q cursor %d", saved, buf.text(), buf.cursor)
	}
}
