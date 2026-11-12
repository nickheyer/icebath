package tuiserver

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func TestDeltasAndSealsCarryTurnSegmentAndUTF16Offset(t *testing.T) {
	commentary := "Looking 👀 now."
	toolTurn := func() any {
		return fakellm.ToolCall{Name: "tool_search", Args: map[string]any{"query": "todo"}, Text: &commentary, ChunkChars: 10}
	}
	hz := newTurnHarness(t, []any{toolTurn(), fakellm.Text{Text: "Done."}, toolTurn(), fakellm.Text{Text: "Done."}},
		"display:\n  interim_assistant_messages: true\n")
	hz.turn(t, "ping")
	hz.turn(t, "ping")
	type stamp struct {
		event           string
		segment, offset any
	}
	var stamps []stamp
	var turnIDs []any
	for _, frame := range hz.frames() {
		event, _ := frame[0].(string)
		if event != "message.start" && event != "message.delta" && event != "message.interim" {
			continue
		}
		payload := frame[1].(map[string]any)
		stamps = append(stamps, stamp{event, payload["segment"], payload["offset"]})
		turnIDs = append(turnIDs, payload["turn_id"])
	}
	oneTurn := []stamp{{"message.start", nil, nil}, {"message.delta", 0, 0}, {"message.delta", 0, 11},
		{"message.interim", 0, nil}, {"message.delta", 1, 0}}
	if fmt.Sprint(stamps) != fmt.Sprint(append(append([]stamp{}, oneTurn...), oneTurn...)) {
		for i, body := range hz.srv.MainRequests() {
			raw, _ := json.Marshal(body["messages"])
			t.Logf("request %d messages %s", i, raw)
		}
		t.Fatalf("stamps %v\nframes %v\nprovider %d main %d aux", stamps, hz.types(), len(hz.srv.MainRequests()), len(hz.srv.AuxRequests()))
	}
	first, second := map[any]bool{}, map[any]bool{}
	for i, id := range turnIDs {
		if i < 5 {
			first[id] = true
		} else {
			second[id] = true
		}
	}
	if len(first) != 1 || len(second) != 1 || first[nil] || second[nil] || reflect.DeepEqual(first, second) {
		t.Fatalf("turn ids %v", turnIDs)
	}
	if _, present := hz.session.Lookup(tuigateway.SessionSegmentsKey); present {
		t.Fatal("a finished turn left its cursor behind")
	}
}
