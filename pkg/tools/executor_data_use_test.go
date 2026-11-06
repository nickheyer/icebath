package tools

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// test_tool_call_incremental_persistence.py test_connector_data_use_rides_the_tool_message_and_survives_a_reload executor half
func TestConnectorDataUseRidesTheToolMessage(t *testing.T) {
	const payload = `{"results": [{"response": "inbox"}]}`
	for _, mode := range []string{"sequential", "concurrent"} {
		t.Run(mode, func(t *testing.T) {
			e, host, _ := registryExecutor(t, handlerDef("web_search", func(ctx context.Context, _ *Call) string {
				recordDataUse(ctx, []string{"no_training"})
				return payload
			}))
			var flushed []*state.Message
			host.flush = func(msgs []*state.Message) (bool, error) {
				last := *msgs[len(msgs)-1]
				flushed = append(flushed, &last)
				return true, nil
			}
			messages := []*state.Message{{Role: "user", Content: "read my mail"}}
			calls := toolCallsFor("web_search", "mail-call")
			run := e.ExecuteToolCallsSequential
			if mode == "concurrent" {
				run = e.ExecuteToolCallsConcurrent
			}
			if err := run(context.Background(), calls, &messages, "task-1", true); err != nil {
				t.Fatal(err)
			}
			plain := MakeToolResultMessage("web_search", payload, "mail-call", "")
			last := messages[len(messages)-1]
			if last.Content != plain.Content {
				t.Fatalf("content %q want %q", last.Content, plain.Content)
			}
			want := []any{"no_training"}
			if !reflect.DeepEqual(last.Extra[state.DataUse], want) || !reflect.DeepEqual(last.DisplayMetadata[state.DataUse], want) {
				t.Fatalf("data_use extra %v display %v", last.Extra[state.DataUse], last.DisplayMetadata[state.DataUse])
			}
			if len(flushed) == 0 {
				t.Fatal("tool message never flushed")
			}
			durable := flushed[len(flushed)-1]
			if durable.Content != last.Content || !reflect.DeepEqual(durable.Extra[state.DataUse], want) {
				t.Fatalf("flushed row content %q data_use %v", durable.Content, durable.Extra[state.DataUse])
			}
		})
	}
}
