package agent

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const parityUni = "日本語テキスト🎉 café Ω ≈ 中文字符串"

// Synthetic user, assistant and tool cycles with malformed and empty arguments, unicode and image parts
func parityHistory(n int, rng *rand.Rand) []*state.Message {
	var msgs []*state.Message
	for i := 0; len(msgs) < n; i++ {
		msgs = append(msgs, &state.Message{Role: "user", Content: fmt.Sprintf("question %d %s ", i, parityUni) + strings.Repeat("x", 10+rng.Intn(391))})
		if i%3 == 0 {
			raw, _ := json.Marshal(map[string]any{"q": fmt.Sprintf("val %d", i), "u": parityUni, "n": i})
			args := string(raw)
			if i%9 == 0 {
				args = `{"broken": tru`
			} else if i%6 == 0 {
				args = ""
			}
			msgs = append(msgs, &state.Message{Role: "assistant", Content: "", ToolCalls: []state.ToolCall{{ID: fmt.Sprintf("call_%d", i), Type: "function",
				Function: state.ToolCallFunction{Name: "web_search", Arguments: args}}}},
				state.MessageFromDict(map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i), "name": "web_search", "content": fmt.Sprintf("result %d %s", i, parityUni)}))
		} else {
			msgs = append(msgs, &state.Message{Role: "assistant", Content: fmt.Sprintf("answer %d ", i) + strings.Repeat("y", 10+rng.Intn(591)), ReasoningContent: fmt.Sprintf("thinking %d", i)})
		}
		if i%7 == 0 {
			msgs[len(msgs)-1].Content = []any{map[string]any{"type": "text", "text": fmt.Sprintf("part %d", i)},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}}
		}
	}
	return msgs[:n]
}

// Rewrites the middle of the history with fresh copies around a summary row
func paritySimulateCompression(msgs *[]*state.Message) {
	head, tail := (*msgs)[:2], (*msgs)[len(*msgs)-6:]
	var out []*state.Message
	for _, m := range head {
		c := *m
		out = append(out, &c)
	}
	out = append(out, &state.Message{Role: "user", Content: "SUMMARY OF DROPPED CONTEXT " + parityUni})
	for _, m := range tail {
		c := *m
		out = append(out, &c)
	}
	*msgs = out
}

func parityEqual(a, b []*state.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(state.MessageToDict(a[i]), state.MessageToDict(b[i])) {
			return false
		}
	}
	return true
}

func TestParitySanitizeCursor(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, n := range []int{50, 200, 500} {
		oldList := parityHistory(n, rand.New(rand.NewSource(1234)))
		newList := parityHistory(n, rand.New(rand.NewSource(1234)))
		cursor := map[string]any{}
		for iteration := range 3 {
			rOld := sanitizeToolCallArguments(&oldList, nil, "", nil)
			rNew := sanitizeToolCallArguments(&newList, nil, "", cursor)
			if rOld != rNew || !parityEqual(oldList, newList) {
				t.Fatalf("n=%d it=%d %d %d", n, iteration, rOld, rNew)
			}
			for _, lst := range []*[]*state.Message{&oldList, &newList} {
				*lst = append(*lst, &state.Message{Role: "assistant", Content: "", ToolCalls: []state.ToolCall{{ID: fmt.Sprintf("c%d", iteration), Type: "function",
					Function: state.ToolCallFunction{Name: "t", Arguments: `{"bad": `}}}}, &state.Message{Role: "tool", ToolCallID: fmt.Sprintf("c%d", iteration), Content: "ok"})
			}
			if iteration == 1 {
				paritySimulateCompression(&oldList)
				paritySimulateCompression(&newList)
			}
		}
		rOld := sanitizeToolCallArguments(&oldList, nil, "", nil)
		rNew := sanitizeToolCallArguments(&newList, nil, "", cursor)
		if rOld != rNew || !parityEqual(oldList, newList) {
			t.Fatalf("n=%d final %d %d", n, rOld, rNew)
		}
	}
}

func TestAgentAbortRequestOpenaiClientDoesNotCallClientClose(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var swept []any
	prev := forceCloseTCPSockets
	forceCloseTCPSockets = func(c any) int { swept = append(swept, c); return 1 }
	t.Cleanup(func() { forceCloseTCPSockets = prev })
	client := &fakeOpenAI{}
	(&AIAgent{provider: "test"}).abortRequestOpenAIClient(client, "interrupt_abort")
	if len(swept) != 1 || swept[0] != client {
		t.Fatalf("sockets were not shut down %v", swept)
	}
	if client.closed {
		t.Fatal("client.close() ran on the abort path")
	}
}
