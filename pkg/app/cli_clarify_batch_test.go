package app

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
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

// Runs the batch callback the way the agent goroutine does and waits for the panel
func startBatch(t *testing.T, c *CLI, questions ...any) (*clarifyModalState, chan map[string]any) {
	t.Helper()
	result := make(chan map[string]any, 1)
	go func() { result <- c.clarifyCallback(questions) }()
	waitFor(t, "clarify panel", func() bool { return c.clarifyModal() != nil })
	return c.clarifyModal(), result
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

func TestClarifyBatchPanel(t *testing.T) {
	t.Run("test_all_locked_returns_answers_dict_keyed_by_qid", func(t *testing.T) {
		c, _ := clarifyBatchCLI(t, 60)
		state, result := startBatch(t, c, batchQuestion(0, "Color?", false, "red", "blue"), batchQuestion(1, "Size?", false, "small", "large"))
		if state.Active != 0 || !reflect.DeepEqual(state.Choices, []string{"red", "blue"}) {
			t.Fatalf("active %d choices %v", state.Active, state.Choices)
		}
		c.clarifyBatchEnter(state)
		if lockedAnswer(state, "q0") != "red" || len(state.Answers) != 1 || state.Active != 1 {
			t.Fatalf("answers %v active %d", state.Answers, state.Active)
		}
		state.Selected = 1
		c.clarifyBatchEnter(state)
		wantBatch(t, batchResult(t, result), "submitted", map[string]any{"q0": "red", "q1": "large"})
		if c.clarifyModal() != nil {
			t.Fatal("panel left open")
		}
	})
	t.Run("test_any_order_answering_via_tab_cycle", func(t *testing.T) {
		c, _ := clarifyBatchCLI(t, 60)
		state, result := startBatch(t, c, batchQuestion(0, "First?", false, "a", "b"), batchQuestion(1, "Second?", false, "c", "d"), batchQuestion(2, "Third?", false, "e", "f"))
		c.clarifyBatchSetActive(state, (state.Active+1)%3)
		c.clarifyBatchSetActive(state, (state.Active+1)%3)
		if state.Active != 2 {
			t.Fatalf("active %d", state.Active)
		}
		c.clarifyBatchEnter(state)
		if state.Active != 0 {
			t.Fatalf("active %d after locking the third", state.Active)
		}
		state.Selected = 1
		c.clarifyBatchEnter(state)
		if state.Active != 1 {
			t.Fatalf("active %d after locking the first", state.Active)
		}
		c.clarifyBatchEnter(state)
		wantBatch(t, batchResult(t, result), "submitted", map[string]any{"q0": "b", "q1": "c", "q2": "e"})
	})
	t.Run("test_reanswer_overwrites_before_completion", func(t *testing.T) {
		c, _ := clarifyBatchCLI(t, 60)
		state, result := startBatch(t, c, batchQuestion(0, "Approach?", false, "quick", "thorough"), batchQuestion(1, "Scope?", false, "narrow", "wide"))
		c.clarifyBatchEnter(state)
		if lockedAnswer(state, "q0") != "quick" || state.Active != 1 {
			t.Fatalf("answers %v active %d", state.Answers, state.Active)
		}
		c.clarifyBatchSetActive(state, 0)
		state.Selected = 1
		c.clarifyBatchEnter(state)
		if lockedAnswer(state, "q0") != "thorough" || state.Active != 1 {
			t.Fatalf("answers %v active %d", state.Answers, state.Active)
		}
		c.clarifyBatchEnter(state)
		wantBatch(t, batchResult(t, result), "submitted", map[string]any{"q0": "thorough", "q1": "narrow"})
	})
	t.Run("test_timeout_returns_partials_with_timed_out_outcome", func(t *testing.T) {
		c, _ := clarifyBatchCLI(t, 1)
		prev := modalPollInterval
		modalPollInterval = 50 * time.Millisecond
		t.Cleanup(func() { modalPollInterval = prev })
		state, result := startBatch(t, c, batchQuestion(0, "Answered?", false, "yes", "no"), batchQuestion(1, "Never answered?", false, "x", "y"))
		c.clarifyBatchEnter(state)
		wantBatch(t, batchResult(t, result), "timed_out", map[string]any{"q0": "yes"})
		if c.clarifyModal() != nil {
			t.Fatal("panel left open")
		}
	})
	t.Run("test_multi_select_lock_produces_json_array_string", func(t *testing.T) {
		c, _ := clarifyBatchCLI(t, 60)
		state, result := startBatch(t, c, batchQuestion(0, "Toppings?", true, "ham", "olives", "basil"))
		if !state.MultiSelect {
			t.Fatal("question is not multi select")
		}
		state.SelectedIndices[0], state.SelectedIndices[2] = true, true
		c.clarifyBatchEnter(state)
		answer, _ := batchResult(t, result)["answers"].(map[string]any)["q0"].(string)
		var picked []string
		if err := json.Unmarshal([]byte(answer), &picked); err != nil || !reflect.DeepEqual(picked, []string{"ham", "basil"}) {
			t.Fatalf("answer %q", answer)
		}
	})
	t.Run("test_open_ended_question_locks_typed_answer", func(t *testing.T) {
		c, _ := clarifyBatchCLI(t, 60)
		state, result := startBatch(t, c, batchQuestion(0, "Anything else?", false), batchQuestion(1, "Pick one", false, "a", "b"))
		if !c.clarifyTyping() {
			t.Fatal("open ended question did not start in typed mode")
		}
		c.setClarifyTyping(false)
		typed := "custom words"
		c.clarifyBatchLock(state, &typed, nil)
		if state.Active != 1 {
			t.Fatalf("active %d", state.Active)
		}
		c.clarifyBatchEnter(state)
		wantBatch(t, batchResult(t, result), "submitted", map[string]any{"q0": "custom words", "q1": "a"})
	})
	t.Run("test_locked_question_persists_scrollback_summary", func(t *testing.T) {
		c, out := clarifyBatchCLI(t, 60)
		state, result := startBatch(t, c, batchQuestion(0, "Color?", false, "red", "blue"), batchQuestion(1, "Size?", false, "small", "large"))
		c.clarifyBatchEnter(state)
		c.clarifyBatchEnter(state)
		batchResult(t, result)
		first := c.t("cli.session.persist_prompt_summary", map[string]any{"icon": "?", "label": "Clarify", "detail": "Color?", "outcome": "red"})
		second := c.t("cli.session.persist_prompt_summary", map[string]any{"icon": "?", "label": "Clarify", "detail": "Size?", "outcome": "small"})
		text := out.String()
		if i, j := strings.Index(text, first), strings.Index(text, second); i < 0 || j < i || strings.Count(text, "Clarify") != 2 {
			t.Fatalf("scrollback %q", text)
		}
	})
	t.Run("test_connection_callback_masks_secret_and_submits_env", func(t *testing.T) {
		c, _ := clarifyBatchCLI(t, 60)
		c.agent = &agent.AIAgent{}
		type sentAnswer struct {
			opID   string
			result map[string]any
		}
		sent := make(chan sentAnswer, 1)
		prev := connectionRespond
		connectionRespond = func(_ context.Context, _ *CLI, opID string, result map[string]any) error {
			sent <- sentAnswer{opID, result}
			return nil
		}
		t.Cleanup(func() { connectionRespond = prev })
		c.connectionCallback(map[string]any{"op_id": "op", "seq": 1, "targets": []any{map[string]any{
			"name": "asana", "state": "pending", "instructions": "Use an Asana app.",
			"required_env": []any{
				map[string]any{"name": "CLIENT_ID", "prompt": "Client ID", "required": true, "default": "default-id"},
				map[string]any{"name": "CLIENT_SECRET", "prompt": "Client secret", "required": true, "secret": true}}}}})
		state := c.connectionModal()
		state.Drafts["asana"]["CLIENT_SECRET"] = "never-render-this"
		lines := c.connectionRenderLines()
		if strings.Contains(strings.Join(lines, "\n"), "never-render-this") || indexOf(lines, "Client secret*: "+c.t("cli.connect.secret_set", nil)) < 0 {
			t.Fatalf("card lines %q", lines)
		}
		c.connectionAnswer(true)
		select {
		case got := <-sent:
			want := map[string]any{"targets": []any{map[string]any{"name": "asana", "status": "approved",
				"env": map[string]any{"CLIENT_ID": "default-id", "CLIENT_SECRET": "never-render-this"}}}}
			if got.opID != "op" || !reflect.DeepEqual(got.result, want) {
				t.Fatalf("sent %v %v", got.opID, got.result)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the answer was never sent")
		}
		c.connectionOnChange(map[string]any{"op_id": "op", "seq": 2, "settled": true, "targets": []any{}})
		if c.connectionModal() != nil {
			t.Fatal("card left open after the operation settled")
		}
	})
	t.Run("test_connection_card_opens_on_the_unfinished_app_and_a_reopen_keeps_its_draft", func(t *testing.T) {
		c, app := clarifyBatchCLI(t, 60)
		fake := c.app.(*fakeTUIApp)
		payload := func(seq int) map[string]any {
			return map[string]any{"op_id": "op", "seq": seq, "targets": []any{
				map[string]any{"name": "linear", "state": "connected"},
				map[string]any{"name": "asana", "state": "pending", "required_env": []any{map[string]any{"name": "TOKEN", "prompt": "Token", "required": true}}}}}
		}
		_ = app
		c.connectionCallback(payload(4))
		if got := c.connectionModal().Target["name"]; got != "asana" {
			t.Fatalf("card opened on %v", got)
		}
		c.connectionModal().Drafts["asana"]["TOKEN"] = "typed-not-sent"
		c.connectionCallback(payload(4))
		if st := c.connectionModal(); st.Target["name"] != "asana" || st.Drafts["asana"]["TOKEN"] != "typed-not-sent" {
			t.Fatalf("replayed snapshot changed the card: %v %v", st.Target["name"], st.Drafts)
		}
		resets := fake.bufferResets()
		fake.SetBuffer("in progress", 3)
		c.connectionCallback(payload(5))
		if st := c.connectionModal(); st.Phase != "form" || st.Drafts["asana"]["TOKEN"] != "typed-not-sent" {
			t.Fatalf("newer frame changed the open form: %s %v", st.Phase, st.Drafts)
		}
		if fake.BufferText() != "in progress" || fake.bufferResets() != resets {
			t.Fatalf("the input buffer was synced again: %q", fake.BufferText())
		}
	})
}

func TestClarifyBatchNavigation(t *testing.T) {
	cancel := func(t *testing.T, state *clarifyModalState, result chan map[string]any) {
		state.response <- nil
		batchResult(t, result)
	}
	t.Run("test_backward_navigation_wraps", func(t *testing.T) {
		c, _ := clarifyBatchCLI(t, 60)
		state, result := startBatch(t, c, batchQuestion(0, "Color?", false, "red", "blue"), batchQuestion(1, "Size?", false, "small", "large"), batchQuestion(2, "Speed?", false, "slow", "fast"))
		c.clarifyBatchSetActive(state, pyMod(state.Active-1, 3))
		if state.Active != 2 {
			t.Fatalf("active %d", state.Active)
		}
		c.clarifyBatchSetActive(state, pyMod(state.Active-1, 3))
		if state.Active != 1 {
			t.Fatalf("active %d", state.Active)
		}
		cancel(t, state, result)
	})
	t.Run("test_revisit_choice_answer_restores_cursor", func(t *testing.T) {
		c, _ := clarifyBatchCLI(t, 60)
		state, result := startBatch(t, c, batchQuestion(0, "Color?", false, "red", "blue"), batchQuestion(1, "Size?", false, "small", "large"))
		state.Selected = 1
		c.clarifyBatchEnter(state)
		if state.Active != 1 {
			t.Fatalf("active %d", state.Active)
		}
		c.clarifyBatchSetActive(state, 0)
		if state.Selected != 1 {
			t.Fatalf("cursor %d", state.Selected)
		}
		cancel(t, state, result)
	})
	t.Run("test_revisit_other_answer_highlights_other_and_prefills_edit", func(t *testing.T) {
		c, _ := clarifyBatchCLI(t, 60)
		state, result := startBatch(t, c, batchQuestion(0, "Color?", false, "red", "blue"), batchQuestion(1, "Size?", false, "small", "large"))
		state.Selected = 2
		c.clarifyBatchEnter(state)
		if !c.clarifyTyping() {
			t.Fatal("Other did not switch to typed mode")
		}
		c.setClarifyTyping(false)
		typed := "chartreuse"
		c.clarifyBatchLock(state, &typed, &clarifyAnswerMeta{Kind: "other", OtherText: "chartreuse"})
		if state.Active != 1 {
			t.Fatalf("active %d", state.Active)
		}
		c.clarifyBatchSetActive(state, 0)
		if state.Selected != 2 {
			t.Fatalf("cursor %d", state.Selected)
		}
		c.clarifyBatchEnter(state)
		if !c.clarifyTyping() || c.clarifyPrefill != "chartreuse" {
			t.Fatalf("typed mode %v prefill %q", c.clarifyTyping(), c.clarifyPrefill)
		}
		cancel(t, state, result)
	})
	t.Run("test_reanswer_overwrites_and_updates_meta", func(t *testing.T) {
		c, _ := clarifyBatchCLI(t, 60)
		state, result := startBatch(t, c, batchQuestion(0, "Color?", false, "red", "blue"), batchQuestion(1, "Size?", false, "small", "large"))
		typed := "teal"
		c.clarifyBatchLock(state, &typed, &clarifyAnswerMeta{Kind: "other", OtherText: "teal"})
		c.clarifyBatchSetActive(state, 0)
		if state.Selected != 2 {
			t.Fatalf("cursor %d", state.Selected)
		}
		state.Selected = 0
		c.clarifyBatchEnter(state)
		if lockedAnswer(state, "q0") != "red" || !reflect.DeepEqual(state.AnswerMeta["q0"], clarifyAnswerMeta{Kind: "choice"}) {
			t.Fatalf("answers %v meta %v", state.Answers, state.AnswerMeta)
		}
		c.clarifyBatchSetActive(state, 1)
		c.clarifyBatchEnter(state)
		wantBatch(t, batchResult(t, result), "submitted", map[string]any{"q0": "red", "q1": "small"})
	})
}

func TestClarifyBellOnPrompt(t *testing.T) {
	t.Run("test_bell_on_prompt_rings_and_off_is_silent", func(t *testing.T) {
		run := func(bell bool) string {
			c, _ := clarifyBatchCLI(t, 60)
			c.bellOnPrompt = bell
			var written strings.Builder
			prev := writeTTY
			writeTTY = func(seq string) { written.WriteString(seq) }
			defer func() { writeTTY = prev }()
			state, result := startBatch(t, c, batchQuestion(0, "Color?", false, "red", "blue"))
			state.response <- nil
			batchResult(t, result)
			return written.String()
		}
		if !strings.Contains(run(true), "\a") || strings.Contains(run(false), "\a") {
			t.Fatal("the bell did not follow display.bell_on_prompt")
		}
	})
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
