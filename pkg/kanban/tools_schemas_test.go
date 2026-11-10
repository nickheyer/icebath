package kanban

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
)

// First difference between two decoded JSON documents as a path and both values, empty when equal
func jsonDiff(path string, want, got any) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: want object, got %#v", path, got)
		}
		keys := make([]string, 0, len(w)+len(g))
		for k := range w {
			keys = append(keys, k)
		}
		for k := range g {
			if _, seen := w[k]; !seen {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			if d := jsonDiff(path+"."+k, w[k], g[k]); d != "" {
				return d
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return fmt.Sprintf("%s: want %#v, got %#v", path, want, got)
		}
		for i := range w {
			if d := jsonDiff(fmt.Sprintf("%s[%d]", path, i), w[i], g[i]); d != "" {
				return d
			}
		}
		return ""
	}
	if !reflect.DeepEqual(want, got) {
		return fmt.Sprintf("%s:\n want %q\n  got %q", path, want, got)
	}
	return ""
}

func TestKanbanToolSchemasMatchTheHermesReference(t *testing.T) {
	data, err := os.ReadFile("testdata/kanban_tool_schemas.json")
	if err != nil {
		t.Fatal(err)
	}
	var reference [][2]json.RawMessage
	if err := json.Unmarshal(data, &reference); err != nil {
		t.Fatal(err)
	}
	ported := map[string]map[string]any{
		"KANBAN_SHOW_SCHEMA": kanbanShowSchema, "KANBAN_LIST_SCHEMA": kanbanListSchema, "KANBAN_COMPLETE_SCHEMA": kanbanCompleteSchema,
		"KANBAN_BLOCK_SCHEMA": kanbanBlockSchema, "KANBAN_SCHEDULE_SCHEMA": kanbanScheduleSchema,
		"KANBAN_REQUEST_REVIEW_SCHEMA": kanbanRequestReviewSchema, "KANBAN_REQUEST_CHANGES_SCHEMA": kanbanRequestChangesSchema,
		"KANBAN_HEARTBEAT_SCHEMA": kanbanHeartbeatSchema, "KANBAN_COMMENT_SCHEMA": kanbanCommentSchema, "KANBAN_ATTACH_SCHEMA": kanbanAttachSchema,
		"KANBAN_ATTACH_URL_SCHEMA": kanbanAttachURLSchema, "KANBAN_ATTACHMENTS_SCHEMA": kanbanAttachmentsSchema,
		"KANBAN_CREATE_SCHEMA": kanbanCreateSchema, "KANBAN_UNBLOCK_SCHEMA": kanbanUnblockSchema, "KANBAN_LINK_SCHEMA": kanbanLinkSchema,
	}
	if len(reference) != len(ported) || len(kanbanTools) != len(reference) {
		t.Fatalf("%d reference schemas, %d ported, %d registered", len(reference), len(ported), len(kanbanTools))
	}
	for i, entry := range reference {
		var attr string
		var want map[string]any
		if err := json.Unmarshal(entry[0], &attr); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(entry[1], &want); err != nil {
			t.Fatal(err)
		}
		schema, known := ported[attr]
		if !known {
			t.Fatalf("no Go schema for %s", attr)
		}
		encoded, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if d := jsonDiff(attr, want, got); d != "" {
			t.Error(d)
		}
		if kanbanTools[i].name != want["name"] || !reflect.DeepEqual(kanbanTools[i].schema, schema) {
			t.Errorf("registration %d is %s, the hermes tool order has %v", i, kanbanTools[i].name, want["name"])
		}
	}
}

func TestAgentToolStatusEnumCoversEveryStatus(t *testing.T) {
	params := kanbanListSchema["parameters"].(map[string]any)
	enum := params["properties"].(map[string]any)["status"].(map[string]any)["enum"].([]any)
	got := map[string]bool{}
	for _, s := range enum {
		got[s.(string)] = true
	}
	if len(got) != len(enum) || !reflect.DeepEqual(got, map[string]bool(ValidStatuses)) {
		t.Fatalf("enum %v, statuses %v", enum, ValidStatuses)
	}
}
