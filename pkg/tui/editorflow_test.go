package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// App whose draft was just handed to an external editor, temp files of the hand over kept inside the test
func appInEditor(t *testing.T, draft string) *app {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("VISUAL", "true")
	a := testApp(t)
	a.card.composer.SetValue(draft)
	if a.openEditor() == nil {
		t.Fatal("the editor did not open")
	}
	return a
}

func queuedDrafts(t *testing.T, a *app) []string {
	t.Helper()
	rows, err := a.queue.queuedDisplay()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestSavedEditorDraftIsSentAtOnce(t *testing.T) {
	a := appInEditor(t, "draft")
	drain(a, a.onEditorDone(editorDoneMsg{text: "hello from the editor\n\n", ok: true}))
	if !a.card.composer.Empty() {
		t.Fatalf("composer still holds %q", a.card.composer.Value())
	}
	if got := queuedDrafts(t, a); !slices.Equal(got, []string{"hello from the editor"}) {
		t.Fatalf("submitted %q", got)
	}
}

func TestEditorDraftIsQueuedWhenTheDraftChangedMeanwhile(t *testing.T) {
	a := appInEditor(t, "draft")
	a.clearComposerTokens()
	a.card.composer.SetValue("newer draft")
	drain(a, a.onEditorDone(editorDoneMsg{text: "edited text\n", ok: true}))
	if a.card.composer.Value() != "newer draft" {
		t.Fatalf("composer holds %q", a.card.composer.Value())
	}
	if got := queuedDrafts(t, a); !slices.Equal(got, []string{"edited text"}) {
		t.Fatalf("queued %q", got)
	}
}

func TestEditorThatSavedNothingLeavesTheDraftUnsent(t *testing.T) {
	for _, m := range []editorDoneMsg{{}, {text: " \n\t\n", ok: true}} {
		a := appInEditor(t, "draft")
		drain(a, a.onEditorDone(m))
		if a.card.composer.Value() != "draft" || len(queuedDrafts(t, a)) != 0 {
			t.Fatalf("after %+v composer %q queue %q", m, a.card.composer.Value(), queuedDrafts(t, a))
		}
	}
}

func TestEditorFailureIsReportedInTheTranscript(t *testing.T) {
	a := appInEditor(t, "draft")
	drain(a, a.onEditorDone(editorDoneMsg{err: errors.New("no space left on device")}))
	if got := screen(a); !strings.Contains(got, "failed to open editor: no space left on device") {
		t.Fatalf("screen lacks the failure:\n%s", got)
	}
	if a.card.composer.Value() != "draft" {
		t.Fatalf("composer holds %q", a.card.composer.Value())
	}
}
