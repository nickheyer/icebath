package gateway

import (
	"context"
	"log/slog"
	"math/big"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Client safe full todo snapshot of a decoded value, nil when malformed or the unused empty store
func normalizeTodoState(value any) *pyjson.Object {
	obj, ok := value.(*pyjson.Object)
	if !ok || obj == nil {
		return nil
	}
	rawTodos, _ := obj.Get("todos")
	todos, ok := rawTodos.([]any)
	if !ok {
		return nil
	}
	rawRevision, _ := obj.Get("revision")
	if !pyTruthy(rawRevision) {
		rawRevision = int64(0)
	}
	parsed, err := pyint.Of(rawRevision)
	if err != nil {
		return nil
	}
	if parsed.Sign() < 0 {
		parsed = big.NewInt(0)
	}
	if len(todos) == 0 && parsed.Sign() == 0 {
		return nil
	}
	return pyjson.ObjectOf("todos", append([]any(nil), todos...), "revision", pyint.Value(parsed))
}

// Cold resume todo snapshot of a session from its persisted todo results, nil when none is usable
func todoStateFromDB(ctx context.Context, db *state.SessionDB, sessionID string) any {
	content, found, err := db.GetLatestTodoResult(ctx, sessionID, nil)
	if err != nil {
		slog.Debug("failed to read persisted todo state", "error", err)
		return nil
	}
	if !found || utf8.RuneCountInString(content) > state.MaxTodoResultChars {
		return nil
	}
	decoded, err := pyjson.Loads(content)
	if err != nil {
		return nil
	}
	if snapshot := normalizeTodoState(decoded); snapshot != nil {
		return snapshot
	}
	return nil
}
