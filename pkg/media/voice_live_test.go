package media

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Home whose config holds the voice section
func voiceLiveHome(t *testing.T, voiceYAML string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOICE_TOOLS_OPENAI_KEY", "")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(voiceYAML), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClientDelegationAndKeyStayServerSide(t *testing.T) {
	captured := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		captured["url"] = r.URL.Path
		captured["auth"] = r.Header.Get("Authorization")
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		captured["body"] = body
		_, _ = w.Write([]byte(`{"session": {"id": "live_x"}, "transport": {"type": "webrtc", "sdp": "answer"}}`))
	}))
	defer srv.Close()
	voiceLiveHome(t, "voice:\n  gpt_live:\n    voice: willow\n    instructions: Speak Spanish.\n    api_key: sk-test\n    base_url: "+srv.URL+"/v1\n")

	result, err := CreateWebRTCSession(context.Background(), "v=0 offer",
		[]any{pyjson.ObjectOf("type", "message", "role", "user", "content", []any{})})
	if err != nil {
		t.Fatal(err)
	}
	answer := pyjson.Plain(result).(map[string]any)
	if answer["transport"].(map[string]any)["sdp"] != "answer" {
		t.Fatalf("answer %v", answer)
	}
	if captured["url"] != "/v1/live/sessions" || captured["auth"] != "Bearer sk-test" {
		t.Fatalf("captured %v", captured)
	}
	body := captured["body"].(map[string]any)
	session := body["session"].(map[string]any)
	if session["delegation"].(map[string]any)["type"] != "client" || session["audio"].(map[string]any)["output"].(map[string]any)["voice"] != "willow" ||
		!strings.HasSuffix(session["instructions"].(string), "Speak Spanish.") || session["input"].([]any)[0].(map[string]any)["role"] != "user" {
		t.Fatalf("session %v", session)
	}
	transport := body["transport"].(map[string]any)
	if transport["type"] != "webrtc" || transport["sdp"] != "v=0 offer" || len(transport) != 2 {
		t.Fatalf("transport %v", transport)
	}
	dumped, _ := pyjson.Dumps(result)
	if strings.Contains(dumped, "sk-test") {
		t.Fatal("key reached the client answer")
	}
}

func TestMissingKeyRefusesBeforeAnyNetwork(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	voiceLiveHome(t, "voice:\n  gpt_live:\n    base_url: "+srv.URL+"\n")

	_, err := CreateWebRTCSession(context.Background(), "v=0 offer", nil)
	var valueErr *VoiceLiveValueError
	if !errors.As(err, &valueErr) || called {
		t.Fatalf("err %v called %v", err, called)
	}
	if ResolveGPTLiveStatus(context.Background()).GetOr("available", nil) != false {
		t.Fatal("available without a key")
	}
}
