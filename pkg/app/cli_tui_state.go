package app

import (
	"context"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
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
	configMCPServers       map[string]any
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

// Localized label of an approval choice id, an unknown id shown as it is
func approvalChoiceLabel(ctx context.Context, choice string) string {
	if key, ok := approvalChoiceKeys[choice]; ok {
		return i18n.T(ctx, key, "", nil)
	}
	return choice
}

// Agent name of the active skin
func agentName(ctx context.Context) string {
	return config.GetActiveSkin(ctx).GetBranding("agent_name", "Icebath")
}

// Plural aware catalog lookup, the one form for exactly one and the other form otherwise
func tn(ctx context.Context, key string, count int, kwargs map[string]any) string {
	args := map[string]any{"count": count}
	for k, v := range kwargs {
		args[k] = v
	}
	if count == 1 {
		return i18n.T(ctx, key+"_one", "", args)
	}
	return i18n.T(ctx, key+"_other", "", args)
}

// Quick select key label, 1 to 9 then 0 for the tenth and blank beyond
func numPrefix(i int) string {
	switch {
	case i < 9:
		return string(rune('1' + i))
	case i == 9:
		return "0"
	}
	return " "
}

// Terminal rows, 24 when the size is unknown
func termRows() int {
	_, rows := pyshutil.GetTerminalSize(100, 24)
	return rows
}

// wrappedRow is one wrapped line tagged with the index of the item it came from
type wrappedRow struct {
	Index int
	Text  string
}

// Wraps each item, every row keeping its item index so selection styling follows it
func wrapRows(wrap func(text string, width int, subsequentIndent string) []string, items []string, width int, indent string) []wrappedRow {
	var out []wrappedRow
	for i, label := range items {
		for _, w := range wrap(label, width, indent) {
			out = append(out, wrappedRow{i, w})
		}
	}
	return out
}

// Wraps a label, then leads the first row with firstPrefix and the rest with indent
func prefixWrappedRows(wrap func(text string, width int, subsequentIndent string) []string, label string, width int, firstPrefix, indent string) []string {
	rows := wrap(label, width, "")
	for i, row := range rows {
		if i == 0 {
			rows[i] = firstPrefix + row
		} else {
			rows[i] = indent + row
		}
	}
	return rows
}

// Panel wrap that trims whitespace
func wrapPanelTextTrim(text string, width int, subsequentIndent string) []string {
	return wrapPanelText(text, width, subsequentIndent, false)
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

// Locks an answer for the active question and moves to the next unanswered, resolving the panel when none is left
func (c *CLI) clarifyBatchLock(state *clarifyModalState, answer *string, meta *clarifyAnswerMeta) {
	entry := state.Questions[state.Active]
	if state.Answers == nil {
		state.Answers = map[string]*string{}
	}
	if state.AnswerMeta == nil {
		state.AnswerMeta = map[string]clarifyAnswerMeta{}
	}
	state.Answers[entry.QID] = answer
	if meta == nil {
		meta = &clarifyAnswerMeta{Kind: "choice"}
	}
	state.AnswerMeta[entry.QID] = *meta
	outcome := ""
	if answer != nil {
		outcome = *answer
	}
	c.persistPromptSummary("?", i18n.T(c.ctx, "cli.clarify.label", "", nil), entry.Question, outcome)
	total := len(state.Questions)
	for offset := 1; offset <= total; offset++ {
		candidate := (state.Active + offset) % total
		if _, answered := state.Answers[state.Questions[candidate].QID]; !answered {
			c.clarifyBatchSetActive(state, candidate)
			return
		}
	}
	offer(state.response, state.answersMap())
	c.uiMu.Lock()
	c.clarifyState, c.clarifyFreetext, c.clarifyMultiBase = nil, false, nil
	c.uiMu.Unlock()
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

// JSON array text of a list of labels, non ASCII kept as typed
func jsonLabels(labels []string) string {
	items := make([]any, len(labels))
	for i, l := range labels {
		items[i] = l
	}
	text, err := pyjson.Dumps(items, pyjson.EnsureASCII(false))
	if err != nil {
		return "[]"
	}
	return text
}

// Enter on the clarify panel in choice mode, locking the selection or switching to typed input for Other
func (c *CLI) clarifyBatchEnter(state *clarifyModalState) {
	choices := state.Choices
	entry := state.Questions[state.Active]
	meta := state.AnswerMeta[entry.QID]
	if state.MultiSelect {
		var indices []int
		for i := range state.SelectedIndices {
			if state.SelectedIndices[i] {
				indices = append(indices, i)
			}
		}
		sort.Ints(indices)
		selected := []string{}
		other := false
		for _, i := range indices {
			if i < len(choices) {
				selected = append(selected, choices[i])
			} else if i == len(choices) {
				other = true
			}
		}
		if other {
			c.uiMu.Lock()
			c.clarifyMultiBase = &selected
			c.clarifyFreetext = true
			c.clarifyPrefill = meta.OtherText
			c.uiMu.Unlock()
			return
		}
		answer := jsonLabels(selected)
		c.clarifyBatchLock(state, &answer, &clarifyAnswerMeta{Kind: "multi", Choices: selected})
		return
	}
	if state.Selected < len(choices) {
		answer := choices[state.Selected]
		c.clarifyBatchLock(state, &answer, &clarifyAnswerMeta{Kind: "choice"})
		return
	}
	c.uiMu.Lock()
	c.clarifyFreetext = true
	c.clarifyPrefill = ""
	if meta.Kind == "other" {
		c.clarifyPrefill = meta.OtherText
	}
	c.uiMu.Unlock()
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
