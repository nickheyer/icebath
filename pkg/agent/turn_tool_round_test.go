package agent

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func roundToolCall(id, n string) state.ToolCall {
	return state.ToolCall{ID: id, Type: "function", Function: state.ToolCallFunction{Name: "read_file", Arguments: `{"path": "` + n + `"}`}}
}

func TestFinalStagedBatchNeverReachesWireAllProviderPrefixed(t *testing.T) {
	shapes := map[string]func([]state.ToolCall) []state.ToolCall{
		"plain":  func(c []state.ToolCall) []state.ToolCall { return c },
		"padded": func(c []state.ToolCall) []state.ToolCall { c[0].ID = " chatcmpl-tool-a"; return c },
		"blank_call_id": func(c []state.ToolCall) []state.ToolCall {
			c[0].Extra = map[string]any{"call_id": " "}
			return c
		},
		"mixed_until_dedup": func(c []state.ToolCall) []state.ToolCall { return append(c, roundToolCall("call_c", "a")) },
	}
	errStaged := errors.New("staged")
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			var staged []state.ToolCall
			orig := stageToolCallMessage
			stageToolCallMessage = func(a *AIAgent, am *provider.Response, _ string, _ *[]*state.Message) (map[string]any, bool, error) {
				staged = append(staged, am.Message.ToolCalls...)
				return nil, false, errStaged
			}
			t.Cleanup(func() { stageToolCallMessage = orig })
			a := &AIAgent{provider: "nous", model: "m", sessionID: "s1", validToolNames: map[string]bool{"read_file": true}, quietMode: true}
			calls := shape([]state.ToolCall{roundToolCall("chatcmpl-tool-a", "a"), roundToolCall("chatcmpl-tool-b", "b")})
			s := &LoopState{
				AssistantMessage: &provider.Response{Message: &state.Message{Role: "assistant", Content: "", ToolCalls: calls}},
				FinishReason:     "tool_calls", APICallCount: 1, EffectiveTaskID: "t", UserMessage: "read",
			}
			if _, err := runToolRound(context.Background(), a, s); !errors.Is(err, errStaged) {
				t.Fatalf("err %v", err)
			}
			rows := make([]any, len(staged))
			msgs := []map[string]any{{"role": "assistant", "content": ""}}
			for i, tc := range staged {
				rows[i] = a.assistantToolCallDict(tc, i)
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": state.CoalesceToolCallID(tc), "content": "ok"})
			}
			msgs[0]["tool_calls"] = rows
			converted, err := provider.GetTransport("chat_completions").ConvertMessages(msgs, nil)
			if err != nil {
				t.Fatal(err)
			}
			wire := converted.([]map[string]any)
			var ids []string
			for _, c := range wire[0]["tool_calls"].([]any) {
				ids = append(ids, c.(map[string]any)["id"].(string))
			}
			if len(ids) != 2 {
				t.Fatalf("ids %v", ids)
			}
			for i, m := range wire[1:] {
				if m["tool_call_id"] != ids[i] {
					t.Fatalf("result %d id %v, call ids %v", i, m["tool_call_id"], ids)
				}
			}
			for _, id := range ids {
				if strings.HasPrefix(id, "chatcmpl-tool-") {
					t.Fatalf("provider prefixed id on the wire %v", ids)
				}
			}
		})
	}
}

func TestAnEmptyFollowUpAfterACardRoundKeepsTheCardsLine(t *testing.T) {
	for _, c := range []struct {
		tool   string
		reused bool
	}{{"setup_beat", true}, {"terminal", false}} {
		t.Run(c.tool, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			a := &AIAgent{provider: "nous", model: "m", sessionID: "s1", quietMode: true, apiMode: "chat_completions"}
			line := "Pick a colour whenever you're back."
			messages := []*state.Message{{Role: "user", Content: "hi"}}
			call := state.ToolCall{ID: "call_" + c.tool, Type: "function", Function: state.ToolCallFunction{Name: c.tool, Arguments: "{}"}}
			staged, _, err := stageToolCallMessage(a, &provider.Response{Message: &state.Message{Content: line, ToolCalls: []state.ToolCall{call}}},
				"tool_calls", &messages)
			if err != nil {
				t.Fatal(err)
			}
			messages = append(messages, state.MessageFromDict(staged),
				&state.Message{Role: "tool", ToolCallID: "call_" + c.tool, Content: "{}"})
			s := &LoopState{Messages: messages, APICallCount: 2, TurnExitReason: nil}
			empty := &provider.Response{Message: &state.Message{Content: ""}, FinishReason: "stop"}
			v, err := recoverEmptyResponse(context.Background(), a, s, empty, "stop", "")
			if err != nil {
				t.Fatal(err)
			}
			if c.reused {
				if v.Action != "break" || s.FinalResponse != line || s.TurnExitReason != "fallback_prior_turn_content" {
					t.Fatalf("got %q %v %v", v.Action, s.FinalResponse, s.TurnExitReason)
				}
			} else if s.FinalResponse == line {
				t.Fatalf("terminal round reused the line")
			}
			if a.mutePostResponse {
				t.Fatal("post response muted")
			}
		})
	}
}

func TestStageToolCallMessageDropsABareMarker(t *testing.T) {
	a := &AIAgent{quietMode: true}
	var messages []*state.Message
	call := state.ToolCall{ID: "call_1", Type: "function", Function: state.ToolCallFunction{Name: "memory", Arguments: "{}"}}
	msg, _, err := stageToolCallMessage(a, &provider.Response{Message: &state.Message{Content: " [memory] ", ToolCalls: []state.ToolCall{call}}}, "tool_calls", &messages)
	if err != nil {
		t.Fatal(err)
	}
	if msg["content"] != "" || a.lastContentWithTools != nil {
		t.Fatalf("marker kept %v %v", msg["content"], a.lastContentWithTools)
	}
}

func mintCall(rawID string, n int, callID string) state.ToolCall {
	tc := roundToolCall(rawID, strconv.Itoa(n))
	if callID != "" {
		tc.Extra = map[string]any{"call_id": callID}
	}
	return tc
}

func mintAgent() *AIAgent {
	return &AIAgent{provider: "nous", model: "m", sessionID: "s1", validToolNames: map[string]bool{"read_file": true}}
}

// Runs the agent validation mint path and returns the rows a store and replay would carry
func mintRows(t *testing.T, a *AIAgent, resp *provider.Response) []map[string]any {
	t.Helper()
	s := &LoopState{APICallCount: 1, EffectiveTaskID: "t1"}
	v, err := validateRoundToolCalls(context.Background(), a, s, resp, "tool_calls")
	if err != nil || v.Action != "ok" {
		t.Fatalf("verdict %q %v", v.Action, err)
	}
	var stored []map[string]any
	for i, tc := range resp.Message.ToolCalls {
		row := a.assistantToolCallDict(tc, i)
		if row["id"] != state.CoalesceToolCallID(tc) {
			t.Fatalf("row id %v, result id %v", row["id"], state.CoalesceToolCallID(tc))
		}
		stored = append(stored, row)
	}
	return stored
}

func mint(t *testing.T, calls []state.ToolCall) []map[string]any {
	t.Helper()
	return mintRows(t, mintAgent(), &provider.Response{Message: &state.Message{Content: "", ToolCalls: calls}})
}

func mintIDs(t *testing.T, raw []string) []string {
	t.Helper()
	calls := make([]state.ToolCall, len(raw))
	for n, id := range raw {
		calls[n] = mintCall(id, n, "")
	}
	var ids []string
	for _, row := range mint(t, calls) {
		ids = append(ids, row["id"].(string))
	}
	return ids
}

func distinctCallIDs(t *testing.T, ids []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, id := range ids {
		if !strings.HasPrefix(id, "call_") || seen[id] {
			t.Fatalf("ids %q", ids)
		}
		seen[id] = true
	}
}

func TestParallelProviderIdsAreRewrittenStably(t *testing.T) {
	raw := []string{"chatcmpl-tool-aaa", "chatcmpl-tool-bbb"}
	ids := mintIDs(t, raw)
	if len(ids) != 2 {
		t.Fatalf("ids %q", ids)
	}
	distinctCallIDs(t, ids)
	if again := mintIDs(t, raw); !reflect.DeepEqual(again, ids) {
		t.Fatalf("re-mint %q, first %q", again, ids)
	}
}

func TestSingleMixedAndOrdinaryBatchesAreUntouched(t *testing.T) {
	for _, raw := range [][]string{{"chatcmpl-tool-aaa"}, {"chatcmpl-tool-aaa", "call_1"}, {"call_0", "call_1"}} {
		if ids := mintIDs(t, raw); !reflect.DeepEqual(ids, raw) {
			t.Fatalf("%q minted %q", raw, ids)
		}
	}
}

func TestResponseItemHalfSurvivesWhenOnlyIdIsComposite(t *testing.T) {
	for name, keys := range map[string][2]string{"distinct": {"a", "b"}, "duplicate": {"a", "a"}} {
		t.Run(name, func(t *testing.T) {
			var calls []state.ToolCall
			for n, k := range keys {
				calls = append(calls, mintCall("chatcmpl-tool-"+k+"|fc_original_"+strconv.Itoa(n), n, "chatcmpl-tool-"+k))
			}
			stored := mint(t, calls)
			var ids []string
			for n, row := range stored {
				if row["response_item_id"] != "fc_original_"+strconv.Itoa(n) {
					t.Fatalf("row %d response item %v", n, row["response_item_id"])
				}
				ids = append(ids, row["id"].(string))
			}
			distinctCallIDs(t, ids)
		})
	}
}

func TestRealTransportToolCallsNormalizeWithoutCrashing(t *testing.T) {
	payloads := map[string]struct {
		mode    string
		payload map[string]any
	}{
		"responses": {"codex_responses", map[string]any{"status": "completed", "output": []any{
			map[string]any{"type": "function_call", "id": "fc_0", "call_id": "chatcmpl-tool-0", "name": "read_file", "arguments": "{}"},
			map[string]any{"type": "function_call", "id": "fc_1", "call_id": "chatcmpl-tool-1", "name": "read_file", "arguments": "{}"},
		}}},
		"chat": {"chat_completions", map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{
			"content": "", "tool_calls": []any{
				map[string]any{"id": "chatcmpl-tool-0", "function": map[string]any{"name": "read_file", "arguments": "{}"}},
				map[string]any{"id": "chatcmpl-tool-1", "function": map[string]any{"name": "read_file", "arguments": "{}"}},
			}}}}}},
	}
	for api, c := range payloads {
		t.Run(api, func(t *testing.T) {
			resp, err := provider.GetTransport(c.mode).NormalizeResponse(c.payload, nil)
			if err != nil {
				t.Fatal(err)
			}
			rows := mintRows(t, mintAgent(), resp)
			var ids []string
			for _, row := range rows {
				ids = append(ids, row["id"].(string))
			}
			if len(ids) != 2 {
				t.Fatalf("ids %q", ids)
			}
			distinctCallIDs(t, ids)
			if api == "responses" {
				for n, row := range rows {
					if row["response_item_id"] != "fc_"+strconv.Itoa(n) {
						t.Fatalf("row %d response item %v", n, row["response_item_id"])
					}
					if got := resp.Message.ToolCalls[n].Extra["call_id"]; got != row["id"] {
						t.Fatalf("call %d call_id %v, row id %v", n, got, row["id"])
					}
				}
			}
		})
	}
}

func TestUnencodableProviderIdsDoNotCrashAndStayDistinct(t *testing.T) {
	ids := mintIDs(t, []string{"chatcmpl-tool-\xed\xa0\x80", "chatcmpl-tool-?"})
	if len(ids) != 2 {
		t.Fatalf("ids %q", ids)
	}
	distinctCallIDs(t, ids)
}

func TestRoundBatchExpansionRewritesTheOrderedBlockCarriers(t *testing.T) {
	batch := state.ToolCall{ID: "toolu_1", Type: "function", Function: state.ToolCallFunction{Name: tools.ToolCallName,
		Arguments: `{"calls": [{"name": "read_file", "arguments": {"path": "a"}}, {"name": "read_file", "arguments": {"path": "b"}}]}`}}
	msg := &state.Message{Role: "assistant", ToolCalls: []state.ToolCall{batch}, Extra: map[string]any{"bedrock_content_blocks": []any{
		map[string]any{"toolUse": map[string]any{"toolUseId": "toolu_1", "name": tools.ToolCallName, "input": map[string]any{}}},
	}}}
	provider.SetAnthropicLiveBlocks(msg, []any{
		map[string]any{"type": "thinking", "thinking": "plan", "signature": "sig"},
		map[string]any{"type": "tool_use", "id": "toolu_1", "name": tools.ToolCallName, "input": map[string]any{}},
	})
	expandRoundToolBatches(msg)
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("calls %+v", msg.ToolCalls)
	}
	blocks, _ := provider.AnthropicLiveBlocks(msg)
	if len(blocks) != 3 || blocks[0].(map[string]any)["type"] != "thinking" {
		t.Fatalf("anthropic blocks %v", blocks)
	}
	bedrock := msg.Extra["bedrock_content_blocks"].([]any)
	if len(bedrock) != 2 {
		t.Fatalf("bedrock blocks %v", bedrock)
	}
	for i, tc := range msg.ToolCalls {
		id := state.CoalesceToolCallID(tc)
		if blocks[i+1].(map[string]any)["id"] != id || bedrock[i].(map[string]any)["toolUse"].(map[string]any)["toolUseId"] != id {
			t.Fatalf("call %d id %s, anthropic %v, bedrock %v", i, id, blocks[i+1], bedrock[i])
		}
	}
}
