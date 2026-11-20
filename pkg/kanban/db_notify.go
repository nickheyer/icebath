package kanban

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

var notifyDeliveryModes = map[string]bool{"notify": true, "notify+wake": true, "wake": true}

const subKeyWhere = "WHERE task_id = ? AND platform = ? AND chat_id = ? AND thread_id = ?"

// Primary key tuple of one subscription row, a nil thread stored as empty
func subKey(taskID, platform, chatID string, threadID *string) []any {
	return []any{taskID, platform, chatID, strOrEmpty(threadID)}
}

// The string or empty for nil, the or empty idiom
func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Keys and values of a dict value in iteration order, false for anything else
func dictItems(d any) ([]string, []any, bool) {
	switch m := d.(type) {
	case *pyjson.Object:
		if m == nil {
			return nil, nil, false
		}
		vals := make([]any, len(m.Keys))
		for i, k := range m.Keys {
			vals[i], _ = m.Get(k)
		}
		return m.Keys, vals, true
	case map[string]any:
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		vals := make([]any, len(keys))
		for i, k := range keys {
			vals[i] = m[k]
		}
		return keys, vals, true
	}
	return nil, nil, false
}

// Reports whether a decoded value is a str, int, float or bool scalar
func isScalar(v any) bool {
	switch v.(type) {
	case string, int64, int, float64, bool:
		return true
	}
	return false
}

// Compact sorted JSON of the scalar entries of platform send metadata, nil when none remain
func encodeNotifyDeliveryMetadata(metadata any) *string {
	keys, vals, ok := dictItems(metadata)
	if !ok {
		return nil
	}
	clean := pyjson.NewObject()
	for i, k := range keys {
		if vals[i] != nil && isScalar(vals[i]) {
			clean.Set(k, vals[i])
		}
	}
	if clean.Len() == 0 {
		return nil
	}
	text, err := pyjson.Dumps(clean, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	if err != nil {
		return nil
	}
	return &text
}

// Decoded delivery metadata with only scalar values kept, empty for anything unreadable
func decodeNotifyDeliveryMetadata(raw any) *pyjson.Object {
	if keys, vals, ok := dictItems(raw); ok {
		out := pyjson.NewObject()
		for i, k := range keys {
			out.Set(k, vals[i])
		}
		return out
	}
	out := pyjson.NewObject()
	if !truthy(raw) {
		return out
	}
	data, err := pyjson.Loads(cellStr(raw))
	if err != nil {
		return out
	}
	keys, vals, ok := dictItems(data)
	if !ok {
		return out
	}
	for i, k := range keys {
		if isScalar(vals[i]) {
			out.Set(k, vals[i])
		}
	}
	return out
}

// NotifySubOptions holds the keyword arguments of a notification subscription
type NotifySubOptions struct {
	TaskID           string
	Platform         string
	ChatID           string
	ThreadID         *string
	UserID           *string
	UserIDAlt        *string
	ChatType         *string
	NotifierProfile  *string
	DeliveryMode     *string
	DeliveryMetadata any
}

// Registers a gateway source wanting terminal state notifications, idempotent on task, platform, chat and thread
func AddNotifySub(ctx context.Context, conn state.SQLiteConn, o NotifySubOptions) error {
	var validMode *string
	if o.DeliveryMode != nil && notifyDeliveryModes[*o.DeliveryMode] {
		validMode = o.DeliveryMode
	}
	insertMode := "notify"
	if o.Platform == "api_server" {
		insertMode = "notify+wake"
	}
	if validMode != nil {
		insertMode = *validMode
	}
	key := subKey(o.TaskID, o.Platform, o.ChatID, o.ThreadID)
	return WriteTxn(ctx, conn, false, func() error {
		existing, err := QueryOne(ctx, conn, "SELECT delivery_metadata FROM kanban_notify_subs "+subKeyWhere, key...)
		if err != nil {
			return err
		}
		merged := pyjson.NewObject()
		if existing != nil {
			merged = decodeNotifyDeliveryMetadata(existing.Get("delivery_metadata"))
		}
		if keys, vals, ok := dictItems(o.DeliveryMetadata); ok && len(keys) > 0 {
			for i, k := range keys {
				merged.Set(k, vals[i])
			}
		}
		var metadataJSON *string
		if merged.Len() > 0 {
			metadataJSON = encodeNotifyDeliveryMetadata(merged)
		}
		chatType := "dm"
		if truthyStr(o.ChatType) {
			chatType = *o.ChatType
		}
		params := append(append([]any{}, key...), optStrValue(o.UserID), optStrValue(o.UserIDAlt), chatType, optStrValue(o.NotifierProfile),
			insertMode, optStrValue(metadataJSON), time.Now().Unix(), o.TaskID)
		if _, err := conn.ExecContext(ctx, `
            INSERT OR IGNORE INTO kanban_notify_subs
                (task_id, platform, chat_id, thread_id, user_id, user_id_alt,
                 chat_type, notifier_profile, delivery_mode, delivery_metadata,
                 created_at, last_event_id)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
                    COALESCE((SELECT MAX(id) FROM task_events WHERE task_id = ?), 0))
            `, params...); err != nil {
			return err
		}
		for _, u := range []struct {
			column   string
			value    *string
			fillOnly bool
		}{
			{"chat_type", o.ChatType, false},
			{"user_id", o.UserID, true},
			{"user_id_alt", o.UserIDAlt, true},
			{"notifier_profile", o.NotifierProfile, true},
			{"delivery_mode", validMode, false},
			{"delivery_metadata", metadataJSON, false},
		} {
			if !truthyStr(u.value) {
				continue
			}
			guard := ""
			if u.fillOnly {
				guard = " AND (" + u.column + " IS NULL OR " + u.column + " = '')"
			}
			if _, err := conn.ExecContext(ctx, "UPDATE kanban_notify_subs SET "+u.column+" = ? "+subKeyWhere+guard, append([]any{*u.value}, key...)...); err != nil {
				return err
			}
		}
		return nil
	})
}

// NotifyOwnerScope restricts subscriptions to notifier profile owners when scoped
type NotifyOwnerScope struct {
	Profiles       []string
	Scoped         bool
	IncludeUnowned bool
}

// SQL predicate and parameters of notification profile ownership, empty when unscoped
func notifyProfileFilter(scope NotifyOwnerScope) (string, []any) {
	if !scope.Scoped {
		return "", nil
	}
	seen := map[string]bool{}
	var profiles []string
	for _, p := range scope.Profiles {
		if s := pystr.Strip(p); s != "" && !seen[s] {
			seen[s] = true
			profiles = append(profiles, s)
		}
	}
	sort.Strings(profiles)
	var clauses []string
	var params []any
	if len(profiles) > 0 {
		clauses = append(clauses, "notifier_profile IN ("+sqlMarks(len(profiles))+")")
		for _, p := range profiles {
			params = append(params, p)
		}
	}
	if scope.IncludeUnowned {
		clauses = append(clauses, "notifier_profile IS NULL OR notifier_profile = ''")
	}
	if len(clauses) == 0 {
		return "0", nil
	}
	return "(" + strings.Join(clauses, ") OR (") + ")", params
}

// Subscriptions as ordered row objects, optionally of one task and restricted to owner profiles
func ListNotifySubs(ctx context.Context, conn state.SQLiteConn, taskID *string, scope NotifyOwnerScope) ([]*pyjson.Object, error) {
	ownerWhere, ownerParams := notifyProfileFilter(scope)
	var where []string
	var params []any
	if taskID != nil {
		where = append(where, "task_id = ?")
		params = append(params, *taskID)
	}
	if ownerWhere != "" {
		where = append(where, ownerWhere)
		params = append(params, ownerParams...)
	}
	q := "SELECT * FROM kanban_notify_subs"
	if len(where) > 0 {
		wrapped := make([]string, len(where))
		for i, w := range where {
			wrapped[i] = "(" + w + ")"
		}
		q += " WHERE " + strings.Join(wrapped, " AND ")
	}
	rows, err := QueryRows(ctx, conn, q, params...)
	if err != nil {
		return nil, err
	}
	out := make([]*pyjson.Object, 0, len(rows))
	for _, row := range rows {
		item := pyjson.NewObject()
		for i, col := range row.Cols {
			item.Set(col, row.Vals[i])
		}
		if raw, ok := item.Get("delivery_metadata"); ok {
			item.Set("delivery_metadata", decodeNotifyDeliveryMetadata(raw))
		}
		out = append(out, item)
	}
	return out, nil
}

// Counts subscription rows over a read only connection, a missing file or table counting as zero
func CountNotifySubs(ctx context.Context, dbPath string, board *string, scope NotifyOwnerScope, platform, chatID, threadID *string) (int64, error) {
	path := dbPath
	if path == "" {
		p, err := KanbanDBPath(ctx, board)
		if err != nil {
			return 0, err
		}
		path = p
	}
	if !pathExists(path) {
		return 0, nil
	}
	ownerWhere, ownerParams := notifyProfileFilter(scope)
	var clauses []string
	var params []any
	if ownerWhere != "" {
		clauses = append(clauses, "("+ownerWhere+")")
		params = append(params, ownerParams...)
	}
	for _, f := range []struct {
		clause string
		value  *string
	}{{"LOWER(platform) = LOWER(?)", platform}, {"chat_id = ?", chatID}, {"thread_id = ?", threadID}} {
		if f.value != nil {
			clauses = append(clauses, f.clause)
			params = append(params, *f.value)
		}
	}
	q := "SELECT COUNT(*) AS n FROM kanban_notify_subs"
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	resolved, err := pypath.Resolve(path)
	if err != nil {
		return 0, err
	}
	uri, err := pypath.AsURI(resolved)
	if err != nil {
		return 0, err
	}
	other, err := state.ConnectSQLite(uri+"?mode=ro", state.SQLiteConfig{Timeout: 5.0, URI: true})
	if err != nil {
		return 0, err
	}
	defer other.Close()
	row, err := QueryOne(ctx, other, q, params...)
	if err != nil {
		if class, ok := state.SQLiteClassOf(err); ok && class == state.SQLiteOperationalError && strings.Contains(pystr.Lower(state.ErrText(err)), "no such table") {
			return 0, nil
		}
		return 0, err
	}
	if row == nil {
		return 0, nil
	}
	return pyIntOf(row.Vals[0])
}

// Deletes one subscription row, true when a row existed
func RemoveNotifySub(ctx context.Context, conn state.SQLiteConn, taskID, platform, chatID string, threadID *string) (bool, error) {
	var removed int64
	err := WriteTxn(ctx, conn, false, func() error {
		res, err := conn.ExecContext(ctx, "DELETE FROM kanban_notify_subs "+subKeyWhere, subKey(taskID, platform, chatID, threadID)...)
		if err != nil {
			return err
		}
		removed = rowsAffected(res)
		return nil
	})
	return removed > 0 && err == nil, err
}

// Deletes subscriptions of tasks settled in done or blocked for longer than the window, zero or less disabling the sweep
func PurgeStaleDoneNotifySubs(ctx context.Context, conn state.SQLiteConn, maxAgeDays int64) (int64, error) {
	if maxAgeDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Unix() - maxAgeDays*86400
	var deleted int64
	err := WriteTxn(ctx, conn, false, func() error {
		res, err := conn.ExecContext(ctx, "DELETE FROM kanban_notify_subs WHERE task_id IN ("+
			" SELECT t.id FROM tasks t"+
			" WHERE t.status IN ('done', 'blocked')"+
			" AND COALESCE("+
			"  (SELECT MAX(e.created_at) FROM task_events e"+
			"   WHERE e.task_id = t.id),"+
			"  t.completed_at, t.created_at, 0"+
			" ) < ?)", cutoff)
		if err != nil {
			return err
		}
		deleted = rowsAffected(res)
		return nil
	})
	return deleted, err
}

// last_event_id of one subscription row, nil when unsubscribed
func notifyCursor(ctx context.Context, conn state.SQLiteConn, taskID, platform, chatID string, threadID *string) (*int64, error) {
	row, err := QueryOne(ctx, conn, "SELECT last_event_id FROM kanban_notify_subs "+subKeyWhere, subKey(taskID, platform, chatID, threadID)...)
	if err != nil || row == nil {
		return nil, err
	}
	n, err := pyIntOf(row.Get("last_event_id"))
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// New cursor and events past the subscription cursor, the cursor itself left in place
func UnseenEventsForSub(ctx context.Context, conn state.SQLiteConn, taskID, platform, chatID string, threadID *string, kinds []string) (int64, []*Event, error) {
	cursor, err := notifyCursor(ctx, conn, taskID, platform, chatID, threadID)
	if err != nil || cursor == nil {
		return 0, nil, err
	}
	q := "SELECT * FROM task_events WHERE task_id = ? AND id > ? "
	params := []any{taskID, *cursor}
	if len(kinds) > 0 {
		q += "AND kind IN (" + sqlMarks(len(kinds)) + ") "
		for _, k := range kinds {
			params = append(params, k)
		}
	}
	rows, err := QueryRows(ctx, conn, q+"ORDER BY id ASC", params...)
	if err != nil {
		return 0, nil, err
	}
	maxID := *cursor
	out := make([]*Event, 0, len(rows))
	for _, r := range rows {
		e, err := EventFromRow(r)
		if err != nil {
			return 0, nil, err
		}
		out = append(out, e)
		if id := cellInt(r.Get("id")); id > maxID {
			maxID = id
		}
	}
	return maxID, out, nil
}

// Atomically claims unseen events for one subscription, advancing its cursor under the writer lock
func ClaimUnseenEventsForSub(ctx context.Context, conn state.SQLiteConn, taskID, platform, chatID string, threadID *string, kinds []string) (int64, int64, []*Event, error) {
	var oldCursor, newCursor int64
	var events []*Event
	err := WriteTxn(ctx, conn, false, func() error {
		cur, err := notifyCursor(ctx, conn, taskID, platform, chatID, threadID)
		if err != nil || cur == nil {
			return err
		}
		oldCursor = *cur
		next, found, err := UnseenEventsForSub(ctx, conn, taskID, platform, chatID, threadID, kinds)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			newCursor = oldCursor
			return nil
		}
		if _, err := casCursor(ctx, conn, subKey(taskID, platform, chatID, threadID), next, oldCursor); err != nil {
			return err
		}
		newCursor, events = next, found
		return nil
	})
	if err != nil {
		return 0, 0, nil, err
	}
	return oldCursor, newCursor, events, nil
}

// Moves last_event_id only while it still equals the expected value
func casCursor(ctx context.Context, conn state.SQLiteConn, key []any, newCursor, expected int64) (sql.Result, error) {
	params := append(append([]any{newCursor}, key...), expected)
	return conn.ExecContext(ctx, "UPDATE kanban_notify_subs SET last_event_id = ? "+subKeyWhere+" AND last_event_id = ?", params...)
}

// Sets the subscription cursor after delivery
func AdvanceNotifyCursor(ctx context.Context, conn state.SQLiteConn, taskID, platform, chatID string, threadID *string, newCursor int64) error {
	return WriteTxn(ctx, conn, false, func() error {
		_, err := conn.ExecContext(ctx, "UPDATE kanban_notify_subs SET last_event_id = ? "+subKeyWhere, append([]any{newCursor}, subKey(taskID, platform, chatID, threadID)...)...)
		return err
	})
}

// Checkpoints a sent ping independently of the retryable wake cursor
func RecordNotifyPing(ctx context.Context, conn state.SQLiteConn, taskID, platform, chatID string, threadID *string, eventID int64) error {
	return WriteTxn(ctx, conn, false, func() error {
		_, err := conn.ExecContext(ctx, "UPDATE kanban_notify_subs SET last_ping_event_id = MAX(last_ping_event_id, ?) "+subKeyWhere, append([]any{eventID}, subKey(taskID, platform, chatID, threadID)...)...)
		return err
	})
}

// Undoes a claim after a delivery failure unless a later notifier already advanced the row
func RewindNotifyCursor(ctx context.Context, conn state.SQLiteConn, taskID, platform, chatID string, threadID *string, claimedCursor, oldCursor int64) (bool, error) {
	var rewound int64
	err := WriteTxn(ctx, conn, false, func() error {
		res, err := casCursor(ctx, conn, subKey(taskID, platform, chatID, threadID), oldCursor, claimedCursor)
		if err != nil {
			return err
		}
		rewound = rowsAffected(res)
		return nil
	})
	return rewound > 0 && err == nil, err
}
