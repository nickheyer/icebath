package mcp

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// test_session_local_mcp_refreeze.py test_held_servers_are_relisted_together_under_one_bound
func TestHeldServersAreRelistedTogetherUnderOneBound(t *testing.T) {
	bound := 2 * time.Second
	scope, names := "editor-session:slow", []string{"editor_a", "editor_b"}
	started := []chan struct{}{make(chan struct{}), make(chan struct{})}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	var mu sync.Mutex
	var met []int
	scoped := tools.WithSessionToolScope(context.Background(), scope)
	for i, name := range names {
		i := i
		var once sync.Once
		respond := func(method string, params map[string]any) (any, error) {
			if method != methodToolsList {
				return map[string]any{}, nil
			}
			once.Do(func() { close(started[i]) })
			<-started[1-i]
			mu.Lock()
			met = append(met, i)
			mu.Unlock()
			<-done
			return map[string]any{"tools": []any{}}, nil
		}
		s := &serverTask{name: name}
		s.session = NewSession(&handlerTransport{in: make(chan *Message, 16), respond: respond}, SessionOptions{Name: name})
		key := serverKey(scoped, name, "", true)
		mcpMu.Lock()
		mcpServers[key] = s
		mcpMu.Unlock()
		t.Cleanup(func() {
			mcpMu.Lock()
			delete(mcpServers, key)
			mcpMu.Unlock()
		})
	}
	began := time.Now()
	if EditorToolsDrifted(scope, names, bound) {
		t.Fatal("an unanswered list must count as unchanged")
	}
	elapsed := time.Since(began)
	mu.Lock()
	got := append([]int(nil), met...)
	mu.Unlock()
	sort.Ints(got)
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("the re-lists ran one after another: %v", got)
	}
	if elapsed >= 2*bound {
		t.Fatalf("elapsed %v", elapsed)
	}
}
