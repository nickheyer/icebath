package googlemeet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Scripted websocket recording sent frames and popping queued replies
type fakeWS struct {
	mu     sync.Mutex
	sent   []map[string]any
	recvQ  []string
	closed bool
}

func (w *fakeWS) Send(text string) error {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return err
	}
	w.mu.Lock()
	w.sent = append(w.sent, m)
	w.mu.Unlock()
	return nil
}

func (w *fakeWS) Recv(time.Duration) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.recvQ) == 0 {
		return "", errors.New("fake ws: no more frames")
	}
	f := w.recvQ[0]
	w.recvQ = w.recvQ[1:]
	return f, nil
}

func (w *fakeWS) Close() { w.closed = true }

// Captured dial of the fake websocket
type dialCapture struct {
	url    string
	header http.Header
}

// Installs the fake websocket as the dial result
func installFakeWS(t *testing.T, ws *fakeWS, frames ...any) *dialCapture {
	t.Helper()
	for _, f := range frames {
		raw, _ := json.Marshal(f)
		ws.recvQ = append(ws.recvQ, string(raw))
	}
	captured := &dialCapture{}
	restore := dialSyncWS
	t.Cleanup(func() { dialSyncWS = restore })
	dialSyncWS = func(url string, header http.Header, _ time.Duration) (syncWS, error) {
		captured.url, captured.header = url, header
		return ws, nil
	}
	return captured
}

func TestConnectSendsSessionUpdateWithVoiceAndInstructions(t *testing.T) {
	ws := &fakeWS{}
	captured := installFakeWS(t, ws)
	sess := newRealtimeSession("sk-test", "gpt-realtime", "verse", "Be brief.", "", 24000)
	if err := sess.connect(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(captured.url, "wss://api.openai.com/v1/realtime") || !strings.Contains(captured.url, "model=gpt-realtime") {
		t.Fatal(captured.url)
	}
	if captured.header.Get("Authorization") != "Bearer sk-test" || captured.header.Get("OpenAI-Beta") != "realtime=v1" {
		t.Fatal(captured.header)
	}
	if len(ws.sent) != 1 || ws.sent[0]["type"] != "session.update" {
		t.Fatal(ws.sent)
	}
	s := ws.sent[0]["session"].(map[string]any)
	mods := s["modalities"].([]any)
	if s["voice"] != "verse" || s["instructions"] != "Be brief." || len(mods) != 2 || mods[0] != "audio" || mods[1] != "text" ||
		s["output_audio_format"] != "pcm16" || s["input_audio_format"] != "pcm16" {
		t.Fatal(s)
	}
}

func TestSpeakSendsCreateAndResponseAndWritesAudio(t *testing.T) {
	audio := []byte("\x01\x02\x03\x04PCM!")
	ws := &fakeWS{}
	installFakeWS(t, ws, map[string]any{"type": "response.created"},
		map[string]any{"type": "response.audio.delta", "delta": base64.StdEncoding.EncodeToString(audio)},
		map[string]any{"type": "response.audio.delta", "delta": base64.StdEncoding.EncodeToString([]byte("more"))},
		map[string]any{"type": "response.done"})
	sink := filepath.Join(t.TempDir(), "out.pcm")
	sess := newRealtimeSession("sk-test", "gpt-realtime", "alloy", "", sink, 24000)
	if err := sess.connect(); err != nil {
		t.Fatal(err)
	}
	result, err := sess.speak("Hello everyone.", 30.0)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, f := range ws.sent {
		types = append(types, f["type"].(string))
	}
	if strings.Join(types, ",") != "session.update,conversation.item.create,response.create" {
		t.Fatal(types)
	}
	item := ws.sent[1]["item"].(map[string]any)
	content := item["content"].([]any)[0].(map[string]any)
	if item["role"] != "user" || content["type"] != "input_text" || content["text"] != "Hello everyone." {
		t.Fatal(item)
	}
	if mods := ws.sent[2]["response"].(map[string]any)["modalities"].([]any); len(mods) != 1 || mods[0] != "audio" {
		t.Fatal(mods)
	}
	data, _ := os.ReadFile(sink)
	if string(data) != string(audio)+"more" {
		t.Fatal(data)
	}
	ok, _ := result.Get("ok")
	n, _ := result.Get("bytes_written")
	ms, _ := result.Get("duration_ms")
	if ok != true || n != len(audio)+4 || ms.(float64) < 0 {
		t.Fatal(result)
	}
}

func TestCloseIsIdempotentAndClosesWS(t *testing.T) {
	ws := &fakeWS{}
	installFakeWS(t, ws)
	sess := newRealtimeSession("sk-test", "gpt-realtime", "alloy", "", "", 24000)
	if err := sess.connect(); err != nil {
		t.Fatal(err)
	}
	sess.close()
	if !ws.closed {
		t.Fatal("not closed")
	}
	sess.close()
}

// Session stand in recording spoken text
type recordingSession struct {
	mu     sync.Mutex
	spoken []string
}

func (s *recordingSession) speak(text string, _ float64) (*pyjson.Object, error) {
	s.mu.Lock()
	s.spoken = append(s.spoken, text)
	s.mu.Unlock()
	return pyjson.ObjectOf("ok", true, "bytes_written", len(text), "duration_ms", 1.0), nil
}

func (s *recordingSession) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.spoken)
}

func TestSpeakerRunUntilStoppedProcessesQueue(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	queue, processed := filepath.Join(dir, "queue.jsonl"), filepath.Join(dir, "processed.jsonl")
	_ = os.WriteFile(queue, []byte(`{"id": "a", "text": "hello one"}`+"\n"+`{"id": "b", "text": "hello two"}`+"\n"), 0o644)
	rec := &recordingSession{}
	speaker := newRealtimeSpeaker(rec, queue, processed)
	stop := func() bool {
		raw, err := os.ReadFile(queue)
		return err == nil && strings.TrimSpace(string(raw)) == ""
	}
	if err := speaker.runUntilStopped(stop, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rec.spoken, "|") != "hello one|hello two" {
		t.Fatal(rec.spoken)
	}
	raw, _ := os.ReadFile(processed)
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec map[string]any
		_ = json.Unmarshal([]byte(line), &rec)
		ids = append(ids, rec["id"].(string))
		if rec["result"].(map[string]any)["ok"] != true {
			t.Fatal(rec)
		}
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatal(ids)
	}
}

func TestSpeakerDrainKeepsASayEnqueuedDuringTheRewrite(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	dir := t.TempDir()
	queue := filepath.Join(dir, "say_queue.jsonl")
	_ = os.WriteFile(queue, []byte(`{"id": "a", "text": "first"}`+"\n"), 0o644)
	_ = writeActive(ctx, pyjson.ObjectOf("pid", 0, "meeting_id", "abc-defg-hij", "out_dir", dir, "mode", "realtime", "url", "https://meet.google.com/abc-defg-hij", "started_at", 0))
	speakerHolds := make(chan struct{})
	contended := make(chan struct{}, 1)
	realLock := sayQueueLock
	defer func() { sayQueueLock = realLock }()
	sayQueueLock = func(path string) (func(), error) {
		select {
		case <-speakerHolds:
			select {
			case contended <- struct{}{}:
			default:
			}
		default:
		}
		return realLock(path)
	}
	rec := &recordingSession{}
	speaker := newRealtimeSpeaker(rec, queue, "")
	reads := 0
	appended := make(chan struct{})
	peerDone := make(chan struct{})
	var peerStarted bool
	speaker.readQueue = func() ([]*pyjson.Object, error) {
		out, err := speaker.readQueueFile()
		reads++
		if reads == 2 {
			close(speakerHolds)
			peerStarted = true
			go func() {
				defer close(peerDone)
				if _, err := pmEnqueueSay(ctx, "second"); err != nil {
					t.Error(err)
				}
				close(appended)
			}()
			select {
			case <-appended:
			case <-contended:
			case <-time.After(10 * time.Second):
				t.Error("the append neither landed nor blocked on the say-queue lock")
			}
		}
		return out, err
	}
	stop := func() bool {
		if rec.count() >= 2 {
			return true
		}
		if !peerStarted {
			return false
		}
		select {
		case <-peerDone:
		default:
			return false
		}
		rest, _ := speaker.readQueueFile()
		return len(rest) == 0
	}
	if err := speaker.runUntilStopped(stop, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	<-peerDone
	if strings.Join(rec.spoken, "|") != "first|second" {
		t.Fatal(rec.spoken)
	}
}
