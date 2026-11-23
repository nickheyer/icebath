package tuihost

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
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

func writeHomeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
