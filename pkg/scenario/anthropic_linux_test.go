package scenario

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Answers an Anthropic Messages request with one text block, streamed as events when the request asks for a stream
func anthropicMessagesPeer(text string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			http.NotFound(w, r)
			return
		}
		model, _ := body["model"].(string)
		if stream, _ := body["stream"].(bool); !stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_scenario", "type": "message", "role": "assistant", "model": model,
				"content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "end_turn",
				"usage": map[string]any{"input_tokens": 3, "output_tokens": 5}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(name string, data map[string]any) {
			data["type"] = name
			encoded, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded)
		}
		event("message_start", map[string]any{"message": map[string]any{"id": "msg_scenario", "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 0}}})
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
		event("content_block_stop", map[string]any{"index": 0})
		event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 5}})
		event("message_stop", map[string]any{})
	})
}

// A named provider on the Anthropic Messages wire whose key comes from a command sends each minted bearer to /v1/messages once
func TestScenarioAnthropicMessagesProviderWithKeyCommand(t *testing.T) {
	s := Start(t, Setup{})
	peer := s.Peer("anth", anthropicMessagesPeer("hello from the messages wire"))
	s.Write("config.yaml", s.Read("config.yaml")+"providers:\n  anth:\n    api: ${PEER_anth}/v1\n    api_mode: anthropic_messages\n"+
		"    key_cmd: printf minted-bearer\n    default_model: claude-scenario\n")

	answer := s.Run("-z", "Say hello", "--provider", "anth").Exit(0)
	if answer.Stdout != "hello from the messages wire\n" {
		s.Failf("the answer of the Messages endpoint did not reach the user\n%s", answer.dump())
	}
	sent := 0
	for _, request := range peer.Requests() {
		if !strings.HasSuffix(request.Path, "/messages") {
			continue
		}
		if request.Method != http.MethodPost || request.Path != "/v1/messages" {
			s.Failf("the endpoint base ends in /v1, so a Messages call is a post to /v1/messages, got %s %s", request.Method, request.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer minted-bearer" || request.Header.Get("X-Api-Key") != "" {
			s.Failf("a key command on a third party Messages host is sent as a bearer only, got Authorization %q and x-api-key %q",
				got, request.Header.Get("X-Api-Key"))
		}
		if request.Header.Get("Anthropic-Version") == "" || request.Header.Get("X-App") != "" {
			s.Failf("the request lacks anthropic-version or carries the Claude Code identity: %v", request.Header)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(request.Body), &body); err != nil {
			s.Failf("request body is not JSON: %s", request.Body)
		}
		if strings.Contains(request.Body, "Say hello") {
			sent++
			if body["model"] != "fake-model" {
				s.Failf("a provider pick without a model keeps the profile model fake-model, the turn used %v", body["model"])
			}
		}
	}
	if sent == 0 {
		s.Failf("no Messages request carried the question, requests %v", peer.Requests())
	}
}
