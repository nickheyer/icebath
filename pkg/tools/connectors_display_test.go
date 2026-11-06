package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type listFake struct {
	mu        sync.Mutex
	connected map[string]bool
}

func (l *listFake) ListConnectors(context.Context, time.Duration) ([]map[string]any, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, s := range []string{"gmail", "linear", "slack"} {
		out = append(out, map[string]any{"connector": s, "connected": l.connected[s]})
	}
	return out, nil
}

func (l *listFake) Connections(context.Context, ConnectionsRequest) (map[string]any, error) {
	panic("display never mints")
}

func (l *listFake) AccountStatus(context.Context, string, time.Duration) (map[string]any, error) {
	panic("display never reads an account")
}

func runDisplayCall(t *testing.T, env *Env, gw *listFake, cb ConnCallback, cardSurface bool) map[string]any {
	raw := ManageConnections(context.Background(), env, connectArgs("display", "slack", "linear"), ManageOptions{
		ClientFactory: func() (ManagedClient, error) { return gw, nil }, Callback: cb, CardSurface: cardSurface, SessionID: "s1"})
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%q: %v", raw, err)
	}
	return out
}

func TestDisplayWaitsForTheUserAndReportsWhatTheyConnected(t *testing.T) {
	for _, ending := range []string{"continue", "deadline", "said"} {
		t.Run(ending, func(t *testing.T) {
			env := managedTestSetup(t)
			gw := &listFake{connected: map[string]bool{"gmail": true}}
			var seen []map[string]any
			card := func(payload *pyjson.Object) string {
				var m map[string]any
				_ = json.Unmarshal([]byte(connDumps(payload)), &m)
				seen = append(seen, m)
				gw.mu.Lock()
				gw.connected["slack"] = true
				gw.mu.Unlock()
				if ending != "deadline" {
					answer := `{"settled_by": "continue"}`
					if ending == "said" {
						answer = `{"said": " maybe later "}`
					}
					id, _ := payload.Get("op_id")
					if err := DisplayApplyAnswer(LiveGet(OperationSessionKey(env, "s1"), id.(string), ""), answer); err != nil {
						t.Error(err)
					}
				}
				return ""
			}
			OperationDeadlineSeconds = 60
			if ending == "deadline" {
				OperationDeadlineSeconds = 2
			}
			out := runDisplayCall(t, env, gw, card, true)
			if len(seen) != 1 || len(seen[0]["targets"].([]any)) != 0 ||
				!reflect.DeepEqual(seen[0]["display"], map[string]any{"suggested": []any{"slack", "linear"}}) {
				t.Fatalf("payload %v", seen)
			}
			want := map[string]any{"connected": []any{"gmail", "slack"}, "newly_connected": []any{"slack"}, "suggested": []any{"slack", "linear"}}
			switch ending {
			case "deadline":
				want["timed_out"] = true
			case "said":
				want["said"] = "maybe later"
			}
			if !reflect.DeepEqual(out, want) || LiveCurrent(OperationSessionKey(env, "s1"), "") != nil {
				t.Fatalf("out %v want %v", out, want)
			}
		})
	}
}

func TestDisplayWithoutACardReturnsTheStatusListAtOnce(t *testing.T) {
	env := managedTestSetup(t)
	old := PortalAccounts
	PortalAccounts = func(context.Context, *Env) ([]map[string]any, error) {
		return nil, &ToolGatewayError{Message: "portal accounts unavailable", Kind: PortalKindConnectorUnavailable}
	}
	defer func() { PortalAccounts = old }()
	out := runDisplayCall(t, env, &listFake{connected: map[string]bool{"gmail": true}}, func(*pyjson.Object) string { return "" }, false)
	byName := map[string]any{}
	for _, raw := range out["connectors"].([]any) {
		row := raw.(map[string]any)
		byName[row["connector"].(string)] = row["connected"]
	}
	note, _ := out["note"].(string)
	if byName["gmail"] != true || !strings.Contains(note, "desktop app") || LiveCurrent(OperationSessionKey(env, "s1"), "") != nil {
		t.Fatalf("out %v", out)
	}
}
