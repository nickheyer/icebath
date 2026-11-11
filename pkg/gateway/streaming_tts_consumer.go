package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tools"
)

func sttsLog() *slog.Logger { return slog.Default().With("logger", "gateway.streaming_tts_consumer") }

// Queue sentinels of the drain loop, the abort and end of text markers
type sttsSentinel int

const (
	sttsAbort sttsSentinel = iota + 1
	sttsDone
)

// Raised when the adapter declined to open its streaming audio handle at the first PCM chunk
var errSTTSHandleDeclined = errors.New("streaming TTS handle declined")

// Drain task of one consumer with its completion channel, the asyncio task of hermes
type STTSDrainTask struct {
	done chan struct{}
}

// Whether the drain task has ended
func (t *STTSDrainTask) Done() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Consumes LLM text deltas and writes streaming PCM audio to an adapter as the reply generates
type StreamingTTSConsumer struct {
	adapter     PlatformAdapter
	chatID      string
	ctx         context.Context
	metadata    map[string]any
	streamer    media.StreamingTTSProvider
	chunkMu     sync.Mutex
	chunker     *media.SentenceChunker
	queue       chan any
	stateMu     sync.Mutex
	audioFormat AudioFormat
	handle      *StreamingTTSHandle
	task        *STTSDrainTask
	completed   bool
	partial     bool
	aborted     bool
	finished    bool
	dropped     bool
	suppress    bool
	lock        sync.Mutex
}

// Consumer resolving the configured chunked streamer, inactive when the provider has no streaming API
func NewStreamingTTSConsumer(ctx context.Context, env *tools.Env, adapter PlatformAdapter, chatID string, ttsConfig map[string]any, metadata map[string]any, audioFormat *AudioFormat) (*StreamingTTSConsumer, error) {
	streamer, err := media.ResolveStreamingProvider(ctx, env, ttsConfig, "")
	if err != nil {
		return nil, err
	}
	return newStreamingTTSConsumerWith(ctx, adapter, chatID, ttsConfig, metadata, audioFormat, streamer), nil
}

// Consumer over an already resolved streamer with its chunker taken from tts.streaming
func newStreamingTTSConsumerWith(ctx context.Context, adapter PlatformAdapter, chatID string, ttsConfig map[string]any, metadata map[string]any, audioFormat *AudioFormat, streamer media.StreamingTTSProvider) *StreamingTTSConsumer {
	c := &StreamingTTSConsumer{adapter: adapter, chatID: chatID, ctx: ctx, metadata: metadata, streamer: streamer,
		chunker: media.SentenceChunkerFromConfig(ttsConfig), queue: make(chan any, 256)}
	switch {
	case streamer != nil:
		c.audioFormat = c.streamerFormat()
	case audioFormat != nil:
		c.audioFormat = *audioFormat
	default:
		c.audioFormat = DefaultAudioFormat()
	}
	return c
}

func (c *StreamingTTSConsumer) streamerFormat() AudioFormat {
	return AudioFormat{SampleRate: c.streamer.SampleRate(), Channels: c.streamer.Channels(), SampleWidth: c.streamer.SampleWidth()}
}

// Whether a usable streaming provider was resolved
func (c *StreamingTTSConsumer) Active() bool { return c.streamer != nil }

// Whether the streaming audio was fully delivered
func (c *StreamingTTSConsumer) Completed() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.completed
}

// Whether some audio was audible before a failure or a dropped clause
func (c *StreamingTTSConsumer) Partial() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.partial
}

// Whether PCM reached the adapter handle
func (c *StreamingTTSConsumer) Audible() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.handle != nil && c.handle.Audible
}

// Whether queue saturation dropped at least one clause
func (c *StreamingTTSConsumer) Dropped() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.dropped
}

// Whether the gateway skips the whole file TTS reply
func (c *StreamingTTSConsumer) SuppressWholeFile() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.suppress
}

// Whether the drain task was started and has ended
func (c *StreamingTTSConsumer) Done() bool {
	c.stateMu.Lock()
	task := c.task
	c.stateMu.Unlock()
	return task != nil && task.Done()
}

// Audio format the handle opens with, refreshed from the streamer at the first chunk
func (c *StreamingTTSConsumer) AudioFormat() AudioFormat {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.audioFormat
}

func (c *StreamingTTSConsumer) isAborted() bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.aborted
}

func (c *StreamingTTSConsumer) isFinished() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.finished
}

func (c *StreamingTTSConsumer) enqueueClauses(clauses []string, fullMsg string) {
	for _, clause := range clauses {
		select {
		case c.queue <- clause:
		default:
			c.stateMu.Lock()
			c.dropped = true
			c.stateMu.Unlock()
			sttsLog().Debug(fullMsg)
			return
		}
	}
}

// Feeds text to the chunker or flushes a segment boundary for nil, never blocking
func (c *StreamingTTSConsumer) OnDelta(text *string) {
	if c.isAborted() || !c.Active() || c.isFinished() {
		return
	}
	c.chunkMu.Lock()
	var clauses []string
	if text == nil {
		clauses = c.chunker.Flush()
	} else {
		clauses = c.chunker.Feed(*text)
	}
	c.chunkMu.Unlock()
	c.enqueueClauses(clauses, "streaming TTS queue full, dropping clause")
}

// Flushes the chunker tail and enqueues the end of text sentinel after every flushed clause
func (c *StreamingTTSConsumer) Finish() {
	c.stateMu.Lock()
	if c.finished {
		c.stateMu.Unlock()
		return
	}
	c.finished = true
	c.stateMu.Unlock()
	if c.isAborted() || !c.Active() {
		return
	}
	c.chunkMu.Lock()
	tail := c.chunker.Flush()
	c.chunkMu.Unlock()
	c.enqueueClauses(tail, "streaming TTS queue full while flushing tail")
	for !c.putSentinel(sttsDone, true) {
	}
}

// Enqueues a sentinel evicting one queued item when full, true when the caller stops retrying
func (c *StreamingTTSConsumer) putSentinel(sentinel sttsSentinel, markDropped bool) bool {
	select {
	case c.queue <- sentinel:
		return true
	default:
	}
	select {
	case <-c.queue:
	default:
		return !markDropped
	}
	c.stateMu.Lock()
	c.dropped = c.dropped || markDropped
	c.stateMu.Unlock()
	return false
}

// Starts the drain task once and returns it
func (c *StreamingTTSConsumer) Start() *STTSDrainTask {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.task == nil {
		task := &STTSDrainTask{done: make(chan struct{})}
		c.task = task
		go func() {
			defer close(task.done)
			c.run()
		}()
	}
	return c.task
}

// Sets the outcome flags from what was audible, never completed after a failure or a dropped clause
func (c *StreamingTTSConsumer) settle(failed bool) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	audible := c.handle != nil && c.handle.Audible
	degraded := failed || c.dropped
	c.completed = audible && !degraded
	c.partial = c.partial || (audible && degraded)
	c.suppress = audible
}

// Opens the adapter streaming handle at the streamer format, false when begin failed
func (c *StreamingTTSConsumer) openHandle() bool {
	c.stateMu.Lock()
	if c.streamer != nil {
		c.audioFormat = c.streamerFormat()
	}
	format := c.audioFormat
	c.stateMu.Unlock()
	handle, err := c.adapter.BeginStreamingTTS(c.ctx, c.chatID, format, c.metadata)
	if err != nil {
		sttsLog().Debug("begin_streaming_tts failed: " + err.Error())
		handle = nil
	}
	c.stateMu.Lock()
	c.handle = handle
	c.stateMu.Unlock()
	return handle != nil
}

func (c *StreamingTTSConsumer) currentHandle() *StreamingTTSHandle {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.handle
}

func (c *StreamingTTSConsumer) handleAborted() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.handle != nil && c.handle.Aborted
}

// Drains clauses until a sentinel or abort, synthesises each and finalises the adapter stream
func (c *StreamingTTSConsumer) run() {
	if !c.Active() || !c.adapter.SupportsStreamingTTS(c.chatID, c.AudioFormat()) {
		sttsLog().Debug("adapter " + c.adapter.Name() + " does not support streaming TTS")
		return
	}
	defer func() {
		for {
			select {
			case <-c.queue:
			default:
				return
			}
		}
	}()
	c.stateMu.Lock()
	c.suppress = false
	c.stateMu.Unlock()
	for !c.isAborted() {
		var item any
		select {
		case item = <-c.queue:
		case <-time.After(100 * time.Millisecond):
			continue
		}
		if item == sttsAbort || item == sttsDone || c.isAborted() {
			break
		}
		clause, ok := item.(string)
		if !ok {
			continue
		}
		if err := c.synthesiseAndWrite(clause); err != nil {
			if errors.Is(err, errSTTSHandleDeclined) {
				return
			}
			sttsLog().Warn("streaming TTS clause failed: " + err.Error())
			c.settle(true)
			c.safeAbort(err.Error())
			return
		}
	}
	handle := c.currentHandle()
	if c.isAborted() || handle == nil {
		return
	}
	if err := c.adapter.FinishStreamingTTS(c.ctx, handle, c.isAborted()); err != nil {
		sttsLog().Debug("finish_streaming_tts error: " + err.Error())
		c.settle(true)
		c.safeAbort("finish_streaming_tts failed")
		return
	}
	c.settle(false)
}

// Synthesises one clause through the streamer and writes its PCM chunks to the handle
func (c *StreamingTTSConsumer) synthesiseAndWrite(clause string) error {
	if c.streamer == nil || c.handleAborted() {
		return nil
	}
	cleaned := strings.TrimSpace(tools.StripMarkdownForTTS(clause))
	if cleaned == "" {
		return nil
	}
	chunks, err := c.streamer.Stream(c.ctx, cleaned)
	if err != nil {
		return err
	}
	for chunk, err := range chunks {
		if err != nil {
			return err
		}
		if c.isAborted() || c.handleAborted() {
			return nil
		}
		if len(chunk) == 0 {
			continue
		}
		if c.currentHandle() == nil && !c.openHandle() {
			return errSTTSHandleDeclined
		}
		handle := c.currentHandle()
		c.stateMu.Lock()
		wasAudible := handle.Audible
		c.stateMu.Unlock()
		if err := c.adapter.WriteStreamingTTS(c.ctx, handle, chunk); err != nil {
			return err
		}
		if !wasAudible {
			c.stateMu.Lock()
			handle.Audible = true
			c.suppress = true
			c.stateMu.Unlock()
		}
	}
	return nil
}

// Aborts the adapter stream swallowing its error and marks the handle aborted
func (c *StreamingTTSConsumer) safeAbort(reason string) {
	handle := c.currentHandle()
	if handle == nil {
		return
	}
	_ = c.adapter.AbortStreamingTTS(c.ctx, handle, &reason)
	c.stateMu.Lock()
	handle.Aborted = true
	c.stateMu.Unlock()
}

// Cancels the stream once from any goroutine and aborts an open adapter handle in the background
func (c *StreamingTTSConsumer) Abort(reason string) {
	c.lock.Lock()
	if c.aborted {
		c.lock.Unlock()
		return
	}
	c.aborted = true
	c.lock.Unlock()
	enqueued := false
	for range 3 {
		if c.putSentinel(sttsAbort, false) {
			enqueued = true
			break
		}
	}
	if !enqueued {
		sttsLog().Debug("streaming TTS _ABORT sentinel could not be enqueued")
	}
	if handle := c.currentHandle(); handle != nil && !c.handleAborted() {
		go c.safeAbort(reason)
	}
}

// Waits up to timeout seconds for the drain task without cancelling it, true only on full success
func (c *StreamingTTSConsumer) WaitComplete(timeout float64) bool {
	c.stateMu.Lock()
	task := c.task
	c.stateMu.Unlock()
	if task != nil {
		select {
		case <-task.done:
		case <-time.After(time.Duration(timeout * float64(time.Second))):
		}
	}
	return c.Completed()
}
