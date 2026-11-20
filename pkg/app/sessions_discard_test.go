package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/nickheyer/icebath/pkg/cliui"
)

func TestSessionsDiscardYesResolvesEveryUnknownAdmissionWithItsFencedGeneration(t *testing.T) {
	lost := []string{"admission-lost-a", "admission-lost-b"}
	snapshot := map[string]any{"session_id": "local-abc", "stored_session_id": "local-abc", "pending": []any{
		map[string]any{"admission_id": lost[0], "status": "unknown", "execution_generation": 4},
		map[string]any{"admission_id": "admission-queued", "status": "queued", "execution_generation": nil},
		map[string]any{"admission_id": lost[1], "status": "unknown", "execution_generation": 5}}}
	type call struct {
		method string
		params map[string]any
	}
	var mu sync.Mutex
	var calls []call
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			_, raw, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var request struct {
				ID     any            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(raw, &request) != nil {
				return
			}
			mu.Lock()
			calls = append(calls, call{request.Method, request.Params})
			mu.Unlock()
			reply := map[string]any{"jsonrpc": "2.0", "id": request.ID}
			_, byID := request.Params["session_id"]
			switch {
			case request.Method == "session.resume" && byID:
				reply["error"] = map[string]any{"code": 4001, "message": "not_found", "data": map[string]any{"reason": "not_found"}}
			case request.Method == "session.resume":
				reply["result"] = snapshot
			default:
				reply["result"] = map[string]any{"status": "terminal"}
			}
			if ws.WriteJSON(reply) != nil {
				return
			}
		}
	}))
	defer server.Close()
	h := sessionsTestHome(t)
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws"+strings.TrimPrefix(server.URL, "http"))
	run := func(yes bool) (int, string, string) {
		out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
		notTTY := func() bool { return false }
		restore := SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out, StdinTTY: notTTY, StdoutTTY: notTTY}, errOut)
		defer restore()
		code, err := CmdSessions(context.Background(), h, map[string]any{"sessions_action": "discard", "session": "my script session",
			"admission": nil, "yes": yes}, out, func() error { return nil })
		if err != nil {
			t.Fatalf("discard failed: %v, stderr %s", err, errOut.String())
		}
		return code, out.String(), errOut.String()
	}
	methods := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var names []string
		for _, c := range calls {
			names = append(names, c.method)
		}
		return names
	}
	code, _, errText := run(false)
	if code != 2 || !strings.Contains(errText, "--yes") {
		t.Fatalf("unconfirmed code %d stderr %q", code, errText)
	}
	if got := methods(); !reflect.DeepEqual(got, []string{"session.resume", "session.resume"}) {
		t.Fatalf("unconfirmed calls %v", got)
	}
	mu.Lock()
	calls = nil
	mu.Unlock()
	code, out, errText := run(true)
	if code != 0 {
		t.Fatalf("confirmed code %d stderr %q", code, errText)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []call{
		{"session.resume", map[string]any{"session_id": "my script session"}},
		{"session.resume", map[string]any{"title": "my script session"}},
		{"prompt.resolve_unknown", map[string]any{"session_id": "local-abc", "admission_id": lost[0], "execution_generation": 4.0}},
		{"prompt.resolve_unknown", map[string]any{"session_id": "local-abc", "admission_id": lost[1], "execution_generation": 5.0}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
	if got := strings.Fields(out); !reflect.DeepEqual(got, []string{"Discarded", lost[0], "Discarded", lost[1]}) {
		t.Fatalf("output %q", out)
	}
}
