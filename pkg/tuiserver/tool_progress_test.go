package tuiserver

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuihost"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestEmptyListAtNonzeroRevisionIsARealClear(t *testing.T) {
	raw, _ := jsonutil.AsObject(map[string]any{"todos": []any{}, "revision": 2})
	state := tuihost.NormalizeTodoState(raw)
	if state == nil || !reflect.DeepEqual(jsonutil.Plain(state), map[string]any{"todos": []any{}, "revision": int64(2)}) {
		t.Fatalf("state = %v", state)
	}
}
