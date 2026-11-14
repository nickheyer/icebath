package tuigateway

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Stdio frame stream of the case, the stdio TUI client with no WebSocket peers
type stdioFrames struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *stdioFrames) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// Event types and payloads written so far, cleared on take
func (s *stdioFrames) take(t *testing.T) ([]string, []map[string]any) {
	t.Helper()
	s.mu.Lock()
	text := s.buf.String()
	s.buf.Reset()
	s.mu.Unlock()
	var events []string
	var payloads []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("frame %q: %v", line, err)
		}
		params, _ := frame["params"].(map[string]any)
		payload, _ := params["payload"].(map[string]any)
		events = append(events, params["type"].(string))
		payloads = append(payloads, payload)
	}
	return events, payloads
}

// Home with a fresh skin baseline, no live peers and stdout as the only client
func skinStdioHome(t *testing.T) (string, *stdioFrames) {
	t.Helper()
	h := t.TempDir()
	if err := os.Mkdir(filepath.Join(h, "skins"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	skinBaselines.Lock()
	skinBaselines.last, skinBaselines.byHome = nil, map[string]SkinSignature{}
	skinBaselines.Unlock()
	liveTransportsMu.Lock()
	savedPeers := liveTransports
	liveTransports = nil
	liveTransportsMu.Unlock()
	savedChannel := stdioIsRPCChannel.Load()
	stdioIsRPCChannel.Store(true)
	frames := &stdioFrames{}
	restore := SwapRealStdoutForTests(frames)
	t.Cleanup(func() {
		restore()
		stdioIsRPCChannel.Store(savedChannel)
		liveTransportsMu.Lock()
		liveTransports = savedPeers
		liveTransportsMu.Unlock()
		skinBaselines.Lock()
		skinBaselines.last, skinBaselines.byHome = nil, map[string]SkinSignature{}
		skinBaselines.Unlock()
	})
	return h, frames
}

func writeHomeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkinLiveSwitchEndToEnd(t *testing.T) {
	h, frames := skinStdioHome(t)
	writeHomeFile(t, filepath.Join(h, "skins", "midnight.yaml"),
		"name: midnight\ndescription: t\ncolors:\n  banner_title: '#00ffcc'\n  background: '#001010'\ncustomCSS: |\n  .chat-input { font-size: 16px; }\n")
	writeHomeFile(t, filepath.Join(h, "config.yaml"), "display:\n  skin: default\n")
	BroadcastSkinIfChanged(t.Context())
	frames.take(t)
	time.Sleep(10 * time.Millisecond)
	writeHomeFile(t, filepath.Join(h, "config.yaml"), "display:\n  skin: midnight\n")
	BroadcastSkinIfChanged(t.Context())
	events, payloads := frames.take(t)
	if len(events) != 1 || events[0] != "skin.changed" {
		t.Fatalf("events %v", events)
	}
	colors, _ := payloads[0]["colors"].(map[string]any)
	if payloads[0]["name"] != "midnight" || colors["banner_title"] != "#00ffcc" || payloads[0]["customCSS"] != ".chat-input { font-size: 16px; }" {
		t.Fatalf("payload %v", payloads[0])
	}
}

func TestBroadcastSkinIfChangedOnAnySignatureMove(t *testing.T) {
	h, frames := skinStdioHome(t)
	for _, name := range []string{"neon", "forest"} {
		writeHomeFile(t, filepath.Join(h, "skins", name+".yaml"), "name: "+name+"\ncolors: {}\n")
		stamp := time.Unix(1, 0)
		if err := os.Chtimes(filepath.Join(h, "skins", name+".yaml"), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	steps := []func(){
		func() { writeHomeFile(t, filepath.Join(h, "config.yaml"), "display:\n  skin: neon\n") },
		func() {},
		func() { writeHomeFile(t, filepath.Join(h, "config.yaml"), "display:\n  skin: forest\n") },
		func() {
			stamp := time.Unix(2, 0)
			if err := os.Chtimes(filepath.Join(h, "skins", "forest.yaml"), stamp, stamp); err != nil {
				t.Fatal(err)
			}
		},
	}
	for _, step := range steps {
		step()
		BroadcastSkinIfChanged(t.Context())
	}
	if events, _ := frames.take(t); len(events) != 3 || events[0] != "skin.changed" || events[1] != "skin.changed" || events[2] != "skin.changed" {
		t.Fatalf("events %v", events)
	}
}
