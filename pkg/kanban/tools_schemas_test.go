package kanban

import (
	"reflect"
	"testing"
)

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
