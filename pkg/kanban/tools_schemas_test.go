package kanban

import (
	"reflect"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestAgentToolStatusEnumCoversEveryStatus(t *testing.T) {
	params := jsonutil.Plain(kanbanListSchema.Value("parameters")).(map[string]any)
	enum := params["properties"].(map[string]any)["status"].(map[string]any)["enum"].([]any)
	got := map[string]bool{}
	for _, s := range enum {
		got[s.(string)] = true
	}
	if len(got) != len(enum) || !reflect.DeepEqual(got, map[string]bool(ValidStatuses)) {
		t.Fatalf("enum %v, statuses %v", enum, ValidStatuses)
	}
}
