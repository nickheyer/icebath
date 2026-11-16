package app

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// One clarify batch entry in the shape the clarify tool hands the callback
func batchQuestion(index int, question string, multi bool, choices ...string) any {
	offered := make([]any, len(choices))
	for i, choice := range choices {
		offered[i] = choice
	}
	return map[string]any{"qid": "q" + string(rune('0'+index)), "question": question, "choices": offered, "multi_select": multi && len(choices) > 0}
}

// A CLI for the clarify panel with the given clarify timeout, its scrollback output captured
func clarifyBatchCLI(t *testing.T, timeout int) (*CLI, *lockedBuffer) {
	t.Helper()
	c, _ := newModalTestCLI(t, map[string]any{"clarify": map[string]any{"timeout": timeout}})
	out := &lockedBuffer{}
	restore := SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out}, out)
	t.Cleanup(restore)
	return c, out
}

// The callback result, failing when it never returns
func batchResult(t *testing.T, result chan map[string]any) map[string]any {
	t.Helper()
	select {
	case r := <-result:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("clarify callback never returned")
		return nil
	}
}

func wantBatch(t *testing.T, got map[string]any, outcome string, answers map[string]any) {
	t.Helper()
	if got["outcome"] != outcome || !reflect.DeepEqual(got["answers"], answers) {
		t.Fatalf("result %v, want %s %v", got, outcome, answers)
	}
}

// Locked answer of a question, empty when unanswered
func lockedAnswer(state *clarifyModalState, qid string) string {
	if a := state.Answers[qid]; a != nil {
		return *a
	}
	return ""
}

// Reports whether the clarify panel is in typed answer mode
func (c *CLI) clarifyTyping() bool {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.clarifyFreetext
}

// Sets the typed answer mode of the clarify panel
func (c *CLI) setClarifyTyping(on bool) {
	c.uiMu.Lock()
	c.clarifyFreetext = on
	c.uiMu.Unlock()
}
