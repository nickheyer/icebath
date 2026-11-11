package dashboard

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Unbounded FIFO whose get waits for an item, the queue.Queue and asyncio.Queue of the audio sockets
type audioQueue[T any] struct {
	mu     sync.Mutex
	items  []T
	signal chan struct{}
}

func newAudioQueue[T any]() *audioQueue[T] { return &audioQueue[T]{signal: make(chan struct{}, 1)} }

func (q *audioQueue[T]) put(v T) {
	q.mu.Lock()
	q.items = append(q.items, v)
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// Next item, false when the timeout passes or the context ends first
func (q *audioQueue[T]) get(ctx context.Context, timeout time.Duration) (T, bool) {
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			v := q.items[0]
			q.items = q.items[1:]
			more := len(q.items) > 0
			q.mu.Unlock()
			if more {
				select {
				case q.signal <- struct{}{}:
				default:
				}
			}
			return v, true
		}
		q.mu.Unlock()
		var zero T
		select {
		case <-q.signal:
		case <-deadline:
			return zero, false
		case <-ctx.Done():
			return zero, false
		}
	}
}

// Context of a socket stream, the native grant home then the config scope of the requested profile
func audioStreamScope(ctx context.Context, ws *WebSocket, profile string) (context.Context, error) {
	sctx, _, err := configProfileScope(grantHomeScope(ctx, wsNativeGrant(ws)), profile)
	return sctx, err
}

// Socket gates of an audio stream, closing with 4401 or 4403 and reporting false when refused
func admitAudioStream(ctx context.Context, ws *WebSocket, socket string) (bool, error) {
	ok, err := wsAuthOK(ctx, ws, false, &socket)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ws.Close(ctx, 4401, "")
	}
	if !wsRequestIsAllowed(ws) {
		return false, ws.Close(ctx, 4403, "")
	}
	return true, ws.Accept(ctx, wsSubprotocol(ws), nil)
}

// Requested profile of a socket query, empty for a missing or blank one
func streamProfile(ws *WebSocket) string { return pystr.Strip(ws.QueryParams().Get("profile")) }

// Decoded JSON frame as a dict, a decode failure the ValueError and any other shape the AttributeError hermes raises
func jsonFrame(text string) (*pyjson.Object, error) {
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, &transcribeValueError{err.Error()}
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(v))
	}
	return obj, nil
}

// Python int() of a JSON value, the ValueError or TypeError it raises otherwise
func pyIntOf(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, &transcribeValueError{fmt.Sprintf("cannot convert float %s to integer", pyrepr.Repr(n))}
		}
		return int64(n), nil
	case string:
		var parsed int64
		if _, err := fmt.Sscan(pystr.Strip(n), &parsed); err != nil || fmt.Sprint(parsed) != strings.TrimPrefix(pystr.Strip(n), "+") {
			return 0, &transcribeValueError{fmt.Sprintf("invalid literal for int() with base 10: %s", pyrepr.Repr(n))}
		}
		return parsed, nil
	}
	return 0, fmt.Errorf("TypeError: int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))
}

// ValueError of the transcribe stream protocol, an early end of the client
type transcribeValueError struct{ msg string }

func (e *transcribeValueError) Error() string { return e.msg }

// Whether an error ends the transcribe stream quietly, a disconnect, ValueError or RuntimeError
func transcribeEndedEarly(err error) bool {
	var disconnect *WebSocketDisconnect
	var value *transcribeValueError
	var runtime *RuntimeError
	var disconnected *WebSocketDisconnected
	return errors.As(err, &disconnect) || errors.As(err, &value) || errors.As(err, &runtime) || errors.As(err, &disconnected)
}

// Live STT for desktop dictation, mic PCM in and partial then final text out
func transcribeStreamWS(ctx context.Context, ws *WebSocket) error {
	if admitted, err := admitAudioStream(ctx, ws, transcribeStreamSocket); !admitted || err != nil {
		return err
	}
	profile := streamProfile(ws)
	partials := newAudioQueue[string]()
	session := func() *media.StreamingSession {
		defer func() { _ = recover() }()
		sctx, err := audioStreamScope(ctx, ws, profile)
		if err != nil {
			return nil
		}
		s, err := media.OpenStreamingSession(sctx, func(text string) { partials.put(text) }, nil)
		if err != nil {
			return nil
		}
		return s
	}()
	if session == nil {
		if err := ws.SendJSON(ctx, pyjson.ObjectOf("type", "error", "message", "no live STT for the configured provider"), false); err == nil {
			_ = ws.Close(ctx, 1000, "")
		}
		return nil
	}
	fctx, stopForwarder := context.WithCancel(ctx)
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		for {
			text, ok := partials.get(fctx, 0)
			if !ok {
				return
			}
			if ws.SendJSON(ctx, pyjson.ObjectOf("type", "partial", "text", text), false) != nil {
				return
			}
		}
	}()
	ended, err := receiveTranscribeAudio(ctx, ws, session)
	if err != nil && !transcribeEndedEarly(err) {
		session.Cancel()
		stopForwarder()
		return err
	}
	if err != nil {
		routersLog().Debug(fmt.Sprintf("transcribe-stream client ended early: %s", err))
	}
	if !ended {
		session.Cancel()
		stopForwarder()
		return nil
	}
	if _, err := audioStreamScope(ctx, ws, profile); err != nil {
		stopForwarder()
		return err
	}
	result := session.Finalize(media.StreamingFinalizeTimeout)
	partials.waitDrained(ctx)
	stopForwarder()
	<-forwarded
	var frame *pyjson.Object
	if pyrepr.Truthy(result["success"]) {
		frame = pyjson.ObjectOf("type", "final", "transcript", dictOr(result, "transcript", ""), "provider", dictOr(result, "provider", ""))
	} else {
		message := any("transcription failed")
		if e := result["error"]; pyrepr.Truthy(e) {
			message = e
		}
		frame = pyjson.ObjectOf("type", "error", "message", message)
	}
	if ws.SendJSON(ctx, frame, false) == nil {
		_ = ws.Close(ctx, 1000, "")
	}
	return nil
}

// Value of a key or the default when absent, the dict get with a default
func dictOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Blocks until every queued item has been taken or the context ends
func (q *audioQueue[T]) waitDrained(ctx context.Context) {
	for {
		q.mu.Lock()
		empty := len(q.items) == 0
		q.mu.Unlock()
		if empty || ctx.Err() != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// Reads the sample rate frame then PCM until end of speech, true when the client sent eos
func receiveTranscribeAudio(ctx context.Context, ws *WebSocket, session *media.StreamingSession) (bool, error) {
	text, err := ws.ReceiveText(ctx)
	if err != nil {
		return false, err
	}
	first, err := jsonFrame(text)
	if err != nil {
		return false, err
	}
	raw := first.GetOr("sample_rate", nil)
	if !pyrepr.Truthy(raw) {
		raw = int64(0)
	}
	rate, err := pyIntOf(raw)
	if err != nil {
		return false, err
	}
	if rate < 8000 || rate > 48000 {
		return false, &transcribeValueError{fmt.Sprintf("unsupported sample_rate %d", rate)}
	}
	session.SetInputRate(int(rate))
	for {
		m, err := ws.Receive(ctx)
		if err != nil {
			return false, err
		}
		if m.Type == "websocket.disconnect" {
			return false, nil
		}
		if len(m.Bytes) > 0 {
			session.PushAudio(m.Bytes)
			continue
		}
		if m.Text != nil && *m.Text != "" {
			frame, err := jsonFrame(*m.Text)
			if err != nil {
				return false, err
			}
			if pyrepr.Truthy(frame.GetOr("eos", nil)) {
				return true, nil
			}
		}
	}
}

// PCM source of the speak stream, its final sample rate and channel count known by the first chunk
type speakStreamer interface {
	SampleRate() int
	Channels() int
	Stream(ctx context.Context, text string) (iter.Seq2[[]byte, error], error)
}

// Speaks one sentence through the sync TTS tool and yields int16 mono PCM
type syncSentencePCMStreamer struct{ sampleRate atomic.Int64 }

func newSyncSentencePCMStreamer() *syncSentencePCMStreamer {
	s := &syncSentencePCMStreamer{}
	s.sampleRate.Store(24000)
	return s
}

func (s *syncSentencePCMStreamer) SampleRate() int { return int(s.sampleRate.Load()) }

func (s *syncSentencePCMStreamer) Channels() int { return 1 }

func (s *syncSentencePCMStreamer) Stream(ctx context.Context, text string) (iter.Seq2[[]byte, error], error) {
	pcm, rate, err := syncSentenceToPCM(ctx, text)
	if err != nil {
		return nil, err
	}
	if rate != 0 {
		s.sampleRate.Store(int64(rate))
	}
	return func(yield func([]byte, error) bool) {
		if len(pcm) > 0 {
			yield(pcm, nil)
		}
	}, nil
}

// Synthesizes text with the configured sync provider into a temp file and decodes it to PCM
func syncSentenceToPCM(ctx context.Context, text string) ([]byte, int, error) {
	tmp, err := os.CreateTemp("", "*.mp3")
	if err != nil {
		return nil, 0, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	extra := ""
	defer func() {
		unlinkQuietly(tmpPath)
		if extra != "" {
			unlinkQuietly(extra)
		}
	}()
	raw, err := media.TextToSpeechTool(ctx, text, tmpPath)
	if err != nil {
		return nil, 0, err
	}
	parsed, err := pyjson.Loads(raw)
	if err != nil {
		return nil, 0, &RuntimeError{Msg: "Invalid TTS response"}
	}
	payload, isDict := parsed.(*pyjson.Object)
	if !isDict || !pyrepr.Truthy(payload.GetOr("success", nil)) {
		detail := ""
		if isDict {
			detail = pyrepr.Str(pyrepr.Or(payload.GetOr("error", nil), ""))
		}
		if detail == "" {
			detail = "Speech synthesis failed"
		}
		return nil, 0, &RuntimeError{Msg: detail}
	}
	written := pyrepr.Or(payload.GetOr("file_path", nil), tmpPath)
	writtenPath, isStr := written.(string)
	if st, err := os.Stat(writtenPath); !isStr || err != nil || !st.Mode().IsRegular() || st.Size() <= 0 {
		return nil, 0, &RuntimeError{Msg: "Audio file missing"}
	}
	absWritten, _ := filepath.Abs(writtenPath)
	absTmp, _ := filepath.Abs(tmpPath)
	if absWritten != absTmp {
		extra = writtenPath
	}
	pcm, rate, err := audioFileToPCM(writtenPath)
	if err != nil {
		return nil, 0, err
	}
	if len(pcm) == 0 {
		return nil, 0, &RuntimeError{Msg: "TTS audio decoded to silence"}
	}
	return pcm, rate, nil
}

// Int16 mono PCM of an audio file, WAV read directly and anything else through ffmpeg
func audioFileToPCM(path string) ([]byte, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	head := make([]byte, 12)
	n, _ := f.Read(head)
	f.Close()
	if n >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE" {
		if pcm, rate := wavS16LEMono(path); len(pcm) > 0 {
			return pcm, rate, nil
		}
	}
	return ffmpegS16LEMono(path)
}

// PCM frames of a 16 bit mono WAV, empty for any other layout or an unreadable file
func wavS16LEMono(path string) ([]byte, int) {
	w, err := media.ReadWave(path)
	if err != nil || w.SampWidth != 2 || w.Channels != 1 || w.FrameRate <= 0 {
		return nil, 0
	}
	return w.Frames, w.FrameRate
}

// Int16 mono 24 kHz PCM of an audio file decoded by ffmpeg
func ffmpegS16LEMono(path string) ([]byte, int, error) {
	ffmpeg, ok := pyshutil.Which("ffmpeg", nil)
	if !ok {
		return nil, 0, &RuntimeError{Msg: "ffmpeg is required to decode TTS audio for speak-stream"}
	}
	code, stdout, stderr, err := media.FfmpegRun(ffmpeg, []string{"-i", path, "-f", "s16le", "-ac", "1", "-ar", "24000", "-loglevel", "error", "pipe:1"}, 60)
	if err != nil {
		return nil, 0, err
	}
	if code != 0 || len(stdout) == 0 {
		return nil, 0, &RuntimeError{Msg: "TTS audio decode failed: " + pystr.Slice(pystr.DecodeUTF8Replace(stderr), 0, 200)}
	}
	return stdout, 24000, nil
}

// Streamer, character cap and TTS config the speak stream resolves under the requesting profile
func resolveSpeakStreamer(ctx context.Context, ws *WebSocket, profile string) (streamer speakStreamer, capLen int, cfg map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			streamer, capLen, cfg, err = nil, 0, nil, fmt.Errorf("%v", r)
		}
	}()
	sctx, err := audioStreamScope(ctx, ws, profile)
	if err != nil {
		return nil, 0, nil, err
	}
	env := &tools.Env{Home: home.Dir(sctx), TerminalScope: tools.GetTerminalScope(sctx)}
	cfg = media.LoadTTSConfig(sctx, env)
	resolved, err := media.ResolveStreamingProvider(sctx, env, cfg, "")
	if err != nil {
		return nil, 0, nil, err
	}
	capLen = media.ResolveMaxTextLength(media.TTSProviderName(cfg), cfg)
	if resolved == nil {
		return nil, capLen, cfg, nil
	}
	return resolved, capLen, cfg, nil
}

// Sentence punctuation that flushes an idle chunker at once
var speakIdleFlushEndings = []string{".", "!", "?", "…", ":"}

// Streaming TTS for the desktop, text deltas in and raw int16 PCM frames out
func speakStreamWS(ctx context.Context, ws *WebSocket) error {
	if admitted, err := admitAudioStream(ctx, ws, speakStreamSocket); !admitted || err != nil {
		return err
	}
	profile := streamProfile(ws)
	streamer, capLen, cfg, err := resolveSpeakStreamer(ctx, ws, profile)
	if err != nil {
		routersLog().Error("speak-stream provider resolution failed", "exc_info", err)
		streamer, capLen, cfg = nil, 0, map[string]any{}
	}
	if streamer == nil {
		streamer = newSyncSentencePCMStreamer()
	}
	startSent := false
	sendStart := func() error {
		if startSent {
			return nil
		}
		startSent = true
		return ws.SendJSON(ctx, pyjson.ObjectOf("type", "start", "sample_rate", int64(streamer.SampleRate()),
			"channels", int64(streamer.Channels())), false)
	}
	var stop atomic.Bool
	stopped := make(chan struct{})
	var stopOnce sync.Once
	setStop := func() { stopOnce.Do(func() { stop.Store(true); close(stopped) }) }
	var producedAudio, synthesisFailed atomic.Bool
	textQ := newAudioQueue[*string]()
	chunks := newAudioQueue[[]byte]()
	go func() {
		sctx, err := audioStreamScope(ctx, ws, profile)
		if err != nil {
			slog.Error(fmt.Sprintf("Exception in thread speak-stream producer: %s", err))
			return
		}
		defer chunks.put(nil)
		if err := synthesizeSpeakStream(sctx, streamer, cfg, capLen, textQ, chunks, &stop, stopped, &producedAudio); err != nil {
			routersLog().Warn(fmt.Sprintf("speak-stream synthesis failed: %s", err))
			synthesisFailed.Store(true)
		}
	}()
	pctx, cancelPump := context.WithCancel(ctx)
	go func() {
		defer func() {
			setStop()
			textQ.put(nil)
		}()
		for {
			text, err := ws.ReceiveText(pctx)
			if err != nil {
				return
			}
			frame, err := jsonFrame(text)
			if err != nil {
				return
			}
			if t := frame.GetOr("text", nil); pyrepr.Truthy(t) {
				s := pyrepr.Str(t)
				textQ.put(&s)
			}
			if pyrepr.Truthy(frame.GetOr("stop", nil)) {
				return
			}
			if pyrepr.Truthy(frame.GetOr("done", nil)) {
				textQ.put(nil)
			}
		}
	}()
	defer func() {
		setStop()
		textQ.put(nil)
		cancelPump()
		_ = ws.Close(ctx, 1000, "")
	}()
	err = func() error {
		for {
			chunk, ok := chunks.get(ctx, 0)
			if !ok {
				return ctx.Err()
			}
			if chunk == nil {
				break
			}
			if err := sendStart(); err != nil {
				return err
			}
			if err := ws.SendBytes(ctx, chunk); err != nil {
				return err
			}
		}
		if stop.Load() {
			return nil
		}
		if synthesisFailed.Load() && !producedAudio.Load() {
			return ws.SendJSON(ctx, pyjson.ObjectOf("type", "fallback"), false)
		}
		if err := sendStart(); err != nil {
			return err
		}
		return ws.SendJSON(ctx, pyjson.ObjectOf("type", "end"), false)
	}()
	var disconnect *WebSocketDisconnect
	var runtime *RuntimeError
	var disconnected *WebSocketDisconnected
	if err != nil && !errors.As(err, &disconnect) && !errors.As(err, &runtime) && !errors.As(err, &disconnected) {
		return err
	}
	return nil
}

// Cuts queued text into sentences and streams each piece's PCM until the text ends or a barge in
func synthesizeSpeakStream(ctx context.Context, streamer speakStreamer, cfg map[string]any, capLen int, textQ *audioQueue[*string],
	chunks *audioQueue[[]byte], stop *atomic.Bool, stopped <-chan struct{}, producedAudio *atomic.Bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	chunker := media.SentenceChunkerFromConfig(cfg)
	const idlePoll = 500 * time.Millisecond
	const idlePollsBeforeForceFlush = 4
	sentences := func(yield func(string) bool) {
		idlePolls := 0
		emit := func(batch []string) bool {
			for _, s := range batch {
				if !yield(s) {
					return false
				}
			}
			return true
		}
		for !stop.Load() {
			pctx, cancel := context.WithCancel(ctx)
			go func() {
				select {
				case <-stopped:
					cancel()
				case <-pctx.Done():
				}
			}()
			delta, ok := textQ.get(pctx, idlePoll)
			stoppedNow := pctx.Err() != nil && ctx.Err() == nil && stop.Load()
			cancel()
			if !ok {
				if stoppedNow || ctx.Err() != nil {
					continue
				}
				idlePolls++
				buf := chunker.Buffered()
				buffered := pystr.Strip(buf)
				if buffered == "" || (strings.Contains(buf, "<think") && !strings.Contains(buf, "</think>")) {
					continue
				}
				flush := idlePolls >= idlePollsBeforeForceFlush
				for _, end := range speakIdleFlushEndings {
					if strings.HasSuffix(buffered, end) {
						flush = true
					}
				}
				if flush && !emit(chunker.Flush()) {
					return
				}
				continue
			}
			idlePolls = 0
			if delta == nil {
				emit(chunker.Flush())
				return
			}
			if !emit(chunker.Feed(*delta)) {
				return
			}
		}
	}
	for sentence := range sentences {
		cleaned := tools.StripMarkdownForTTS(sentence)
		if cleaned == "" {
			continue
		}
		for _, piece := range splitTextForSpeakStream(cleaned, capLen) {
			seq, err := streamer.Stream(ctx, piece)
			if err != nil {
				return err
			}
			for chunk, err := range seq {
				if err != nil {
					return err
				}
				if stop.Load() {
					return nil
				}
				producedAudio.Store(true)
				chunks.put(chunk)
			}
		}
	}
	return nil
}
