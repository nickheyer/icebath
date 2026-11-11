package gateway

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync"
	"testing"
)

// test_streaming_tts_consumer.py FakeStreamer
type fakeTTSStreamer struct {
	mu              sync.Mutex
	chunksPerClause int
	failOnClause    int
	rate, ch, width int
	clauseCount     int
}

func newFakeTTSStreamer(chunks int) *fakeTTSStreamer {
	return &fakeTTSStreamer{chunksPerClause: chunks, rate: 24000, ch: 1, width: 2}
}

func (s *fakeTTSStreamer) SampleRate() int  { return s.rate }
func (s *fakeTTSStreamer) Channels() int    { return s.ch }
func (s *fakeTTSStreamer) SampleWidth() int { return s.width }

func (s *fakeTTSStreamer) Stream(ctx context.Context, text string) (iter.Seq2[[]byte, error], error) {
	s.mu.Lock()
	s.clauseCount++
	count := s.clauseCount
	s.mu.Unlock()
	if s.failOnClause != 0 && count >= s.failOnClause {
		return nil, fmt.Errorf("fake streamer failure on clause %d", count)
	}
	return func(yield func([]byte, error) bool) {
		for i := range s.chunksPerClause {
			if !yield([]byte(fmt.Sprintf("chunk-%d-%d", count, i)), nil) {
				return
			}
		}
	}, nil
}

// test_streaming_tts_consumer.py FakeVoiceAdapter
type fakeVoiceAdapter struct {
	*BasePlatformAdapter
	mu             sync.Mutex
	supports       bool
	failAfterWrite bool
	handle         *StreamingTTSHandle
	writtenChunks  [][]byte
	beginCount     int
	finishCount    int
	abortCount     int
}

func newFakeVoiceAdapter() *fakeVoiceAdapter {
	a := &fakeVoiceAdapter{supports: true}
	a.BasePlatformAdapter = NewBasePlatformAdapter(NewPlatformConfig(), PlatformTelegram, a)
	return a
}

func (a *fakeVoiceAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *fakeVoiceAdapter) Disconnect(context.Context) error            { return nil }
func (a *fakeVoiceAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (a *fakeVoiceAdapter) Send(context.Context, string, string, string, map[string]any) (SendResult, error) {
	return SendResult{Success: true}, nil
}

func (a *fakeVoiceAdapter) SupportsStreamingTTS(chatID string, format AudioFormat) bool {
	return a.supports
}

func (a *fakeVoiceAdapter) BeginStreamingTTS(ctx context.Context, chatID string, format AudioFormat, metadata map[string]any) (*StreamingTTSHandle, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.beginCount++
	if !a.supports {
		return nil, nil
	}
	a.handle = &StreamingTTSHandle{ChatID: chatID, AudioFormat: format}
	return a.handle, nil
}

func (a *fakeVoiceAdapter) WriteStreamingTTS(ctx context.Context, handle *StreamingTTSHandle, chunk []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failAfterWrite && len(a.writtenChunks) >= 2 {
		return errors.New("adapter write failure after partial output")
	}
	a.writtenChunks = append(a.writtenChunks, chunk)
	return nil
}

func (a *fakeVoiceAdapter) FinishStreamingTTS(ctx context.Context, handle *StreamingTTSHandle, interrupted bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.finishCount++
	return nil
}

func (a *fakeVoiceAdapter) AbortStreamingTTS(ctx context.Context, handle *StreamingTTSHandle, errText *string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.abortCount++
	return nil
}

func ttsText(s string) *string { return &s }

// test_streaming_tts_consumer.py TestConsumerLifecycle test_successful_stream_produces_ordered_chunks
func TestSuccessfulStreamProducesOrderedChunks(t *testing.T) {
	adapter := newFakeVoiceAdapter()
	consumer := newStreamingTTSConsumerWith(t.Context(), adapter, "chat1", map[string]any{}, nil, nil, newFakeTTSStreamer(2))
	consumer.Start()
	consumer.OnDelta(ttsText("This is the first sentence. "))
	consumer.OnDelta(ttsText("Here is the second one. "))
	consumer.Finish()
	if !consumer.WaitComplete(5.0) {
		t.Fatal("stream did not complete")
	}
	if adapter.beginCount != 1 || adapter.finishCount != 1 || adapter.abortCount != 0 {
		t.Fatalf("lifecycle counts begin=%d finish=%d abort=%d", adapter.beginCount, adapter.finishCount, adapter.abortCount)
	}
	want := [][]byte{[]byte("chunk-1-0"), []byte("chunk-1-1"), []byte("chunk-2-0"), []byte("chunk-2-1")}
	if !slices.EqualFunc(adapter.writtenChunks, want, slices.Equal) {
		t.Fatalf("written chunks %q", adapter.writtenChunks)
	}
}

// test_streaming_tts_consumer.py TestStreamerFormatAndLooping test_audio_format_tracks_resolved_streamer
func TestAudioFormatTracksResolvedStreamer(t *testing.T) {
	streamer := &fakeTTSStreamer{chunksPerClause: 1, rate: 48000, ch: 2, width: 4}
	consumer := newStreamingTTSConsumerWith(t.Context(), newFakeVoiceAdapter(), "chat1", map[string]any{}, nil, nil, streamer)
	if got := consumer.AudioFormat(); got != (AudioFormat{SampleRate: 48000, Channels: 2, SampleWidth: 4}) {
		t.Fatalf("audio format %+v", got)
	}
}

// test_streaming_tts_consumer.py TestStreamerFormatAndLooping test_chunker_min_len_comes_from_tts_streaming_config
func TestChunkerMinLenComesFromTtsStreamingConfig(t *testing.T) {
	consumer := newStreamingTTSConsumerWith(t.Context(), newFakeVoiceAdapter(), "chat1",
		map[string]any{"streaming": map[string]any{"min_len": 6}}, nil, nil, nil)
	if consumer.chunker.MinLen != 6 {
		t.Fatalf("min_len %d", consumer.chunker.MinLen)
	}
}

// test_streaming_tts_consumer.py TestAbortAndCancellation test_abort_is_idempotent
func TestAbortIsIdempotent(t *testing.T) {
	adapter := newFakeVoiceAdapter()
	consumer := newStreamingTTSConsumerWith(t.Context(), adapter, "chat1", map[string]any{}, nil, nil, newFakeTTSStreamer(10))
	consumer.Start()
	consumer.OnDelta(ttsText("A sentence. "))
	consumer.Abort("test")
	consumer.Abort("test2")
	consumer.Abort("test3")
	consumer.Finish()
	consumer.WaitComplete(5.0)
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.abortCount > 1 {
		t.Fatalf("abort count %d", adapter.abortCount)
	}
}

// test_streaming_tts_consumer.py TestFallbackSafety test_pre_audio_failure_falls_back
func TestPreAudioFailureFallsBack(t *testing.T) {
	adapter := newFakeVoiceAdapter()
	streamer := newFakeTTSStreamer(3)
	streamer.failOnClause = 1
	consumer := newStreamingTTSConsumerWith(t.Context(), adapter, "chat1", map[string]any{}, nil, nil, streamer)
	consumer.Start()
	consumer.OnDelta(ttsText("A sentence. "))
	consumer.Finish()
	if consumer.WaitComplete(5.0) {
		t.Fatal("a failed stream reported completion")
	}
	if consumer.SuppressWholeFile() || adapter.beginCount != 0 {
		t.Fatalf("suppress=%v begin=%d", consumer.SuppressWholeFile(), adapter.beginCount)
	}
}

// test_streaming_tts_consumer.py TestConcurrentTurnIsolation test_two_concurrent_turns_do_not_cross_contaminate
func TestTwoConcurrentTurnsDoNotCrossContaminate(t *testing.T) {
	adapter1, adapter2 := newFakeVoiceAdapter(), newFakeVoiceAdapter()
	streamer := newFakeTTSStreamer(2)
	c1 := newStreamingTTSConsumerWith(t.Context(), adapter1, "chat1", map[string]any{}, nil, nil, streamer)
	c2 := newStreamingTTSConsumerWith(t.Context(), adapter2, "chat2", map[string]any{}, nil, nil, streamer)
	c1.Start()
	c2.Start()
	c1.OnDelta(ttsText("Sentence for chat one. "))
	c2.OnDelta(ttsText("Sentence for chat two. "))
	c1.Finish()
	c2.Finish()
	c1.WaitComplete(5.0)
	c2.WaitComplete(5.0)
	if slices.EqualFunc(adapter1.writtenChunks, adapter2.writtenChunks, slices.Equal) {
		t.Fatal("both chats received the same chunks")
	}
	if !c1.Completed() || !c2.Completed() {
		t.Fatalf("completed c1=%v c2=%v", c1.Completed(), c2.Completed())
	}
}
