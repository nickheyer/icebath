package media

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"math"
	"os"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Queue of text deltas the speaker drains, a nil item ending the text
type TextQueue interface {
	Get(timeout time.Duration) (any, bool)
}

// Threading event surface of the stop and done signals
type Event interface {
	Set()
	Clear()
	IsSet() bool
	Wait(timeout time.Duration) bool
}

const (
	speakerMaxReinit      = 3
	speakerChunkQueueMax  = 64
	speakerPrefetchLimit  = 3
	speakerChunkPutWait   = 30 * time.Second
	speakerWorkerJoinWait = 300 * time.Second
	speakerPrefetchJoin   = 10 * time.Second
)

// Temp file the WAV fallback writes, closed before the system player opens it
type speakerTempFile interface {
	io.WriteSeeker
	Name() string
	Close() error
}

var (
	speakerPlayAudioFile            = PlayAudioFile
	speakerTextToSpeechTool         = TextToSpeechTool
	speakerResolveStreamingProvider = ResolveStreamingProvider
	ttsImportSounddevice            = pysounddevice.Import
	speakerNamedTempFile            = func(suffix string) (speakerTempFile, error) { return speakerCreateTemp(suffix) }
	speakerMkstemp                  = func(suffix string) (string, error) {
		f, err := speakerCreateTemp(suffix)
		if err != nil {
			return "", err
		}
		return f.Name(), f.Close()
	}
	speakerDeviceUsable = func() bool {
		if voicePlatformSystem() == "Darwin" {
			return false
		}
		if _, err := ttsImportSounddevice(); err != nil {
			ttsLog().Debug(fmt.Sprintf("sounddevice not available, streamer→tempfile: %s", err))
			return false
		}
		return true
	}
)

// New file named tmp, random text and the suffix inside the Python temp directory
func speakerCreateTemp(suffix string) (*os.File, error) {
	dir, err := pypath.Gettempdir()
	if err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, "tmp*"+suffix)
}

// Starts a goroutine and returns once it runs, the channel closing when it ends
func startThread(fn func()) chan struct{} {
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		fn()
	}()
	<-started
	return done
}

// Waits for a thread to end, giving up after the timeout
func joinThread(done <-chan struct{}, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// Yields int16 aligned byte chunks, a dangling odd byte answered by one zero byte when padTail is set
func alignInt16Chunks(chunks iter.Seq[[]byte], stop Event, padTail bool) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		var leftover []byte
		for chunk := range chunks {
			if stop.IsSet() {
				break
			}
			buf := append(slices.Clone(leftover), chunk...)
			aligned := len(buf) - len(buf)%2
			if aligned >= 2 && !yield(buf[:aligned]) {
				return
			}
			leftover = buf[aligned:]
		}
		if len(leftover) > 0 && padTail {
			yield([]byte{0})
		}
	}
}

// WAV header the wave module writes for 16 bit mono PCM at a frame rate and data length
func wavePCMHeader(rate, dataLen int) ([]byte, error) {
	if rate > math.MaxUint32/2 || dataLen > math.MaxUint32-36 {
		return nil, &ttsRuntimeError{msg: "'L' format requires 0 <= number <= 4294967295"}
	}
	h := make([]byte, 0, 44)
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(36+dataLen))
	h = append(h, "WAVEfmt "...)
	h = binary.LittleEndian.AppendUint32(h, 16)
	h = binary.LittleEndian.AppendUint16(h, 1)
	h = binary.LittleEndian.AppendUint16(h, 1)
	h = binary.LittleEndian.AppendUint32(h, uint32(rate))
	h = binary.LittleEndian.AppendUint32(h, uint32(rate*2))
	h = binary.LittleEndian.AppendUint16(h, 2)
	h = binary.LittleEndian.AppendUint16(h, 16)
	h = append(h, "data"...)
	return binary.LittleEndian.AppendUint32(h, uint32(dataLen)), nil
}

// Writes the aligned chunks as a WAV file, the header patched with the written length at the end
func writeWaveFile(f io.WriteSeeker, chunks iter.Seq[[]byte], sampleRate int) error {
	if sampleRate <= 0 {
		return &ttsRuntimeError{msg: "bad frame rate"}
	}
	header, err := wavePCMHeader(sampleRate, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(header); err != nil {
		return err
	}
	written := 0
	for chunk := range chunks {
		n, err := f.Write(chunk)
		written += n
		if err != nil {
			return err
		}
	}
	if written == 0 {
		return nil
	}
	if header, err = wavePCMHeader(sampleRate, written); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := f.Write(header); err != nil {
		return err
	}
	_, err = f.Seek(0, io.SeekEnd)
	return err
}

// Writes PCM chunks to a temp WAV file and plays it with the system player
func playViaTempfile(chunks iter.Seq[[]byte], stop Event, sampleRate int) {
	var tmp speakerTempFile
	tmpPath := ""
	err := pluginCall(func() error {
		var err error
		if tmp, err = speakerNamedTempFile(".wav"); err != nil {
			return err
		}
		tmpPath = tmp.Name()
		if err := writeWaveFile(tmp, alignInt16Chunks(chunks, stop, true), sampleRate); err != nil {
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		speakerPlayAudioFile(tmpPath)
		return nil
	})
	if err != nil {
		ttsLog().Warn(fmt.Sprintf("Temp-file TTS fallback failed: %s", err))
	}
	if tmp != nil {
		_ = pluginCall(tmp.Close)
	}
	removeQuietly(tmpPath)
}

// One queued PCM chunk of a sentence, end marking the last
type pcmItem struct {
	data []byte
	end  bool
}

// Collects the PCM chunks of one sentence up to its end marker
func drainChunks(q <-chan pcmItem) [][]byte {
	var out [][]byte
	for item := range q {
		if item.end {
			break
		}
		out = append(out, item.data)
	}
	return out
}

// Whether a path names a regular file holding at least one byte
func isNonEmptyFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// The audio path the TTS tool reports having written, the requested path when none exists with content
func firstWrittenArtifact(raw any, requested string) string {
	payload := raw
	if text, ok := raw.(string); ok {
		decoded, err := pyjson.Loads(text)
		if err != nil {
			return requested
		}
		payload = decoded
	}
	var reported any
	var get func(key string) any
	switch p := payload.(type) {
	case *pyjson.Object:
		get = func(key string) any { return p.GetOr(key, nil) }
	case map[string]any:
		get = func(key string) any { return p[key] }
	default:
		return requested
	}
	if reported = get("file_paths"); !pyTruthy(reported) {
		reported = []any{}
		if one := get("file_path"); pyTruthy(one) {
			reported = []any{one}
		}
	}
	var candidates []any
	switch list := reported.(type) {
	case []any:
		candidates = list
	case []string:
		for _, s := range list {
			candidates = append(candidates, s)
		}
	}
	for _, c := range candidates {
		if path, ok := c.(string); ok && isNonEmptyFile(path) {
			return path
		}
	}
	return requested
}

// One sentence of the sync pipeline and the future of its synthesized file
type syncSentenceJob struct {
	sentence string
	result   chan string
}

// Overlaps per sentence synthesis with playback for providers without a chunked API
type syncSentencePipeline struct {
	ctx        context.Context
	stop       Event
	queue      chan *syncSentenceJob
	lastSynth  chan struct{}
	playerDone chan struct{}
}

func newSyncSentencePipeline(ctx context.Context, stop Event, lookahead int) *syncSentencePipeline {
	p := &syncSentencePipeline{ctx: ctx, stop: stop, queue: make(chan *syncSentenceJob, max(1, lookahead))}
	p.playerDone = startThread(p.drain)
	return p
}

// Queues one sentence for synthesis in order, blocking only while the lookahead bound is full
func (p *syncSentencePipeline) speak(cleaned string) {
	if p.stop.IsSet() {
		return
	}
	job := &syncSentenceJob{sentence: cleaned, result: make(chan string, 1)}
	prev, done := p.lastSynth, make(chan struct{})
	go func() {
		defer close(done)
		if prev != nil {
			<-prev
		}
		job.result <- p.synthesizeToTmp(cleaned)
	}()
	p.lastSynth = done
	p.queue <- job
}

// Flushes the queued sentences in order, then waits for the player and the synthesis worker
func (p *syncSentencePipeline) close() {
	p.queue <- nil
	<-p.playerDone
	if p.lastSynth != nil {
		<-p.lastSynth
	}
}

// Synthesizes one sentence into a temp file, returning the written artifact or nothing on failure
func (p *syncSentencePipeline) synthesizeToTmp(cleaned string) string {
	if p.stop.IsSet() {
		return ""
	}
	tmpPath, written := "", ""
	err := pluginCall(func() error {
		var err error
		if tmpPath, err = speakerMkstemp(".mp3"); err != nil {
			return err
		}
		raw, err := speakerTextToSpeechTool(p.ctx, cleaned, tmpPath)
		if err != nil {
			return err
		}
		written = firstWrittenArtifact(raw, tmpPath)
		if absPath(written) != absPath(tmpPath) {
			removeQuietly(tmpPath)
		}
		return nil
	})
	if err != nil {
		ttsLog().Warn(fmt.Sprintf("Sync per-sentence TTS synthesis failed: %s", err))
		removeQuietly(tmpPath)
		return ""
	}
	return written
}

// Plays each synthesized sentence in submission order and removes its file
func (p *syncSentencePipeline) drain() {
	for job := range p.queue {
		if job == nil {
			return
		}
		tmpPath := ""
		if err := pluginCall(func() error {
			tmpPath = <-job.result
			if tmpPath != "" && !p.stop.IsSet() && isNonEmptyFile(tmpPath) {
				speakerPlayAudioFile(tmpPath)
			}
			return nil
		}); err != nil {
			ttsLog().Warn(fmt.Sprintf("Sync per-sentence TTS failed: %s", err))
		}
		removeQuietly(tmpPath)
	}
}

// Unbounded queue of per sentence chunk queues, a nil entry ending playback
type sentenceQueue struct {
	mu    sync.Mutex
	items []chan pcmItem
	wake  chan struct{}
}

func (q *sentenceQueue) put(item chan pcmItem) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *sentenceQueue) get() chan pcmItem {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			item := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return item
		}
		q.mu.Unlock()
		<-q.wake
	}
}

// Prefetches each sentence of a chunked streamer and plays the sentences in order through one worker
type streamerPlayback struct {
	ctx             context.Context
	streamer        StreamingTTSProvider
	stop            Event
	mu              sync.Mutex
	outputStream    pysounddevice.OutputStream
	useDevice       bool
	audioQueue      *sentenceQueue
	prefetchThreads []chan struct{}
	prefetchSem     chan struct{}
	workerDone      chan struct{}
	reinitCount     int
	currentStream   pysounddevice.OutputStream
	currentRate     int
}

func newStreamerPlayback(ctx context.Context, streamer StreamingTTSProvider, stop Event) *streamerPlayback {
	p := &streamerPlayback{ctx: ctx, streamer: streamer, stop: stop, useDevice: speakerDeviceUsable(),
		audioQueue: &sentenceQueue{wake: make(chan struct{}, 1)}, prefetchSem: make(chan struct{}, speakerPrefetchLimit)}
	p.workerDone = startThread(p.playbackWorker)
	return p
}

func (p *streamerPlayback) setOutputStream(stream pysounddevice.OutputStream) {
	p.mu.Lock()
	p.outputStream = stream
	p.mu.Unlock()
}

func (p *streamerPlayback) getOutputStream() pysounddevice.OutputStream {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outputStream
}

// Opens and starts a PortAudio output stream at the current rate and channel count of the streamer
func (p *streamerPlayback) createOutputStream() (pysounddevice.OutputStream, error) {
	var stream pysounddevice.OutputStream
	err := pluginCall(func() error {
		sd, err := ttsImportSounddevice()
		if err != nil {
			return err
		}
		opened, err := sd.OutputStream(pysounddevice.StreamParams{Samplerate: float64(p.streamer.SampleRate()), Channels: p.streamer.Channels()})
		if err != nil {
			return err
		}
		if err := opened.Start(); err != nil {
			return err
		}
		stream = opened
		return nil
	})
	return stream, err
}

// Opens PortAudio at the current streamer rate, reopening on a rate change, false routing to a temp WAV
func (p *streamerPlayback) ensureOutputStream() bool {
	rate := p.streamer.SampleRate()
	if p.currentStream != nil && p.currentRate == rate {
		return true
	}
	if p.currentStream == nil && p.reinitCount >= speakerMaxReinit {
		return false
	}
	p.closeOutputStream()
	stream, err := p.createOutputStream()
	if err != nil {
		ttsLog().Warn(fmt.Sprintf("sounddevice OutputStream failed: %s", err))
		stream, p.reinitCount = nil, speakerMaxReinit
	}
	p.setOutputStream(stream)
	p.currentStream, p.currentRate = stream, rate
	return p.currentStream != nil
}

// Stops and closes the open output stream so a later stream can claim the device
func (p *streamerPlayback) closeOutputStream() {
	if stream := p.getOutputStream(); stream != nil {
		_ = pluginCall(func() error {
			if err := stream.Stop(true); err != nil {
				return err
			}
			return stream.Close(true)
		})
	}
}

// Starts the streamer on one sentence and prefetches its chunks on a new thread
func (p *streamerPlayback) speak(text string) {
	var audio iter.Seq2[[]byte, error]
	if err := pluginCall(func() (err error) {
		audio, err = p.streamer.Stream(p.ctx, text)
		return err
	}); err != nil {
		ttsLog().Warn(fmt.Sprintf("Streaming TTS synthesis failed: %s", err))
		return
	}
	p.prefetchSem <- struct{}{}
	q := make(chan pcmItem, speakerChunkQueueMax)
	p.audioQueue.put(q)
	p.prefetchThreads = append(p.prefetchThreads, startThread(func() { p.consumeToQueue(audio, q) }))
}

// Buffers the chunks of one sentence into its bounded queue, always ending with the end marker
func (p *streamerPlayback) consumeToQueue(audio iter.Seq2[[]byte, error], q chan pcmItem) {
	defer func() {
		q <- pcmItem{end: true}
		<-p.prefetchSem
	}()
	if err := pluginCall(func() error {
		for chunk, err := range audio {
			if err != nil {
				return err
			}
			if p.stop.IsSet() {
				ttsLog().Info("TTS CUT: prefetch cancelled (stop_event set mid-sentence) — partial audio only")
				return nil
			}
			timer := time.NewTimer(speakerChunkPutWait)
			select {
			case q <- pcmItem{data: chunk}:
				timer.Stop()
			case <-timer.C:
				return &queueFullError{}
			}
		}
		return nil
	}); err != nil {
		ttsLog().Warn(fmt.Sprintf("TTS CUT: streaming TTS prefetch failed mid-sentence (partial audio only): %s", err))
	}
}

// Python queue.Full raised by a bounded put that timed out
type queueFullError struct{}

func (*queueFullError) Error() string      { return "" }
func (*queueFullError) PyTypeName() string { return "Full" }

func (p *streamerPlayback) playSentenceViaTempfile(q chan pcmItem) {
	chunks := drainChunks(q)
	playViaTempfile(slices.Values(chunks), p.stop, p.streamer.SampleRate())
}

// Feeds queued sentences to play in order until the end entry, skipping them once stopped
func (p *streamerPlayback) forEachSentence(play func(chan pcmItem)) {
	for q := p.audioQueue.get(); q != nil; q = p.audioQueue.get() {
		if !p.stop.IsSet() {
			play(q)
		}
	}
}

// Writes one aligned PCM buffer to the open stream as a mono int16 column
func (p *streamerPlayback) writePCM(buf []byte) error {
	return pluginCall(func() error {
		samples := make([]int16, len(buf)/2)
		for i := range samples {
			samples[i] = int16(binary.LittleEndian.Uint16(buf[2*i:]))
		}
		if p.streamer.Channels() != 1 {
			return &pysounddevice.ValueError{Msg: "number of channels must match"}
		}
		_, err := p.currentStream.Write(samples)
		return err
	})
}

// Replaces the broken stream with a fresh one after a failed write, false once the reinit budget is spent
func (p *streamerPlayback) recoverStream() bool {
	if p.reinitCount >= speakerMaxReinit {
		ttsLog().Warn(fmt.Sprintf("TTS: PortAudio reinit exhausted after %d attempts, falling back to tempfile for remaining sentences", speakerMaxReinit))
		p.currentStream = nil
		return false
	}
	p.reinitCount++
	p.closeOutputStream()
	stream, err := p.createOutputStream()
	if err != nil {
		ttsLog().Warn(fmt.Sprintf("TTS: PortAudio stream reinit failed: %s", err))
		stream = nil
	} else {
		ttsLog().Info("TTS: PortAudio output stream reinitialized after error")
	}
	p.setOutputStream(stream)
	p.currentStream = stream
	return p.currentStream != nil
}

// Writes the PCM of one sentence to PortAudio, dropping the rest after an unrecoverable write failure
func (p *streamerPlayback) playSentenceViaStream(q chan pcmItem) {
	first := <-q
	if first.end {
		return
	}
	chunks := func(yield func([]byte) bool) {
		if !yield(first.data) {
			return
		}
		for item := range q {
			if item.end || !yield(item.data) {
				return
			}
		}
	}
	if !p.ensureOutputStream() {
		playViaTempfile(slices.Values(slices.Collect(chunks)), p.stop, p.streamer.SampleRate())
		return
	}
	for aligned := range alignInt16Chunks(chunks, p.stop, false) {
		if err := p.writePCM(aligned); err != nil {
			ttsLog().Warn(fmt.Sprintf("PortAudio write failed, attempting stream reinit: %s", err))
			if !p.recoverStream() {
				return
			}
			_ = p.writePCM(aligned)
		}
	}
}

// Single consumer playing the queued sentences in order through the device or temp files
func (p *streamerPlayback) playbackWorker() {
	defer func() {
		if r := recover(); r != nil {
			ttsLog().Error(fmt.Sprintf("Exception in thread streamer playback: %v", r))
		}
	}()
	if !p.useDevice {
		p.forEachSentence(p.playSentenceViaTempfile)
		return
	}
	p.reinitCount, p.currentStream, p.currentRate = 0, nil, 0
	MarkAudioOutputActive(true)
	defer MarkAudioOutputActive(false)
	p.forEachSentence(p.playSentenceViaStream)
}

// Sends the end entry, then waits for the playback worker and the prefetch threads
func (p *streamerPlayback) finish() {
	p.audioQueue.put(nil)
	joinThread(p.workerDone, speakerWorkerJoinWait)
	for _, t := range p.prefetchThreads {
		joinThread(t, speakerPrefetchJoin)
	}
	p.closeOutputStream()
}

// Cuts queued text deltas into sentences and speaks each one as soon as it completes
func StreamTTSToSpeaker(ctx context.Context, textQueue TextQueue, stopEvent, ttsDoneEvent Event, displayCallback func(string), provider string) {
	ttsDoneEvent.Clear()
	var syncPipeline *syncSentencePipeline
	var playback *streamerPlayback
	defer func() {
		if syncPipeline != nil {
			_ = pluginCall(func() error {
				syncPipeline.close()
				return nil
			})
		}
		if playback != nil {
			playback.finish()
		}
		ttsDoneEvent.Set()
	}()
	if err := pluginCall(func() error {
		ttsConfig := loadTTSConfig(ctx, nil)
		streamer, err := speakerResolveStreamingProvider(ctx, nil, ttsConfig, provider)
		if err != nil {
			return err
		}
		streamMaxLen := 0
		if streamer == nil {
			syncPipeline = newSyncSentencePipeline(ctx, stopEvent, 2)
		} else {
			_ = pluginCall(func() error {
				name := provider
				if name == "" {
					name = getTTSProvider(ttsConfig)
				}
				streamMaxLen = resolveMaxTextLength(name, ttsConfig)
				return nil
			})
			playback = newStreamerPlayback(ctx, streamer, stopEvent)
		}
		chunker := SentenceChunkerFromConfig(ttsConfig)
		var spokenSentences []string
		speakSentence := func(sentence string) {
			if stopEvent.IsSet() {
				return
			}
			cleaned := pystr.Strip(tools.StripMarkdownForTTS(sentence))
			if cleaned == "" {
				return
			}
			cleanedLower := pystr.RstripChars(pystr.Lower(cleaned), ".!,")
			for _, prev := range spokenSentences {
				if pystr.RstripChars(pystr.Lower(prev), ".!,") == cleanedLower {
					return
				}
			}
			spokenSentences = append(spokenSentences, cleaned)
			if displayCallback != nil {
				displayCallback(sentence)
			}
			if syncPipeline != nil {
				syncPipeline.speak(cleaned)
				return
			}
			if streamMaxLen != 0 && utf8.RuneCountInString(cleaned) > streamMaxLen {
				cleaned = string([]rune(cleaned)[:streamMaxLen])
			}
			playback.speak(cleaned)
		}
		for !stopEvent.IsSet() {
			var sentences []string
			item, ok := textQueue.Get(500 * time.Millisecond)
			switch delta := item.(type) {
			case nil:
				if ok || utf8.RuneCountInString(chunker.buf) > 100 {
					sentences = chunker.Flush()
				}
			case string:
				sentences = chunker.Feed(delta)
			default:
				return &ttsTypeError{msg: fmt.Sprintf("can only concatenate str (not \"%s\") to str", pyrepr.TypeName(item))}
			}
			for _, sentence := range sentences {
				speakSentence(sentence)
			}
			if ok && item == nil {
				break
			}
		}
		for {
			if _, ok := textQueue.Get(0); !ok {
				return nil
			}
		}
	}); err != nil {
		ttsLog().Warn(fmt.Sprintf("Streaming TTS pipeline error: %s", err))
	}
}
