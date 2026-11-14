package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

type speakerTestQueue struct {
	mu    sync.Mutex
	items []any
}

func (q *speakerTestQueue) Get(timeout time.Duration) (any, bool) {
	q.mu.Lock()
	if len(q.items) > 0 {
		item := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		return item, true
	}
	q.mu.Unlock()
	time.Sleep(timeout)
	return nil, false
}

// Queue holding the items followed by the end of text entry
func drainQueue(items ...any) *speakerTestQueue {
	return &speakerTestQueue{items: append(items, nil)}
}

type speakerTestEvent struct{ pythread.Event }

func (e *speakerTestEvent) Wait(timeout time.Duration) bool { return e.Event.Wait(timeout.Seconds()) }

type fakeStreamer struct {
	rate   atomic.Int64
	stream func(s *fakeStreamer, text string) iter.Seq2[[]byte, error]
}

func newFakeStreamer(stream func(s *fakeStreamer, text string) iter.Seq2[[]byte, error]) *fakeStreamer {
	s := &fakeStreamer{stream: stream}
	s.rate.Store(24000)
	return s
}

func (s *fakeStreamer) SampleRate() int  { return int(s.rate.Load()) }
func (s *fakeStreamer) Channels() int    { return 1 }
func (s *fakeStreamer) SampleWidth() int { return 2 }
func (s *fakeStreamer) Stream(_ context.Context, text string) (iter.Seq2[[]byte, error], error) {
	return s.stream(s, text), nil
}

// Generator body running first then yielding the chunks, like a Python generator function
func pcmGenerator(first func(), chunks ...[]byte) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		if first != nil {
			first()
		}
		for _, c := range chunks {
			if !yield(c, nil) {
				return
			}
		}
	}
}

type speakerOut struct {
	*fakeStream
	wmu      sync.Mutex
	writes   [][]int16
	writeErr error
	onWrite  func()
}

func (o *speakerOut) Write(data []int16) (bool, error) {
	o.wmu.Lock()
	o.writes = append(o.writes, append([]int16(nil), data...))
	o.wmu.Unlock()
	if o.onWrite != nil {
		o.onWrite()
	}
	return false, o.writeErr
}
func (o *speakerOut) WriteAvailable() (int, error) { return 0, nil }
func (o *speakerOut) writeCount() int {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	return len(o.writes)
}

func newSpeakerOut(writeErr error) *speakerOut {
	return &speakerOut{fakeStream: &fakeStream{}, writeErr: writeErr}
}

type speakerSD struct {
	*fakeSD
	omu    sync.Mutex
	opened []pysounddevice.StreamParams
	next   func() pysounddevice.OutputStream
}

func (f *speakerSD) OutputStream(p pysounddevice.StreamParams) (pysounddevice.OutputStream, error) {
	f.omu.Lock()
	defer f.omu.Unlock()
	f.opened = append(f.opened, p)
	return f.next(), nil
}

// Installs a fake sounddevice whose every output stream is the returned one
func sdMock(t *testing.T, writeErr error) (*speakerSD, *speakerOut) {
	out := newSpeakerOut(writeErr)
	sd := &speakerSD{fakeSD: &fakeSD{}, next: func() pysounddevice.OutputStream { return out }}
	installSpeakerSD(t, sd)
	return sd, out
}

func installSpeakerSD(t *testing.T, sd *speakerSD) {
	swap(t, &ttsImportSounddevice, func() (pysounddevice.Module, error) { return sd, nil })
}

func patchResolveStreamer(t *testing.T, s StreamingTTSProvider) {
	swap(t, &speakerResolveStreamingProvider, func(context.Context, *tools.Env, map[string]any, string) (StreamingTTSProvider, error) {
		return s, nil
	})
}

func skipOnDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS deliberately skips the sounddevice OutputStream path (PR #62601)")
	}
}

func speakerHome(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_DISABLE_LAZY_INSTALLS", "1")
}

func pcm(pattern string, n int) []byte { return bytes.Repeat([]byte(pattern), n) }

func TestStreamerPathHandlesMisalignedPcmChunks(t *testing.T) {
	skipOnDarwin(t)
	speakerHome(t)
	provider := newFakeStreamer(func(*fakeStreamer, string) iter.Seq2[[]byte, error] {
		return pcmGenerator(nil, []byte("\x01\x00\x02"), []byte("\x00\x03\x00\x04"), []byte("\x00\x05\x00"))
	})
	_, out := sdMock(t, nil)
	patchResolveStreamer(t, provider)
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	StreamTTSToSpeaker(context.Background(), drainQueue("A complete sentence for testing."), stop, done, nil, "")
	if out.writeCount() == 0 {
		t.Fatal("expected PCM chunks written despite odd byte counts")
	}
	var written []byte
	for _, w := range out.writes {
		for _, sample := range w {
			written = binary.LittleEndian.AppendUint16(written, uint16(sample))
		}
	}
	if len(written) != 10 || string(written) != "\x01\x00\x02\x00\x03\x00\x04\x00\x05\x00" {
		t.Fatalf("expected 10 bytes of PCM data, got %d (%q), misaligned chunks were likely dropped", len(written), written)
	}
	if !done.IsSet() {
		t.Fatal("done not set")
	}
}

func TestStreamerPathSurvivesPortaudioWriteError(t *testing.T) {
	skipOnDarwin(t)
	speakerHome(t)
	provider := newFakeStreamer(func(*fakeStreamer, string) iter.Seq2[[]byte, error] {
		return pcmGenerator(nil, pcm("\x01\x00", 50), pcm("\x02\x00", 50))
	})
	_, out := sdMock(t, errors.New("Internal PortAudio error [PaErrorCode -9986]"))
	patchResolveStreamer(t, provider)
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	StreamTTSToSpeaker(context.Background(), drainQueue("A complete sentence for testing."), stop, done, nil, "")
	if out.writeCount() == 0 {
		t.Fatal("expected at least one write attempt")
	}
	if !done.IsSet() {
		t.Fatal("done event must fire even after PortAudio error")
	}
}

func TestStreamerReinitAfterPortaudioErrorPlaysRemainingSentences(t *testing.T) {
	skipOnDarwin(t)
	speakerHome(t)
	provider := newFakeStreamer(func(*fakeStreamer, string) iter.Seq2[[]byte, error] {
		return pcmGenerator(nil, pcm("\x01\x00", 50), pcm("\x02\x00", 50))
	})
	brokenOut := newSpeakerOut(errors.New("Internal PortAudio error [PaErrorCode -9986]"))
	freshOut := newSpeakerOut(nil)
	pool := []*speakerOut{brokenOut, freshOut}
	sd := &speakerSD{fakeSD: &fakeSD{}}
	sd.next = func() pysounddevice.OutputStream {
		if len(pool) == 0 {
			return newSpeakerOut(nil)
		}
		out := pool[0]
		pool = pool[1:]
		return out
	}
	installSpeakerSD(t, sd)
	patchResolveStreamer(t, provider)
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	q := drainQueue("First sentence triggers PortAudio error here. ", "Second sentence must still play after reinit. ")
	StreamTTSToSpeaker(context.Background(), q, stop, done, nil, "")
	if brokenOut.writeCount() == 0 {
		t.Fatal("first stream should have received a write")
	}
	if freshOut.writeCount() == 0 {
		t.Fatal("second (reinit) stream should have received writes for the remaining sentence")
	}
	if !done.IsSet() {
		t.Fatal("done event must fire after recovery")
	}
}

func TestStreamerTempfileFallbackAfterReinitExhausted(t *testing.T) {
	skipOnDarwin(t)
	speakerHome(t)
	provider := newFakeStreamer(func(*fakeStreamer, string) iter.Seq2[[]byte, error] {
		return pcmGenerator(nil, pcm("\x01\x00", 50))
	})
	sd, _ := sdMock(t, errors.New("Internal PortAudio error [PaErrorCode -9986]"))
	var mu sync.Mutex
	var playCalls []string
	swap(t, &speakerPlayAudioFile, func(path string) bool {
		mu.Lock()
		playCalls = append(playCalls, path)
		mu.Unlock()
		return true
	})
	patchResolveStreamer(t, provider)
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	q := drainQueue("First sentence triggers PortAudio error. ", "Second sentence fails after first reinit. ",
		"Third sentence fails after second reinit. ", "Fourth sentence fails after third reinit. ", "Fifth sentence plays via tempfile fallback. ")
	StreamTTSToSpeaker(context.Background(), q, stop, done, nil, "")
	if len(sd.opened) != 4 {
		t.Fatalf("expected 4 OutputStream calls (initial + 3 reinits), got %d", len(sd.opened))
	}
	if !done.IsSet() {
		t.Fatal("done event must fire even after reinit exhaustion")
	}
	if len(playCalls) < 1 {
		t.Fatal("tempfile fallback should have been invoked for remaining sentences after reinit exhaustion")
	}
}

// Streamer recording the text of every stream call in generator order
func trackingStreamer(calls *[]string, mu *sync.Mutex, chunks ...[]byte) *fakeStreamer {
	return newFakeStreamer(func(_ *fakeStreamer, text string) iter.Seq2[[]byte, error] {
		return pcmGenerator(func() {
			mu.Lock()
			*calls = append(*calls, text)
			mu.Unlock()
		}, chunks...)
	})
}

func TestSpeakerHonoursTtsStreamingMinLenForShortCjkOpener(t *testing.T) {
	skipOnDarwin(t)
	var mu sync.Mutex
	var streamCalls []string
	sdMock(t, nil)
	patchResolveStreamer(t, trackingStreamer(&streamCalls, &mu, pcm("\x00\x00", 10)))
	fixedTTSConfig(t, map[string]any{"streaming": map[string]any{"min_len": 6}})
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	StreamTTSToSpeaker(context.Background(), drainQueue("记得，叫团团. ", "然后我们再说第二句话，这一句要长一些才行. "), stop, done, nil, "")
	if len(streamCalls) == 0 || streamCalls[0] != "记得，叫团团." {
		t.Fatalf("stream calls = %q", streamCalls)
	}
	if !done.IsSet() {
		t.Fatal("done not set")
	}
}

func TestHybridSubsequentSentencesPrefetchedIndividually(t *testing.T) {
	skipOnDarwin(t)
	speakerHome(t)
	var mu sync.Mutex
	var streamCalls []string
	sdMock(t, nil)
	patchResolveStreamer(t, trackingStreamer(&streamCalls, &mu, pcm("\x00\x00", 10)))
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	q := drainQueue("This is the very first sentence here. ", "This is the second complete sentence. ",
		"This is the third complete sentence. ", "This is the fourth complete sentence. ")
	StreamTTSToSpeaker(context.Background(), q, stop, done, nil, "")
	if len(streamCalls) != 4 {
		t.Fatalf("expected 4 stream() calls (1 per sentence), got %d: %q", len(streamCalls), streamCalls)
	}
	for i, want := range []string{"first sentence", "second", "third", "fourth"} {
		if !strings.Contains(strings.ToLower(streamCalls[i]), want) {
			t.Fatalf("stream call %d = %q, want it to contain %q", i, streamCalls[i], want)
		}
	}
	if !done.IsSet() {
		t.Fatal("done not set")
	}
}

func TestHybridDoneEventWaitsForPrefetch(t *testing.T) {
	skipOnDarwin(t)
	speakerHome(t)
	prefetchDone := &pythread.Event{}
	provider := newFakeStreamer(func(*fakeStreamer, string) iter.Seq2[[]byte, error] {
		return func(yield func([]byte, error) bool) {
			if !yield(pcm("\x00\x00", 10), nil) {
				return
			}
			time.Sleep(300 * time.Millisecond)
			prefetchDone.Set()
		}
	})
	sdMock(t, nil)
	patchResolveStreamer(t, provider)
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	q := drainQueue("This is the first sentence here. ", "This is the second sentence here. ", "This is the third sentence here. ")
	StreamTTSToSpeaker(context.Background(), q, stop, done, nil, "")
	if !done.IsSet() {
		t.Fatal("done not set")
	}
	if !prefetchDone.IsSet() {
		t.Fatal("done event fired before the prefetch thread finished, this would cause audio overlap in continuous voice mode")
	}
}

func TestHybridSingleSentenceStillWorks(t *testing.T) {
	skipOnDarwin(t)
	speakerHome(t)
	var mu sync.Mutex
	var streamCalls []string
	sdMock(t, nil)
	patchResolveStreamer(t, trackingStreamer(&streamCalls, &mu, pcm("\x00\x00", 10)))
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	StreamTTSToSpeaker(context.Background(), drainQueue("Just one complete sentence."), stop, done, nil, "")
	if len(streamCalls) != 1 {
		t.Fatalf("single sentence should trigger exactly 1 stream() call, got %q", streamCalls)
	}
	if !done.IsSet() {
		t.Fatal("done not set")
	}
}

func TestHybridPlaybackSerializedNoOverlap(t *testing.T) {
	skipOnDarwin(t)
	speakerHome(t)
	var active, maxConcurrent atomic.Int64
	provider := newFakeStreamer(func(*fakeStreamer, string) iter.Seq2[[]byte, error] {
		chunk := pcm("\x00\x00", 20)
		return pcmGenerator(nil, chunk, chunk, chunk, chunk, chunk)
	})
	_, out := sdMock(t, nil)
	out.onWrite = func() {
		n := active.Add(1)
		for {
			m := maxConcurrent.Load()
			if n <= m || maxConcurrent.CompareAndSwap(m, n) {
				break
			}
		}
		active.Add(-1)
	}
	patchResolveStreamer(t, provider)
	var sentences []any
	for i := range 10 {
		sentences = append(sentences, "This is sentence number "+string(rune('0'+i))+" here. ")
	}
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	StreamTTSToSpeaker(context.Background(), drainQueue(sentences...), stop, done, nil, "")
	if !done.IsSet() {
		t.Fatal("done not set")
	}
	if maxConcurrent.Load() > 1 {
		t.Fatalf("playback threads overlapped: max concurrent writes = %d", maxConcurrent.Load())
	}
}

type syncRunEvent struct {
	kind, text string
	start, end float64
}

type syncRunOptions struct {
	synthFailOn    string
	stopAfterPlays int
}

// Drives the speaker over the sync path with timed synth and play fakes, returning their ordered events
func timedSyncRun(t *testing.T, sentences []any, o syncRunOptions) ([]syncRunEvent, *speakerTestEvent, *speakerTestEvent) {
	t.Helper()
	speakerHome(t)
	origin := time.Now()
	var mu sync.Mutex
	var events []syncRunEvent
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	swap(t, &speakerTextToSpeechTool, func(_ context.Context, text, outputPath string) (string, error) {
		t0 := time.Since(origin).Seconds()
		if o.synthFailOn != "" && strings.Contains(text, o.synthFailOn) {
			return "", errors.New("synth exploded")
		}
		time.Sleep(120 * time.Millisecond)
		if err := os.WriteFile(outputPath, bytes.Repeat([]byte("x"), 100), 0o600); err != nil {
			return "", err
		}
		mu.Lock()
		events = append(events, syncRunEvent{"synth", text, t0, time.Since(origin).Seconds()})
		mu.Unlock()
		return "", nil
	})
	swap(t, &speakerPlayAudioFile, func(path string) bool {
		t0 := time.Since(origin).Seconds()
		time.Sleep(120 * time.Millisecond)
		mu.Lock()
		events = append(events, syncRunEvent{"play", path, t0, time.Since(origin).Seconds()})
		plays := 0
		for _, e := range events {
			if e.kind == "play" {
				plays++
			}
		}
		mu.Unlock()
		if o.stopAfterPlays != 0 && plays >= o.stopAfterPlays {
			stop.Set()
		}
		return true
	})
	patchResolveStreamer(t, nil)
	StreamTTSToSpeaker(context.Background(), drainQueue(sentences...), stop, done, nil, "")
	mu.Lock()
	defer mu.Unlock()
	return append([]syncRunEvent(nil), events...), stop, done
}

func eventsOfKind(events []syncRunEvent, kind string) []syncRunEvent {
	var out []syncRunEvent
	for _, e := range events {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestSyncPipelineOverlapsSynthesisWithPlayback(t *testing.T) {
	events, _, done := timedSyncRun(t, []any{"First full sentence here. ", "Second full sentence here. ", "Third full sentence here. "}, syncRunOptions{})
	synths, plays := eventsOfKind(events, "synth"), eventsOfKind(events, "play")
	if len(synths) != 3 || len(plays) != 3 {
		t.Fatalf("synths = %d, plays = %d", len(synths), len(plays))
	}
	if !done.IsSet() {
		t.Fatal("done not set")
	}
	if synths[1].start >= plays[0].end {
		t.Fatalf("no overlap: synth2 started at %.3f, play1 ended at %.3f", synths[1].start, plays[0].end)
	}
}

func TestSyncPipelinePreservesOrderAndIsolatesFailures(t *testing.T) {
	events, _, done := timedSyncRun(t, []any{"Alpha sentence spoken first. ", "Bravo sentence explodes here. ", "Charlie sentence still plays. "},
		syncRunOptions{synthFailOn: "Bravo"})
	var firstWords []string
	for _, e := range eventsOfKind(events, "synth") {
		firstWords = append(firstWords, strings.Fields(e.text)[0])
	}
	if strings.Join(firstWords, ",") != "Alpha,Charlie" {
		t.Fatalf("synths = %q", firstWords)
	}
	if n := len(eventsOfKind(events, "play")); n != 2 {
		t.Fatalf("plays = %d", n)
	}
	if !done.IsSet() {
		t.Fatal("done not set")
	}
}

func TestSyncPipelineStopSkipsQueuedPlayback(t *testing.T) {
	events, stop, done := timedSyncRun(t, []any{"First full sentence here. ", "Second full sentence here. ", "Third full sentence here. ", "Fourth full sentence here. "},
		syncRunOptions{stopAfterPlays: 1})
	if n := len(eventsOfKind(events, "play")); n != 1 {
		t.Fatalf("stop after first play must skip the rest, got %d", n)
	}
	if !stop.IsSet() || !done.IsSet() {
		t.Fatal("stop and done must both be set")
	}
}

func TestSyncPipelineCleansTempFiles(t *testing.T) {
	var mu sync.Mutex
	var created []string
	realMkstemp := speakerMkstemp
	swap(t, &speakerMkstemp, func(suffix string) (string, error) {
		path, err := realMkstemp(suffix)
		mu.Lock()
		created = append(created, path)
		mu.Unlock()
		return path, err
	})
	events, _, _ := timedSyncRun(t, []any{"First full sentence here. ", "Second full sentence here. "}, syncRunOptions{})
	if n := len(eventsOfKind(events, "play")); n != 2 {
		t.Fatalf("plays = %d", n)
	}
	if len(created) == 0 {
		t.Fatal("expected temp files to be created via mkstemp")
	}
	for _, p := range created {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("temp file not cleaned: %s", p)
		}
	}
}

func TestOpenaiStreamerHonorsEndpointReportedRateInWavPlayback(t *testing.T) {
	patchOpenAISpeech(t, map[string]string{"content-type": "audio/pcm", "x-audio-sample-rate": "44100"}, nil)
	streamer, err := newOpenAIStreamer(nil, map[string]any{}, map[string]any{"api_key": "sk-x", "pcm_sample_rate": "22050"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var wavRates []int
	swap(t, &speakerDeviceUsable, func() bool { return false })
	swap(t, &speakerPlayAudioFile, func(path string) bool {
		_, rate, err := readWAVFrames(path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
		}
		mu.Lock()
		wavRates = append(wavRates, rate)
		mu.Unlock()
		return true
	})
	playback := newStreamerPlayback(context.Background(), streamer, &speakerTestEvent{})
	playback.speak("One sentence.")
	playback.finish()
	if streamer.SampleRate() != 44100 {
		t.Fatalf("sample rate = %d", streamer.SampleRate())
	}
	if len(wavRates) != 1 || wavRates[0] != 44100 {
		t.Fatalf("wav rates = %v", wavRates)
	}
}

func TestSpeakerOutputStreamOpensAtRateLearnedFromFirstChunk(t *testing.T) {
	skipOnDarwin(t)
	speakerHome(t)
	provider := newFakeStreamer(func(s *fakeStreamer, _ string) iter.Seq2[[]byte, error] {
		return pcmGenerator(func() { s.rate.Store(44100) }, pcm("\x01\x00", 50))
	})
	sd, out := sdMock(t, nil)
	patchResolveStreamer(t, provider)
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	q := drainQueue("The first sentence is long enough. ", "The second sentence is long enough too. ")
	StreamTTSToSpeaker(context.Background(), q, stop, done, nil, "")
	if !done.IsSet() {
		t.Fatal("done not set")
	}
	if len(sd.opened) != 1 || sd.opened[0].Samplerate != 44100 {
		t.Fatalf("opened = %+v", sd.opened)
	}
	if out.writeCount() != 2 {
		t.Fatalf("writes = %d", out.writeCount())
	}
}

func TestSyncPipelinePlaysTheArtifactTheToolReported(t *testing.T) {
	speakerHome(t)
	ogg := filepath.Join(t.TempDir(), "sentence.ogg")
	if err := os.WriteFile(ogg, bytes.Repeat([]byte("x"), 32), 0o600); err != nil {
		t.Fatal(err)
	}
	swap(t, &speakerTextToSpeechTool, func(context.Context, string, string) (string, error) {
		return pyjson.Dumps(map[string]any{"success": true, "file_path": ogg, "file_paths": []any{ogg}})
	})
	var mu sync.Mutex
	var played []string
	swap(t, &speakerPlayAudioFile, func(path string) bool {
		mu.Lock()
		played = append(played, path)
		mu.Unlock()
		return true
	})
	patchResolveStreamer(t, nil)
	StreamTTSToSpeaker(context.Background(), drainQueue("Hello there. "), &speakerTestEvent{}, &speakerTestEvent{}, nil, "")
	if len(played) != 1 || played[0] != ogg {
		t.Fatalf("sentence dropped: playback ignored the reported artifact, played = %q", played)
	}
}

func TestSyncPipelineFallsBackToRequestedPathWhenReportedMissing(t *testing.T) {
	speakerHome(t)
	swap(t, &speakerTextToSpeechTool, func(_ context.Context, _, outputPath string) (string, error) {
		if err := os.WriteFile(outputPath, bytes.Repeat([]byte("x"), 100), 0o600); err != nil {
			return "", err
		}
		return pyjson.Dumps(map[string]any{"success": false, "error": "shape without paths"})
	})
	var mu sync.Mutex
	var sizes []int64
	swap(t, &speakerPlayAudioFile, func(path string) bool {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat %s: %v", path, err)
			return false
		}
		mu.Lock()
		sizes = append(sizes, info.Size())
		mu.Unlock()
		return true
	})
	patchResolveStreamer(t, nil)
	StreamTTSToSpeaker(context.Background(), drainQueue("Hello there. "), &speakerTestEvent{}, &speakerTestEvent{}, nil, "")
	if len(sizes) != 1 || sizes[0] <= 0 {
		t.Fatalf("requested-path fallback no longer plays, sizes = %v", sizes)
	}
}

// Drives the speaker once with an empty sentence list, reporting whether sounddevice was imported
func runStreamForSounddeviceProbe(t *testing.T) bool {
	t.Helper()
	fixedTTSConfig(t, map[string]any{})
	patchResolveStreamer(t, newFakeStreamer(func(*fakeStreamer, string) iter.Seq2[[]byte, error] { return pcmGenerator(nil) }))
	var hit atomic.Bool
	swap(t, &ttsImportSounddevice, func() (pysounddevice.Module, error) {
		hit.Store(true)
		return nil, &pysounddevice.LibraryError{Msg: "no audio device in test"}
	})
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	StreamTTSToSpeaker(context.Background(), drainQueue(), stop, done, nil, "")
	if !done.IsSet() {
		t.Fatal("done not set")
	}
	return hit.Load()
}

func TestStreamingTtsSkipsSounddeviceOnMacos(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS output policy")
	}
	if runStreamForSounddeviceProbe(t) {
		t.Fatal("sounddevice imported on macOS")
	}
}

func TestStreamingTtsUsesSounddeviceOffMacos(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux output policy")
	}
	if !runStreamForSounddeviceProbe(t) {
		t.Fatal("sounddevice not imported off macOS")
	}
}

func TestStreamTtsToSpeakerNoneSentinelFlushesBuffer(t *testing.T) {
	speakerHome(t)
	var mu sync.Mutex
	var spoken []string
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	StreamTTSToSpeaker(context.Background(), drainQueue("Hello world."), stop, done, func(text string) {
		mu.Lock()
		spoken = append(spoken, text)
		mu.Unlock()
	}, "")
	if !done.IsSet() {
		t.Fatal("done not set")
	}
	if !strings.Contains(strings.Join(spoken, "\n"), "Hello") {
		t.Fatalf("spoken = %q", spoken)
	}
}

func TestStreamTtsToSpeakerStopEventAbortsEarly(t *testing.T) {
	speakerHome(t)
	var spoken []string
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	stop.Set()
	StreamTTSToSpeaker(context.Background(), drainQueue("Should not be spoken."), stop, done, func(text string) { spoken = append(spoken, text) }, "")
	if !done.IsSet() {
		t.Fatal("done not set")
	}
	if len(spoken) != 0 {
		t.Fatalf("spoken = %q", spoken)
	}
}

func TestStreamTtsToSpeakerDoneEventSetOnException(t *testing.T) {
	speakerHome(t)
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	StreamTTSToSpeaker(context.Background(), drainQueue(12345), stop, done, nil, "")
	if !done.IsSet() {
		t.Fatal("done not set")
	}
}

type trackedTempFile struct {
	speakerTempFile
	onClose func(name string)
}

func (f *trackedTempFile) Close() error {
	f.onClose(f.Name())
	return f.speakerTempFile.Close()
}

func TestStreamTtsTempfileFallbackTempfileHandleClosedBeforePlayback(t *testing.T) {
	speakerHome(t)
	chunk := pcm("\x00\x00", 240)
	patchResolveStreamer(t, newFakeStreamer(func(*fakeStreamer, string) iter.Seq2[[]byte, error] { return pcmGenerator(nil, chunk, chunk) }))
	swap(t, &ttsImportSounddevice, func() (pysounddevice.Module, error) {
		return nil, &ttsImportError{msg: "sounddevice unavailable in test"}
	})
	type event struct{ kind, path string }
	var mu sync.Mutex
	var events []event
	var played []string
	realTemp := speakerNamedTempFile
	swap(t, &speakerNamedTempFile, func(suffix string) (speakerTempFile, error) {
		f, err := realTemp(suffix)
		if err != nil {
			return nil, err
		}
		return &trackedTempFile{speakerTempFile: f, onClose: func(name string) {
			mu.Lock()
			events = append(events, event{"close", name})
			mu.Unlock()
		}}, nil
	})
	swap(t, &speakerPlayAudioFile, func(path string) bool {
		mu.Lock()
		events = append(events, event{"play", path})
		played = append(played, path)
		mu.Unlock()
		frames, _, err := readWAVFrames(path)
		if err != nil || len(frames) == 0 {
			t.Errorf("at play time the file must be a fully written WAV, frames = %d, err = %v", len(frames), err)
		}
		return true
	})
	stop, done := &speakerTestEvent{}, &speakerTestEvent{}
	StreamTTSToSpeaker(context.Background(), drainQueue("This is a spoken sentence for the fallback. "), stop, done, nil, "")
	if !done.IsSet() {
		t.Fatal("done not set")
	}
	if len(played) == 0 {
		t.Fatal("temp-file fallback player was never invoked")
	}
	closedBeforePlay := false
	for _, e := range events {
		if e.kind == "play" {
			break
		}
		if e.kind == "close" {
			closedBeforePlay = true
		}
	}
	if !closedBeforePlay {
		t.Fatal("temp WAV handle must be closed BEFORE play_audio_file")
	}
	if _, err := os.Stat(played[0]); err == nil {
		t.Fatal("temp WAV was not unlinked")
	}
}

func TestAlignInt16ChunksCarriesOddBytesAndPadsTheTail(t *testing.T) {
	chunks := [][]byte{[]byte("\x01\x00\x02"), []byte("\x00\x03")}
	collect := func(stop Event, padTail bool) string {
		var parts []string
		for c := range alignInt16Chunks(func(yield func([]byte) bool) {
			for _, c := range chunks {
				if !yield(c) {
					return
				}
			}
		}, stop, padTail) {
			parts = append(parts, string(c))
		}
		return strings.Join(parts, "|")
	}
	if got := collect(&speakerTestEvent{}, true); got != "\x01\x00|\x02\x00|\x00" {
		t.Fatalf("padded = %q", got)
	}
	if got := collect(&speakerTestEvent{}, false); got != "\x01\x00|\x02\x00" {
		t.Fatalf("unpadded = %q", got)
	}
	stopped := &speakerTestEvent{}
	stopped.Set()
	if got := collect(stopped, true); got != "" {
		t.Fatalf("stopped = %q", got)
	}
}

func TestFirstWrittenArtifactPrefersTheReportedFileWithContent(t *testing.T) {
	dir := t.TempDir()
	real, empty := filepath.Join(dir, "a.ogg"), filepath.Join(dir, "empty.ogg")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dump := func(v any) string {
		s, err := pyjson.Dumps(v)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	cases := []struct {
		raw  any
		want string
	}{
		{dump(map[string]any{"file_paths": []any{empty, 7, real}}), real},
		{dump(map[string]any{"file_path": real}), real},
		{dump(map[string]any{"file_paths": real}), "req"},
		{dump(map[string]any{"file_paths": []any{}, "file_path": empty}), "req"},
		{map[string]any{"file_paths": []string{real}}, real},
		{"not json", "req"},
		{dump([]any{real}), "req"},
		{nil, "req"},
		{dir, "req"},
	}
	for i, c := range cases {
		if got := firstWrittenArtifact(c.raw, "req"); got != c.want {
			t.Fatalf("case %d: got %q, want %q", i, got, c.want)
		}
	}
}

func TestWriteWaveFileMatchesTheWaveModuleLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	chunks := func(yield func([]byte) bool) { _ = yield([]byte("\x01\x00\x02\x00")) && yield([]byte("\x00")) }
	if err := writeWaveFile(f, chunks, 22050); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "RIFF\x29\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x22\x56\x00\x00\x44\xac\x00\x00\x02\x00\x10\x00data\x05\x00\x00\x00\x01\x00\x02\x00\x00"
	if string(got) != want {
		t.Fatalf("wav = %q", got)
	}
	if err := writeWaveFile(f, chunks, 0); err == nil || err.Error() != "bad frame rate" {
		t.Fatalf("err = %v", err)
	}
	if _, err := wavePCMHeader(1<<31, 0); err == nil || err.Error() != "'L' format requires 0 <= number <= 4294967295" {
		t.Fatalf("err = %v", err)
	}
}
