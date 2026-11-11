package app

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

// Runs a stream json chat query on an owner fake, its exit code and JSONL records
func runStreamJSONChat(t *testing.T, runTurn func(p *fakeGatewayPeer) (map[string]any, bool), credentialsOK bool) (int, []map[string]any) {
	t.Helper()
	tempHome(t)
	chdirFor(t, t.TempDir())
	out, _ := captureStreams(t)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		switch method {
		case "runtime.describe":
			return map[string]any{"session_create": map[string]any{"sources": []any{"cli"}, "parameters": []any{"cwd", "model", "request_id", "source"}}}, nil
		case "session.create":
			return map[string]any{"stored_session_id": "session-123", "info": map[string]any{"model": "test-model"}}, nil
		case "prompt.receipt":
			return map[string]any{"status": "terminal", "result": map[string]any{"completed": true, "final_response": "hello"}}, nil
		}
		if method != "prompt.submit" {
			t.Fatalf("unexpected %s", method)
		}
		payload, completes := runTurn(p)
		if !completes {
			return nil, errFakeBlock
		}
		payload["admission_id"] = "adm-1"
		p.emit(map[string]any{"type": "message.complete", "session_id": "session-123", "admission_id": "adm-1", "payload": payload})
		return map[string]any{"admission_id": "adm-1"}, nil
	})
	streamJSON, code := StreamJSONRequested(StreamJSONArgs{OutputFormat: "stream-json", Query: "hello"})
	if !streamJSON || code != 0 {
		t.Fatalf("stream json not requested: %v %d", streamJSON, code)
	}
	ensure := func() (bool, error) { return credentialsOK, nil }
	args := map[string]any{"query": "hello", "output_format": "stream-json", "quiet": true}
	code = launchFromArgs(context.Background(), args, ensure, peer.connector())
	return code, jsonlRecords(t, out.String())
}

// Turn that streams text and one tool call before completing
func okStreamTurn(p *fakeGatewayPeer) (map[string]any, bool) {
	emit := func(kind string, payload map[string]any) {
		p.emit(map[string]any{"type": kind, "session_id": "session-123", "admission_id": "adm-1", "payload": payload})
	}
	emit("message.delta", map[string]any{"text": "hello"})
	emit("tool.start", map[string]any{"tool_call_id": "c1", "tool_name": "read_file", "args": map[string]any{"path": "f"}})
	emit("tool.complete", map[string]any{"tool_call_id": "c1", "tool_name": "read_file", "args": map[string]any{"path": "f"},
		"is_error": false, "result": "contents"})
	return map[string]any{"text": "hello", "outcome": "completed"}, true
}

// Turn interrupted by the user at the terminal while the owner works
func interruptedStreamTurn(p *fakeGatewayPeer) (map[string]any, bool) {
	self, err := os.FindProcess(os.Getpid())
	if err == nil {
		_ = self.Signal(os.Interrupt)
	}
	return nil, false
}

func TestChatStreamJSONImpliesQuietAndClosesWithResult(t *testing.T) {
	cases := []struct {
		turn          func(p *fakeGatewayPeer) (map[string]any, bool)
		credentialsOK bool
		exitCode      int
		types         []string
	}{
		{okStreamTurn, true, 0, []string{"system", "text", "tool_use", "tool_result", "result"}},
		{interruptedStreamTurn, true, 130, []string{"system", "result"}},
		{okStreamTurn, false, 1, []string{"system", "result"}},
	}
	for i, tc := range cases {
		code, events := runStreamJSONChat(t, tc.turn, tc.credentialsOK)
		var types []string
		for _, e := range events {
			types = append(types, e["type"].(string))
		}
		if code != tc.exitCode || !slices.Equal(types, tc.types) {
			t.Fatalf("case %d: code %d types %v", i, code, types)
		}
		last := events[len(events)-1]
		want := ""
		if tc.credentialsOK {
			want = "session-123"
		}
		if last["exit_code"] != float64(tc.exitCode) || last["session_id"] != want {
			t.Fatalf("case %d: result %v", i, last)
		}
	}
}

func TestChatStreamJSONRejectsInteractiveCombinations(t *testing.T) {
	cases := []struct {
		args    StreamJSONArgs
		message string
	}{
		{StreamJSONArgs{OutputFormat: "stream-json"}, "requires -q/--query"},
		{StreamJSONArgs{OutputFormat: "stream-json", Query: "hi", TUI: true}, "cannot be used with --tui"},
		{StreamJSONArgs{OutputFormat: "stream-json", Query: "hi", TUI: true}, "cannot be used with --tui"},
	}
	for _, tc := range cases {
		_, errOut := captureStreams(t)
		_, code := StreamJSONRequested(tc.args)
		if code != 2 || !strings.Contains(errOut.String(), tc.message) {
			t.Fatalf("%v: code %d stderr %q", tc.args, code, errOut.String())
		}
	}
}
