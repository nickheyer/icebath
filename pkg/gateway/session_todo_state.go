package gateway

import (
	"context"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Client safe full todo snapshot of a decoded value, nil when malformed or the unused empty store
func normalizeTodoState(value any) *jsonutil.Object {
	obj, ok := value.(*jsonutil.Object)
	if !ok || obj == nil {
		return nil
	}
	rawTodos, _ := obj.Get("todos")
	todos, ok := rawTodos.([]any)
	if !ok {
		return nil
	}
	rawRevision, _ := obj.Get("revision")
	if !anyutil.IsTrue(rawRevision) {
		rawRevision = int64(0)
	}
	parsed, err := anyutil.ToInt(rawRevision)
	if err != nil {
		return nil
	}
	if parsed < 0 {
		parsed = 0
	}
	if len(todos) == 0 && parsed == 0 {
		return nil
	}
	return jsonutil.ObjectOf("todos", append([]any(nil), todos...), "revision", parsed)
}

// Cold resume todo snapshot of a session from its persisted todo results, nil when none is usable
func todoStateFromDB(ctx context.Context, db *state.SessionDB, sessionID string) any {
	content, found, err := db.GetLatestTodoResult(ctx, sessionID, nil)
	if err != nil {
		pkgLog().Debug("failed to read persisted todo state", "error", err)
		return nil
	}
	if !found || utf8.RuneCountInString(content) > state.MaxTodoResultChars {
		return nil
	}
	decoded, err := jsonutil.Decode(content)
	if err != nil {
		return nil
	}
	if snapshot := normalizeTodoState(decoded); snapshot != nil {
		return snapshot
	}
	return nil
}
