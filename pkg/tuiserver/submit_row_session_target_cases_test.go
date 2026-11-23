package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
)

// Production rotation onto a continuation, the parent reopened the way a resume of the old id does
func submitRowRotateToChild(t *testing.T, db *state.SessionDB, parent string, a *agent.AIAgent, reopenParent bool) string {
	t.Helper()
	ctx := context.Background()
	child, model := state.NewSessionID(), "test-model"
	if err := db.PublishCompressionChild(ctx, state.CompressionChildPublish{ParentSessionID: parent, ChildSessionID: child, Source: "tui", Model: &model,
		Messages: []map[string]any{{"role": "user", "content": "earlier turn"}, {"role": "assistant", "content": "earlier reply"}}}); err != nil {
		t.Fatal(err)
	}
	a.SetSessionID(child)
	if reopenParent {
		if err := db.ReopenSession(ctx, parent); err != nil {
			t.Fatal(err)
		}
	}
	return child
}

// Role and the first 48 characters of every row of the session, inactive rows included
func submitRowRows(t *testing.T, db *state.SessionDB, key string) [][2]string {
	t.Helper()
	rows, err := db.GetMessagesAsConversation(context.Background(), key, state.ConversationOptions{IncludeInactive: true})
	if err != nil {
		t.Fatal(err)
	}
	out := make([][2]string, 0, len(rows))
	for _, r := range rows {
		content := []rune(strOr(r["content"]))
		if len(content) > 48 {
			content = content[:48]
		}
		out = append(out, [2]string{strOr(r["role"]), string(content)})
	}
	return out
}
