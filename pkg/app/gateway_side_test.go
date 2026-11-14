package app

import (
	"context"
	"strings"
	"testing"
)

const sideSID = "local-side"

// Connection to a gateway that answers every side verb with one task whose answer is text
func sideConnection(t *testing.T, text string) (*scriptedClient, func(verb, prompt string) (string, func() (string, error), error), func() bool) {
	t.Helper()
	client := &scriptedClient{}
	client.override = func(method string, params map[string]any) (any, bool, error) {
		client.record(method, params)
		kind := "btw.complete"
		if method == "prompt.background" {
			kind = "background.complete"
		}
		client.events.PutNowait(wireValue(client.t, map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{
			"type": kind, "session_id": sideSID, "payload": map[string]any{"task_id": "bg_1", "text": text}}}))
		return map[string]any{"task_id": "bg_1"}, true, nil
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	conn := scriptedConnection(t, client)
	start := func(verb, prompt string) (string, func() (string, error), error) {
		return gatewaySideStart(context.Background(), conn, sideSID, verb, prompt)
	}
	return client, start, conn.Events.Empty
}

func TestSideTaskRunsOnTheGatewayAndItsAnswerIsRoutedByTaskId(t *testing.T) {
	for _, verb := range []string{"background", "btw"} {
		t.Run(verb, func(t *testing.T) {
			client, start, eventsEmpty := sideConnection(t, "DOG14 background answer")
			taskID, wait, err := start(verb, "compute something")
			if err != nil || taskID != "bg_1" {
				t.Fatalf("start %q %v", taskID, err)
			}
			if answer, err := wait(); err != nil || answer != "DOG14 background answer" {
				t.Fatalf("answer %q %v", answer, err)
			}
			client.mu.Lock()
			calls := append([]scriptedCall(nil), client.calls...)
			client.mu.Unlock()
			if len(calls) != 1 || calls[0].method != "prompt."+verb || !sameJSON(calls[0].params, map[string]any{"session_id": sideSID, "text": "compute something"}) {
				t.Fatalf("calls %v", calls)
			}
			if !eventsEmpty() {
				t.Fatal("the side answer was left on the event queue")
			}
		})
	}
}

func TestAFailedSideTaskRaisesTheGatewaysError(t *testing.T) {
	_, start, _ := sideConnection(t, "error: model unavailable")
	_, wait, err := start("background", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wait(); err == nil || !strings.Contains(err.Error(), "model unavailable") {
		t.Fatalf("wait answered %v", err)
	}
}
