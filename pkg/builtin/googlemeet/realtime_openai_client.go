package googlemeet

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const realtimeURL = "wss://api.openai.com/v1/realtime"

var terminalFrames = map[string]bool{"response.done": true, "response.completed": true, "response.cancelled": true}

// Synchronous websocket surface the realtime session and node client use
type syncWS interface {
	Send(text string) error
	Recv(timeout time.Duration) (string, error)
	Close()
}

// Timeout of one websocket receive
type wsTimeoutError struct{}

func (wsTimeoutError) Error() string { return "timed out while waiting for a message" }

// Client websocket over pkg/browser
type browserWS struct{ c *browser.WSConn }

func (w *browserWS) Send(text string) error {
	return w.c.WriteText(context.Background(), []byte(text))
}

func (w *browserWS) Recv(timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, payload, err := w.c.ReadMessage(ctx)
	if err != nil && ctx.Err() != nil {
		return "", wsTimeoutError{}
	}
	return string(payload), err
}

func (w *browserWS) Close() { _ = w.c.Close(1000, "") }

// Opens a client websocket with extra headers and an opening handshake timeout, swapped by tests
var dialSyncWS = func(url string, header http.Header, openTimeout time.Duration) (syncWS, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()
	c, err := browser.DialWS(ctx, url, browser.WSOptions{Header: header, HandshakeTimeout: openTimeout})
	if err != nil {
		return nil, err
	}
	return &browserWS{c: c}, nil
}

// Decoded audio delta, empty for a missing or undecodable payload
func decodeAudio(b64 string) []byte {
	if b64 == "" {
		return nil
	}
	out, err := pybase64.B64Decode(b64)
	if err != nil {
		return nil
	}
	return out
}

// OpenAI Realtime websocket client whose speak and cancel writes are serialized by a lock
type realtimeSession struct {
	apiKey, model, voice, instructions string
	audioSinkPath                      string
	sampleRate                         int
	ws                                 syncWS
	sendMu                             sync.Mutex
	countMu                            sync.Mutex
	audioBytesOut                      int
	lastAudioOutAt                     any
}

// Session for a key, model, voice and instructions writing audio to sinkPath when set
func newRealtimeSession(apiKey, model, voice, instructions, sinkPath string, sampleRate int) *realtimeSession {
	return &realtimeSession{apiKey: apiKey, model: model, voice: voice, instructions: instructions, audioSinkPath: sinkPath, sampleRate: sampleRate}
}

// Audio bytes written so far and the time of the last chunk, nil before any
func (s *realtimeSession) counters() (int, any) {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	return s.audioBytesOut, s.lastAudioOutAt
}

// Opens the websocket and sends session.update with voice and instructions
func (s *realtimeSession) connect() error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.apiKey)
	header.Set("OpenAI-Beta", "realtime=v1")
	ws, err := dialSyncWS(realtimeURL+"?model="+s.model, header, 10*time.Second)
	if err != nil {
		return err
	}
	s.ws = ws
	return s.sendJSON(pyjson.ObjectOf("type", "session.update", "session", pyjson.ObjectOf(
		"voice", s.voice, "instructions", s.instructions, "modalities", []any{"audio", "text"},
		"output_audio_format", "pcm16", "input_audio_format", "pcm16")))
}

// Closes the websocket once, later calls doing nothing
func (s *realtimeSession) close() {
	if s.ws != nil {
		s.ws.Close()
		s.ws = nil
	}
}

// Sends text and appends the audio response to the sink, returning ok, bytes written and duration
func (s *realtimeSession) speak(text string, timeout float64) (*pyjson.Object, error) {
	if s.ws == nil {
		return nil, errors.New("RealtimeSession.connect() must be called first")
	}
	start := time.Now()
	if err := s.sendJSON(pyjson.ObjectOf("type", "conversation.item.create", "item", pyjson.ObjectOf(
		"type", "message", "role", "user", "content", []any{pyjson.ObjectOf("type", "input_text", "text", text)}))); err != nil {
		return nil, err
	}
	if err := s.sendJSON(pyjson.ObjectOf("type", "response.create", "response", pyjson.ObjectOf("modalities", []any{"audio"}))); err != nil {
		return nil, err
	}
	bytesWritten := 0
	var sink *os.File
	if s.audioSinkPath != "" {
		if err := os.MkdirAll(filepath.Dir(s.audioSinkPath), 0o777); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(s.audioSinkPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		sink = f
	}
	deadline := start.Add(time.Duration(timeout * float64(time.Second)))
	for {
		frame, err := s.recvFrame(deadline, timeout)
		if err != nil {
			return nil, err
		}
		if frame == nil {
			break
		}
		ftype, _ := frame.Get("type")
		if t, ok := ftype.(string); ok && terminalFrames[t] {
			break
		}
		if ftype == "error" {
			detail, _ := frame.Get("error")
			if !truthyJSON(detail) {
				detail = frame
			}
			return nil, fmt.Errorf("realtime error: %s", pyrepr.Str(detail))
		}
		if ftype != "response.audio.delta" || sink == nil {
			continue
		}
		raw, _ := frame.Get("delta")
		if !truthyJSON(raw) {
			raw, _ = frame.Get("audio")
		}
		b64, _ := raw.(string)
		chunk := decodeAudio(b64)
		if len(chunk) == 0 {
			continue
		}
		if _, err := sink.Write(chunk); err != nil {
			return nil, err
		}
		bytesWritten += len(chunk)
		s.countMu.Lock()
		s.audioBytesOut += len(chunk)
		s.lastAudioOutAt = nowSeconds()
		s.countMu.Unlock()
	}
	return pyjson.ObjectOf("ok", true, "bytes_written", bytesWritten,
		"duration_ms", float64(time.Since(start).Nanoseconds())/1e6), nil
}

// Sends response.cancel for barge in, false when the socket is closed or the send fails
func (s *realtimeSession) cancelResponse() bool {
	if s.ws == nil {
		return false
	}
	return s.sendJSON(pyjson.ObjectOf("type", "response.cancel")) == nil
}

// Serializes one JSON frame under the send lock
func (s *realtimeSession) sendJSON(payload *pyjson.Object) error {
	text, err := pyjson.Dumps(payload)
	if err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.ws.Send(text)
}

// Next object frame before deadline, skipping non object and unparsable frames
func (s *realtimeSession) recvFrame(deadline time.Time, timeout float64) (*pyjson.Object, error) {
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("realtime response did not complete within %ss", pyrepr.Float(timeout))
		}
		raw, err := s.ws.Recv(remaining)
		if err != nil {
			var closed *browser.ErrWSClosed
			if errors.As(err, &closed) {
				return nil, nil
			}
			return nil, err
		}
		if v, err := pyjson.Loads(raw); err == nil {
			if obj, ok := v.(*pyjson.Object); ok {
				return obj, nil
			}
		}
	}
}

// Python truthiness of a decoded JSON value
func truthyJSON(v any) bool {
	switch x := v.(type) {
	case *pyjson.Object:
		return x.Len() > 0
	case *big.Int:
		return x.Sign() != 0
	case int64:
		return x != 0
	}
	return truthy(v)
}

// Python type name of a decoded JSON value
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case *pyjson.Object:
		return "dict"
	}
	return "int"
}

// Session surface the speaker needs
type speakerSession interface {
	speak(text string, timeout float64) (*pyjson.Object, error)
}

// JSONL say queue drained one entry at a time through a realtime session
type realtimeSpeaker struct {
	session       speakerSession
	queuePath     string
	processedPath string
	readQueue     func() ([]*pyjson.Object, error)
}

// Speaker over queuePath recording results in processedPath when set
func newRealtimeSpeaker(session speakerSession, queuePath, processedPath string) *realtimeSpeaker {
	s := &realtimeSpeaker{session: session, queuePath: queuePath, processedPath: processedPath}
	s.readQueue = s.readQueueFile
	return s
}

// Queue entries, blank and malformed lines skipped and entries without an id given one
func (s *realtimeSpeaker) readQueueFile() ([]*pyjson.Object, error) {
	raw, err := os.ReadFile(s.queuePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	var out []*pyjson.Object
	for _, line := range pystr.Splitlines(text, false) {
		if pystr.Strip(line) == "" {
			continue
		}
		v, err := pyjson.Loads(line)
		if err != nil {
			continue
		}
		if entry, ok := v.(*pyjson.Object); ok {
			if _, has := entry.Get("id"); !has {
				entry.Set("id", uuid.NewString())
			}
			out = append(out, entry)
		}
	}
	return out, nil
}

// Rewrites the queue with the remaining entries, keeping an empty file when drained
func (s *realtimeSpeaker) rewriteQueue(remaining []*pyjson.Object) error {
	var b strings.Builder
	for _, e := range remaining {
		text, err := pyjson.Dumps(e)
		if err != nil {
			return err
		}
		b.WriteString(text + "\n")
	}
	return os.WriteFile(s.queuePath, []byte(b.String()), 0o666)
}

// Appends the spoken entry and its result to the processed log when one is configured
func (s *realtimeSpeaker) appendProcessed(entry, result *pyjson.Object) error {
	if s.processedPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.processedPath), 0o777); err != nil {
		return err
	}
	id, _ := entry.Get("id")
	text, ok := entry.Get("text")
	if !ok {
		text = ""
	}
	line, err := pyjson.Dumps(pyjson.ObjectOf("id", id, "text", text, "result", result))
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.processedPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(line + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// Speaks queued entries one per iteration until stop returns true
func (s *realtimeSpeaker) runUntilStopped(stop func() bool, poll time.Duration) error {
	for !stop() {
		entries, err := s.readQueue()
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			time.Sleep(poll)
			continue
		}
		head := entries[0]
		rawText, _ := head.Get("text")
		text := ""
		if truthyJSON(rawText) {
			str, ok := rawText.(string)
			if !ok {
				return fmt.Errorf("'%s' object has no attribute 'strip'", pyTypeName(rawText))
			}
			text = pystr.Strip(str)
		}
		result := pyjson.ObjectOf("ok", true, "bytes_written", 0, "duration_ms", 0.0)
		if text != "" {
			if spoken, err := s.session.speak(text, 30.0); err != nil {
				result = pyjson.ObjectOf("ok", false, "error", err.Error())
			} else {
				result = spoken
			}
		}
		if err := s.appendProcessed(head, result); err != nil {
			return err
		}
		if err := s.dropHead(head); err != nil {
			return err
		}
	}
	return nil
}

// Re reads the queue under the lock and drops the spoken head by position or id
func (s *realtimeSpeaker) dropHead(head *pyjson.Object) error {
	release, err := sayQueueLock(s.queuePath)
	if err != nil {
		return err
	}
	defer release()
	latest, err := s.readQueue()
	if err != nil {
		return err
	}
	headID, _ := head.Get("id")
	if len(latest) > 0 {
		if firstID, _ := latest[0].Get("id"); pyjson.Equal(firstID, headID) {
			return s.rewriteQueue(latest[1:])
		}
	}
	var keep []*pyjson.Object
	for _, e := range latest {
		if id, _ := e.Get("id"); !pyjson.Equal(id, headID) {
			keep = append(keep, e)
		}
	}
	return s.rewriteQueue(keep)
}
