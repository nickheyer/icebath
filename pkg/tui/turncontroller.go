package tui

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	streamBatch         = 16 * time.Millisecond
	streamIdleBatch     = 16 * time.Millisecond
	streamScrollBatch   = 96 * time.Millisecond
	streamTypingBatch   = 80 * time.Millisecond
	reasoningPulse      = 700 * time.Millisecond
	interruptCooldown   = 1500 * time.Millisecond
	activityLimit       = 8
	trailLimit          = 8
	reasoningTextCap    = 80_000
	reasoningTextKeep   = 60_000
	analyzingToolOutput = "analyzing tool output…"
)

var (
	diffSegmentFence = regexp.MustCompile("^```diff\\n([\\s\\S]*?)\\n```$")
	ownDiffFence     = regexp.MustCompile("(?i)```(?:diff|patch)\\b")
	diffHeaderChrome = regexp.MustCompile(`^[` + jsWS + `]*┊[^\n]*\n?`)
)

// Timers of the turn controller, each cancelled by bumping its generation
type turnTimer int

const (
	timerStatus turnTimer = iota
	timerNotice
	timerReasoningPulse
	timerReasoningBatch
	timerStream
	turnTimerCount
)

// Timer of the turn controller firing, stale when its generation moved on
type turnTimerMsg struct {
	kind turnTimer
	gen  int
}

// What an interrupt sealed into the transcript, the bubble text when it appended one and its partial reply
type sealedInterrupt struct {
	Partial string
	Text    *string
}

// Bubble rewrite of an honoured interrupt whose persisted partial extends the sealed one
type interruptedReply struct {
	From *string
	To   string
}

// Final rows of a completed turn and the spawn tree its caller persists
type messageCompleteResult struct {
	FinalMessages     []msg
	FinalText         string
	InterruptedReply  *interruptedReply
	WasInterrupted    bool
	FinishedSubagents []subagentProgress
	SessionID         string
}

// Requests an interrupt needs, the gateway client in production
type turnGateway interface {
	request(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error)
	canonical() bool
}

// Transcript writers and session of an interrupt
type interruptDeps struct {
	AppendMessage func(msg)
	Gateway       turnGateway
	Sid           string
	Sys           func(string)
}

// Answer of a canonical session.interrupt, settled against the turn it was sent for
type interruptReplyMsg struct {
	deps        interruptDeps
	keepBusy    bool
	destination submissionDestination
	info        map[string]any
	response    map[string]any
	err         error
}

// Text of a failed request as the TUI shows it
func rpcErrorMessage(err error) string {
	if err != nil && err.Error() != "" {
		return describeRPCError(err)
	}
	return tr("rpc.requestFailed")
}

// Trail rows of a finished call, one per label with the verbose blocks on the last
func toolTrailLines(done *activeTool, name string, labels []toolLabel, summary, resultText string, took *float64) []string {
	var heads []string
	for _, label := range labels {
		heads = append(heads, formatToolLabel(label))
	}
	verbose := resultText != ""
	context := ""
	if done != nil {
		context = done.Context
		verbose = verbose || done.VerboseArgs != ""
	}
	if len(heads) == 0 {
		heads = []string{formatToolCall(name, context)}
	}
	out := make([]string, len(heads))
	for i, head := range heads {
		switch {
		case i < len(heads)-1:
			out[i] = toolTrailLine(head, false, "", nil)
		case verbose:
			args := ""
			if done != nil {
				args = done.VerboseArgs
			}
			result := resultText
			if result == "" {
				result = summary
			}
			out[i] = verboseToolTrailLine(head, false, took, args, result)
		default:
			out[i] = toolTrailLine(head, false, summary, took)
		}
	}
	return out
}

// Patch of a diff only segment, ok false for anything else
func diffSegmentBody(m msg) (string, bool) {
	if m.Kind != "diff" {
		return "", false
	}
	if found := diffSegmentFence.FindStringSubmatch(m.Text); found != nil {
		return found[1], true
	}
	return "", false
}

func hasDetails(m msg) bool { return m.Thinking != "" || len(m.Tools) > 0 || m.ToolTokens != 0 }

var todoStatuses = []string{"pending", "in_progress", "completed", "cancelled"}

// Task list of the planning tool payload, ok false when it is not a list
func parseTodos(value any) ([]todoItem, bool) {
	list, isList := value.([]any)
	if !isList {
		return nil, false
	}
	field := func(row map[string]any, k string) string {
		v, has := row[k]
		if !has || v == nil {
			return ""
		}
		return strings.TrimFunc(jsString(v), isJSSpace)
	}
	todos := []todoItem{}
	for _, item := range list {
		row, isDict := item.(map[string]any)
		if !isDict {
			continue
		}
		status, isStr := row["status"].(string)
		if !isStr || !slices.Contains(todoStatuses, status) {
			continue
		}
		todo := todoItem{Content: field(row, "content"), ID: field(row, "id"), Status: status}
		if parent := field(row, "parent"); parent != "" && parent != todo.ID {
			todo.Parent = parent
		}
		if todo.ID != "" && todo.Content != "" {
			todos = append(todos, todo)
		}
	}
	return todos, true
}

// Final text with the streamed segments it repeats cut from its head
func finalTail(finalText string, segments []msg) string {
	tail := finalText
	for _, m := range segments {
		if m.Role != "assistant" || m.Kind == "diff" {
			continue
		}
		if trimmed := strings.TrimFunc(m.Text, isJSSpace); trimmed != "" && strings.HasPrefix(tail, trimmed) {
			tail = strings.TrimLeftFunc(tail[len(trimmed):], isJSSpace)
		}
	}
	return tail
}

// Whether the streamed tail is a segment of its own, absent from a final text no plugin rewrote
func keepsStreamedTail(tail string, completion *string, transformed bool) bool {
	return tail != "" && completion != nil && !transformed && !strings.Contains(*completion, tail)
}

func interruptedText(partial string) string {
	if partial != "" {
		return partial + "\n\n*[interrupted]*"
	}
	return "*[interrupted]*"
}

// Bubble fix of an honoured interrupt whose persisted partial extends what was sealed, nil otherwise
func lateInterruptedReply(sealed *sealedInterrupt, payload map[string]any) *interruptedReply {
	persisted := ""
	if text, isStr := payload["text"].(string); isStr {
		persisted = strings.TrimFunc(text, isJSSpace)
	}
	if sealed == nil {
		return nil
	}
	shown := strings.TrimRightFunc(sealed.Partial, isJSSpace)
	if payload["status"] != "interrupted" || unitLen(persisted) <= unitLen(shown) || !strings.HasPrefix(persisted, shown) {
		return nil
	}
	return &interruptedReply{From: sealed.Text, To: interruptedText(persisted)}
}

// Turn state machine of streaming batches, reasoning segments, tool trail, interrupts and notices
type turnController struct {
	store *tuiStore
	cmds  []tea.Cmd
	gen   [turnTimerCount]int
	armed [turnTimerCount]bool

	bufRef              string
	interrupted         bool
	sealed              *sealedInterrupt
	lastStatusNote      string
	statusFromBusy      bool
	persistedToolLabels map[string]bool
	protocolWarned      bool
	reasoningText       string
	segmentMessages     []msg
	pendingSegmentTools []string
	toolTokenAcc        int
	turnTools           []string

	activeTools           []activeTool
	activeReasoningText   string
	reasoningSegmentIndex int
	interimBoundaryIndex  int
	activityID            int
	streamDelay           time.Duration

	pendingNotice *notice
	noticeArmedID string
	noticeIDSeq   int
}

// Controller of the store's turn
func newTurnController(store *tuiStore) *turnController {
	return &turnController{store: store, persistedToolLabels: map[string]bool{}, reasoningSegmentIndex: -1, interimBoundaryIndex: -1, streamDelay: streamIdleBatch}
}

// Timer commands queued since the last call, for the program to run
func (c *turnController) takeCmds() []tea.Cmd {
	out := c.cmds
	c.cmds = nil
	return out
}

func (c *turnController) arm(kind turnTimer, d time.Duration) {
	c.gen[kind]++
	c.armed[kind] = true
	gen := c.gen[kind]
	c.cmds = append(c.cmds, tea.Tick(d, func(time.Time) tea.Msg { return turnTimerMsg{kind: kind, gen: gen} }))
}

func (c *turnController) clear(kind turnTimer) {
	c.gen[kind]++
	c.armed[kind] = false
}

// Runs a due timer of the current generation
func (c *turnController) fire(m turnTimerMsg) {
	if !c.armed[m.kind] || c.gen[m.kind] != m.gen {
		return
	}
	c.armed[m.kind] = false
	switch m.kind {
	case timerStatus:
		c.store.ui.Status = "ready"
		if c.statusFromBusy {
			c.store.ui.Status = c.store.statusFromBusy()
		}
	case timerNotice:
		if n := c.store.ui.Notice; n != nil && n.ID == c.noticeArmedID {
			c.store.ui.Notice = nil
		}
	case timerReasoningPulse:
		c.store.turn.ReasoningStreaming = false
	case timerReasoningBatch:
		c.store.turn.Reasoning = c.reasoningText
		c.store.turn.ReasoningTokens = estimateTokensRough(c.reasoningText)
	case timerStream:
		c.store.turn.Streaming = boundedLiveRenderText(c.visibleStream(), liveRenderMaxChars, liveRenderMaxLines)
	}
}

// Streamed text without its reasoning tags
func (c *turnController) visibleStream() string {
	raw := strings.TrimLeftFunc(c.bufRef, isJSSpace)
	if hasReasoningTag(raw) {
		return splitReasoning(raw).Text
	}
	return raw
}

func (c *turnController) boostStreamingForTyping() { c.streamDelay = streamTypingBatch }

func (c *turnController) boostStreamingForScroll() {
	c.streamDelay = max(c.streamDelay, streamScrollBatch)
}

func (c *turnController) relaxStreaming() { c.streamDelay = streamIdleBatch }

// Drops the turn's reasoning and tool token counts
func (c *turnController) clearReasoning() {
	c.clear(timerReasoningBatch)
	c.activeReasoningText = ""
	c.reasoningSegmentIndex = -1
	c.reasoningText = ""
	c.toolTokenAcc = 0
	c.store.turn.Reasoning, c.store.turn.ReasoningTokens, c.store.turn.ToolTokens = "", 0, 0
}

func (c *turnController) clearStatusTimer() { c.clear(timerStatus) }

// Shows a notice, held until the turn ends while one is in flight
func (c *turnController) showNotice(n notice) {
	if n.ID == "" {
		c.noticeIDSeq++
		n.ID = "n" + jsString(float64(c.noticeIDSeq))
	}
	if c.store.ui.Busy {
		c.pendingNotice = &n
		return
	}
	c.applyNotice(n)
}

// Clears the visible notice and a pending one only when its key matches
func (c *turnController) clearNotice(key string) {
	if c.pendingNotice != nil && c.pendingNotice.Key == key {
		c.pendingNotice = nil
	}
	if n := c.store.ui.Notice; n != nil && n.Key == key {
		c.clear(timerNotice)
		c.store.ui.Notice = nil
	}
}

// Makes a notice visible and starts its time to live clock
func (c *turnController) applyNotice(n notice) {
	c.clear(timerNotice)
	c.store.ui.Notice = &n
	if n.Kind == "ttl" && n.TTLms != nil && *n.TTLms > 0 {
		c.noticeArmedID = n.ID
		c.arm(timerNotice, time.Duration(*n.TTLms*float64(time.Millisecond)))
	}
}

// Applies the notice held back while busy, at a real turn end
func (c *turnController) flushPendingNotice() {
	if c.pendingNotice == nil {
		return
	}
	n := *c.pendingNotice
	c.pendingNotice = nil
	c.applyNotice(n)
}

// Drops pending, timed and visible notices at a session boundary
func (c *turnController) clearNoticeState() {
	c.pendingNotice = nil
	c.clear(timerNotice)
	c.store.ui.Notice = nil
}

// Ends the reasoning phase, sealing its open segment
func (c *turnController) endReasoningPhase() {
	c.clear(timerReasoningPulse)
	if c.reasoningSegmentIndex >= 0 {
		c.syncReasoningSegment(false)
	}
	c.store.turn.ReasoningActive, c.store.turn.ReasoningStreaming = false, false
}

// Leaves the turn with tools, stream and live rows cleared, busy off and flow prompts closed
func (c *turnController) idle(keepTurnArchive bool) {
	c.endReasoningPhase()
	c.activeTools = nil
	c.clear(timerStream)
	c.bufRef = ""
	if !keepTurnArchive {
		c.pendingSegmentTools, c.segmentMessages = nil, nil
	}
	t := &c.store.turn
	t.StreamPendingTools, t.StreamSegments = nil, nil
	if keepTurnArchive {
		t.StreamPendingTools, t.StreamSegments = c.pendingSegmentTools, c.segmentMessages
	}
	t.Streaming, t.Subagents, t.Tools, t.TurnTrail = "", nil, nil, nil
	c.store.ui.Busy, c.store.ui.Compacting = false, false
	c.store.resetFlowOverlays()
}

// Interrupts the turn, a canonical one only after the owner confirms the running generation
func (c *turnController) interruptTurn(deps interruptDeps, keepBusy bool) (tea.Cmd, error) {
	if !deps.Gateway.canonical() {
		gw, sid := deps.Gateway, deps.Sid
		request := func() tea.Msg {
			_, _ = gw.request(context.Background(), "session.interrupt", map[string]any{"session_id": sid}, 0)
			return nil
		}
		c.sealInterrupt(deps, keepBusy)
		return request, nil
	}
	destination, err := c.store.captureDestination()
	if err != nil {
		return nil, err
	}
	info := c.store.ui.Info
	gw, sid := deps.Gateway, deps.Sid
	return func() tea.Msg {
		reply := interruptReplyMsg{deps: deps, keepBusy: keepBusy, destination: destination, info: info}
		params := map[string]any{"session_id": sid}
		if generation, has := info["execution_generation"]; has {
			params["execution_generation"] = generation
		}
		value, err := gw.request(context.Background(), "session.interrupt", params, 0)
		reply.response, _ = value.(map[string]any)
		reply.err = err
		return reply
	}, nil
}

// Seals a canonical interrupt once its answer shows the same turn still running
func (c *turnController) settleInterrupt(m interruptReplyMsg) {
	isActiveTurn := func() bool {
		ui := c.store.ui
		return ui.Sid == m.deps.Sid && c.store.isCurrentDestination(m.destination) && ui.Busy && !c.interrupted &&
			jsStrictEqual(ui.Info["execution_epoch"], m.info["execution_epoch"]) &&
			jsStrictEqual(ui.Info["execution_generation"], m.info["execution_generation"])
	}
	if m.err != nil {
		if isActiveTurn() {
			m.deps.Sys(tr("canonical.submit.interruptFailed", rpcErrorMessage(m.err)))
		}
		return
	}
	if m.response["execution_state"] != "running" || !jsStrictEqual(m.response["execution_generation"], m.info["execution_generation"]) {
		return
	}
	if !isActiveTurn() {
		return
	}
	c.sealInterrupt(m.deps, m.keepBusy)
}

// Writes the interrupted turn into the transcript and leaves it, busy kept for a queued drain
func (c *turnController) sealInterrupt(deps interruptDeps, keepBusy bool) {
	c.interrupted = true
	c.closeReasoningSegment()
	segments := c.segmentMessages
	partial := strings.TrimLeftFunc(c.bufRef, isJSSpace)
	tools := c.pendingSegmentTools
	c.idle(false)
	c.clearReasoning()
	c.turnTools = nil
	c.store.turn.Activity, c.store.turn.Outcome = nil, ""
	for _, m := range segments {
		deps.AppendMessage(m)
	}
	if partial != "" || len(tools) > 0 {
		text := interruptedText(partial)
		deps.AppendMessage(msg{Role: "assistant", Text: text, Tools: tools})
		c.sealed = &sealedInterrupt{Partial: partial, Text: &text}
	} else {
		deps.Sys(tr("session.turn.interrupted"))
		c.sealed = &sealedInterrupt{}
	}
	c.clearStatusTimer()
	if keepBusy {
		c.store.ui.Busy, c.store.ui.Status = true, tr("session.status.interrupting")
		return
	}
	c.store.ui.Status = "interrupted"
	c.statusFromBusy = false
	c.arm(timerStatus, interruptCooldown)
	c.flushPendingNotice()
}

// Puts the busy or ready status back after a transient status line
func (c *turnController) restoreStatusAfter(d time.Duration) {
	c.clearStatusTimer()
	c.statusFromBusy = true
	c.arm(timerStatus, d)
}

// Status of the session when no transient line shows
func (s *tuiStore) statusFromBusy() string {
	if s.ui.Busy {
		return "running…"
	}
	return "ready"
}

// Drops passing status lines from the trail
func (c *turnController) pruneTransient() {
	c.turnTools = slices.DeleteFunc(slices.Clone(c.turnTools), isTransientTrailLine)
	if next := slices.DeleteFunc(slices.Clone(c.store.turn.TurnTrail), isTransientTrailLine); len(next) != len(c.store.turn.TurnTrail) {
		c.store.turn.TurnTrail = next
	}
}

// Writes the open reasoning into its segment, live while the phase runs
func (c *turnController) syncReasoningSegment(live bool) {
	thinking := strings.TrimFunc(c.activeReasoningText, isJSSpace)
	if thinking == "" {
		return
	}
	m := msg{Kind: "trail", Role: "system", Thinking: thinking, ThinkingTokens: estimateTokensRough(thinking), ToolTokens: c.toolTokenAcc, IsLiveReasoning: live}
	if c.reasoningSegmentIndex < 0 {
		c.reasoningSegmentIndex = len(c.segmentMessages)
		c.segmentMessages = append(slices.Clone(c.segmentMessages), m)
	} else {
		c.segmentMessages = slices.Clone(c.segmentMessages)
		c.segmentMessages[c.reasoningSegmentIndex] = m
	}
	c.store.turn.StreamSegments = c.segmentMessages
}

func (c *turnController) closeReasoningSegment() {
	c.syncReasoningSegment(false)
	c.activeReasoningText = ""
	c.reasoningSegmentIndex = -1
}

func (c *turnController) pushSegment(m msg) {
	c.segmentMessages = appendToolShelfMessage(c.segmentMessages, m)
}

// Seals the streamed text and pending tool rows into a segment
func (c *turnController) flushStreamingSegment() {
	raw := strings.TrimLeftFunc(c.bufRef, isJSSpace)
	split := splitReply{Text: raw}
	if raw != "" && hasReasoningTag(raw) {
		split = splitReasoning(raw)
	}
	if split.Reasoning != "" && strings.TrimFunc(c.reasoningText, isJSSpace) == "" {
		c.reasoningText = split.Reasoning
		c.activeReasoningText = split.Reasoning
		c.store.turn.Reasoning, c.store.turn.ReasoningTokens = c.reasoningText, estimateTokensRough(c.reasoningText)
		c.syncReasoningSegment(true)
	}
	m := msg{Role: "system", Text: split.Text, Tools: c.pendingSegmentTools}
	if split.Text != "" {
		m.Role = "assistant"
	} else {
		m.Kind = "trail"
	}
	c.clear(timerStream)
	if split.Text != "" || hasDetails(m) {
		c.pushSegment(m)
	}
	c.pendingSegmentTools = nil
	c.bufRef = ""
	c.store.turn.StreamPendingTools, c.store.turn.StreamSegments, c.store.turn.Streaming = nil, c.segmentMessages, ""
}

// Marks reasoning as streaming, falling quiet after the pulse
func (c *turnController) pulseReasoningStreaming() {
	c.clear(timerReasoningPulse)
	c.store.turn.ReasoningActive, c.store.turn.ReasoningStreaming = true, true
	c.arm(timerReasoningPulse, reasoningPulse)
}

// Takes the planning tool list as the task list of the turn
func (c *turnController) recordTodos(value any) {
	if c.interrupted {
		return
	}
	if todos, ok := parseTodos(value); ok {
		c.store.turn.Todos = todos
	}
}

// Merges pending tool rows into the last shelf segment, false when they would start a new one
func (c *turnController) flushPendingToolsIntoLastSegment() bool {
	if len(c.pendingSegmentTools) == 0 {
		return false
	}
	next := appendToolShelfMessage(c.segmentMessages, msg{Kind: "trail", Role: "system", Tools: c.pendingSegmentTools})
	if len(next) == len(c.segmentMessages)+1 {
		return false
	}
	c.segmentMessages = next
	c.pendingSegmentTools = nil
	c.store.turn.StreamPendingTools, c.store.turn.StreamSegments = nil, c.segmentMessages
	return true
}

// Anchors an edit's diff between the narration before it and what streams after
func (c *turnController) pushInlineDiffSegment(diffText string, tools []string) {
	stripped := strings.TrimFunc(diffHeaderChrome.ReplaceAllString(diffText, ""), isJSSpace)
	if stripped == "" {
		return
	}
	c.flushStreamingSegment()
	block := "```diff\n" + stripped + "\n```"
	if n := len(c.segmentMessages); n > 0 && c.segmentMessages[n-1].Text == block {
		return
	}
	c.segmentMessages = append(slices.Clone(c.segmentMessages), msg{Kind: "diff", Role: "assistant", Text: block, Tools: tools})
	c.store.turn.StreamSegments = c.segmentMessages
}

// Adds an activity line, replacing the label's group and skipping a repeat of the last line
func (c *turnController) pushActivity(text, tone, replaceLabel string) {
	if tone == "" {
		tone = "info"
	}
	base := c.store.turn.Activity
	if replaceLabel != "" {
		base = slices.DeleteFunc(slices.Clone(base), func(item activityItem) bool { return sameToolTrailGroup(replaceLabel, item.Text) })
	}
	if n := len(base); n > 0 && base[n-1].Text == text && base[n-1].Tone == tone {
		return
	}
	c.activityID++
	next := append(slices.Clone(base), activityItem{ID: c.activityID, Text: text, Tone: tone})
	c.store.turn.Activity = next[max(0, len(next)-activityLimit):]
}

// Adds a trail line, dropping passing status lines and keeping the newest eight
func (c *turnController) pushTrail(line string) {
	if c.interrupted {
		return
	}
	trail := c.store.turn.TurnTrail
	if n := len(trail); n > 0 && trail[n-1] == line {
		return
	}
	next := append(slices.DeleteFunc(slices.Clone(trail), isTransientTrailLine), line)
	next = next[max(0, len(next)-trailLimit):]
	c.turnTools = next
	c.store.turn.TurnTrail = next
}

// Discards a failed turn whole and applies a held notice
func (c *turnController) recordError() {
	c.idle(false)
	c.clearReasoning()
	c.clearStatusTimer()
	c.pendingSegmentTools, c.segmentMessages, c.turnTools = nil, nil, nil
	clear(c.persistedToolLabels)
	c.flushPendingNotice()
}

// Seals the turn into its final rows, the streamed tail kept and repeated segments deduped
func (c *turnController) recordMessageComplete(payload map[string]any) messageCompleteResult {
	c.closeReasoningSegment()
	var completion *string
	if text, isStr := payload["text"].(string); isStr {
		completion = &text
	} else if rendered, isStr := payload["rendered"].(string); isStr {
		completion = &rendered
	}
	source := c.bufRef
	if completion != nil {
		source = *completion
	}
	rawText := strings.TrimLeftFunc(source, isJSSpace)
	if keepsStreamedTail(strings.TrimFunc(c.bufRef, isJSSpace), completion, truthyJS(payload["response_transformed"])) {
		c.flushStreamingSegment()
	}
	split := splitReasoning(rawText)
	dedupeStart := 0
	if !truthyJS(payload["response_previewed"]) && c.interimBoundaryIndex >= 0 {
		dedupeStart = min(c.interimBoundaryIndex, len(c.segmentMessages))
	}
	finalText := finalTail(split.Text, c.segmentMessages[dedupeStart:])
	existing := strings.TrimFunc(c.reasoningText, isJSSpace)
	if existing == "" {
		if r, has := payload["reasoning"]; has && r != nil {
			existing = strings.TrimFunc(jsString(r), isJSSpace)
		}
	}
	saved := existing
	if existing == "" {
		saved = split.Reasoning
	}
	savedToolTokens := c.toolTokenAcc
	tools := c.pendingSegmentTools
	if n := len(c.segmentMessages); len(tools) > 0 && n > 0 && isToolShelfMessage(&c.segmentMessages[n-1]) {
		last := c.segmentMessages[n-1]
		last.Tools = append(slices.Clone(last.Tools), tools...)
		c.segmentMessages = append(slices.Clone(c.segmentMessages[:n-1]), last)
		c.pendingSegmentTools, tools = nil, nil
	}
	ownFence := ownDiffFence.MatchString(finalText)
	var segments []msg
	for _, m := range c.segmentMessages {
		body, isDiff := diffSegmentBody(m)
		if !isDiff || (!ownFence && !strings.Contains(finalText, body)) {
			segments = append(segments, m)
		}
	}
	hasReasoningSegment := c.reasoningSegmentIndex >= 0 || slices.ContainsFunc(segments, func(m msg) bool { return strings.TrimFunc(m.Thinking, isJSSpace) != "" })
	finalThinking := ""
	if !hasReasoningSegment {
		finalThinking = strings.TrimFunc(saved, isJSSpace)
	}
	details := msg{Kind: "trail", Role: "system", Thinking: finalThinking, ToolTokens: savedToolTokens, Tools: tools}
	if finalThinking != "" {
		details.ThinkingTokens = estimateTokensRough(finalThinking)
	}
	finalMessages := append(c.store.archiveTodosAtTurnEnd(), segments...)
	if hasDetails(details) {
		finalMessages = append(finalMessages, details)
	}
	if finalText != "" {
		finalMessages = append(finalMessages, msg{Role: "assistant", Text: finalText})
	}
	result := messageCompleteResult{FinalMessages: finalMessages, FinalText: finalText, WasInterrupted: c.interrupted}
	if c.interrupted {
		result.InterruptedReply = lateInterruptedReply(c.sealed, payload)
	}
	if finished := c.store.turn.Subagents; len(finished) > 0 {
		c.store.pushSnapshot(finished, c.store.ui.Sid, nil)
		result.FinishedSubagents, result.SessionID = finished, c.store.ui.Sid
	}
	c.idle(false)
	c.clearReasoning()
	c.turnTools = nil
	clear(c.persistedToolLabels)
	c.bufRef = ""
	c.interrupted = false
	c.sealed = nil
	c.store.turn.Activity, c.store.turn.Outcome = nil, ""
	c.flushPendingNotice()
	return result
}

// Accumulates a reply delta, scheduling the batched live render when streaming is on
func (c *turnController) recordMessageDelta(text string) {
	if c.interrupted || text == "" {
		return
	}
	c.pruneTransient()
	c.endReasoningPhase()
	c.bufRef += text
	if c.store.ui.Streaming {
		c.scheduleStreaming()
	}
}

// Seals an authoritative interim message as a segment the final dedupe keeps
func (c *turnController) recordInterimMessage(text string) {
	if c.interrupted {
		return
	}
	authoritative := strings.TrimLeftFunc(text, isJSSpace)
	if authoritative == "" {
		return
	}
	if strings.TrimLeftFunc(c.bufRef, isJSSpace) != authoritative {
		c.bufRef = authoritative
	}
	c.flushStreamingSegment()
	c.interimBoundaryIndex = len(c.segmentMessages)
}

// Takes a whole reasoning block when none streamed, shown only with reasoning on unless forced
func (c *turnController) recordReasoningAvailable(text string, force bool) {
	if c.interrupted || (!force && !c.store.ui.ShowReasoning) {
		return
	}
	incoming := strings.TrimFunc(text, isJSSpace)
	if incoming == "" || strings.TrimFunc(c.reasoningText, isJSSpace) != "" {
		return
	}
	c.reasoningText = incoming
	c.activeReasoningText = incoming
	c.scheduleReasoning()
	c.syncReasoningSegment(true)
	c.pulseReasoningStreaming()
}

// Commits one mixture of agents reference output as its own labelled thinking segment
func (c *turnController) recordMoaReference(label, text string, index, count int) {
	if c.interrupted {
		return
	}
	c.closeReasoningSegment()
	header := "◇ Reference — " + label
	if index != 0 && count != 0 {
		header = "◇ Reference " + jsString(float64(index)) + "/" + jsString(float64(count)) + " — " + label
	}
	thinking := header
	if body := strings.TrimFunc(text, isJSSpace); body != "" {
		thinking = header + "\n" + body
	}
	c.pushSegment(msg{Kind: "trail", Role: "system", Thinking: thinking, IsMoaReference: true, ThinkingTokens: estimateTokensRough(thinking)})
	c.store.turn.StreamSegments = c.segmentMessages
}

// Accumulates a reasoning delta, the text kept to its newest sixty thousand units past eighty thousand
func (c *turnController) recordReasoningDelta(text string, force bool) {
	if c.interrupted || (!force && !c.store.ui.ShowReasoning) {
		return
	}
	if strings.TrimFunc(c.activeReasoningText, isJSSpace) == "" && len(c.pendingSegmentTools) > 0 {
		c.flushStreamingSegment()
	}
	c.reasoningText += text
	c.activeReasoningText += text
	if n := unitLen(c.reasoningText); n > reasoningTextCap {
		c.reasoningText = jsSlice(c.reasoningText, -reasoningTextKeep, n)
	}
	c.scheduleReasoning()
	c.syncReasoningSegment(true)
	c.pulseReasoningStreaming()
}

// Finishes a tool call into trail rows, merged into the last shelf segment when one holds them
func (c *turnController) recordToolComplete(toolID, fallbackName, summary string, duration *float64, todos any, resultText string, labels []toolLabel) {
	if c.interrupted {
		return
	}
	c.recordTodos(todos)
	lines := c.completeTool(toolID, fallbackName, summary, duration, resultText, labels)
	c.pendingSegmentTools = append(slices.Clone(c.pendingSegmentTools), lines...)
	c.flushPendingToolsIntoLastSegment()
	c.publishToolState()
}

// Finishes an editing tool call into an inline diff segment carrying its trail rows
func (c *turnController) recordInlineDiffToolComplete(diffText, toolID, fallbackName string, duration *float64, resultText string, labels []toolLabel) {
	if c.interrupted {
		return
	}
	c.flushStreamingSegment()
	c.pushInlineDiffSegment(diffText, c.completeTool(toolID, fallbackName, "", duration, resultText, labels))
	c.publishToolState()
}

// Removes a call from the active set and returns its trail rows, never marked as errors
func (c *turnController) completeTool(toolID, fallbackName, summary string, duration *float64, resultText string, eventLabels []toolLabel) []string {
	var done *activeTool
	if i := slices.IndexFunc(c.activeTools, func(t activeTool) bool { return t.ID == toolID }); i >= 0 {
		found := c.activeTools[i]
		done = &found
	}
	name := fallbackName
	if done != nil {
		name = done.Name
	} else if name == "" {
		name = "tool"
	}
	label := toolTrailLabel(name)
	labels := eventLabels
	if len(labels) == 0 && done != nil {
		labels = done.Labels
	}
	took := duration
	if took == nil && done != nil && !done.StartedAt.IsZero() {
		elapsed := float64(c.store.now().Sub(done.StartedAt).Milliseconds()) / 1000
		took = &elapsed
	}
	lines := toolTrailLines(done, name, labels, summary, resultText, took)
	c.activeTools = slices.DeleteFunc(slices.Clone(c.activeTools), func(t activeTool) bool { return t.ID == toolID })
	next := slices.DeleteFunc(slices.Clone(c.turnTools), func(item string) bool { return sameToolTrailGroup(label, item) })
	if len(c.activeTools) == 0 {
		next = append(next, analyzingToolOutput)
	}
	c.turnTools = next[max(0, len(next)-trailLimit):]
	return lines
}

func (c *turnController) publishToolState() {
	c.store.turn.StreamPendingTools, c.store.turn.Tools, c.store.turn.TurnTrail = c.pendingSegmentTools, c.activeTools, c.turnTools
}

// Starts a tool call, sealing the streamed text and reasoning before it
func (c *turnController) recordToolStart(toolID, name, context, verboseArgs string, labels []toolLabel) {
	if c.interrupted {
		return
	}
	c.flushStreamingSegment()
	c.closeReasoningSegment()
	c.pruneTransient()
	c.endReasoningPhase()
	if sample := strings.TrimFunc(name+" "+context, isJSSpace); sample != "" {
		c.toolTokenAcc += estimateTokensRough(sample)
	}
	c.activeTools = append(slices.Clone(c.activeTools), activeTool{Context: context, ID: toolID, Labels: labels, Name: name, StartedAt: c.store.now(), VerboseArgs: verboseArgs})
	c.store.turn.ToolTokens, c.store.turn.Tools = c.toolTokenAcc, c.activeTools
}

// Clears the turn and every notice at a session boundary
func (c *turnController) reset() {
	c.clearReasoning()
	c.clearStatusTimer()
	c.idle(false)
	c.bufRef = ""
	c.interrupted = false
	c.sealed = nil
	c.lastStatusNote = ""
	c.activeReasoningText = ""
	c.pendingSegmentTools = nil
	c.protocolWarned = false
	c.reasoningSegmentIndex = -1
	c.interimBoundaryIndex = -1
	c.segmentMessages = nil
	c.turnTools = nil
	c.toolTokenAcc = 0
	clear(c.persistedToolLabels)
	c.clearNoticeState()
	c.store.turn.Activity, c.store.turn.Outcome = nil, ""
}

// Resets the controller and the turn state
func (c *turnController) fullReset() {
	c.reset()
	c.store.resetTurnState()
}

// Publishes the accumulated reasoning on the next batch tick
func (c *turnController) scheduleReasoning() {
	if c.armed[timerReasoningBatch] {
		return
	}
	c.arm(timerReasoningBatch, streamBatch)
}

// Publishes the streamed text on the next batch tick at the current delay
func (c *turnController) scheduleStreaming() {
	if c.armed[timerStream] {
		return
	}
	c.arm(timerStream, c.streamDelay)
}

// Replaces the streamed text with a resumed turn's buffer and shows it at once
func (c *turnController) hydrateStreamingText(text string) {
	c.clear(timerStream)
	c.bufRef = text
	c.store.turn.Streaming = boundedLiveRenderText(c.visibleStream(), liveRenderMaxChars, liveRenderMaxLines)
}

// Starts a turn, yielding flash notices and dropping any archive a lost final left parked
func (c *turnController) startMessage() {
	c.endReasoningPhase()
	c.clearReasoning()
	c.activeTools = nil
	c.activeReasoningText = ""
	c.reasoningSegmentIndex = -1
	c.interimBoundaryIndex = -1
	c.turnTools = nil
	c.toolTokenAcc = 0
	c.interrupted = false
	c.sealed = nil
	clear(c.persistedToolLabels)
	if n := c.store.ui.Notice; n != nil && (n.Key == "credits.usage" || n.Key == "credits.grant_spent") {
		c.clearNotice(n.Key)
	}
	c.store.ui.Busy = true
	c.segmentMessages, c.pendingSegmentTools = nil, nil
	t := &c.store.turn
	t.Activity, t.Outcome, t.StreamPendingTools, t.StreamSegments, t.Subagents, t.ToolTokens, t.Tools, t.TurnTrail = nil, "", nil, nil, nil, 0, nil, nil
}

// Optional number of a payload field, nil when absent or null
func payloadNumber(p map[string]any, k string) *float64 {
	if v, isNum := p[k].(float64); isNum {
		return &v
	}
	return nil
}

// Optional string list of a payload field, nil when absent or null
func payloadStrings(p map[string]any, k string) []string {
	list, isList := p[k].([]any)
	if !isList {
		return nil
	}
	out := []string{}
	for _, item := range list {
		out = append(out, jsString(item))
	}
	return out
}

// Creates or updates a subagent from its event, keeping spawn order and dropping late events when asked
func (c *turnController) upsertSubagent(p map[string]any, patch func(base subagentProgress, next *subagentProgress), createIfMissing bool) {
	goal, _ := p["goal"].(string)
	id, _ := p["subagent_id"].(string)
	if id == "" {
		label := goal
		if label == "" {
			label = "subagent"
		}
		id = "sa:" + jsString(p["task_index"]) + ":" + label
	}
	subagents := c.store.turn.Subagents
	at := slices.IndexFunc(subagents, func(s subagentProgress) bool { return s.ID == id })
	if at < 0 && !createIfMissing {
		return
	}
	str := func(k string) (string, bool) {
		v, isStr := p[k].(string)
		return v, isStr
	}
	intOf := func(k string) (int, bool) {
		v, isNum := p[k].(float64)
		return int(v), isNum
	}
	var base subagentProgress
	if at >= 0 {
		base = subagents[at]
	} else {
		started := c.store.nowMillis()
		base = subagentProgress{Goal: goal, ID: id, Notes: []string{}, StartedAt: &started, Status: "running", TaskCount: 1, Thinking: []string{}, Tools: []string{}, Toolsets: payloadStrings(p, "toolsets")}
		base.DelegationID, _ = str("delegation_id")
		base.Depth, _ = intOf("depth")
		base.Index, _ = intOf("task_index")
		base.Model, _ = str("model")
		base.ParentID, _ = str("parent_id")
		if n, has := intOf("task_count"); has {
			base.TaskCount = n
		}
		base.ToolCount, _ = intOf("tool_count")
	}
	next := base
	if tail, isList := p["output_tail"].([]any); isList {
		next.OutputTail = []subagentOutput{}
		for _, item := range tail {
			entry, _ := item.(map[string]any)
			preview, tool := "", "tool"
			if v, has := entry["preview"]; has && v != nil {
				preview = jsString(v)
			}
			if v, has := entry["tool"]; has && v != nil {
				tool = jsString(v)
			}
			next.OutputTail = append(next.OutputTail, subagentOutput{IsError: truthyJS(entry["is_error"]), Preview: preview, Tool: tool})
		}
	}
	for k, dst := range map[string]**float64{"api_calls": &next.APICalls, "input_tokens": &next.InputTokens, "output_tokens": &next.OutputTokens, "reasoning_tokens": &next.ReasoningTokens} {
		if v := payloadNumber(p, k); v != nil {
			*dst = v
		}
	}
	if v, isStr := str("delegation_id"); isStr {
		next.DelegationID = v
	}
	if v, has := intOf("depth"); has {
		next.Depth = v
	}
	if v := payloadStrings(p, "files_read"); v != nil {
		next.FilesRead = v
	}
	if v := payloadStrings(p, "files_written"); v != nil {
		next.FilesWritten = v
	}
	if goal != "" {
		next.Goal = goal
	}
	if v, isStr := str("model"); isStr {
		next.Model = v
	}
	if v, isStr := str("parent_id"); isStr {
		next.ParentID = v
	}
	if v, has := intOf("task_count"); has {
		next.TaskCount = v
	}
	if v, has := intOf("tool_count"); has {
		next.ToolCount = v
	}
	if v := payloadStrings(p, "toolsets"); v != nil {
		next.Toolsets = v
	}
	if patch != nil {
		patch(base, &next)
	}
	if at >= 0 {
		subagents = slices.Clone(subagents)
		subagents[at] = next
	} else {
		subagents = append(slices.Clone(subagents), next)
		slices.SortStableFunc(subagents, func(a, b subagentProgress) int {
			if a.Depth != b.Depth {
				return a.Depth - b.Depth
			}
			return a.Index - b.Index
		})
	}
	c.store.turn.Subagents = subagents
}
