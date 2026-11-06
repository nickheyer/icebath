package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

type batchAccount struct{ NousAccountSource }

func (batchAccount) IdentityState(*Env) (bool, bool) { return true, true }

// Registry of the local and connector tools a batch names, each handler recording its invocation
func batchRegistry(mu *sync.Mutex, invoked *[]string) *Registry {
	r := NewRegistry()
	for _, d := range [][2]string{{"session_search", "session_search"}, {"todo_list", "todo"}, {"connectors__gmail__SEND_EMAIL", "connections"}} {
		name := d[0]
		r.Register(Def{Name: name, Toolset: d[1], Schema: map[string]any{"name": name, "description": "Batch probe " + name,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
			Handler: func(context.Context, *Call) string {
				mu.Lock()
				*invoked = append(*invoked, name)
				mu.Unlock()
				return `{"ok": true}`
			}})
	}
	return r
}

func batchErrorOf(t *testing.T, raw string) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("result %q", raw)
	}
	msg, _ := out["error"].(string)
	return msg
}

func TestLocalBatchesRejectedBeforeAnyEntryExecutes(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "mixed"}[mixed], func(t *testing.T) {
			tbhInstall(t, batchAccount{})
			env := &Env{Home: t.TempDir()}
			second := "todo_list"
			if mixed {
				second = "connectors__gmail__SEND_EMAIL"
			}
			calls := []any{map[string]any{"name": "session_search", "arguments": map[string]any{}},
				map[string]any{"name": second, "arguments": map[string]any{}}}
			name, _, errMsg := ResolveUnderlyingCall(context.Background(), env, map[string]any{"calls": calls}, nil)
			if name != "" || !strings.Contains(errMsg, "exactly one entry") {
				t.Fatalf("resolved %q error %q", name, errMsg)
			}
			var mu sync.Mutex
			var invoked []string
			d := NewDispatcher(batchRegistry(&mu, &invoked))
			raw := d.DispatchWith(context.Background(), &Call{Name: "tool_call", Args: map[string]any{"calls": calls}, Env: env},
				DispatchOptions{EnabledToolsets: []string{"connections", "session_search", "todo"}})
			if msg := batchErrorOf(t, raw); !strings.Contains(msg, "exactly one entry") {
				t.Fatalf("result %q", raw)
			}
			if len(invoked) != 0 {
				t.Fatalf("invoked %v", invoked)
			}
		})
	}
}

func TestDispatchConnectorBatchGuardEchoesFirstEntry(t *testing.T) {
	var mu sync.Mutex
	var invoked []string
	d := NewDispatcher(batchRegistry(&mu, &invoked))
	calls := []any{map[string]any{"name": "session_search", "arguments": map[string]any{"query": "alpha"}},
		map[string]any{"name": "connectors__gmail__SEND_EMAIL", "arguments": map[string]any{}}}
	raw := dispatchConnectorBatch(context.Background(), d, &Call{Name: "tool_call", Env: &Env{Home: t.TempDir()}}, calls, DispatchOptions{})
	if msg := batchErrorOf(t, raw); !strings.Contains(msg, `Retry with only: {"calls":[{"name":"session_search","arguments":{"query":"alpha"}}]}`) {
		t.Fatalf("result %q", raw)
	}
	if len(invoked) != 0 {
		t.Fatalf("invoked %v", invoked)
	}
}
