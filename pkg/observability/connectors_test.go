package observability

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestConnectorChainCountsSearchesAndDescribedFirst(t *testing.T) {
	conn := tools.ConnectorNamePrefix + "acme_send"
	other := tools.ConnectorNamePrefix + "acme_list"
	var s ConnectorSessionState
	if s.Observe(map[string]any{"tool_name": "tool_search"}) != nil || s.Observe(map[string]any{"tool_name": "tool_search"}) != nil {
		t.Fatal("search row")
	}
	s.Observe(map[string]any{"tool_name": "tool_describe", "args": map[string]any{"names": []any{" " + conn + " ", nil, "web_search"}}})
	event := map[string]any{"tool_name": conn}
	row := s.Observe(event)
	want := map[string]string{"described_first": "yes", "outcome": ToolOutcome(event), "searches_before_bucket": CountBucket(2)}
	if !reflect.DeepEqual(row, want) || s.SearchesSinceExecute != 0 {
		t.Fatalf("row %v", row)
	}
	s.Observe(map[string]any{"tool_name": "tool_describe", "args": map[string]any{"names": other}})
	if !s.Described[other] || s.Observe(map[string]any{"tool_name": "terminal"}) != nil {
		t.Fatal("bare string describe or non connector")
	}
	if row := s.Observe(map[string]any{"tool_name": tools.ConnectorNamePrefix + "fresh"}); row["described_first"] != "no" || row["searches_before_bucket"] != CountBucket(0) {
		t.Fatalf("undescribed %v", row)
	}
	for i := 0; i < maxDescribed+5; i++ {
		s.Observe(map[string]any{"tool_name": "tool_describe", "args": map[string]any{"names": []any{tools.ConnectorNamePrefix + string(rune('a'+i%26)) + string(rune('0'+i/26))}}})
	}
	if len(s.Described) != maxDescribed {
		t.Fatalf("described %d", len(s.Described))
	}
}

func TestConnectorBatchesAndEntries(t *testing.T) {
	conn := tools.ConnectorNamePrefix + "acme_send"
	batch := map[string]any{"tool_name": "tool_call", "args": map[string]any{"calls": []any{map[string]any{"name": conn, "arguments": map[string]any{}}}}}
	if !IsConnectorBatch(batch) {
		t.Fatal("connector batch")
	}
	mixed := map[string]any{"tool_name": "tool_call", "args": map[string]any{"calls": []any{map[string]any{"name": conn, "arguments": map[string]any{}},
		map[string]any{"name": "terminal", "arguments": map[string]any{}}}}}
	if IsConnectorBatch(mixed) || IsConnectorBatch(map[string]any{"tool_name": "tool_call", "args": "x"}) || IsConnectorBatch(map[string]any{"tool_name": conn}) {
		t.Fatal("not a connector batch")
	}
	wrapper := ToolCallIdentity{"s", "t", "call_1"}
	if !IsBatchEntry(ToolCallIdentity{"s", "t", "call_1#0"}, wrapper) || IsBatchEntry(ToolCallIdentity{"s", "u", "call_1#0"}, wrapper) ||
		IsBatchEntry(ToolCallIdentity{"s", "t", "call_10"}, wrapper) || IsBatchEntry(ToolCallIdentity{"s", "t", "#0"}, ToolCallIdentity{"s", "t", ""}) {
		t.Fatal("batch entry")
	}
	if !BatchEntryRan([]ToolCallIdentity{{"s", "t", "x"}, {"s", "t", "call_1#2"}}, wrapper) || BatchEntryRan(nil, wrapper) {
		t.Fatal("entry ran")
	}
	route := map[string]string{"model": "m", "provider": "p"}
	got := ToolBatchFields(route, []ToolCallFunction{
		{Name: "tool_call", Arguments: `{"calls": [{"name": "` + conn + `", "arguments": {}}, {"name": "` + conn + `", "arguments": {}}]}`},
		{Name: "terminal", Arguments: "{}"}, {Name: "tool_call", Arguments: "not json"},
	})
	want := map[string]string{"model": "m", "provider": "p", "connector_share": "some", "size_bucket": CountBucket(4)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batch %v", got)
	}
	if ToolBatchFields(route, nil) != nil || ToolBatchFields(nil, []ToolCallFunction{{Name: conn}})["connector_share"] != "all" ||
		ToolBatchFields(nil, []ToolCallFunction{{}})["connector_share"] != "none" {
		t.Fatal("share")
	}
	if ToolSearchFields("bogus", 1, "hit") != nil {
		t.Fatal("unknown scope kept")
	}
	for scope := range ToolSearchScopes {
		for outcome := range ToolSearchOutcomes {
			if f := ToolSearchFields(scope, 3, outcome); !CounterDimensionsAreValid(ToolSearchMetric, anyDims(f)) {
				t.Fatalf("invalid %v", f)
			}
		}
	}
}
