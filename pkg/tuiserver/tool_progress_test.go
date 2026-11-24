package tuiserver

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

func TestEmptyListAtNonzeroRevisionIsARealClear(t *testing.T) {
	raw, _ := pyjson.AsObject(map[string]any{"todos": []any{}, "revision": 2})
	state := tuihost.NormalizeTodoState(raw)
	if state == nil || !reflect.DeepEqual(pyjson.Plain(state), map[string]any{"todos": []any{}, "revision": int64(2)}) {
		t.Fatalf("state = %v", state)
	}
}
