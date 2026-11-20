package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Recorded adapter call of a stream consumer case
type streamCall struct {
	op        string
	content   string
	finalize  bool
	messageID string
}

// Adapter double recording sends and edits with scripted results
type streamRecorder struct {
	*BasePlatformAdapter
	mu       sync.Mutex
	calls    []streamCall
	sendFn   func(content string) (SendResult, error)
	editFn   func(content string, finalize bool) (SendResult, error)
	events   *[]string
	sendSeen int
}

func newStreamRecorder(requiresFinalize bool) *streamRecorder {
	a := &streamRecorder{}
	a.BasePlatformAdapter = NewBasePlatformAdapter(&PlatformConfig{}, Platform("telegram"), a)
	a.Traits.RequiresEditFinalize = requiresFinalize
	a.Traits.MaxMessageLength = 4096
	return a
}

func (a *streamRecorder) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *streamRecorder) Disconnect(context.Context) error            { return nil }
func (a *streamRecorder) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}

func (a *streamRecorder) Send(_ context.Context, _ string, content, _ string, _ map[string]any) (SendResult, error) {
	a.mu.Lock()
	a.calls = append(a.calls, streamCall{op: "send", content: content})
	a.sendSeen++
	if a.events != nil {
		*a.events = append(*a.events, "send")
	}
	a.mu.Unlock()
	if a.sendFn != nil {
		return a.sendFn(content)
	}
	return SendResult{Success: true, MessageID: "msg_1"}, nil
}

func (a *streamRecorder) EditMessage(_ context.Context, _ string, messageID string, content string, finalize bool) (SendResult, error) {
	a.mu.Lock()
	a.calls = append(a.calls, streamCall{op: "edit", content: content, finalize: finalize, messageID: messageID})
	if a.events != nil {
		*a.events = append(*a.events, "edit")
	}
	a.mu.Unlock()
	if a.editFn != nil {
		return a.editFn(content, finalize)
	}
	return SendResult{Success: true, MessageID: "msg_1"}, nil
}

func (a *streamRecorder) ops(op string) []streamCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []streamCall
	for _, c := range a.calls {
		if c.op == op {
			out = append(out, c)
		}
	}
	return out
}

func newTestConsumer(adapter PlatformAdapter, cfg *StreamConsumerConfig) *GatewayStreamConsumer {
	return NewGatewayStreamConsumer(adapter, "chat_123", StreamConsumerOptions{Config: cfg})
}

func TestNoMediaPassthrough(t *testing.T) {
	text := "Here is your analysis of the image."
	if got := cleanForDisplay(text); got != text {
		t.Fatalf("clean = %q", got)
	}
}

func TestMediaTagSingleQuotedStripped(t *testing.T) {
	if got := cleanForDisplay("Result: 'MEDIA:/path/file.png'"); strings.Contains(got, "MEDIA:") {
		t.Fatalf("clean = %q", got)
	}
}

func TestMediaTagDoubleQuotedJSONContextStaysVisible(t *testing.T) {
	if got := cleanForDisplay(`Result: "MEDIA:/path/file.png"`); !strings.Contains(got, `"MEDIA:/path/file.png"`) {
		t.Fatalf("clean = %q", got)
	}
}

func TestMediaTagInBackticksBogusPathStaysVisible(t *testing.T) {
	if got := cleanForDisplay("Result: `MEDIA:/path/file.png`"); !strings.Contains(got, "`MEDIA:/path/file.png`") {
		t.Fatalf("clean = %q", got)
	}
}

func TestAudioAsVoiceStripped(t *testing.T) {
	got := cleanForDisplay("[[audio_as_voice]]\nMEDIA:/tmp/voice.ogg")
	if strings.Contains(got, "[[audio_as_voice]]") || strings.Contains(got, "MEDIA:") {
		t.Fatalf("clean = %q", got)
	}
}

func TestIdenticalTextSkipRespectsAdapterFlag(t *testing.T) {
	ctx := context.Background()
	plain := newStreamRecorder(false)
	c1 := NewGatewayStreamConsumer(plain, "chat_1", StreamConsumerOptions{})
	c1.sendOrEdit(ctx, "hello", false, true)
	c1.sendOrEdit(ctx, "hello", true, true)
	if n := len(plain.ops("edit")); n != 0 {
		t.Fatalf("plain edits = %d", n)
	}
	picky := newStreamRecorder(true)
	c2 := NewGatewayStreamConsumer(picky, "chat_1", StreamConsumerOptions{})
	c2.sendOrEdit(ctx, "hello", false, true)
	c2.sendOrEdit(ctx, "hello", true, true)
	edits := picky.ops("edit")
	if len(edits) != 1 || !edits[0].finalize {
		t.Fatalf("picky edits = %+v", edits)
	}
}

func TestFirstSendStripsMedia(t *testing.T) {
	adapter := newStreamRecorder(false)
	consumer := newTestConsumer(adapter, nil)
	consumer.sendOrEdit(context.Background(), "Here is your image\nMEDIA:/tmp/test.png", false, true)
	sends := adapter.ops("send")
	if len(sends) != 1 || strings.Contains(sends[0].content, "MEDIA:") || !strings.Contains(sends[0].content, "Here is your image") {
		t.Fatalf("sends = %+v", sends)
	}
}

func TestEditStripsMedia(t *testing.T) {
	adapter := newStreamRecorder(false)
	consumer := newTestConsumer(adapter, nil)
	consumer.sendOrEdit(context.Background(), "Starting response...", false, true)
	consumer.sendOrEdit(context.Background(), "Here is the result\nMEDIA:/tmp/image.png", false, true)
	edits := adapter.ops("edit")
	if len(edits) != 1 || strings.Contains(edits[0].content, "MEDIA:") {
		t.Fatalf("edits = %+v", edits)
	}
}

func TestShortTextWithCursorSkipsNewMessage(t *testing.T) {
	adapter := newStreamRecorder(false)
	cfg := DefaultStreamConsumerConfig()
	cfg.Cursor = " ▉"
	consumer := newTestConsumer(adapter, cfg)
	if consumer.messageID != "" {
		t.Fatal("message id preset")
	}
	if !consumer.sendOrEdit(context.Background(), "I ▉", false, true) || len(adapter.ops("send")) != 0 {
		t.Fatal("short first send was not skipped")
	}
	if !consumer.sendOrEdit(context.Background(), "Hi! ▉", false, true) || len(adapter.ops("send")) != 0 {
		t.Fatal("three char first send was not skipped")
	}
}

func TestStreamWithMediaTag(t *testing.T) {
	adapter := newStreamRecorder(false)
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnDelta("Here is your generated image\n")
	consumer.OnDelta("MEDIA:/home/user/.icebath/cache/images/abc123.png")
	consumer.Finish(nil)
	consumer.Run(context.Background())
	for _, call := range adapter.calls {
		if strings.Contains(call.content, "MEDIA:") {
			t.Fatalf("MEDIA: leaked into display: %q", call.content)
		}
	}
	if !consumer.AlreadySent() {
		t.Fatal("already_sent not set")
	}
}

func TestHookRunsBeforeFinalizeEdit(t *testing.T) {
	var events []string
	adapter := newStreamRecorder(true)
	adapter.events = &events
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := NewGatewayStreamConsumer(adapter, "chat_123", StreamConsumerOptions{Config: cfg,
		OnBeforeFinalize: func(context.Context) { events = append(events, "pause") }})
	consumer.OnDelta("Hello")
	consumer.Finish(nil)
	consumer.Run(context.Background())
	if strings.Join(events, ",") != "send,pause,edit" {
		t.Fatalf("events = %v", events)
	}
}

func TestSegmentBreakRemovesCursor(t *testing.T) {
	adapter := newStreamRecorder(false)
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold, cfg.Cursor = 0.01, 5, " ▉"
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnDelta("Thinking...")
	consumer.OnDelta(nil)
	consumer.OnDelta("Done.")
	consumer.Finish(nil)
	consumer.Run(context.Background())
	var thinking []string
	for _, call := range append(adapter.ops("send"), adapter.ops("edit")...) {
		if strings.Contains(call.content, "Thinking") {
			thinking = append(thinking, call.content)
		}
	}
	if len(thinking) == 0 {
		t.Fatal("expected at least one message with Thinking")
	}
	if last := thinking[len(thinking)-1]; strings.Contains(last, "▉") {
		t.Fatalf("Cursor found in finalized segment: %q", last)
	}
}

func makeFilterConsumer() *GatewayStreamConsumer {
	return NewGatewayStreamConsumer(newStreamRecorder(false), "chat_test", StreamConsumerOptions{})
}

func TestPlainTextPassesThrough(t *testing.T) {
	c := makeFilterConsumer()
	c.filterAndAccumulate("Hello world")
	if c.accumulated != "Hello world" {
		t.Fatalf("accumulated = %q", c.accumulated)
	}
}

func TestCompleteThinkBlockStripped(t *testing.T) {
	c := makeFilterConsumer()
	c.filterAndAccumulate("<think>internal reasoning</think>Answer here")
	if c.accumulated != "Answer here" {
		t.Fatalf("accumulated = %q", c.accumulated)
	}
}

func TestOpeningTagSplitAcrossDeltas(t *testing.T) {
	c := makeFilterConsumer()
	c.filterAndAccumulate("<thi")
	if c.accumulated != "" {
		t.Fatalf("partial tag leaked: %q", c.accumulated)
	}
	c.filterAndAccumulate("nk>hidden</think>shown")
	if c.accumulated != "shown" {
		t.Fatalf("accumulated = %q", c.accumulated)
	}
}

func TestMultipleThinkBlocksWithTextBetween(t *testing.T) {
	c := makeFilterConsumer()
	c.filterAndAccumulate("<think>block1</think>A<think>block2</think>B")
	if !strings.Contains(c.accumulated, "A") || !strings.Contains(c.accumulated, "B") {
		t.Fatalf("accumulated = %q", c.accumulated)
	}
}

func TestReasoningTagsAreCaseInsensitive(t *testing.T) {
	for _, tag := range []string{"THINK", "Think", "ThInK", "THOUGHT", "REASONING", "Thinking"} {
		c := makeFilterConsumer()
		c.filterAndAccumulate("<" + tag + ">hidden reasoning</" + tag + ">Visible answer")
		if c.accumulated != "Visible answer" {
			t.Fatalf("%s: accumulated = %q", tag, c.accumulated)
		}
	}
}

func TestEveryScrubberTagIsFilteredWhenStreamed(t *testing.T) {
	for _, name := range provider.ThinkTagNames {
		c := makeFilterConsumer()
		for _, chunk := range []string{"<" + name + ">", "hidden", "</" + name + ">", "visible"} {
			c.filterAndAccumulate(chunk)
		}
		c.flushThinkBuffer()
		if c.accumulated != "visible" {
			t.Fatalf("%s: accumulated = %q", name, c.accumulated)
		}
	}
}

func TestProseMentionNotStripped(t *testing.T) {
	c := makeFilterConsumer()
	c.filterAndAccumulate("The <think> tag is used for reasoning")
	if !strings.Contains(c.accumulated, "<think>") || !strings.Contains(c.accumulated, "used for reasoning") {
		t.Fatalf("accumulated = %q", c.accumulated)
	}
}

func TestThinkWithOnlyWhitespaceBefore(t *testing.T) {
	c := makeFilterConsumer()
	c.filterAndAccumulate("  <think>hidden</think>visible")
	if c.accumulated != "  visible" {
		t.Fatalf("accumulated = %q", c.accumulated)
	}
}

func TestFlushThinkBufferOnNonTag(t *testing.T) {
	c := makeFilterConsumer()
	c.filterAndAccumulate("<thi")
	if c.accumulated != "" {
		t.Fatalf("accumulated = %q", c.accumulated)
	}
	c.flushThinkBuffer()
	if c.accumulated != "<thi" {
		t.Fatalf("accumulated = %q", c.accumulated)
	}
}

func TestAllCloseTagVariantsStripped(t *testing.T) {
	for _, tag := range []string{"</think>", "</thinking>", "</thought>", "</reasoning>", "</REASONING_SCRATCHPAD>", "</THINKING>"} {
		result := provider.StripOrphanThinkCloseTags("before" + tag + "after")
		if strings.Contains(result, tag) || !strings.Contains(result, "before") || !strings.Contains(result, "after") {
			t.Fatalf("%s: result = %q", tag, result)
		}
	}
}

func TestNoCloseTagPassthrough(t *testing.T) {
	text := "Just normal text with no tags."
	if got := provider.StripOrphanThinkCloseTags(text); got != text {
		t.Fatalf("result = %q", got)
	}
}

func TestFindsDeliveredSegmentAfterSegmentBreak(t *testing.T) {
	c := makeFilterConsumer()
	c.lastSentText = "Here is the first segment"
	c.resetSegmentState(false)
	if !c.HasDeliveredText("Here is the first segment") {
		t.Fatal("delivered segment not found after reset")
	}
}

// Send results handed out in order, the last one repeating
func sendSequence(results ...SendResult) func(string) (SendResult, error) {
	var mu sync.Mutex
	i := 0
	return func(string) (SendResult, error) {
		mu.Lock()
		defer mu.Unlock()
		r := results[min(i, len(results)-1)]
		i++
		return r, nil
	}
}

// Recorder that also deletes messages, recording each delete
type streamDeleter struct {
	*streamRecorder
	deleted [][2]string
}

func (a *streamDeleter) DeleteMessage(_ context.Context, chatID, messageID string) (bool, error) {
	a.mu.Lock()
	a.deleted = append(a.deleted, [2]string{chatID, messageID})
	a.mu.Unlock()
	return false, nil
}

func newStreamDeleter() *streamDeleter {
	a := &streamDeleter{streamRecorder: &streamRecorder{}}
	a.BasePlatformAdapter = NewBasePlatformAdapter(&PlatformConfig{}, Platform("telegram"), a)
	a.Traits.MaxMessageLength = 4096
	return a
}

// Starts the consumer run loop, the returned channel closing when it returns
func startConsumer(consumer *GatewayStreamConsumer) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.Run(context.Background())
	}()
	return done
}

func sentContents(a *streamRecorder) []string {
	var out []string
	for _, c := range a.ops("send") {
		out = append(out, c.content)
	}
	return out
}

func TestSegmentBreakClearsFailedEditFallbackState(t *testing.T) {
	adapter := newStreamRecorder(false)
	adapter.sendFn = sendSequence(SendResult{Success: true, MessageID: "msg_1"}, SendResult{Success: true, MessageID: "msg_2"}, SendResult{Success: true, MessageID: "msg_3"})
	adapter.editFn = func(string, bool) (SendResult, error) {
		return SendResult{Success: false, Error: "flood_control:6"}, nil
	}
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold, cfg.Cursor = 0.01, 5, " ▉"
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnDelta("Hello")
	done := startConsumer(consumer)
	time.Sleep(80 * time.Millisecond)
	consumer.OnDelta(" world")
	time.Sleep(80 * time.Millisecond)
	consumer.OnDelta(nil)
	consumer.OnDelta("Next segment")
	consumer.Finish(nil)
	<-done
	if got := sentContents(adapter); strings.Join(got, "|") != "Hello ▉|world|Next segment" {
		t.Fatalf("sent = %q", got)
	}
}

func TestSegmentBreakAfterMidStreamEditFailurePreservesTail(t *testing.T) {
	adapter := newStreamRecorder(false)
	adapter.sendFn = sendSequence(SendResult{Success: true, MessageID: "msg_1"}, SendResult{Success: true, MessageID: "msg_2"}, SendResult{Success: true, MessageID: "msg_3"})
	edits := sendSequence(SendResult{Success: true}, SendResult{Success: false, Error: "flood_control:6.0"})
	adapter.editFn = func(content string, _ bool) (SendResult, error) { return edits(content) }
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold, cfg.Cursor = 0.01, 5, " ▉"
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnDelta("Hello")
	done := startConsumer(consumer)
	time.Sleep(80 * time.Millisecond)
	consumer.OnDelta(" world")
	time.Sleep(80 * time.Millisecond)
	consumer.OnDelta(" more")
	time.Sleep(80 * time.Millisecond)
	consumer.OnDelta(nil)
	consumer.OnDelta("Here is the tool result.")
	consumer.Finish(nil)
	<-done
	all := strings.Join(sentContents(adapter), " ")
	if !strings.Contains(all, "more") || !strings.Contains(all, "Here is the tool result.") {
		t.Fatalf("sent = %q", sentContents(adapter))
	}
}

func TestFallbackFinalSendsFullTextAtToolBoundary(t *testing.T) {
	adapter := newStreamRecorder(false)
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	preTool := "I'll run that code now."
	consumer.OnDelta(preTool)
	done := startConsumer(consumer)
	time.Sleep(50 * time.Millisecond)
	consumer.Finish(nil)
	<-done
	consumer.lastSentText = preTool
	consumer.sendFallbackFinal(context.Background(), "⏰ Script timed out after 30s and was killed.")
	for _, content := range sentContents(adapter) {
		if strings.Contains(content, "timed out") {
			return
		}
	}
	t.Fatalf("post tool response dropped: %q", sentContents(adapter))
}

func TestFallbackFinalDeletesPartialAfterFullResend(t *testing.T) {
	adapter := newStreamDeleter()
	adapter.sendFn = sendSequence(SendResult{Success: true, MessageID: "msg_new"})
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	consumer.messageID = "msg_partial"
	consumer.lastSentText = "Let me check that for you…"
	consumer.sendFallbackFinal(context.Background(), "Working on it. Done!")
	if len(adapter.deleted) != 1 || adapter.deleted[0] != [2]string{"chat_123", "msg_partial"} {
		t.Fatalf("deleted = %v", adapter.deleted)
	}
	if !consumer.finalResponseSent {
		t.Fatal("final response not marked sent")
	}
}

func TestFallbackFinalKeepsPartialAfterTailOnlySend(t *testing.T) {
	adapter := newStreamDeleter()
	adapter.sendFn = sendSequence(SendResult{Success: true, MessageID: "msg_new"})
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	consumer.messageID = "msg_partial"
	consumer.lastSentText = "Working on i"
	consumer.sendFallbackFinal(context.Background(), "Working on it. Done!")
	tailOnly := false
	for _, content := range sentContents(adapter.streamRecorder) {
		if strings.Contains(content, "Done!") && !strings.Contains(content, "Working on i") {
			tailOnly = true
		}
	}
	if !tailOnly || len(adapter.deleted) != 0 || !consumer.finalResponseSent {
		t.Fatalf("sent = %q deleted = %v final = %v", sentContents(adapter.streamRecorder), adapter.deleted, consumer.finalResponseSent)
	}
}

// Recorder with the adapter truncator splitting at the limit into two chunks
type streamTruncRecorder struct {
	*streamRecorder
}

func (a *streamTruncRecorder) TruncateMessage(content string, maxLength int, _ func(string) int) []string {
	runes := []rune(content)
	cut := min(maxLength, len(runes))
	return []string{string(runes[:cut]), string(runes[cut:])}
}

func newStreamTruncRecorder(maxLength int) *streamTruncRecorder {
	a := &streamTruncRecorder{streamRecorder: &streamRecorder{}}
	a.BasePlatformAdapter = NewBasePlatformAdapter(&PlatformConfig{}, Platform("telegram"), a)
	a.Traits.MaxMessageLength = maxLength
	return a
}

func TestSplitOverflowFailedSendDoesNotMarkFinalSent(t *testing.T) {
	adapter := newStreamTruncRecorder(100)
	adapter.sendFn = sendSequence(SendResult{Success: false, Error: "network down"})
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	consumer.alreadySent = true
	consumer.OnDelta(strings.Repeat("x", 200))
	done := startConsumer(consumer)
	time.Sleep(50 * time.Millisecond)
	consumer.Finish(nil)
	<-done
	if consumer.finalResponseSent {
		t.Fatal("_already_sent leaked into _final_response_sent")
	}
}

func TestFailedHeadSendYieldsTheLoopInsteadOfSpinning(t *testing.T) {
	adapter := newStreamTruncRecorder(100)
	adapter.sendFn = sendSequence(SendResult{Success: false, Error: "network down"})
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnDelta(strings.Repeat("x", 1200))
	done := startConsumer(consumer)
	time.Sleep(300 * time.Millisecond)
	adapter.mu.Lock()
	passes := adapter.sendSeen
	adapter.mu.Unlock()
	consumer.Finish(nil)
	<-done
	if passes >= 50 {
		t.Fatalf("stream consumer spun %d head-send attempts in 0.3s without yielding", passes)
	}
}

func TestFailedFirstSendLeavesNoUneditablePartialPreview(t *testing.T) {
	adapter := newStreamRecorder(false)
	var delivered []string
	first := true
	adapter.sendFn = func(content string) (SendResult, error) {
		if first {
			first = false
			return SendResult{Success: false, Error: "timeout"}, nil
		}
		delivered = append(delivered, content)
		return SendResult{Success: true, MessageID: "m" + string(rune('0'+len(delivered)))}, nil
	}
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold, cfg.Cursor = 0.01, 5, ""
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnDelta("preview never landed ")
	done := startConsumer(consumer)
	time.Sleep(80 * time.Millisecond)
	consumer.OnDelta("and more streamed text ")
	time.Sleep(80 * time.Millisecond)
	consumer.OnDelta("then the end.")
	consumer.Finish(nil)
	<-done
	if strings.Join(delivered, "|") != "preview never landed and more streamed text then the end." {
		t.Fatalf("delivered = %q", delivered)
	}
}

func TestFallbackPartialSendDoesNotMarkFinalSent(t *testing.T) {
	adapter := newStreamRecorder(false)
	calls := 0
	adapter.sendFn = func(string) (SendResult, error) {
		calls++
		if calls <= 2 {
			return SendResult{Success: true, MessageID: "msg_1"}, nil
		}
		return SendResult{Success: false, Error: "flood_control:13.0"}, nil
	}
	adapter.editFn = func(string, bool) (SendResult, error) {
		return SendResult{Success: false, Error: "flood_control:13.0"}, nil
	}
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnDelta("Initial streaming text...\n")
	done := startConsumer(consumer)
	time.Sleep(50 * time.Millisecond)
	consumer.OnDelta(strings.Repeat("x", 3000) + "\n" + strings.Repeat("y", 3000) + "\nFinal answer.\n")
	time.Sleep(100 * time.Millisecond)
	consumer.Finish(nil)
	<-done
	if consumer.finalResponseSent {
		t.Fatal("partial fallback send set _final_response_sent")
	}
}

func TestConsumerAdvancesMessageIDOnSplitAndDeliver(t *testing.T) {
	adapter := newStreamRecorder(false)
	adapter.sendFn = sendSequence(SendResult{Success: true, MessageID: "msg_initial"})
	adapter.editFn = func(string, bool) (SendResult, error) {
		return SendResult{Success: true, MessageID: "msg_continuation_2", ContinuationMessageIDs: []any{"msg_continuation_1", "msg_continuation_2"}}, nil
	}
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold, cfg.Cursor = 0.01, 5, ""
	newMessages := 0
	consumer := NewGatewayStreamConsumer(adapter, "chat_999", StreamConsumerOptions{Config: cfg, OnNewMessage: func() { newMessages++ }})
	consumer.messageID, consumer.lastSentText, consumer.alreadySent = "msg_initial", "old", true
	if !consumer.sendOrEdit(context.Background(), "new full text after overflow", false, true) {
		t.Fatal("send or edit failed")
	}
	if consumer.messageID != "msg_continuation_2" || consumer.lastSentText != "" || newMessages != 1 {
		t.Fatalf("message id %q last sent %q new messages %d", consumer.messageID, consumer.lastSentText, newMessages)
	}
}

func TestCommentaryMessageStaysSeparateFromFinalStream(t *testing.T) {
	adapter := newStreamRecorder(false)
	adapter.sendFn = sendSequence(SendResult{Success: true, MessageID: "msg_1"}, SendResult{Success: true, MessageID: "msg_2"})
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnCommentary("I'll inspect the repository first.")
	consumer.OnDelta("Done.")
	consumer.Finish(nil)
	consumer.Run(context.Background())
	if got := sentContents(adapter); strings.Join(got, "|") != "I'll inspect the repository first.|Done." {
		t.Fatalf("sent = %q", got)
	}
	if !consumer.FinalResponseSent() {
		t.Fatal("final response not marked sent")
	}
}

func TestCancelledWithAlreadySentMarksFinalResponseSent(t *testing.T) {
	adapter := newStreamRecorder(false)
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnDelta("Hello world")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.Run(ctx)
	}()
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done
	if !consumer.AlreadySent() || !consumer.FinalResponseSent() {
		t.Fatalf("already sent %v final sent %v", consumer.AlreadySent(), consumer.FinalResponseSent())
	}
}

func TestThinkBlockNotSentToPlatform(t *testing.T) {
	adapter := newStreamRecorder(false)
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := NewGatewayStreamConsumer(adapter, "chat_test", StreamConsumerOptions{Config: cfg})
	consumer.OnDelta("<think>deep reasoning here</think>")
	consumer.OnDelta("The answer is 42.")
	consumer.Finish(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.Run(ctx)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	for _, call := range adapter.calls {
		if strings.Contains(call.content, "<think>") || strings.Contains(call.content, "deep reasoning") {
			t.Fatalf("Think tag leaked: %q", call.content)
		}
	}
}

func TestFlushesOnSegmentBreak(t *testing.T) {
	adapter := newStreamRecorder(false)
	adapter.sendFn = sendSequence(SendResult{Success: true, MessageID: "msg1"}, SendResult{Success: true, MessageID: "msg2"})
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold, cfg.Cursor, cfg.BufferOnly = 0.01, 5, "", true
	consumer := NewGatewayStreamConsumer(adapter, "!room:server", StreamConsumerOptions{Config: cfg})
	consumer.OnDelta("Before tool call")
	consumer.OnDelta(nil)
	consumer.OnDelta("After tool call")
	consumer.Finish(nil)
	consumer.Run(context.Background())
	sent := sentContents(adapter)
	if len(sent) != 2 || !strings.Contains(sent[0], "Before tool call") || !strings.Contains(sent[1], "After tool call") {
		t.Fatalf("sent = %q", sent)
	}
	if n := len(adapter.ops("edit")); n != 0 {
		t.Fatalf("edits = %d", n)
	}
}

func TestCursorStrippedWhenContinuationEmpty(t *testing.T) {
	adapter := newStreamRecorder(false)
	adapter.editFn = func(string, bool) (SendResult, error) { return SendResult{Success: true, MessageID: "msg-1"}, nil }
	cfg := DefaultStreamConsumerConfig()
	cfg.Cursor = " ▉"
	consumer := NewGatewayStreamConsumer(adapter, "chat-1", StreamConsumerOptions{Config: cfg})
	consumer.messageID, consumer.lastSentText, consumer.fallbackFinalSend = "msg-1", "Hello world ▉", false
	consumer.sendFallbackFinal(context.Background(), "Hello world")
	edits := adapter.ops("edit")
	if len(edits) != 1 || edits[0].content != "Hello world" {
		t.Fatalf("edits = %+v", edits)
	}
	if !consumer.alreadySent || consumer.lastSentText != "Hello world" {
		t.Fatalf("already sent %v last sent %q", consumer.alreadySent, consumer.lastSentText)
	}
}

func TestCallbackFiresOncePerSegment(t *testing.T) {
	adapter := newStreamRecorder(false)
	adapter.sendFn = sendSequence(SendResult{Success: true, MessageID: "msg_1"}, SendResult{Success: true, MessageID: "msg_2"}, SendResult{Success: true, MessageID: "msg_3"})
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 1
	var events []string
	consumer := NewGatewayStreamConsumer(adapter, "chat", StreamConsumerOptions{Config: cfg, OnNewMessage: func() { events = append(events, "reset") }})
	consumer.OnDelta("First preamble")
	consumer.OnDelta(nil)
	consumer.OnDelta("Second preamble")
	consumer.OnDelta(nil)
	consumer.OnDelta("Final answer")
	consumer.Finish(nil)
	consumer.Run(context.Background())
	if strings.Join(events, ",") != "reset,reset,reset" {
		t.Fatalf("events = %v", events)
	}
}

// Telegram like recorder measuring length in UTF-16 units
type utf16Recorder struct {
	*streamRecorder
}

func (a *utf16Recorder) MessageLenFn() func(string) int { return UTF16Len }

func TestEmojiTextExceedingUtf16LimitTriggersOverflowSplit(t *testing.T) {
	adapter := &utf16Recorder{streamRecorder: &streamRecorder{}}
	adapter.BasePlatformAdapter = NewBasePlatformAdapter(&PlatformConfig{}, Platform("telegram"), adapter)
	adapter.Traits.MaxMessageLength = 4096
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	emojiText := strings.Repeat("🚀", 2200)
	if len([]rune(emojiText)) >= 4096 || UTF16Len(emojiText) <= 4096 {
		t.Fatal("fixture invariant broken")
	}
	consumer.OnDelta(emojiText)
	done := startConsumer(consumer)
	time.Sleep(50 * time.Millisecond)
	consumer.Finish(nil)
	<-done
	sent := sentContents(adapter.streamRecorder)
	if len(sent) != 2 {
		t.Fatalf("UTF-16 overflow not detected, sent %d", len(sent))
	}
	for _, text := range sent {
		if UTF16Len(text) > 4096 {
			t.Fatalf("chunk exceeds limit: %d", UTF16Len(text))
		}
	}
}

func TestAdapterDeclineFreshFinalOverridesTimeThreshold(t *testing.T) {
	adapter := newStreamDeleter()
	adapter.sendFn = sendSequence(SendResult{Success: true, MessageID: "rich_msg"})
	adapter.editFn = func(string, bool) (SendResult, error) { return SendResult{Success: true, MessageID: "edit_msg"}, nil }
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold, cfg.FreshFinalAfterSeconds, cfg.Cursor = 0.01, 5, 0.001, " ▉"
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnDelta("Hello world")
	done := startConsumer(consumer)
	time.Sleep(50 * time.Millisecond)
	consumer.OnDelta(" final")
	consumer.Finish(nil)
	<-done
	if n := len(adapter.ops("send")); n != 1 {
		t.Fatalf("sends = %d", n)
	}
	finalized := 0
	for _, edit := range adapter.ops("edit") {
		if edit.finalize {
			finalized++
		}
	}
	if finalized < 1 {
		t.Fatal("expected a finalize edit")
	}
}

func TestAbandonsStreamAfterOneEditWhenSessionReset(t *testing.T) {
	adapter := newStreamRecorder(false)
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 3
	calls := 0
	consumer := NewGatewayStreamConsumer(adapter, "chat_123", StreamConsumerOptions{Config: cfg,
		RunStillCurrent: func() bool { calls++; return calls == 1 }})
	consumer.OnDelta("First segment")
	consumer.OnDelta(nil)
	consumer.OnDelta("Second segment text that will be stale")
	consumer.Run(context.Background())
	sent := sentContents(adapter)
	if len(sent) != 1 || !strings.Contains(sent[0], "First segment") || consumer.finalResponseSent {
		t.Fatalf("sent = %q final = %v", sent, consumer.finalResponseSent)
	}
}

func TestNormalDeliveryWhenSessionStaysCurrent(t *testing.T) {
	adapter := newStreamRecorder(false)
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := NewGatewayStreamConsumer(adapter, "chat_123", StreamConsumerOptions{Config: cfg, RunStillCurrent: func() bool { return true }})
	consumer.OnDelta("Hello, world!")
	consumer.Finish(nil)
	consumer.Run(context.Background())
	if len(adapter.ops("send")) < 1 || !consumer.finalResponseSent {
		t.Fatalf("sends %d final %v", len(adapter.ops("send")), consumer.finalResponseSent)
	}
}

func TestFlushDeliversBufferedCommentaryBeforeReturning(t *testing.T) {
	adapter := newStreamRecorder(false)
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnCommentary("Now I get the full picture. Here's the situation.")
	done := startConsumer(consumer)
	if !consumer.FlushPendingSync(3.0) {
		t.Fatal("flush_pending_sync did not complete within timeout")
	}
	delivered := false
	for _, call := range adapter.ops("send") {
		delivered = delivered || strings.Contains(call.content, "full picture")
	}
	if !delivered {
		t.Fatal("commentary not delivered before flush returned")
	}
	consumer.Finish(nil)
	<-done
}

// Recorder splitting into fixed size chunks and counting its splits
type chunkingRecorder struct {
	*streamRecorder
	splits int
	lenFn  func(string) int
	base   bool
}

func (a *chunkingRecorder) TruncateMessage(content string, maxLength int, lenFn func(string) int) []string {
	a.mu.Lock()
	a.splits++
	a.mu.Unlock()
	if a.base {
		return TruncateMessage(content, maxLength, lenFn)
	}
	runes := []rune(content)
	var out []string
	for i := 0; i < len(runes); i += maxLength {
		out = append(out, string(runes[i:min(i+maxLength, len(runes))]))
	}
	return out
}

func (a *chunkingRecorder) MessageLenFn() func(string) int {
	if a.lenFn != nil {
		return a.lenFn
	}
	return a.streamRecorder.MessageLenFn()
}

func newChunkingRecorder(maxLength int) *chunkingRecorder {
	a := &chunkingRecorder{streamRecorder: &streamRecorder{}}
	a.BasePlatformAdapter = NewBasePlatformAdapter(&PlatformConfig{}, Platform("telegram"), a)
	a.Traits.MaxMessageLength = maxLength
	return a
}

func TestFlushCompletesOnOversizedBufferedProse(t *testing.T) {
	adapter := newChunkingRecorder(4096)
	count := 0
	adapter.sendFn = func(string) (SendResult, error) {
		count++
		return SendResult{Success: true, MessageID: "m" + strings.Repeat("1", count)}, nil
	}
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	consumer.OnCommentary(strings.Repeat("X", 9000))
	done := startConsumer(consumer)
	if !consumer.FlushPendingSync(2.0) {
		t.Fatal("flush stalled")
	}
	total := 0
	for _, content := range sentContents(adapter.streamRecorder) {
		total += len([]rune(content))
	}
	if total < 9000 {
		t.Fatalf("delivered %d of 9000", total)
	}
	consumer.Finish(nil)
	<-done
}

func TestInitialOverflowUsesAdapterFenceAwareSplit(t *testing.T) {
	adapter := newChunkingRecorder(700)
	adapter.base, adapter.lenFn = true, UTF16Len
	adapter.sendFn = sendSequence(SendResult{Success: true, MessageID: "msg_1"}, SendResult{Success: true, MessageID: "msg_2"}, SendResult{Success: true, MessageID: "msg_3"})
	adapter.editFn = func(string, bool) (SendResult, error) { return SendResult{Success: true, MessageID: "msg_3"}, nil }
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold, cfg.Cursor = 0.01, 5, " ▉"
	consumer := NewGatewayStreamConsumer(adapter, "chat_fenced", StreamConsumerOptions{Config: cfg})
	fenced := "```python\n" + strings.Repeat("print('x')\n", 100) + "```"
	safeLimit := 700 - UTF16Len(cfg.Cursor) - 100
	expected := TruncateMessage(fenced, safeLimit, UTF16Len)
	consumer.OnDelta(fenced)
	done := startConsumer(consumer)
	time.Sleep(80 * time.Millisecond)
	consumer.OnDelta("\nTail after the fenced stream.")
	time.Sleep(80 * time.Millisecond)
	consumer.Finish(nil)
	<-done
	sent := sentContents(adapter.streamRecorder)
	var edited []string
	for _, e := range adapter.ops("edit") {
		edited = append(edited, e.content)
	}
	if adapter.splits < 1 {
		t.Fatal("adapter splitter never called")
	}
	for _, text := range append(append([]string(nil), sent...), edited...) {
		if strings.Count(text, "```")%2 != 0 {
			t.Fatalf("unbalanced fence: %q", text)
		}
	}
	if len(sent) != len(expected) || strings.Join(sent[:len(sent)-1], "|") != strings.Join(expected[:len(expected)-1], "|") {
		t.Fatalf("sent %d chunks, expected %d", len(sent), len(expected))
	}
	indicator := fmt.Sprintf(" (%d/%d)", len(expected), len(expected))
	last := expected[len(expected)-1]
	if !strings.HasSuffix(last, indicator) || !strings.HasPrefix(sent[len(sent)-1], strings.TrimSuffix(last, indicator)) {
		t.Fatalf("tail %q", sent[len(sent)-1])
	}
	tailEdited := false
	for _, text := range edited {
		if strings.Contains(text, indicator) {
			t.Fatalf("indicator leaked into edit: %q", text)
		}
		tailEdited = tailEdited || strings.Contains(text, "Tail after the fenced stream.")
	}
	if !tailEdited {
		t.Fatal("tail never edited in")
	}
	for _, text := range sent {
		if UTF16Len(text) > safeLimit {
			t.Fatalf("chunk over limit: %d", UTF16Len(text))
		}
	}
}

func TestMidStreamEditSuccessDoesNotMarkContentDelivered(t *testing.T) {
	adapter := newStreamRecorder(true)
	adapter.editFn = func(string, bool) (SendResult, error) { return SendResult{Success: false}, nil }
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 5
	consumer := newTestConsumer(adapter, cfg)
	sends := 0
	adapter.sendFn = func(string) (SendResult, error) {
		sends++
		if sends == 1 {
			consumer.cfg.BufferThreshold = 10_000
			consumer.currentEditInterval = 10.0
			return SendResult{Success: true, MessageID: "msg_1"}, nil
		}
		return SendResult{Success: false, Error: "network down"}, nil
	}
	consumer.OnDelta("Part one of the response...\n")
	done := startConsumer(consumer)
	time.Sleep(50 * time.Millisecond)
	consumer.OnDelta("Part two, the complete final answer.\n")
	time.Sleep(50 * time.Millisecond)
	consumer.Finish(nil)
	<-done
	if consumer.finalContentDelivered || consumer.finalResponseSent {
		t.Fatalf("content delivered %v final sent %v", consumer.finalContentDelivered, consumer.finalResponseSent)
	}
}
