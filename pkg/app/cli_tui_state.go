package app

import (
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Rows below an overlay panel taken by spinner, status bar, input, rules and prompt symbol
const panelReservedBelow = 6

// Characters that type into the picker and palette filters
const tuiTypingChars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_.:/ "

// Approval choice ids to the catalog keys of their labels
var approvalChoiceKeys = map[string]string{"once": "cli.tui.approval_once", "session": "cli.tui.approval_session",
	"always": "cli.tui.approval_always", "deny": "cli.tui.approval_deny", "view": "cli.tui.approval_view"}

// tuiRunState is the prompt state one interactive run owns
type tuiRunState struct {
	app                    *ptApplication
	multilineShortcuts     bool
	pasteCounter           int
	prevTextLen            int
	prevNewlineCount       int
	pasteJustCollapsed     bool
	skipPasteCollapse      bool
	lastTextChange         float64
	pasteBurstAt           *float64
	heldEnter              *struct{}
	styleBase              *styleDict
	petWidget              *ptWindow
	stashPanelWidget       *ptWindow
	subagentDockWidget     *ptWindow
	extraWidgets           func() []*ptWindow
	extraKeybindings       func(kb *ptKeyBindings, app *ptApplication)
	configSig              []any
	configMCPServers       any
	lastConfigCheck        float64
	lastFocusRedraw        *float64
	chromePaints           [][]chromePaint
	chromePaintsScreen     uint64
	resizeSeenAt           *float64
	resizeHoldSince        *float64
	resizeNarrowest        *int
	resizeOriginalOnResize func()
	resizeWidthProbe       func() (int, bool)
	unsuppressOverride     func(app tuiScreenApp, delay float64)
	inlineCommandHook      func(e *ptEvent, text string, hasImages bool) bool
	enterWhileBusyHook     func(text string, images []string, payload any, mode string)
}

// slashConfirmChoice is one answer of a slash command confirmation
type slashConfirmChoice struct{ Value, Label, Desc string }

// slashConfirmModalState is the open confirmation panel of a destructive slash command
type slashConfirmModalState struct {
	Title, Detail string
	Choices       []slashConfirmChoice
	Selected      int
	response      chan string
}

// commandPaletteEntry is one palette row, command, category and description
type commandPaletteEntry struct{ Command, Category, Description string }

// commandPaletteModalState is the open command palette
type commandPaletteModalState struct {
	Entries      []commandPaletteEntry
	Filter       string
	Selected     int
	ScrollOffset int
	VisibleCount *int
}

// clarifyQuestion is one question of a clarify batch
type clarifyQuestion struct {
	QID, Question string
	Choices       []string
	MultiSelect   bool
}

// clarifyAnswerMeta remembers how a clarify question was answered so a revisit restores it
type clarifyAnswerMeta struct {
	Kind      string
	Choices   []string
	OtherText string
}

// wrappedRow is one wrapped line tagged with the index of the item it came from
type wrappedRow struct {
	Index int
	Text  string
}

// Entries of the clarify callback payload as questions
func clarifyQuestionsFrom(entries []any) []clarifyQuestion {
	out := make([]clarifyQuestion, 0, len(entries))
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		q := clarifyQuestion{QID: cliStr(entry["qid"], ""), Question: cliStr(entry["question"], ""), MultiSelect: entry["multi_select"] == true}
		switch choices := entry["choices"].(type) {
		case []any:
			for _, choice := range choices {
				q.Choices = append(q.Choices, cliStr(choice, ""))
			}
		case []string:
			q.Choices = append(q.Choices, choices...)
		}
		out = append(out, q)
	}
	return out
}

// Index of a value in a list, minus one when absent
func indexOf(list []string, value string) int {
	for i, v := range list {
		if v == value {
			return i
		}
	}
	return -1
}

// Points the clarify panel at a question, restoring the cursor and checkboxes of an earlier answer
func (c *CLI) clarifyBatchSetActive(state *clarifyModalState, index int) {
	index = max(0, min(index, len(state.Questions)-1))
	entry := state.Questions[index]
	state.Active = index
	state.Question = entry.Question
	state.Choices = entry.Choices
	state.Selected = 0
	state.MultiSelect = entry.MultiSelect
	state.SelectedIndices = nil
	if entry.MultiSelect {
		state.SelectedIndices = map[int]bool{}
	}
	c.uiMu.Lock()
	c.clarifyFreetext = len(entry.Choices) == 0
	c.clarifyMultiBase = nil
	c.uiMu.Unlock()
	meta, ok := state.AnswerMeta[entry.QID]
	if !ok {
		return
	}
	switch meta.Kind {
	case "choice":
		if answer := state.Answers[entry.QID]; answer != nil {
			if i := indexOf(entry.Choices, *answer); i >= 0 {
				state.Selected = i
			}
		}
	case "other":
		state.Selected = len(entry.Choices)
	case "multi":
		checked := map[int]bool{}
		for _, choice := range meta.Choices {
			if i := indexOf(entry.Choices, choice); i >= 0 {
				checked[i] = true
			}
		}
		if meta.OtherText != "" {
			checked[len(entry.Choices)] = true
		}
		state.SelectedIndices = checked
	}
}

// Locked answers keyed by question id, a skipped question mapping to nil
func (s *clarifyModalState) answersMap() map[string]any {
	out := make(map[string]any, len(s.Answers))
	for qid, answer := range s.Answers {
		if answer == nil {
			out[qid] = nil
		} else {
			out[qid] = *answer
		}
	}
	return out
}

// Clarify deadline, absent while the wait is unlimited
func (c *CLI) clarifyDeadlineAt() (float64, bool) {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	if c.clarifyDeadline == nil {
		return 0, false
	}
	return *c.clarifyDeadline, true
}

// Shows the clarify panel and blocks until every answer is locked, the wait ends or an interrupt cancels
func (c *CLI) clarifyCallback(questions []any) map[string]any {
	timeout := tools.ResolveClarifyTimeout(config.Tree(CLIConfig(c.ctx)))
	response := make(chan map[string]any, 1)
	state := &clarifyModalState{Questions: clarifyQuestionsFrom(questions), Answers: map[string]*string{},
		AnswerMeta: map[string]clarifyAnswerMeta{}, response: response}
	c.uiMu.Lock()
	c.clarifyState = state
	c.uiMu.Unlock()
	c.clarifyBatchSetActive(state, 0)
	c.uiMu.Lock()
	c.clarifyDeadline = nil
	if timeout > 0 {
		deadline := monotonic() + float64(timeout)
		c.clarifyDeadline = &deadline
	}
	c.uiMu.Unlock()
	c.ringBell(true, i18n.T(c.ctx, "cli.clarify.bell_context", "", nil), "")
	c.paintNow()
	result, timedOut := pollModalQueue(c, c.ctx, response, c.clarifyDeadlineAt, 1.0, nil, nil)
	if !timedOut {
		c.uiMu.Lock()
		c.clarifyDeadline = nil
		c.uiMu.Unlock()
		outcome := "submitted"
		if result == nil {
			outcome = "cancelled"
		}
		return map[string]any{"answers": state.answersMap(), "outcome": outcome}
	}
	partial := state.answersMap()
	c.clarifyTeardown()
	cprint("\n" + dimANSI + i18n.T(c.ctx, "cli.clarify.timed_out_locked_answers", "", map[string]any{"timeout": timeout}) + resetANSI)
	return map[string]any{"answers": partial, "outcome": "timed_out"}
}

// Seconds since the epoch, the wall clock the Ctrl C double press window reads
var wallClock = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
