package state

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Keyword arguments of get_anchored_view, a nil KeepRoles disabling the role filter
type AnchoredViewOptions struct {
	Window    int
	Bookend   int
	KeepRoles []string
}

// Anchored view defaults, a five message window, three bookends and user plus assistant roles
func NewAnchoredViewOptions() AnchoredViewOptions {
	return AnchoredViewOptions{Window: 5, Bookend: 3, KeepRoles: []string{"user", "assistant"}}
}

// Anchored message window plus the first and last non empty messages outside it, empty when the anchor is foreign
func (s *sessionSearch) GetAnchoredView(ctx context.Context, sessionID string, aroundMessageID int64, o AnchoredViewOptions) (*pyjson.Object, error) {
	bookend := max(o.Bookend, 0)
	primitive, err := s.host.GetMessagesAround(ctx, sessionID, aroundMessageID, o.Window)
	if err != nil {
		return nil, err
	}
	rawWindow, _ := primitive.Get("window")
	windowRows, _ := rawWindow.([]any)
	if len(windowRows) == 0 {
		empty := pyjson.NewObject()
		empty.Set("window", []any{})
		empty.Set("messages_before", int64(0))
		empty.Set("messages_after", int64(0))
		empty.Set("bookend_start", []any{})
		empty.Set("bookend_end", []any{})
		return empty, nil
	}
	filtered := windowRows
	if o.KeepRoles != nil {
		keep := setOf(o.KeepRoles...)
		filtered = []any{}
		for _, item := range windowRows {
			m := item.(*pyjson.Object)
			id, _ := m.Get("id")
			role, _ := m.Get("role")
			if name, ok := role.(string); pyrepr.Equal(id, aroundMessageID) || (ok && keep[name]) {
				filtered = append(filtered, item)
			}
		}
	}
	var startRows, endRows []*pyjson.Object
	if bookend > 0 {
		roleClause := ""
		var roleParams []any
		if o.KeepRoles != nil {
			roleClause = " AND role IN (" + strings.TrimSuffix(strings.Repeat("?,", len(o.KeepRoles)), ",") + ")"
			for _, role := range o.KeepRoles {
				roleParams = append(roleParams, role)
			}
		}
		err := s.host.readCtx(ctx, true, func(conn SQLiteConn) error {
			fetch := func(op string, boundaryID any, order string) ([]*pyjson.Object, error) {
				args := append(append([]any{sessionID, boundaryID}, roleParams...), bookend)
				return queryObjects(ctx, conn, "SELECT * FROM messages "+
					"WHERE session_id = ? AND id "+op+" ?"+roleClause+" "+
					"AND length(content) > 0 "+
					"ORDER BY id "+order+" LIMIT ?", args...)
			}
			firstID, _ := windowRows[0].(*pyjson.Object).Get("id")
			var err error
			if startRows, err = fetch("<", firstID, "ASC"); err != nil {
				return err
			}
			lastID, _ := windowRows[len(windowRows)-1].(*pyjson.Object).Get("id")
			if endRows, err = fetch(">", lastID, "DESC"); err != nil {
				return err
			}
			for i, j := 0, len(endRows)-1; i < j; i, j = i+1, j-1 {
				endRows[i], endRows[j] = endRows[j], endRows[i]
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	hydrate := func(rows []*pyjson.Object) []any {
		out := []any{}
		for _, row := range rows {
			out = append(out, s.host.rowToMessageDict(row, "get_anchored_view", false))
		}
		return out
	}
	view := pyjson.NewObject()
	view.Set("window", filtered)
	before, _ := primitive.Get("messages_before")
	after, _ := primitive.Get("messages_after")
	view.Set("messages_before", before)
	view.Set("messages_after", after)
	view.Set("bookend_start", hydrate(startRows))
	view.Set("bookend_end", hydrate(endRows))
	return view, nil
}

// Most recent real user turns newest first as id, timestamp and an 80 character whitespace collapsed preview
func (s *sessionSearch) ListRecentUserMessages(ctx context.Context, sessionID string, limit int, includeInactive bool) ([]*pyjson.Object, error) {
	activeClause := ""
	if !includeInactive {
		activeClause = " AND active = 1"
	}
	displayClause := " AND (display_kind IS NULL OR display_kind = '' OR display_kind = 'steer')"
	var rows []*pyjson.Object
	err := s.host.readCtx(ctx, true, func(conn SQLiteConn) error {
		var err error
		rows, err = queryObjects(ctx, conn,
			"SELECT id, timestamp, content FROM messages WHERE session_id = ? AND role = 'user'"+
				activeClause+displayClause+" "+
				"ORDER BY id DESC LIMIT ?", sessionID, limit*2+5)
		return err
	})
	if err != nil {
		return nil, err
	}
	result := []*pyjson.Object{}
	for _, row := range rows {
		if len(result) >= limit {
			break
		}
		content, _ := row.Get("content")
		decoded := s.host.decodeContent(content)
		if IsContextSummaryContent(decoded) {
			continue
		}
		var preview string
		if text, ok := decoded.(string); ok {
			preview = text
			if described, ok := DescribeSkillInvocation(text, " — "); ok && described != "" {
				preview = described
			}
		} else if preview, err = flattenText(decoded); err != nil {
			return nil, err
		}
		preview = strings.Join(pystr.SplitWhitespace(preview, -1), " ")
		if runes := pystr.Runes(preview); len(runes) > 80 {
			preview = pystr.FromRunes(runes[:77]) + "..."
		}
		id, _ := row.Get("id")
		timestamp, _ := row.Get("timestamp")
		entry := pyjson.NewObject()
		entry.Set("id", id)
		entry.Set("timestamp", timestamp)
		entry.Set("preview", preview)
		result = append(result, entry)
	}
	return result, nil
}
