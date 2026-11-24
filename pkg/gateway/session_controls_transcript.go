package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// TUI display rows of a snapshot's stored rows through the sidecar's own history projection, row ids carried as _row_id
func displayProjection(ctx context.Context, history []map[string]any, profileHome string) ([]any, error) {
	rows := make([]any, len(history))
	for i, row := range history {
		rowID, ok := row["row_id"]
		if !ok {
			rows[i] = row
			continue
		}
		copied := make(map[string]any, len(row)+1)
		for k, v := range row {
			copied[k] = v
		}
		copied["_row_id"] = rowID
		rows[i] = copied
	}
	return tuihost.HistoryToMessages(ctx, rows, profileHome, true)
}

// Whether a live row is a micro compaction merge the model reads but nobody typed, never rendered
func modelOnly(row map[string]any) bool {
	switch metadata := row["display_metadata"].(type) {
	case map[string]any:
		return anyutil.IsTrue(metadata[state.ModelOnlyDisplayMetadataKey])
	case *jsonutil.Object:
		return metadata != nil && anyutil.IsTrue(metadata.ValueOr(state.ModelOnlyDisplayMetadataKey, nil))
	}
	return false
}

// Transcript fields of a resume reply, its rows and task plan or the omitted count and plan of the target
func resumeTranscript(ctx context.Context, a *SessionAuthority, ref SessionRef, snapshot SubscriptionSnapshot, history []map[string]any, omit bool) (*jsonutil.Object, error) {
	if !omit {
		var todo any
		if plan := tuihost.TodoStateFromHistory(history); plan != nil {
			todo = plan
		}
		messages := make([]any, len(history))
		for i, row := range history {
			messages[i] = row
		}
		return jsonutil.ObjectOf("messages", messages, "message_count", int64(len(history)), "todo_state", todo), nil
	}
	type omitted struct {
		todo  any
		count int
	}
	target := ""
	if snapshot.TranscriptTarget != nil {
		target = *snapshot.TranscriptTarget
	}
	read, err := unlessDeleted(ctx, a, ref, func() (omitted, error) {
		todo, count, err := omittedHistoryState(ctx, a.DB, target)
		return omitted{todo, count}, err
	})
	if err != nil {
		return nil, err
	}
	return jsonutil.ObjectOf("messages", []any{}, "messages_omitted", true, "message_count", int64(read.count), "todo_state", read.todo), nil
}

// Display rows with the reasoning of every reply left out, for a session that does not show it
func withoutReasoning(rows []any) {
	for _, row := range rows {
		if m, isRow := row.(map[string]any); isRow {
			for _, key := range historyReasoningKeys {
				delete(m, key)
			}
		}
	}
}

// Reply fields of a display row that carry the model's reasoning
var historyReasoningKeys = []string{"reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items"}
