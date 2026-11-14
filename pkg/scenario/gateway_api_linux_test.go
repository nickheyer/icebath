package scenario

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

const apiKey = "scenario-api-key-0123456789abcdef"

// Starts a foreground gateway with the API server enabled on a free port and waits for its health route
func startAPIGateway(s *S) *Proc {
	s.t.Helper()
	s.Port("api")
	s.Write(".env", "API_SERVER_ENABLED=true\nAPI_SERVER_KEY="+apiKey+"\nAPI_SERVER_PORT=${PORT_api}\nAPI_SERVER_HOST=127.0.0.1\nAPI_SERVER_MODEL_NAME=scenario-agent\n")
	s.Set("API", "http://127.0.0.1:${PORT_api}")
	gw := s.Spawn("gateway", "run")
	s.WaitHTTP("${API}/health")
	s.Eventually("gateway_state.json reporting running", waitLimit, func() bool {
		var state map[string]any
		raw, err := os.ReadFile(s.Path("gateway_state.json"))
		return err == nil && json.Unmarshal(raw, &state) == nil && state["gateway_state"] == "running"
	})
	return gw
}

// The API server answers health, models, chat completions plain and streamed, and requires the bearer key
func TestScenarioGatewayAPIChatCompletions(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "Hello through the gateway."}, fakellm.Text{Text: "Streamed answer in pieces.", ChunkChars: 5}}})
	gw := startAPIGateway(s)
	auth := []string{"Authorization", "Bearer " + apiKey}

	for _, route := range []string{"/health", "/v1/health"} {
		health := s.HTTP("GET", "${API}"+route, nil).Code(200)
		s.HasJSON(health.Body, `{"status": "ok", "platform": "icebath-agent"}`)
		if version, _ := Dig(health.JSON(), "version").(string); version == "" {
			s.Failf("%s reports no version: %s", route, health.Body)
		}
	}
	s.HTTP("GET", "${API}/v1/models", nil).Code(401)
	s.HTTP("GET", "${API}/v1/models", nil, "Authorization", "Bearer wrong-key-0123456789abcdef").Code(401)
	s.HasJSON(s.HTTP("GET", "${API}/v1/models", nil, auth...).Code(200).Body, `{"object": "list", "data": [{"id": "scenario-agent", "object": "model"}]}`)

	body := map[string]any{"model": "scenario-agent", "messages": []any{
		map[string]any{"role": "system", "content": "You are terse."}, map[string]any{"role": "user", "content": "Say hello"}}}
	s.HTTP("POST", "${API}/v1/chat/completions", body).Code(401)
	if len(s.Model.MainRequests()) != 0 {
		s.Failf("a request without the key reached the model")
	}
	reply := s.HTTP("POST", "${API}/v1/chat/completions", body, auth...).Code(200)
	s.HasJSON(reply.Body, `{"object": "chat.completion", "model": "scenario-agent", "choices": [
		{"index": 0, "message": {"role": "assistant", "content": "Hello through the gateway."}, "finish_reason": "stop"}]}`)
	answer := reply.JSON()
	if id, _ := Dig(answer, "id").(string); !strings.HasPrefix(id, "chatcmpl-") {
		s.Failf("completion id %q does not start with chatcmpl-", id)
	}
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if _, ok := Dig(answer, "usage", key).(float64); !ok {
			s.Failf("usage lacks %s: %v", key, Dig(answer, "usage"))
		}
	}
	msgs := s.LastMessages()
	if Dig(msgs, -1, "role") != "user" || Dig(msgs, -1, "content") != "Say hello" {
		s.Failf("the model did not receive the user message last, got %v", Dig(msgs, -1))
	}
	if system, _ := Dig(msgs, 0, "content").(string); Dig(msgs, 0, "role") != "system" || !strings.Contains(system, "You are terse.") {
		s.Failf("the caller's system message is not part of the system prompt")
	}

	body["stream"] = true
	stream := s.HTTP("POST", "${API}/v1/chat/completions", body, auth...).Code(200)
	if ct := stream.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		s.Failf("a streamed completion has content type %q", ct)
	}
	events := stream.Events()
	if len(events) < 3 || events[len(events)-1].Data != "[DONE]" {
		s.Failf("the stream does not end with [DONE]\n%s", clip(stream.Body))
	}
	var text strings.Builder
	finish := ""
	for _, ev := range events[:len(events)-1] {
		if ev.Name != "" {
			continue
		}
		chunk := s.value(ev.Data)
		if Dig(chunk, "object") != "chat.completion.chunk" {
			s.Failf("stream chunk object is %v", Dig(chunk, "object"))
		}
		if piece, ok := Dig(chunk, "choices", 0, "delta", "content").(string); ok {
			text.WriteString(piece)
		}
		if reason, ok := Dig(chunk, "choices", 0, "finish_reason").(string); ok {
			finish = reason
		}
	}
	if text.String() != "Streamed answer in pieces." || finish != "stop" {
		s.Failf("streamed text %q finish %q\n%s", text.String(), finish, clip(stream.Body))
	}
	gw.Stop()
}

// A run is created, streams its lifecycle as server sent events, and reports its output when polled
func TestScenarioGatewayAPIRuns(t *testing.T) {
	s := Start(t, Setup{Model: []any{
		fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "echo run-tool-output"}},
		fakellm.Text{Text: "The run finished."},
	}})
	gw := startAPIGateway(s)
	auth := []string{"Authorization", "Bearer " + apiKey}

	s.HTTP("POST", "${API}/v1/runs", map[string]any{"input": "Run the command"}).Code(401)
	created := s.HTTP("POST", "${API}/v1/runs", map[string]any{"input": "Run the command"}, auth...).Code(202)
	s.HasJSON(created.Body, `{"status": "started"}`)
	run, _ := Dig(created.JSON(), "run_id").(string)
	if !strings.HasPrefix(run, "run_") {
		s.Failf("run id %q does not start with run_", run)
	}
	stream := s.HTTP("GET", "${API}/v1/runs/"+run+"/events", nil, auth...).Code(200)
	seen := map[string]map[string]any{}
	var order []string
	for _, ev := range stream.Events() {
		payload, ok := s.value(ev.Data).(map[string]any)
		if !ok {
			continue
		}
		name, _ := payload["event"].(string)
		if _, dup := seen[name]; !dup {
			order = append(order, name)
		}
		seen[name] = payload
	}
	for _, want := range []string{"tool.started", "tool.completed", "run.completed"} {
		if seen[want] == nil {
			s.Failf("the event stream lacks %s, saw %v\n%s", want, order, clip(stream.Body))
		}
	}
	s.HasJSON(seen["tool.started"], `{"tool": "terminal"}`)
	s.HasJSON(seen["tool.completed"], `{"tool": "terminal", "error": false}`)
	s.HasJSON(seen["run.completed"], `{"run_id": "`+run+`", "output": "The run finished."}`)
	if out := s.ToolResult("terminal"); !strings.Contains(out, "run-tool-output") {
		s.Failf("the terminal tool result did not reach the model: %s", out)
	}
	s.Eventually("the run status completed", waitLimit, func() bool {
		return Dig(s.HTTP("GET", "${API}/v1/runs/"+run, nil, auth...).Code(200).JSON(), "status") == "completed"
	})
	s.HasJSON(s.HTTP("GET", "${API}/v1/runs/"+run, nil, auth...).Code(200).Body,
		`{"object": "hermes.run", "run_id": "`+run+`", "status": "completed", "output": "The run finished."}`)
	s.HTTP("GET", "${API}/v1/runs/run_does_not_exist", nil, auth...).Code(404)
	gw.Stop()
}
