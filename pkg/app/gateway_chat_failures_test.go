package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Owner fake for a fresh one shot session whose turn settles with the given text
func oneshotSessionPeer(t *testing.T, submitted *[]string) *fakeGatewayPeer {
	return newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		switch method {
		case "runtime.describe":
			return map[string]any{"session_create": map[string]any{"sources": []any{"cli", "oneshot"},
				"parameters": []any{"cwd", "request_id", "source"}}}, nil
		case "session.create":
			return map[string]any{"stored_session_id": "stored", "execution_generation": 1}, nil
		case "prompt.submit":
			*submitted = append(*submitted, params["text"].(string))
			p.emit(map[string]any{"type": "message.complete", "session_id": "stored", "admission_id": "mine",
				"payload": map[string]any{"text": "ok", "outcome": "completed"}})
			return map[string]any{"admission_id": "mine"}, nil
		case "prompt.receipt":
			return map[string]any{"status": "terminal", "result": map[string]any{"completed": true}}, nil
		}
		t.Fatalf("unexpected %s", method)
		return nil, nil
	})
}

func TestDirectQueryAliasSurvivesNoninteractiveLaunch(t *testing.T) {
	tempHome(t)
	captureStreams(t)
	var seen []string
	peer := oneshotSessionPeer(t, &seen)
	ensure := func() (bool, error) { return true, nil }
	if code := launchFromArgs(context.Background(), map[string]any{"q": "literal"}, ensure, peer.connector()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(seen) != 1 || seen[0] != "literal" {
		t.Fatalf("submitted %q", seen)
	}
}

func TestExplicitRemoteFailureNeverEnsuresLocal(t *testing.T) {
	tempHome(t)
	unavailable, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := unavailable.Addr().(*net.TCPAddr).Port
	unavailable.Close()
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws://127.0.0.1:"+strconv.Itoa(port)+"/api/ws?token=private-test-value")
	_, err = gateway.ConnectGateway(context.Background())
	var refused *gateway.GatewayClientError
	if !errors.As(err, &refused) || !strings.Contains(refused.Msg, "no local fallback") {
		t.Fatalf("connect error %v", err)
	}
	if strings.Contains(err.Error(), "private-test-value") {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestOneshotMatchesOwnTerminalReceiptNotNeighbor(t *testing.T) {
	out, _ := captureStreams(t)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		if method == "prompt.receipt" {
			if params["admission_id"] != "mine" {
				t.Fatalf("receipt of %v", params["admission_id"])
			}
			return map[string]any{"status": "terminal"}, nil
		}
		if method != "prompt.submit" {
			t.Fatalf("unexpected %s", method)
		}
		for _, pair := range [][2]string{{"neighbor", "completed"}, {"mine", "failed"}} {
			p.emit(map[string]any{"type": "message.complete", "session_id": "stored", "admission_id": pair[0],
				"payload": map[string]any{"text": pair[0], "outcome": pair[1]}})
		}
		return map[string]any{"admission_id": "mine"}, nil
	})
	view := newGatewayChatView(peer, map[string]any{"stored_session_id": "stored"}, true, nil, "")
	code, err := runViewWithin(t, view, "query", true)
	if err != nil || code != 1 {
		t.Fatalf("code %d err %v", code, err)
	}
	if out.String() != "mine\n" {
		t.Fatalf("stdout %q", out.String())
	}
}

// Every non empty stdout line decoded as one JSONL record
func jsonlRecords(t *testing.T, text string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("bad JSONL %q: %v", line, err)
		}
		records = append(records, row)
	}
	return records
}

func TestStreamJSONOneshotFiltersNeighborEventsThatArriveBeforeSubmitReceipt(t *testing.T) {
	out, _ := captureStreams(t)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		if method == "prompt.receipt" {
			return map[string]any{"status": "terminal", "result": map[string]any{"completed": true, "final_response": "right"}}, nil
		}
		for _, pair := range [][2]string{{"neighbor", "wrong"}, {"mine", "right"}} {
			admission, text := pair[0], pair[1]
			p.emit(map[string]any{"type": "message.delta", "session_id": "stored", "admission_id": admission, "payload": map[string]any{"text": text}})
			p.emit(map[string]any{"type": "tool.start", "session_id": "stored", "admission_id": admission,
				"payload": map[string]any{"tool_call_id": admission + "-tool", "tool_name": "read_file", "args": map[string]any{}}})
			p.emit(map[string]any{"type": "tool.complete", "session_id": "stored", "admission_id": admission,
				"payload": map[string]any{"tool_call_id": admission + "-tool", "tool_name": "read_file", "args": map[string]any{}, "result": text, "is_error": false}})
			p.emit(map[string]any{"type": "message.complete", "session_id": "stored", "admission_id": admission,
				"payload": map[string]any{"text": text, "outcome": "completed"}})
		}
		p.drained()
		return map[string]any{"admission_id": "mine"}, nil
	})
	emitter := NewStreamJSONEmitter("m", "stored")
	view := newGatewayChatView(peer, map[string]any{"stored_session_id": "stored"}, false, emitter, "")
	code, err := runViewWithin(t, view, "query", true)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	records := jsonlRecords(t, out.String())
	text := ""
	var toolIDs []any
	for _, row := range records {
		if row["type"] == "text" {
			text += row["text"].(string)
		}
		if row["type"] == "tool_use" || row["type"] == "tool_result" {
			toolIDs = append(toolIDs, row["tool_call_id"])
		}
		raw, _ := json.Marshal(row)
		if strings.Contains(string(raw), "wrong") || strings.Contains(string(raw), "neighbor-tool") {
			t.Fatalf("neighbor output leaked: %s", raw)
		}
	}
	if text != "right" || len(toolIDs) != 2 || toolIDs[0] != "mine-tool" || toolIDs[1] != "mine-tool" {
		t.Fatalf("text %q tool ids %v", text, toolIDs)
	}
}

// Owner fake whose turn settles with outcome while its committed result says result
func settledPeer(t *testing.T, outcome string, result map[string]any) *fakeGatewayPeer {
	return newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		if method == "prompt.receipt" {
			if len(params) != 3 || params["session_id"] != "stored" || params["admission_id"] != "mine" || params["include_result"] != true {
				t.Fatalf("receipt params %v", params)
			}
			return map[string]any{"status": "terminal", "outcome": outcome, "result": result}, nil
		}
		if method != "prompt.submit" {
			t.Fatalf("unexpected %s", method)
		}
		text, _ := result["final_response"].(string)
		p.emit(map[string]any{"type": "message.complete", "session_id": "stored", "admission_id": "mine",
			"payload": map[string]any{"text": text, "outcome": outcome}})
		return map[string]any{"admission_id": "mine"}, nil
	})
}

// Committed result of a turn the iteration budget stopped
func budgetResult() map[string]any {
	return map[string]any{"completed": false, "failed": false, "interrupted": false, "partial": false,
		"turn_exit_reason": "max_iterations_reached(1/1)",
		"final_response":   "I reached the iteration limit and couldn't generate a summary."}
}

func TestFiniteExitCodeFollowsTheCommittedResultNotTheOutcome(t *testing.T) {
	cases := []struct {
		outcome         string
		result          map[string]any
		chatCode, zCode int
	}{
		{"completed", budgetResult(), 1, 2},
		{"completed", map[string]any{"completed": true, "final_response": "done"}, 0, 0},
		{"cancelled", map[string]any{"interrupted": true, "final_response": ""}, 130, 130},
	}
	for _, tc := range cases {
		out, _ := captureStreams(t)
		for _, run := range []struct {
			unattended bool
			expected   int
		}{{false, tc.chatCode}, {true, tc.zCode}} {
			view := newGatewayChatView(settledPeer(t, tc.outcome, tc.result), map[string]any{"stored_session_id": "stored"}, true, nil, "")
			view.unattended = run.unattended
			code, err := runViewWithin(t, view, "query", true)
			if err != nil || code != run.expected {
				t.Fatalf("%s unattended=%v code %d err %v", tc.outcome, run.unattended, code, err)
			}
		}
		final := tc.result["final_response"].(string) + "\n"
		if out.String() != final+final {
			t.Fatalf("stdout %q", out.String())
		}
	}
}

func TestTextlessOneshotNamesTheFailureOnStderr(t *testing.T) {
	_, errOut := captureStreams(t)
	view := newGatewayChatView(settledPeer(t, "completed", map[string]any{"completed": true, "final_response": ""}),
		map[string]any{"stored_session_id": "stored"}, true, nil, "")
	view.unattended = true
	code, err := runViewWithin(t, view, "query", true)
	if err != nil || code != 1 {
		t.Fatalf("code %d err %v", code, err)
	}
	if !strings.Contains(errOut.String(), "icebath -z: no final response was produced") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestStreamJSONResultRecordReportsAnUnfinishedTurn(t *testing.T) {
	out, _ := captureStreams(t)
	result := budgetResult()
	result["input_tokens"], result["output_tokens"], result["total_tokens"] = 10, 5, 15
	view := newGatewayChatView(settledPeer(t, "completed", result), map[string]any{"stored_session_id": "stored"}, false,
		NewStreamJSONEmitter("m", "stored"), "")
	code, err := runViewWithin(t, view, "query", true)
	if err != nil || code != 1 {
		t.Fatalf("code %d err %v", code, err)
	}
	records := jsonlRecords(t, out.String())
	record := records[len(records)-1]
	if record["type"] != "result" || record["exit_code"] != float64(1) {
		t.Fatalf("record %v", record)
	}
	tokens := record["tokens"].(map[string]any)
	if tokens["total"] != float64(15) || record["text"] != budgetResult()["final_response"] {
		t.Fatalf("record %v", record)
	}
}

func TestOneshotReplayGapFailsInsteadOfWaitingForever(t *testing.T) {
	captureStreams(t)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		if method != "prompt.submit" {
			return nil, errors.New("AssertionError: " + method)
		}
		p.emit(map[string]any{"type": "session.replay_gap", "session_id": "stored", "payload": map[string]any{"reason": "subscriber_overflow"}})
		return map[string]any{"admission_id": "mine"}, nil
	})
	view := newGatewayChatView(peer, map[string]any{"stored_session_id": "stored"}, true, nil, "")
	_, err := runViewWithin(t, view, "query", true)
	var refused *gateway.GatewayClientError
	if !errors.As(err, &refused) || !strings.Contains(refused.Msg, "session_replay_gap") {
		t.Fatalf("err %v", err)
	}
}

func TestOneshotRefusesUnknownBlockedSessionBeforeSubmitting(t *testing.T) {
	_, errOut := captureStreams(t)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		t.Fatalf("nothing may be submitted behind an unknown row: %s", method)
		return nil, nil
	})
	lost := "admission-unknown-0123456789abcdef"
	snapshot := map[string]any{"stored_session_id": "stored", "pending": []any{map[string]any{"admission_id": lost, "status": "unknown", "execution_generation": int64(4)}}}
	code, err := runViewWithin(t, newGatewayChatView(peer, snapshot, true, nil, ""), "pong", true)
	if err != nil || code != 3 {
		t.Fatalf("code %d err %v", code, err)
	}
	text := errOut.String()
	if !strings.Contains(text, "icebath sessions discard stored --yes") {
		t.Fatalf("stderr %q", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "/discard") {
			t.Fatalf("one shot printed a discard line: %q", text)
		}
	}
}

func TestStreamJSONOneshotAlwaysClosesWithAResultRecord(t *testing.T) {
	for _, blocker := range []string{"unknown_row", "approval_prompt"} {
		out, _ := captureStreams(t)
		peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
			if blocker != "approval_prompt" || method != "prompt.submit" {
				t.Fatalf("%s: unexpected %s", blocker, method)
			}
			p.emit(map[string]any{"type": "approval.request", "session_id": "stored", "admission_id": "mine",
				"payload": map[string]any{"prompt_id": "p1", "kind": "approval", "command": "rm -rf /", "choices": []any{"yes", "no"},
					"execution_generation": 1}})
			return map[string]any{"admission_id": "mine"}, nil
		})
		pending := []any{}
		if blocker == "unknown_row" {
			pending = append(pending, map[string]any{"admission_id": "lost", "status": "unknown", "execution_generation": int64(4)})
		}
		emitter := NewStreamJSONEmitter("m", "stored")
		view := newGatewayChatView(peer, map[string]any{"stored_session_id": "stored", "pending": pending}, false, emitter, "")
		code, err := runViewWithin(t, view, "pong", true)
		if err != nil || code != 3 {
			t.Fatalf("%s: code %d err %v", blocker, code, err)
		}
		records := jsonlRecords(t, out.String())
		last := records[len(records)-1]
		if records[0]["type"] != "system" || last["type"] != "result" || last["exit_code"] != float64(3) || last["error"] == "" || last["error"] == nil {
			t.Fatalf("%s: records %v", blocker, records)
		}
		results := 0
		for _, r := range records {
			if r["type"] == "result" {
				results++
			}
		}
		if results != 1 {
			t.Fatalf("%s: %d result records", blocker, results)
		}
	}
}
