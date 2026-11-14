package media

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// An xAI shaped STT socket answering every audio chunk with a growing partial
func xaiLikeServer(t *testing.T, received *[][]byte, mu *sync.Mutex) *httptest.Server {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		send := func(v map[string]any) {
			raw, _ := json.Marshal(v)
			_ = ws.WriteMessage(websocket.TextMessage, raw)
		}
		send(map[string]any{"type": "transcript.created"})
		var words []string
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				mu.Lock()
				*received = append(*received, data)
				mu.Unlock()
				words = append(words, "w"+string(rune('0'+len(words))))
				send(map[string]any{"type": "transcript.partial", "text": strings.Join(words, " "), "is_final": false, "speech_final": false})
				continue
			}
			var msg map[string]any
			_ = json.Unmarshal(data, &msg)
			if msg["type"] == "audio.done" {
				send(map[string]any{"type": "transcript.partial", "text": strings.Join(words, " "), "is_final": true, "speech_final": true})
				send(map[string]any{"type": "transcript.done", "text": ""})
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestLiveSessionStreamsPartialsBeforeEndAndKeepsEarlyAudio(t *testing.T) {
	var mu sync.Mutex
	var received [][]byte
	server := xaiLikeServer(t, &received, &mu)
	var partials []string
	seenPartial := &pythread.Event{}
	onPartial := func(text string) {
		mu.Lock()
		partials = append(partials, text)
		mu.Unlock()
		seenPartial.Set()
	}
	session, err := NewXAIStreamingSession(context.Background(), "k", server.URL+"/v1", "grok-voice-transcribe-2.0", "en", onPartial)
	if err != nil {
		t.Fatal(err)
	}
	session.Start()
	session.PushAudio([]byte(strings.Repeat("\x01\x00", 160)))
	session.PushAudio([]byte(strings.Repeat("\x02\x00", 160)))
	if !seenPartial.Wait(5) {
		t.Fatal("a partial must arrive while audio is still streaming")
	}
	result := session.Finalize(5)
	if result["success"] != true || result["transcript"] != "w0 w1" || result["provider"] != "xai" || len(result) != 3 {
		t.Fatalf("result %v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 || partials[len(partials)-1] != "w0 w1" {
		t.Fatalf("received %d partials %v", len(received), partials)
	}
}

type parkedSession struct {
	result map[string]any
	ended  bool
}

func (s *parkedSession) SetInputRate(int)                {}
func (s *parkedSession) PushAudio([]byte)                {}
func (s *parkedSession) EndAudio()                       { s.ended = true }
func (s *parkedSession) Finalize(float64) map[string]any { return s.result }
func (s *parkedSession) Cancel()                         {}
func (s *parkedSession) Provider() string                { return "xai" }

func TestRecordingUsesTheLiveTranscriptAndFallsBackToTheFile(t *testing.T) {
	withVoiceHome(t, "")
	var uploads []string
	swapVoice(t, &voiceTranscribeAudio, func(_ context.Context, path, _, _ string) (map[string]any, error) {
		uploads = append(uploads, path)
		return map[string]any{"success": true, "transcript": "from file"}, nil
	})
	dir := t.TempDir()
	liveWAV, failedWAV := dir+"/a.wav", dir+"/b.wav"
	live := &parkedSession{result: map[string]any{"success": true, "transcript": "from live", "provider": "xai"}}
	parkLiveSession(liveWAV, live)
	if !live.ended {
		t.Fatal("parking ends the audio so the provider flushes while the caller works")
	}
	if result, _ := TranscribeRecording(context.Background(), liveWAV, ""); result["transcript"] != "from live" || len(uploads) != 0 {
		t.Fatalf("%v uploads %v", result, uploads)
	}
	parkLiveSession(failedWAV, &parkedSession{result: map[string]any{"success": false, "transcript": "", "error": "socket"}})
	if result, _ := TranscribeRecording(context.Background(), failedWAV, ""); result["transcript"] != "from file" || !slices.Equal(uploads, []string{failedWAV}) {
		t.Fatalf("%v uploads %v", result, uploads)
	}
}
