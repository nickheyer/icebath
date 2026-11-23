package tui

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"

	tea "charm.land/bubbletea/v2"
)

const (
	streamBatch       = 16 * time.Millisecond
	streamIdleBatch   = 16 * time.Millisecond
	streamScrollBatch = 96 * time.Millisecond
	streamTypingBatch = 80 * time.Millisecond
	reasoningPulse    = 700 * time.Millisecond
	interruptCooldown = 1500 * time.Millisecond
	activityLimit     = 8
	trailLimit        = 8
	reasoningTextCap  = 80_000
	reasoningTextKeep = 60_000
)

// Trail row shown between the last finished call and the next thing the model does, in the active language
func analyzingToolOutput() string { return tr(txt.ToolAnalyzing) }

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
	info        *tuigateway.SessionInfo
	handle      *tuigateway.CanonicalSessionHandle
	err         error
}

// Text of a failed request as the TUI shows it
func rpcErrorMessage(err error) string {
	if err != nil && err.Error() != "" {
		return describeRPCError(err)
	}
	return tr(txt.RpcRequestFailed)
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

func hasDetails(m msg) bool { return m.Thinking != "" || len(m.Tools) > 0 }

// Task list of the planning tool, items without an id or content left out and a parent naming itself dropped
func parseTodos(items []tuigateway.TodoItem) []todoItem {
	todos := []todoItem{}
	for _, item := range items {
		todo := todoItem{Content: strings.TrimFunc(item.Content, isJSSpace), ID: strings.TrimFunc(item.ID, isJSSpace), Status: string(item.Status)}
		if parent := strings.TrimFunc(val(item.Parent), isJSSpace); parent != "" && parent != todo.ID {
			todo.Parent = parent
		}
		if todo.ID != "" && todo.Content != "" {
			todos = append(todos, todo)
		}
	}
	return todos
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
func lateInterruptedReply(sealed *sealedInterrupt, payload tuigateway.MessageCompletePayload) *interruptedReply {
	persisted := strings.TrimFunc(val(payload.Text), isJSSpace)
	if sealed == nil {
		return nil
	}
	shown := strings.TrimRightFunc(sealed.Partial, isJSSpace)
	if val(payload.Status) != "interrupted" || unitLen(persisted) <= unitLen(shown) || !strings.HasPrefix(persisted, shown) {
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
	pendingSegmentTools []toolRow
	toolTokenAcc        int
	// What the turn did so far by its finished tool calls, when it began, and the UTF-16 units of everything the model produced in it
	summary   *turnSummaryCollector
	began     time.Time
	flowUnits int
	turnTools []string

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
	return &turnController{store: store, summary: newTurnSummaryCollector(), persistedToolLabels: map[string]bool{}, reasoningSegmentIndex: -1, interimBoundaryIndex: -1, streamDelay: streamIdleBatch}
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
		n.ID = "n" + strconv.Itoa(c.noticeIDSeq)
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
	c.store.setBusy(false)
	c.store.ui.Compacting = false
	c.store.resetFlowOverlays()
}

// Interrupts the turn, sealed only after the owner confirms the running generation
func (c *turnController) interruptTurn(deps interruptDeps, keepBusy bool) (tea.Cmd, error) {
	destination, err := c.store.captureDestination()
	if err != nil {
		return nil, err
	}
	info := c.store.ui.Info
	gw, sid := deps.Gateway, deps.Sid
	return func() tea.Msg {
		reply := interruptReplyMsg{deps: deps, keepBusy: keepBusy, destination: destination, info: info}
		generation := infoOf(info).ExecutionGeneration
		if generation == nil {
			reply.err = tuigateway.MissingParam(tuigateway.CanonicalSessionInterrupt.Name(), "execution_generation")
			return reply
		}
		value, err := gw.request(context.Background(), tuigateway.CanonicalSessionInterrupt.Name(), wireParams(tuigateway.CanonicalControlParams{SessionID: sid, ExecutionGeneration: *generation}), 0)
		if err == nil {
			var handle tuigateway.CanonicalSessionHandle
			if handle, err = tuigateway.CanonicalSessionInterrupt.Result(value); err == nil {
				reply.handle = &handle
			} else {
				err = fmt.Errorf("%s %w", misfitText(tr(txt.SessionCommonInvalidResponse, tuigateway.CanonicalSessionInterrupt.Name())), err)
			}
		}
		reply.err = err
		return reply
	}, nil
}

// Seals a canonical interrupt once its answer shows the same turn still running
func (c *turnController) settleInterrupt(m interruptReplyMsg) {
	isActiveTurn := func() bool {
		ui := c.store.ui
		return ui.Sid == m.deps.Sid && c.store.isCurrentDestination(m.destination) && ui.Busy && !c.interrupted &&
			sameStamp(infoOf(ui.Info).AuthorityEpoch, infoOf(m.info).AuthorityEpoch) &&
			sameStamp(infoOf(ui.Info).ExecutionGeneration, infoOf(m.info).ExecutionGeneration)
	}
	if m.err != nil {
		if isActiveTurn() {
			m.deps.Sys(tr(txt.CanonicalSubmitInterruptFailed, rpcErrorMessage(m.err)))
		}
		return
	}
	if held := infoOf(m.info).ExecutionGeneration; m.handle == nil || m.handle.ExecutionState != "running" || held == nil || m.handle.ExecutionGeneration != *held {
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
		deps.Sys(tr(txt.SessionTurnInterrupted))
		c.sealed = &sealedInterrupt{}
	}
	c.clearStatusTimer()
	if keepBusy {
		c.store.setBusy(true)
		c.store.ui.Status = tr(txt.SessionStatusInterrupting)
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

// Writes the open reasoning into its segment, live while the phase runs, the segment keeping the identity the transcript gave it
func (c *turnController) syncReasoningSegment(live bool) {
	thinking := strings.TrimFunc(c.activeReasoningText, isJSSpace)
	if thinking == "" {
		return
	}
	m := msg{Kind: "trail", Role: "system", Thinking: thinking, IsLiveReasoning: live}
	if c.reasoningSegmentIndex < 0 {
		c.reasoningSegmentIndex = len(c.segmentMessages)
		c.segmentMessages = append(slices.Clone(c.segmentMessages), m)
	} else {
		m.ID = c.segmentMessages[c.reasoningSegmentIndex].ID
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
func (c *turnController) recordTodos(items []tuigateway.TodoItem) {
	if c.interrupted || items == nil {
		return
	}
	c.store.turn.Todos = parseTodos(items)
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
func (c *turnController) pushInlineDiffSegment(diffText string, tools []toolRow) {
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

// Hangs the image a tool made or looked at under the trail line of its call, ahead of whatever streams next
func (c *turnController) pushImageSegment(path, toolID string) {
	if path == "" || c.interrupted {
		return
	}
	c.flushStreamingSegment()
	c.segmentMessages = append(slices.Clone(c.segmentMessages), msg{Kind: "image", Role: "system", Images: []string{path}, ImageRevision: toolID})
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
func (c *turnController) recordMessageComplete(payload tuigateway.MessageCompletePayload) messageCompleteResult {
	c.closeReasoningSegment()
	completion := payload.Text
	if completion == nil {
		completion = payload.Rendered
	}
	source := c.bufRef
	if completion != nil {
		source = *completion
	}
	rawText := strings.TrimLeftFunc(source, isJSSpace)
	if keepsStreamedTail(strings.TrimFunc(c.bufRef, isJSSpace), completion, val(payload.ResponseTransformed)) {
		c.flushStreamingSegment()
	}
	split := splitReasoning(rawText)
	dedupeStart := 0
	if !val(payload.ResponsePreviewed) && c.interimBoundaryIndex >= 0 {
		dedupeStart = min(c.interimBoundaryIndex, len(c.segmentMessages))
	}
	finalText := finalTail(split.Text, c.segmentMessages[dedupeStart:])
	existing := strings.TrimFunc(c.reasoningText, isJSSpace)
	if existing == "" {
		existing = strings.TrimFunc(val(payload.Reasoning), isJSSpace)
	}
	saved := existing
	if existing == "" {
		saved = split.Reasoning
	}
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
	details := msg{Kind: "trail", Role: "system", Thinking: finalThinking, Tools: tools}
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
	c.flowUnits += unitLen(text)
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
func (c *turnController) recordMoaReference(label, text string, index, count int64) {
	if c.interrupted {
		return
	}
	c.closeReasoningSegment()
	header := tr(txt.ChatBitsReferenceHeader, label)
	if index != 0 && count != 0 {
		header = tr(txt.ChatBitsReferenceHeaderOf, index, count, label)
	}
	thinking := header
	if body := strings.TrimFunc(text, isJSSpace); body != "" {
		thinking = header + "\n" + body
	}
	c.pushSegment(msg{Kind: "trail", Role: "system", Thinking: thinking, IsMoaReference: true})
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
func (c *turnController) recordToolComplete(toolID, fallbackName, summary string, duration *float64, todos []tuigateway.TodoItem, resultText string, labels []toolLabel, v toolVerdict) {
	if c.interrupted {
		return
	}
	c.recordTodos(todos)
	lines := c.completeTool(toolID, fallbackName, summary, duration, resultText, labels, v)
	c.pendingSegmentTools = append(slices.Clone(c.pendingSegmentTools), lines...)
	c.flushPendingToolsIntoLastSegment()
	c.publishToolState()
}

// Finishes an editing tool call into an inline diff segment carrying its trail rows
func (c *turnController) recordInlineDiffToolComplete(diffText, toolID, fallbackName string, duration *float64, resultText string, labels []toolLabel, v toolVerdict) {
	if c.interrupted {
		return
	}
	c.flushStreamingSegment()
	c.pushInlineDiffSegment(diffText, c.completeTool(toolID, fallbackName, "", duration, resultText, labels, v))
	c.publishToolState()
}

// Removes a call from the active set and returns its trail rows marked with the verdict, none for a call whose row a settled prompt put into the transcript
func (c *turnController) completeTool(toolID, fallbackName, summary string, duration *float64, resultText string, eventLabels []toolLabel, v toolVerdict) []toolRow {
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
	context, args := "", ""
	if done != nil {
		context, args = done.Context, done.VerboseArgs
	}
	lines := finishedToolRows(name, context, labels, args, summary, resultText, took, v)
	if c.persistedToolLabels[label] {
		delete(c.persistedToolLabels, label)
		lines = nil
	}
	c.activeTools = slices.DeleteFunc(slices.Clone(c.activeTools), func(t activeTool) bool { return t.ID == toolID })
	next := slices.DeleteFunc(slices.Clone(c.turnTools), func(item string) bool { return sameToolTrailGroup(label, item) })
	if len(c.activeTools) == 0 {
		next = append(next, analyzingToolOutput())
	}
	c.turnTools = next[max(0, len(next)-trailLimit):]
	return lines
}

func (c *turnController) publishToolState() {
	c.store.turn.StreamPendingTools, c.store.turn.Tools, c.store.turn.TurnTrail = c.pendingSegmentTools, c.activeTools, c.turnTools
}

// Shows the progress a running call reported on its row
func (c *turnController) recordToolProgress(toolID, text string) {
	if c.interrupted || text == "" {
		return
	}
	i := slices.IndexFunc(c.activeTools, func(t activeTool) bool { return t.ID == toolID })
	if i < 0 {
		return
	}
	next := slices.Clone(c.activeTools)
	next[i].Progress = text
	c.activeTools = next
	c.store.turn.Tools = c.activeTools
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
		c.flowUnits += unitLen(sample)
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
	c.summary.begin()
	c.began, c.flowUnits = c.store.now(), 0
	clear(c.persistedToolLabels)
	c.store.setBusy(true)
	c.segmentMessages, c.pendingSegmentTools = nil, nil
	t := &c.store.turn
	t.Activity, t.Outcome, t.StreamPendingTools, t.StreamSegments, t.Subagents, t.ToolTokens, t.Tools, t.TurnTrail = nil, "", nil, nil, nil, 0, nil, nil
}

// Tokens the model produced in the running turn, estimated from the text, reasoning and tool calls that streamed in
func (c *turnController) flowTokens() int { return (c.flowUnits + 3) >> 2 }

// Counts reasoning the model streamed toward the tokens of the turn, shown or not
func (c *turnController) countReasoning(text string) {
	if !c.interrupted {
		c.flowUnits += unitLen(text)
	}
}

// Line of what the finished turn did and how long it took, empty for a quick reply without tools and when the line is turned off or the focus view
// keeps tool activity out of the transcript
func (c *turnController) summaryLine() string {
	if !c.store.ui.TurnSummary || c.store.ui.FocusView || c.began.IsZero() {
		return ""
	}
	return c.summary.render(max(c.store.now().Sub(c.began).Seconds(), 0))
}

// Creates or updates a subagent from its event, a child of a finished tree patched there with that tree's index returned
func (c *turnController) upsertSubagent(p tuigateway.SubagentEventPayload, patch func(base subagentProgress, next *subagentProgress), createIfMissing bool) int {
	id := subagentKey(p)
	subagents := c.store.turn.Subagents
	at := slices.IndexFunc(subagents, func(s subagentProgress) bool { return s.ID == id })
	snap, inSnap := -1, -1
	if at < 0 {
		snap, inSnap = c.store.finishedTreeAgent(id)
	}
	if at < 0 && snap < 0 && !createIfMissing {
		return -1
	}
	var base subagentProgress
	switch {
	case at >= 0:
		base = subagents[at]
	case snap >= 0:
		base = c.store.spawnHistory[snap].Subagents[inSnap]
	default:
		started := c.store.nowMillis()
		base = subagentProgress{Goal: p.Goal, ID: id, Notes: []string{}, StartedAt: &started, Status: "running", TaskCount: 1, Thinking: []string{}, Tools: []string{}, Toolsets: p.Toolsets,
			DelegationID: val(p.DelegationID), Depth: int(val(p.Depth)), Index: int(p.TaskIndex), Model: val(p.Model), ParentID: val(p.ParentID), ToolCount: int(val(p.ToolCount))}
	}
	next := base
	if p.OutputTail != nil {
		next.OutputTail = make([]subagentOutput, len(p.OutputTail))
		for i, entry := range p.OutputTail {
			next.OutputTail[i] = subagentOutput{IsError: entry.IsError, Preview: entry.Preview, Tool: firstText(entry.Tool, "tool")}
		}
	}
	for src, dst := range map[*int64]**float64{p.APICalls: &next.APICalls, p.InputTokens: &next.InputTokens, p.OutputTokens: &next.OutputTokens, p.ReasoningTokens: &next.ReasoningTokens} {
		if src != nil {
			n := float64(*src)
			*dst = &n
		}
	}
	if p.CostUSD != nil {
		next.CostUSD = p.CostUSD
	}
	if p.DelegationID != nil {
		next.DelegationID = *p.DelegationID
	}
	if p.Depth != nil {
		next.Depth = int(*p.Depth)
	}
	if p.FilesRead != nil {
		next.FilesRead = p.FilesRead
	}
	if p.FilesWritten != nil {
		next.FilesWritten = p.FilesWritten
	}
	if p.Goal != "" {
		next.Goal = p.Goal
	}
	if p.Model != nil {
		next.Model = *p.Model
	}
	if p.ParentID != nil {
		next.ParentID = *p.ParentID
	}
	if p.TaskCount != 0 {
		next.TaskCount = int(p.TaskCount)
	}
	if p.ToolCount != nil {
		next.ToolCount = int(*p.ToolCount)
	}
	if p.Toolsets != nil {
		next.Toolsets = p.Toolsets
	}
	if patch != nil {
		patch(base, &next)
	}
	if snap >= 0 {
		history := slices.Clone(c.store.spawnHistory)
		agents := slices.Clone(history[snap].Subagents)
		agents[inSnap] = next
		history[snap].Subagents = agents
		c.store.spawnHistory = history
		return snap
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
	return -1
}
