package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Engine compressing once to a fixed compacted set, recording the session row seen when compression starts
type sessionRowEngine struct {
	compressOnceEngine
	db            *state.SessionDB
	sessionID     string
	compacted     []map[string]any
	rowAtCompress *pyjson.Object
}

func (e *sessionRowEngine) Compress([]map[string]any, *int, *string, bool, string) ([]map[string]any, error) {
	e.rowAtCompress, _ = e.db.GetSession(context.Background(), e.sessionID)
	out := make([]map[string]any, len(e.compacted))
	for i, row := range e.compacted {
		out[i] = map[string]any{"role": row["role"], "content": row["content"]}
	}
	return out, nil
}

// Content normalized through JSON so typed and generic part lists compare equal
func jsonParts(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
