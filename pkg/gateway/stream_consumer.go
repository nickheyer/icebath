package gateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Boundary finalize text when nothing has accumulated yet
const defaultBoundaryText = "⏸ 等待审批中..."

// Consecutive flood failures before progressive edits are disabled
const maxFloodStrikes = 3

// Runtime config of one stream consumer
type StreamConsumerConfig struct {
	EditInterval           float64
	BufferThreshold        int
	Cursor                 string
	BufferOnly             bool
	FreshFinalAfterSeconds float64
	Transport              string
	ChatType               string
}

// Config carrying the dataclass defaults
func DefaultStreamConsumerConfig() *StreamConsumerConfig {
	return &StreamConsumerConfig{EditInterval: DefaultStreamingEditInterval, BufferThreshold: DefaultStreamingBufferThreshold,
		Cursor: DefaultStreamingCursor, Transport: "edit"}
}

func streamLog() *slog.Logger { return slog.Default().With("logger", "gateway.stream_consumer") }

// Class wide draft id counter seeded with 49 random bits
var streamDraftIDCounter atomic.Int64

func init() {
	var b [8]byte
	_, _ = rand.Read(b[:])
	streamDraftIDCounter.Store(int64(binary.BigEndian.Uint64(b[:]) >> 15))
}

// Barrier a worker thread blocks on until the consumer delivered everything queued before it
type StreamFlushSignal struct {
	once sync.Once
	ch   chan struct{}
}

func newStreamFlushSignal() *StreamFlushSignal { return &StreamFlushSignal{ch: make(chan struct{})} }

func (f *StreamFlushSignal) set() { f.once.Do(func() { close(f.ch) }) }

// Future of an interaction boundary, true once the consumer processed it cleanly
type StreamBoundary struct {
	once sync.Once
	done chan struct{}
	ok   bool
}

func newStreamBoundary() *StreamBoundary { return &StreamBoundary{done: make(chan struct{})} }

func (b *StreamBoundary) settle(ok bool) {
	b.once.Do(func() {
		b.ok = ok
		close(b.done)
	})
}

// Channel closed once the boundary settled
func (b *StreamBoundary) Done() <-chan struct{} { return b.done }

// Whether the boundary settled
func (b *StreamBoundary) Settled() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

// Outcome of a settled boundary, waiting until it settles or ctx ends
func (b *StreamBoundary) Wait(ctx context.Context) (bool, error) {
	select {
	case <-b.done:
		return b.ok, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

type streamItemKind int

const (
	streamItemDelta streamItemKind = iota
	streamItemDone
	streamItemNewSegment
	streamItemCommentary
	streamItemToolProgress
	streamItemFinalText
	streamItemFlush
	streamItemApprovalBoundary
	streamItemReopenSeed
)

type streamItem struct {
	kind     streamItemKind
	text     string
	flush    *StreamFlushSignal
	boundary *StreamBoundary
}

// Everything one drain of the queue decided
type streamTick struct {
	gotDone             bool
	gotSegmentBreak     bool
	gotFlush            bool
	flushEvent          *StreamFlushSignal
	gotReopenSeed       bool
	approvalBoundary    *StreamBoundary
	commentaryText      *string
	updateVisible       bool
	draftFinalFreshSend bool
}

// Mid stream tick that neither finalizes, breaks a segment nor carries commentary
func (t *streamTick) isInterim() bool {
	return !t.gotDone && !t.gotSegmentBreak && t.commentaryText == nil
}

// Insertion ordered set of message ids
type idSet []string

func (s idSet) has(id string) bool {
	for _, v := range s {
		if v == id {
			return true
		}
	}
	return false
}

func (s *idSet) add(id string) {
	if !s.has(id) {
		*s = append(*s, id)
	}
}

func (s idSet) clone() idSet { return append(idSet(nil), s...) }

// Message, creation time, text and segment previews of the last finalized segment
type segmentDelivery struct {
	messageID  string
	createdTS  *float64
	text       string
	previewIDs idSet
}

// Async consumer that progressively edits a platform message with streamed tokens
type GatewayStreamConsumer struct {
	adapter          PlatformAdapter
	chatID           string
	cfg              *StreamConsumerConfig
	metadata         map[string]any
	onNewMessage     func()
	onBeforeFinalize func(ctx context.Context)
	initialReplyToID string
	turnID           string
	runStillCurrent  func() bool
	// Whether this consumer is fed the final reply's stream deltas
	StreamDeltasEnabled bool

	adapterRequiresFinalize      bool
	maxFallbackFloodRetrySeconds float64

	queueMu sync.Mutex
	queue   []streamItem

	previewMessageIDs        idSet
	alreadySent              bool
	editSupported            bool
	lastEditTime             float64
	lastEditOverflowed       bool
	floodStrikes             int
	currentEditInterval      float64
	deliveredCommentaryTexts []string
	deliveredSegmentTexts    []string
	lastSegmentDelivery      *segmentDelivery
	supersededReplyIDs       idSet
	inThinkBlock             bool
	thinkBuffer              string
	beforeFinalizeNotified   bool

	messageID                       string
	messageCreatedTS                *float64
	accumulated                     string
	streamLedger                    string
	lastSentText                    string
	fallbackFinalSend               bool
	fallbackPrefix                  string
	fallbackPreservePartialMessages bool
	segmentPreviewMessageIDs        idSet
	toolProgressLines               []string
	toolProgressActive              bool

	finalResponseSent     bool
	finalContentDelivered bool
	deliveredFinalText    *string
	turnSplitDelivery     bool
	deliveryAmbiguous     bool

	useDraftStreaming   bool
	draftID             *int64
	draftFailures       int
	egressDeclined      bool
	useNativeStreaming  atomic.Bool
	nativeStreamOpened  atomic.Bool
	nativeLastPushedLen int

	boundaryMu     sync.Mutex
	boundaryText   string
	boundaryReason string
	boundaryReopen bool

	awaitingReopenAfterBoundary atomic.Bool
	reopenSeededEagerly         bool

	lenFn     func(string) int
	safeLimit int
}

// Options of a stream consumer beyond its adapter and chat
type StreamConsumerOptions struct {
	Config           *StreamConsumerConfig
	Metadata         map[string]any
	OnNewMessage     func()
	OnBeforeFinalize func(ctx context.Context)
	InitialReplyToID string
	RunStillCurrent  func() bool
}

// Consumer for one chat with every per run transport state fresh
func NewGatewayStreamConsumer(adapter PlatformAdapter, chatID string, opts StreamConsumerOptions) *GatewayStreamConsumer {
	cfg := opts.Config
	if cfg == nil {
		cfg = DefaultStreamConsumerConfig()
	}
	runStillCurrent := opts.RunStillCurrent
	if runStillCurrent == nil {
		runStillCurrent = func() bool { return true }
	}
	s := &GatewayStreamConsumer{
		adapter: adapter, chatID: chatID, cfg: cfg, metadata: opts.Metadata, onNewMessage: opts.OnNewMessage,
		onBeforeFinalize: opts.OnBeforeFinalize, initialReplyToID: opts.InitialReplyToID, turnID: uuid.NewString(),
		runStillCurrent: runStillCurrent, StreamDeltasEnabled: true, maxFallbackFloodRetrySeconds: 5.0,
		editSupported: true, currentEditInterval: cfg.EditInterval,
		boundaryText: defaultBoundaryText, boundaryReason: "Approval",
	}
	if adapter != nil && adapter.Base() != nil {
		s.adapterRequiresFinalize = adapter.Base().Traits.RequiresEditFinalize
	}
	s.resetMessageState()
	return s
}

// Per segment state, fresh at construction and after each segment break
func (s *GatewayStreamConsumer) resetMessageState() {
	s.messageID = ""
	s.messageCreatedTS = nil
	s.accumulated, s.streamLedger = "", ""
	s.lastSentText = ""
	s.fallbackFinalSend = false
	s.fallbackPrefix = ""
	s.fallbackPreservePartialMessages = false
	s.segmentPreviewMessageIDs = nil
	s.toolProgressLines = nil
	s.toolProgressActive = false
	s.clearTurnFinalFlags()
}

// Resets every turn final delivery flag to nothing delivered yet
func (s *GatewayStreamConsumer) clearTurnFinalFlags() {
	s.finalResponseSent = false
	s.finalContentDelivered = false
	s.deliveredFinalText = nil
	s.turnSplitDelivery = false
	s.deliveryAmbiguous = false
}

// Adapter with a per chat probe of whether the draft stream is the message
type streamIsMessageProbe interface {
	StreamIsMessageForChat(chatID string) bool
}

// Adapter whose draft stream is the message for every chat
type draftStreamIsMessageAdapter interface {
	DraftStreamIsMessage() bool
}

// Whether this chat's transport treats the stream as the message
func (s *GatewayStreamConsumer) streamIsMessage() bool {
	if probe, ok := s.adapter.(streamIsMessageProbe); ok {
		return probe.StreamIsMessageForChat(s.chatID)
	}
	if legacy, ok := s.adapter.(draftStreamIsMessageAdapter); ok {
		return legacy.DraftStreamIsMessage()
	}
	return false
}

// True only while native streaming is active
func (s *GatewayStreamConsumer) AcceptsToolProgress() bool { return s.useNativeStreaming.Load() }

// Overlays a tool progress line in the native bubble until the next delta, safe from any goroutine
func (s *GatewayStreamConsumer) OnToolProgress(line string) {
	if line != "" {
		s.put(streamItem{kind: streamItemToolProgress, text: line})
	}
}

func (s *GatewayStreamConsumer) put(item streamItem) {
	s.queueMu.Lock()
	s.queue = append(s.queue, item)
	s.queueMu.Unlock()
}

func (s *GatewayStreamConsumer) getNowait() (streamItem, bool) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if len(s.queue) == 0 {
		return streamItem{}, false
	}
	item := s.queue[0]
	s.queue = s.queue[1:]
	return item, true
}

// Native frame content, the text with any tool progress lines below a rule
func (s *GatewayStreamConsumer) composeFrameContent() string {
	progress := strings.Join(s.toolProgressLines, "\n")
	var parts []string
	for _, p := range []string{s.accumulated, progress} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n---\n")
}

// Per send metadata, final asking for a notification and expectEdits keeping editable previews
func (s *GatewayStreamConsumer) metadataForSend(final, expectEdits bool) map[string]any {
	meta := map[string]any{}
	for k, v := range s.metadata {
		meta[k] = v
	}
	if s.initialReplyToID != "" {
		meta["reply_to_message_id"] = s.initialReplyToID
	}
	if expectEdits {
		meta["expect_edits"] = true
	}
	if final {
		meta["notify"] = true
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// Whether anything durable was sent to the chat
func (s *GatewayStreamConsumer) AlreadySent() bool { return s.alreadySent }

// Whether the turn final answer was delivered by the stream
func (s *GatewayStreamConsumer) FinalResponseSent() bool { return s.finalResponseSent }

// Id of the live preview message, empty when none
func (s *GatewayStreamConsumer) MessageID() string { return s.messageID }

// Whether the final content landed even when the cosmetic finalize edit failed
func (s *GatewayStreamConsumer) FinalContentDelivered() bool { return s.finalContentDelivered }

// Whether the turn final was delivered split across several messages
func (s *GatewayStreamConsumer) TurnSplitDelivery() bool { return s.turnSplitDelivery }

// Text the consumer last put on the wire, read by the reply clock
func (s *GatewayStreamConsumer) LastSentText() any { return s.lastSentText }

// Adapter the reply clock attributes the delivery to
func (s *GatewayStreamConsumer) MetricsAdapter() any { return s.adapter }

// Chat the reply clock attributes the delivery to
func (s *GatewayStreamConsumer) MetricsChatID() any { return s.chatID }

// Runs the pre finalize hook exactly once
func (s *GatewayStreamConsumer) notifyBeforeFinalize(ctx context.Context) {
	if s.beforeFinalizeNotified {
		return
	}
	s.beforeFinalizeNotified = true
	if s.onBeforeFinalize != nil {
		func() {
			defer func() { _ = recover() }()
			s.onBeforeFinalize(ctx)
		}()
	}
}

// Appends to the live buffer and the split stable stream ledger
func (s *GatewayStreamConsumer) appendAccumulated(text string) {
	if text == "" {
		return
	}
	if len(s.toolProgressLines) > 0 {
		s.toolProgressLines = nil
		s.toolProgressActive = false
	}
	s.accumulated += text
	s.streamLedger += text
}

// Marks the turn final delivered by a prior mid stream edit, recording what was acked on the wire
func (s *GatewayStreamConsumer) markSkipRedundantFinalize() {
	acked := s.lastSentText
	if acked == "" {
		acked = s.accumulated
	}
	if s.cfg.Cursor != "" && strings.HasSuffix(acked, s.cfg.Cursor) {
		acked = strings.TrimSuffix(acked, s.cfg.Cursor)
	}
	s.markFinalDelivered(&acked)
}

// Sets both turn final flags, recording the delivered payload when given
func (s *GatewayStreamConsumer) markFinalDelivered(record *string) {
	s.finalResponseSent = true
	s.finalContentDelivered = true
	if record != nil {
		s.recordTurnFinalPayload(*record)
	}
}

// Text normalized like send output, directives stripped, fences closed and trimmed
func (s *GatewayStreamConsumer) displayPayload(text string) string {
	return pystr.Strip(EnsureClosedCodeFences(cleanForDisplay(text)))
}

// Records what the user saw as the turn final answer, the whole ledger on a split delivery
func (s *GatewayStreamConsumer) recordTurnFinalPayload(text string) {
	if s.turnSplitDelivery && s.streamLedger != "" {
		text = s.streamLedger
	}
	payload := s.displayPayload(text)
	s.deliveredFinalText = &payload
}

// Tri state reconcile of the recorded turn final payload against the final text, nil when the flags are trusted
func (s *GatewayStreamConsumer) DeliveredFinalMatches(finalText string) *bool {
	target := s.displayPayload(finalText)
	if target == "" {
		return nil
	}
	yes, no := true, false
	if s.deliveredFinalText != nil {
		if pystr.Strip(*s.deliveredFinalText) == target || s.HasDeliveredText(finalText) {
			return &yes
		}
		return &no
	}
	if s.turnSplitDelivery {
		return &no
	}
	if s.alreadySent && s.HasDeliveredText(finalText) {
		return &yes
	}
	if s.deliveryAmbiguous {
		return nil
	}
	return &no
}

// Whether text was already delivered as visible chat content
func (s *GatewayStreamConsumer) HasDeliveredText(text string) bool {
	target := pystr.Strip(cleanForDisplay(text))
	if target == "" {
		return false
	}
	seen := append([]string{s.visiblePrefix()}, s.deliveredCommentaryTexts...)
	seen = append(seen, s.deliveredSegmentTexts...)
	for _, sent := range seen {
		if pystr.Strip(sent) == target {
			return true
		}
	}
	return false
}

// Delivered text match restricted to deliveries that outlive the turn
func (s *GatewayStreamConsumer) HasDurablyDeliveredText(text string) bool {
	target := pystr.Strip(cleanForDisplay(text))
	if target == "" {
		return false
	}
	seen := append(append([]string(nil), s.deliveredCommentaryTexts...), s.deliveredSegmentTexts...)
	if s.alreadySent {
		seen = append(seen, s.visiblePrefix())
	}
	for _, sent := range seen {
		if pystr.Strip(sent) == target {
			return true
		}
	}
	return false
}

// Finalizes the current stream segment and starts a fresh message
func (s *GatewayStreamConsumer) OnSegmentBreak() { s.put(streamItem{kind: streamItemNewSegment}) }

// Queues an approval or clarify boundary, an already settled boundary without native streaming
func (s *GatewayStreamConsumer) CloseForApprovalPrompt(boundaryText, reason string, reopen bool) *StreamBoundary {
	boundary := newStreamBoundary()
	if !s.useNativeStreaming.Load() {
		boundary.settle(true)
		return boundary
	}
	s.boundaryMu.Lock()
	s.boundaryText = boundaryText
	if s.boundaryText == "" {
		s.boundaryText = defaultBoundaryText
	}
	s.boundaryReason = reason
	if s.boundaryReason == "" {
		s.boundaryReason = "Approval"
	}
	s.boundaryReopen = reopen
	s.boundaryMu.Unlock()
	s.put(streamItem{kind: streamItemApprovalBoundary, boundary: boundary})
	return boundary
}

// Queues a completed interim assistant commentary message
func (s *GatewayStreamConsumer) OnCommentary(text string) {
	if text != "" {
		s.put(streamItem{kind: streamItemCommentary, text: text})
	}
}

// Blocks until everything queued so far is delivered, false on timeout
func (s *GatewayStreamConsumer) FlushPendingSync(timeout float64) bool {
	signal := newStreamFlushSignal()
	s.put(streamItem{kind: streamItemFlush, flush: signal})
	select {
	case <-signal.ch:
		return true
	case <-time.After(time.Duration(max(0, timeout) * float64(time.Second))):
		return false
	}
}

// Native stream with a reopen requested after a boundary and nothing open yet
func (s *GatewayStreamConsumer) reopenSeedPending() bool {
	return s.useNativeStreaming.Load() && s.awaitingReopenAfterBoundary.Load() && !s.nativeStreamOpened.Load()
}

// Requests an eager native re seed after a clarify answer, a no op unless one is pending
func (s *GatewayStreamConsumer) RequestReopenSeed() {
	if s.reopenSeedPending() {
		s.put(streamItem{kind: streamItemReopenSeed})
	}
}

// Fires the new message callback, swallowing its panics
func (s *GatewayStreamConsumer) notifyNewMessage() {
	if s.onNewMessage == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			streamLog().Debug("on_new_message callback error", "error", r)
		}
	}()
	s.onNewMessage()
}

// Wakes a goroutine blocked in the flush barrier
func signalFlush(flush *StreamFlushSignal) {
	if flush != nil {
		flush.set()
	}
}

// Ends the segment keeping its visible text, the no edit sentinel kept when asked
func (s *GatewayStreamConsumer) resetSegmentState(preserveNoEdit bool) {
	if preserveNoEdit && s.messageID == "__no_edit__" {
		return
	}
	if finalized := pystr.Strip(cleanForDisplay(s.lastSentText)); finalized != "" {
		s.deliveredSegmentTexts = append(s.deliveredSegmentTexts, finalized)
		s.lastSegmentDelivery = nil
		if s.messageID != "" && s.messageID != "__no_edit__" {
			s.lastSegmentDelivery = &segmentDelivery{messageID: s.messageID, createdTS: s.messageCreatedTS,
				text: s.lastSentText, previewIDs: s.stalePreviewIDs(true)}
		}
	}
	s.resetMessageState()
	if s.useDraftStreaming && !s.streamIsMessage() {
		s.bumpDraftID()
	}
}

func (s *GatewayStreamConsumer) bumpDraftID() {
	id := streamDraftIDCounter.Add(1)
	s.draftID = &id
}

// Serially processes an interaction boundary dequeued by the run loop
func (s *GatewayStreamConsumer) handleApprovalBoundary(ctx context.Context, boundary *StreamBoundary) {
	s.boundaryMu.Lock()
	reason, reopen := s.boundaryReason, s.boundaryReopen
	s.boundaryMu.Unlock()
	if reason == "" {
		reason = "Approval"
	}
	boundaryOK := true
	defer func() { boundary.settle(boundaryOK) }()
	if s.nativeStreamOpened.Load() {
		boundaryOK = s.finalizeBoundaryStream(ctx, reason)
	}
	if reopen {
		s.closeNativeState()
		s.awaitingReopenAfterBoundary.Store(true)
	} else {
		s.degradeNativeToBufferedSend()
	}
	s.resetSegmentState(false)
	if reopen {
		streamLog().Info(fmt.Sprintf("[latency] Clarify boundary finalized, awaiting first post-answer delta to re-seed (chat=%s, turn=%s)", s.chatID, s.turnID))
	}
}

// Closes the open native stream at a boundary, sending the pre prompt text when that fails
func (s *GatewayStreamConsumer) finalizeBoundaryStream(ctx context.Context, reason string) bool {
	s.boundaryMu.Lock()
	closingText := s.boundaryText
	s.boundaryMu.Unlock()
	finalizeText := s.accumulated
	if finalizeText == "" {
		finalizeText = closingText
	}
	ok, err := s.sendFrame(ctx, finalizeText, true)
	if err != nil {
		streamLog().Warn(fmt.Sprintf("%s boundary: finalize failed: %s", reason, pyrepr.ExceptionMessage(err)))
	} else if ok {
		streamLog().Debug(fmt.Sprintf("%s boundary: finalized stream (chat=%s, turn=%s)", reason, s.chatID, s.turnID))
		return true
	}
	streamLog().Warn(fmt.Sprintf("%s boundary: finalize not confirmed, falling back to send() for pre-prompt text (chat=%s)", reason, s.chatID))
	result, err := s.adapter.Send(ctx, s.chatID, finalizeText, "", nil)
	if err != nil {
		streamLog().Warn(fmt.Sprintf("%s boundary: fallback send also failed: %s", reason, pyrepr.ExceptionMessage(err)))
	} else if result.Success {
		return true
	}
	streamLog().Error(fmt.Sprintf("%s boundary: both finalize and fallback send failed (chat=%s) — pre-prompt text may not have been delivered", reason, s.chatID))
	return false
}

// Agent worker callback, a text delta queued and nil marking a tool boundary
func (s *GatewayStreamConsumer) OnDelta(text any) {
	switch v := text.(type) {
	case nil:
		s.OnSegmentBreak()
	case string:
		if v != "" {
			s.put(streamItem{kind: streamItemDelta, text: v})
		}
	case *string:
		if v == nil {
			s.OnSegmentBreak()
		} else if *v != "" {
			s.put(streamItem{kind: streamItemDelta, text: *v})
		}
	}
}

// Signals stream completion, the authoritative final text adopted as the finalize payload when given
func (s *GatewayStreamConsumer) Finish(finalText *string) {
	if finalText != nil {
		s.put(streamItem{kind: streamItemFinalText, text: *finalText})
	}
	s.put(streamItem{kind: streamItemDone})
}

// Sleeps for seconds or until ctx ends, false when ctx ended
func streamSleep(ctx context.Context, seconds float64) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(seconds * float64(time.Second))):
		return true
	}
}

// Drains the queue and edits the platform message until the turn finishes, the stream is stale or ctx ends
func (s *GatewayStreamConsumer) Run(ctx context.Context) {
	defer s.wakeFlushWaiters()
	s.lenFn, s.safeLimit = s.resolveLengthBudget()
	s.startTransports(ctx)
	defer func() {
		if r := recover(); r != nil {
			streamLog().Error(fmt.Sprintf("Stream consumer error: %v", r))
		}
	}()
	for {
		if ctx.Err() != nil {
			s.onCancelled(context.WithoutCancel(ctx))
			return
		}
		if !s.runStillCurrent() {
			s.abandonNativeStream(ctx)
			return
		}
		tick := s.drainQueue()
		if tick.approvalBoundary != nil {
			s.handleApprovalBoundary(ctx, tick.approvalBoundary)
			continue
		}
		if tick.gotReopenSeed {
			s.eagerReopenSeed(ctx)
			continue
		}
		if tick.gotDone {
			s.flushThinkBuffer()
			if IsIntentionalSilenceResponse(cleanForDisplay(s.accumulated)) {
				s.suppressSilenceMarker(ctx)
				return
			}
			s.accumulated = pyrepr.Str(StripTrailingSilenceMarker(s.accumulated))
		}
		if s.shouldEdit(tick) && (s.accumulated != "" || (s.useNativeStreaming.Load() && s.toolProgressActive)) {
			s.sealOverflowHeads(ctx)
			if !s.useNativeStreaming.Load() && s.firstSendOverflows() {
				if s.splitFirstSend(ctx, tick) {
					return
				}
				if s.firstSendOverflows() {
					signalFlush(tick.flushEvent)
					if !streamSleep(ctx, 0.05) {
						s.onCancelled(context.WithoutCancel(ctx))
						return
					}
					continue
				}
			}
			s.pushUpdate(ctx, tick)
		}
		if tick.gotDone {
			s.finalizeTurn(ctx, tick)
			return
		}
		if tick.commentaryText != nil {
			s.deliverCommentary(ctx, *tick.commentaryText)
		}
		if tick.gotSegmentBreak {
			s.endSegment(ctx, tick)
		}
		if tick.gotFlush {
			signalFlush(tick.flushEvent)
		}
		if !streamSleep(ctx, 0.05) {
			s.onCancelled(context.WithoutCancel(ctx))
			return
		}
	}
}

// Per chat length function and the overflow budget it measures against
func (s *GatewayStreamConsumer) resolveLengthBudget() (func(string) int, int) {
	lenFn, _ := s.fallbackLenBudget()
	return lenFn, max(500, s.rawMessageLimit()-lenFn(s.cfg.Cursor)-100)
}

// Resolves the native or draft transport, native winning and seeding a typing frame first
func (s *GatewayStreamConsumer) startTransports(ctx context.Context) {
	s.useNativeStreaming.Store(s.resolveNativeStreaming())
	if s.useNativeStreaming.Load() {
		streamLog().Debug(fmt.Sprintf("Stream consumer using native-stream transport (chat=%s)", s.chatID))
		if s.trySeedFrame(ctx, "Native streaming seed frame raised; disabling native", true) {
			s.nativeStreamOpened.Store(true)
			s.useDraftStreaming = false
			return
		}
		s.useNativeStreaming.Store(false)
	}
	s.useDraftStreaming = s.resolveDraftStreaming()
	if s.useDraftStreaming {
		s.bumpDraftID()
		streamLog().Debug(fmt.Sprintf("Stream consumer using native-draft transport (chat=%s draft_id=%d)", s.chatID, *s.draftID))
	}
}

// Drains everything queued so far into one tick, control items ending the drain
func (s *GatewayStreamConsumer) drainQueue() *streamTick {
	tick := &streamTick{}
	for {
		item, ok := s.getNowait()
		if !ok {
			return tick
		}
		switch item.kind {
		case streamItemDone:
			tick.gotDone = true
			return tick
		case streamItemNewSegment:
			tick.gotSegmentBreak = true
			return tick
		case streamItemReopenSeed:
			tick.gotReopenSeed = true
			return tick
		case streamItemFinalText:
			s.adoptFinalText(item.text)
		case streamItemToolProgress:
			if s.useNativeStreaming.Load() {
				s.toolProgressLines = append(s.toolProgressLines, item.text)
				s.toolProgressActive = true
			}
		case streamItemApprovalBoundary:
			tick.approvalBoundary = item.boundary
			return tick
		case streamItemCommentary:
			text := item.text
			tick.commentaryText = &text
			return tick
		case streamItemFlush:
			tick.gotFlush, tick.gotSegmentBreak = true, true
			tick.flushEvent = item.flush
			return tick
		default:
			s.filterAndAccumulate(item.text)
		}
	}
}

// Reconciles a marker followed reply against its own prior segment, not older commentary
func (s *GatewayStreamConsumer) adoptRecoveredReply(finalRaw string) bool {
	if s.messageID != "" || s.cumulativeTransport() || s.lastSegmentDelivery == nil ||
		!IsIntentionalSilenceResponse(cleanForDisplay(s.accumulated)) || IsIntentionalSilenceResponse(cleanForDisplay(finalRaw)) {
		return false
	}
	prior := s.lastSegmentDelivery
	if len(prior.previewIDs) == 1 && s.displayPayload(prior.text) == s.displayPayload(finalRaw) {
		s.accumulated, s.streamLedger = "", ""
		s.markFinalDelivered(&finalRaw)
		return true
	}
	s.messageID, s.messageCreatedTS = prior.messageID, prior.createdTS
	s.lastSentText = prior.text
	s.alreadySent = true
	s.segmentPreviewMessageIDs = idSet{prior.messageID}
	s.previewMessageIDs = prior.previewIDs.clone()
	s.supersededReplyIDs = nil
	for _, id := range prior.previewIDs {
		if id != prior.messageID {
			s.supersededReplyIDs.add(id)
		}
	}
	if n := len(s.deliveredSegmentTexts); n > 0 {
		s.deliveredSegmentTexts = s.deliveredSegmentTexts[:n-1]
	}
	s.lastSegmentDelivery = nil
	s.accumulated, s.streamLedger = finalRaw, finalRaw
	return true
}

// Adopts the authoritative final as the finalize content when this consumer streamed something
func (s *GatewayStreamConsumer) adoptFinalText(finalRaw string) {
	if s.accumulated == "" && s.messageID == "" && s.lastSentText == "" {
		return
	}
	if s.adoptRecoveredReply(finalRaw) {
		return
	}
	if !s.turnSplitDelivery {
		finalPayload := cleanForDisplay(finalRaw)
		if finalPayload != "" && finalPayload != cleanForDisplay(s.accumulated) {
			s.accumulated = finalRaw
			s.streamLedger = finalRaw
		}
		return
	}
	ledger := s.streamLedger
	if ledger != "" && strings.HasPrefix(finalRaw, ledger) && len(finalRaw) > len(ledger) {
		s.accumulated += finalRaw[len(ledger):]
		s.streamLedger = finalRaw
	}
}

// Eager native re seed after a clarify answer, degrading to one buffered send when it fails
func (s *GatewayStreamConsumer) eagerReopenSeed(ctx context.Context) {
	if !s.reopenSeedPending() {
		return
	}
	if s.trySeedFrame(ctx, "Eager reopen seed raised, disabling native: %s", false) {
		s.nativeStreamOpened.Store(true)
		s.nativeLastPushedLen = 0
		s.awaitingReopenAfterBoundary.Store(false)
		s.reopenSeededEagerly = true
		streamLog().Info(fmt.Sprintf("[latency] Eager re-seed after clarify answer (typing bubble reopened immediately, turn=%s)", s.turnID))
		return
	}
	s.degradeNativeToBufferedSend()
}

// Whether this tick flushes an edit or frame
func (s *GatewayStreamConsumer) shouldEdit(tick *streamTick) bool {
	if !tick.isInterim() {
		return true
	}
	if s.cfg.BufferOnly {
		return false
	}
	var should bool
	if s.useNativeStreaming.Load() {
		should = s.accumulated != "" || s.toolProgressActive
	} else {
		elapsed := pythread.Monotonic() - s.lastEditTime
		should = (elapsed >= s.currentEditInterval && s.accumulated != "") ||
			(utf8.RuneCountInString(s.accumulated) >= s.cfg.BufferThreshold && s.floodStrikes == 0)
	}
	cleaned := cleanForDisplay(s.accumulated)
	return should && !IsPartialSilenceMarker(cleaned) && !EndsWithPartialSilenceMarker(s.accumulated) && !HasPendingMediaDirective(cleaned)
}

// Seals only the head chunks of an overflowing first send, true when the turn finished here
func (s *GatewayStreamConsumer) splitFirstSend(ctx context.Context, tick *streamTick) bool {
	chunks := s.truncateForStream(s.accumulated, s.safeLimit, s.lenFn)
	if len(chunks) <= 1 {
		chunks = splitTextChunks(s.accumulated, s.safeLimit, s.lenFn)
	}
	replyTo := s.initialReplyToID
	headsDelivered := len(chunks) > 1
	for _, chunk := range chunks[:len(chunks)-1] {
		newID := s.sendNewChunk(ctx, chunk, replyTo, tick.gotDone)
		if newID == "" || newID == replyTo {
			headsDelivered = false
			break
		}
		replyTo = newID
	}
	if headsDelivered {
		tail := chunks[len(chunks)-1]
		indicator := fmt.Sprintf(" (%d/%d)", len(chunks), len(chunks))
		s.accumulated = strings.TrimSuffix(tail, indicator)
		s.turnSplitDelivery = true
	}
	s.messageID = ""
	s.messageCreatedTS = nil
	s.lastSentText = ""
	s.lastEditTime = pythread.Monotonic()
	if tick.gotDone {
		tailDelivered := s.accumulated == "" || s.sendOrEdit(ctx, s.accumulated, true, true)
		s.finalResponseSent = headsDelivered && tailDelivered
		if s.finalResponseSent {
			s.turnSplitDelivery = true
			acc := s.accumulated
			s.markFinalDelivered(&acc)
		}
		return true
	}
	if tick.gotSegmentBreak {
		s.fallbackFinalSend = false
		s.fallbackPrefix = ""
	}
	return false
}

func (s *GatewayStreamConsumer) overflows() bool { return s.lenFn(s.accumulated) > s.safeLimit }

func (s *GatewayStreamConsumer) firstSendOverflows() bool { return s.messageID == "" && s.overflows() }

// Seals an overflowing existing message with its head and starts a new message for the rest
func (s *GatewayStreamConsumer) sealOverflowHeads(ctx context.Context) {
	for s.overflows() && s.messageID != "" && s.editSupported {
		runes := []rune(s.accumulated)
		cpBudget := customUnitToCP(runes, s.safeLimit, s.lenFn)
		splitAt := lastRuneIndex(runes[:cpBudget], '\n')
		if splitAt < cpBudget/2 {
			splitAt = cpBudget
		}
		chunk := string(runes[:splitAt])
		ok := s.sendOrEdit(ctx, chunk, true, false)
		if s.fallbackFinalSend || !ok {
			break
		}
		s.accumulated = strings.TrimLeft(string(runes[splitAt:]), "\n")
		s.messageID = ""
		s.lastSentText = ""
		s.turnSplitDelivery = true
	}
}

// Sends or edits this tick's visible text, cursor suffixed unless finalizing
func (s *GatewayStreamConsumer) pushUpdate(ctx context.Context, tick *streamTick) {
	displayText := s.accumulated
	if tick.isInterim() {
		if s.useNativeStreaming.Load() {
			displayText = s.composeFrameContent()
			if displayText != "" && s.cfg.Cursor != "" {
				displayText += s.cfg.Cursor
			}
		} else {
			displayText += s.cfg.Cursor
		}
	}
	tick.draftFinalFreshSend = tick.gotDone && s.useDraftStreaming && s.messageID == ""
	tick.updateVisible = s.sendOrEdit(ctx, displayText, tick.gotDone || tick.gotSegmentBreak, tick.gotDone)
	s.lastEditTime = pythread.Monotonic()
	s.toolProgressActive = false
}

// Final edit without cursor, or one continuation send when edits failed
func (s *GatewayStreamConsumer) finalizeTurn(ctx context.Context, tick *streamTick) {
	if s.accumulated != "" || s.messageID != "" || s.alreadySent {
		s.notifyBeforeFinalize(ctx)
	}
	switch {
	case s.reopenSeedPending() && s.accumulated == "":
		streamLog().Debug(fmt.Sprintf("Clarify reopen boundary with no post-prompt content — skipping lone-placeholder finalize (turn=%s)", s.turnID))
	case s.reopenSeededEagerly && s.nativeStreamOpened.Load() && s.accumulated == "" && !tick.updateVisible:
		s.closeEmptyNativeBubble(ctx, "Eager-seed empty finalize failed: %s")
		streamLog().Debug(fmt.Sprintf("Eager reopen seed but no post-answer content — closed empty typing bubble (turn=%s)", s.turnID))
	case s.useNativeStreaming.Load():
		if !tick.updateVisible {
			text := s.accumulated
			if text == "" {
				text = "✅"
			}
			s.finalizeEdit(ctx, text, false)
		} else {
			s.markFinalDelivered(nil)
		}
	case s.accumulated != "":
		s.finalizeEditPath(ctx, tick)
	}
	if s.finalContentDelivered && len(s.supersededReplyIDs) > 0 {
		s.deletePreviews(ctx, s.supersededReplyIDs, s.messageID, "Recovered reply", false, true)
		s.supersededReplyIDs = nil
	}
}

// Edit transport finalize, the non native done branches in priority order
func (s *GatewayStreamConsumer) finalizeEditPath(ctx context.Context, tick *streamTick) {
	switch {
	case s.fallbackFinalSend:
		s.sendFallbackFinal(ctx, s.accumulated)
	case s.finalResponseSent:
		acc := s.accumulated
		s.markFinalDelivered(&acc)
	case tick.updateVisible && (!s.adapterRequiresFinalize || s.lastEditOverflowed || tick.draftFinalFreshSend):
		s.markSkipRedundantFinalize()
	case s.messageID != "":
		if !s.finalizeEdit(ctx, s.accumulated, true) && s.fallbackFinalSend {
			s.sendFallbackFinal(ctx, s.accumulated)
		}
	case !s.alreadySent:
		s.finalizeEdit(ctx, s.accumulated, true)
	}
}

// Finalize send or edit, marking the turn delivered on success
func (s *GatewayStreamConsumer) finalizeEdit(ctx context.Context, text string, record bool) bool {
	s.finalResponseSent = s.sendOrEdit(ctx, text, true, true)
	if s.finalResponseSent {
		if record {
			s.markFinalDelivered(&text)
		} else {
			s.markFinalDelivered(nil)
		}
	}
	return s.finalResponseSent
}

// Stream is the message drafts and native streams, one append only stream per turn
func (s *GatewayStreamConsumer) cumulativeTransport() bool {
	return (s.streamIsMessage() && s.useDraftStreaming) || s.useNativeStreaming.Load()
}

// Posts commentary as its own message, cumulative transports keeping the stream going
func (s *GatewayStreamConsumer) deliverCommentary(ctx context.Context, commentaryText string) {
	cumulative := s.cumulativeTransport()
	if !cumulative {
		s.resetSegmentState(false)
	}
	s.sendCommentary(ctx, commentaryText)
	s.lastEditTime = pythread.Monotonic()
	if !cumulative {
		s.resetSegmentState(false)
	}
}

// Tool boundary, edit transports reset so the next chunk is a fresh message
func (s *GatewayStreamConsumer) endSegment(ctx context.Context, tick *streamTick) {
	if s.cumulativeTransport() {
		return
	}
	if s.accumulated != "" && !tick.updateVisible && s.messageID != "__no_edit__" {
		s.flushSegmentTailOnEditFailure(ctx)
	}
	s.resetSegmentState(true)
}

// Final edit attempt on cancellation, only a successful edit confirming delivery
func (s *GatewayStreamConsumer) onCancelled(ctx context.Context) {
	ok := false
	if s.accumulated != "" && s.messageID != "" {
		func() {
			defer func() { _ = recover() }()
			ok = s.sendOrEdit(ctx, s.accumulated, true, false)
		}()
	} else if s.messageID == "" {
		s.abandonNativeStream(ctx)
	}
	if ok && !s.finalResponseSent {
		acc := s.accumulated
		s.markFinalDelivered(&acc)
	}
}

// Wakes still queued flush waiters so a consumer dying mid flush never stalls them
func (s *GatewayStreamConsumer) wakeFlushWaiters() {
	for {
		item, ok := s.getNowait()
		if !ok {
			return
		}
		if item.kind == streamItemFlush {
			signalFlush(item.flush)
		}
	}
}

// Text with media directives and image tags hidden and a closing silence marker dropped, a strip failure raised
func cleanForDisplay(text string) string {
	stripped, err := StripMediaDirectivesForDisplay(context.Background(), text)
	if err != nil {
		panic(err)
	}
	return pyrepr.Str(StripTrailingSilenceMarker(StripImageTagsForDisplay(stripped)))
}
