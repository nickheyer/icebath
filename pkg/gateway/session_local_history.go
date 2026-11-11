package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Byte budget the archived rows of a resume reply fill only where the live rows leave room
var resumeHistoryBytes = 3 * 1024 * 1024

// Lineage read of a physical transcript, the live row ids it saw with its display rows
type preparedHistory struct {
	target    string
	live      map[int64]bool
	display   []map[string]any
	truncated bool
}

// Durable row id a conversation row carries
func historyRowID(row map[string]any) (int64, error) {
	raw, ok := row["_row_id"]
	if !ok {
		return 0, &pyKeyError{key: "_row_id"}
	}
	n, err := pyint.Of(raw)
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// Row ids of a list of conversation rows
func historyRowIDs(rows []map[string]any) (map[int64]bool, error) {
	ids := make(map[int64]bool, len(rows))
	for _, row := range rows {
		id, err := historyRowID(row)
		if err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, nil
}

// Length of a row as the JSON text a resume reply carries it in
func historyRowSize(row map[string]any) (int, error) {
	text, err := pyjson.Dumps(row, pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		return 0, err
	}
	return pystr.Len(text), nil
}

// Live row ids, display rows and truncation of a physical transcript, the live ids read first
func displayRows(ctx context.Context, db *state.SessionDB, target string) (map[int64]bool, []map[string]any, bool, error) {
	liveRows, err := db.GetMessagesAsConversation(ctx, target, state.ConversationOptions{IncludeRowIDs: true})
	if err != nil {
		return nil, nil, false, err
	}
	live, err := historyRowIDs(liveRows)
	if err != nil {
		return nil, nil, false, err
	}
	if _, err := db.AssertResumeSafe(ctx, target, nil, false); err != nil {
		var tooLarge *state.SessionResumeTooLargeError
		if errors.As(err, &tooLarge) {
			return live, []map[string]any{}, false, nil
		}
		return nil, nil, false, err
	}
	display, err := db.GetMessagesAsConversation(ctx, target,
		state.ConversationOptions{IncludeAncestors: true, IncludeCompacted: true, IncludeRowIDs: true})
	if err != nil {
		return nil, nil, false, err
	}
	kept, truncated, err := fitArchive(liveRows, display)
	return live, kept, truncated, err
}

// Display rows kept and whether older archived ones were cut, every live row plus the newest archived rows that fit
func fitArchive(liveRows, display []map[string]any) ([]map[string]any, bool, error) {
	live, err := historyRowIDs(liveRows)
	if err != nil {
		return nil, false, err
	}
	used, truncated := 0, false
	for _, row := range liveRows {
		size, err := historyRowSize(row)
		if err != nil {
			return nil, false, err
		}
		used += size
	}
	kept := make([]map[string]any, 0, len(display))
	for i := len(display) - 1; i >= 0; i-- {
		row := display[i]
		id, err := historyRowID(row)
		if err != nil {
			return nil, false, err
		}
		if !live[id] {
			size, err := historyRowSize(row)
			if err != nil {
				return nil, false, err
			}
			used += size
			truncated = truncated || used > resumeHistoryBytes
			if truncated {
				continue
			}
		}
		kept = append(kept, row)
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return kept, truncated, nil
}

// Lineage read of a logical session's history taken before the locked part re-reads only the live tip
func prepareHistory(ctx context.Context, a *SessionAuthority, ref SessionRef) (*preparedHistory, error) {
	target, err := transcriptTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	live, display, truncated, err := displayRows(ctx, a.DB, target)
	if err != nil {
		return nil, err
	}
	return &preparedHistory{target: target, live: live, display: display, truncated: truncated}, nil
}

// Task plan and stored message count of a resume that omits its transcript
func omittedHistoryState(ctx context.Context, db *state.SessionDB, target string) (any, int, error) {
	row, err := db.GetSession(ctx, target)
	if err != nil {
		return nil, 0, err
	}
	if row == nil {
		return nil, 0, runtimeRefusal("not_found")
	}
	plan := todoStateFromDB(ctx, db, target)
	count := 0
	if raw, _ := row.Get("message_count"); pyTruthy(raw) {
		n, err := pyint.Of(raw)
		if err != nil {
			return nil, 0, err
		}
		count = int(n.Int64())
	}
	return plan, count, nil
}

// Live rows in stored order, each archived display row placed before the live row it preceded in display order
func mergeLiveInStoredOrder(display, live []map[string]any) ([]map[string]any, error) {
	current, err := historyRowIDs(live)
	if err != nil {
		return nil, err
	}
	var groups [][]map[string]any
	pending := []map[string]any{}
	shownIDs := map[int64]bool{}
	for _, row := range display {
		id, err := historyRowID(row)
		if err != nil {
			return nil, err
		}
		if current[id] {
			groups = append(groups, pending)
			pending = []map[string]any{}
			shownIDs[id] = true
			continue
		}
		archived := maps.Clone(row)
		archived["archived"] = true
		pending = append(pending, archived)
	}
	ordered := append([]map[string]any{}, live...)
	sortErr := error(nil)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, errA := historyRowID(ordered[i])
		b, errB := historyRowID(ordered[j])
		sortErr = errors.Join(sortErr, errA, errB)
		return a < b
	})
	if sortErr != nil {
		return nil, sortErr
	}
	rows := []map[string]any{}
	seen := 0
	for _, row := range ordered {
		id, _ := historyRowID(row)
		if shownIDs[id] {
			if seen >= len(groups) {
				return nil, &PyError{Kind: "IndexError", Msg: "list index out of range"}
			}
			rows = append(rows, groups[seen]...)
			seen++
		}
		rows = append(rows, row)
	}
	return append(rows, pending...), nil
}

// Display history of a logical session, every stored row the user saw in display order under its durable row id
func localHistory(ctx context.Context, a *SessionAuthority, ref SessionRef, prepared *preparedHistory) ([]map[string]any, error) {
	db := a.DB
	target, err := transcriptTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	var readLive map[int64]bool
	var display []map[string]any
	var truncated bool
	if prepared != nil && prepared.target == target {
		readLive, display, truncated = prepared.live, prepared.display, prepared.truncated
	} else if readLive, display, truncated, err = displayRows(ctx, db, target); err != nil {
		return nil, err
	}
	live, err := db.GetMessagesAsConversation(ctx, target, state.ConversationOptions{IncludeRowIDs: true})
	if err != nil {
		return nil, err
	}
	liveIDs, err := historyRowIDs(live)
	if err != nil {
		return nil, err
	}
	subset, appended := true, false
	for id := range readLive {
		subset = subset && liveIDs[id]
	}
	for id := range liveIDs {
		appended = appended || !readLive[id]
	}
	if !subset {
		if _, display, truncated, err = displayRows(ctx, db, target); err != nil {
			return nil, err
		}
		if live, err = db.GetMessagesAsConversation(ctx, target, state.ConversationOptions{IncludeRowIDs: true}); err != nil {
			return nil, err
		}
	} else if appended {
		var cut bool
		if display, cut, err = fitArchive(live, display); err != nil {
			return nil, err
		}
		truncated = truncated || cut
	}
	merged, err := mergeLiveInStoredOrder(display, live)
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(merged))
	for _, row := range merged {
		out := make(map[string]any, len(row))
		for k, v := range row {
			if k != "_row_id" {
				out[k] = v
			}
		}
		out["row_id"] = row["_row_id"]
		rows = append(rows, out)
	}
	if truncated && len(rows) > 0 {
		rows[0]["archived_truncated"] = true
	}
	return rows, nil
}

// Clears the end stamp of a finalized local session's lineage tip when its first admitted turn runs
func reopenLocalSession(ctx context.Context, a *SessionAuthority, ref SessionRef) error {
	live, ok := a.live(ref.SessionID)
	if !ok || live.Source == nil || live.Source.Platform != PlatformLocal {
		return nil
	}
	target, err := receiptEntryTarget(ctx, a.DB, ref.SessionID)
	if err != nil {
		return err
	}
	row, err := a.DB.GetSession(ctx, target)
	if err != nil || row == nil {
		return err
	}
	if reason, _ := row.Get("end_reason"); reason != nil {
		return a.DB.ReopenSession(ctx, target)
	}
	return nil
}

// Receipts boot restores, an open or compressed row, unsettled input or a stored close
const bootReceiptsSQL = "SELECT m.key FROM state_meta m LEFT JOIN sessions s " +
	"ON s.id = CASE WHEN json_valid(m.value) THEN json_extract(m.value, '$.entry.session_id') END " +
	"WHERE m.key LIKE ? AND (s.ended_at IS NULL OR s.end_reason = 'compression' " +
	"OR EXISTS (SELECT 1 FROM session_admissions a WHERE a.target_session_id = substr(m.key, ?) " +
	"AND a.status IN ('queued','started','unknown')) " +
	"OR EXISTS (SELECT 1 FROM state_meta c WHERE c.key = ? || substr(m.key, ?)))"

// Restores the ended local session a background completion reports to after a restart left it unrestored
func restoreCompletionRoute(ctx context.Context, runner any, sessionKey string) error {
	authority, err := activeAuthority(ctx, runner)
	if err != nil {
		return err
	}
	parsed := parseSessionKey(sessionKey)
	if authority == nil || parsed["platform"] != any(string(PlatformLocal)) {
		return nil
	}
	chatID, has := parsed["chat_id"]
	if !has {
		return &pyKeyError{key: "chat_id"}
	}
	sid := pyStrOf(chatID)
	if _, live := authority.live(sid); live {
		return nil
	}
	if _, err := restoreLocalSession(ctx, authority, sid); err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return nil
		}
		return err
	}
	return nil
}

// Retires every local receipt whose current target a messaging lane now holds, logging each release
func releaseHandedOffReceipts(ctx context.Context, db *state.SessionDB, epoch int64) error {
	released, err := db.ReleaseHandedOffReceipts(ctx, epoch, func(source any) bool {
		_, messaging := messagingPlatform(ctx, source)
		return messaging
	})
	if err != nil {
		return err
	}
	for _, moved := range released {
		slog.Info(fmt.Sprintf("Local session %s was handed off to %s (row %s); released its local receipt",
			moved.Owner, pyrepr.Str(moved.Source), moved.Target))
	}
	return nil
}

// Session ids of the local receipts boot restores
func localReceiptIDs(ctx context.Context, a *SessionAuthority) ([]string, error) {
	start := pystr.Len(state.LocalPolicyPrefix) + 1
	var ids []string
	err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		ids = nil
		rows, err := conn.QueryContext(ctx, bootReceiptsSQL, state.LocalPolicyPrefix+"%", start, state.ViewerClosePrefix, start)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			ids = append(ids, key[len(state.LocalPolicyPrefix):])
		}
		return rows.Err()
	})
	return ids, err
}

// True when any admission row is in the unknown state
func anyUnknown(rows []*pyjson.Object) bool {
	for _, row := range rows {
		if rowString(row, "status") == AdmissionUnknown {
			return true
		}
	}
	return false
}

// Restores every boot receipt, scheduling pending input and rearming stored closes when asked
func recoverLocalSessions(ctx context.Context, a *SessionAuthority, schedule bool) error {
	ids, err := localReceiptIDs(ctx, a)
	if err != nil {
		return err
	}
	var orphans []string
	for _, sid := range ids {
		ref, err := restoreLocalSession(ctx, a, sid)
		var pending []*pyjson.Object
		if err == nil {
			pending, err = a.DB.ListSessionAdmissions(ctx, sid, true)
		}
		if err != nil {
			var refused *receiptRefused
			var catchUp *catchUpPending
			var refusal *state.RuntimeStoreError
			switch {
			case errors.As(err, &refused), errors.As(err, &catchUp):
			case errors.As(err, &refusal):
				slog.Warn(fmt.Sprintf("Local session %s paused: %s", sid, refusal.Reason))
			default:
				return err
			}
			continue
		}
		if anyUnknown(pending) {
			continue
		}
		if schedule && len(pending) > 0 {
			scheduleSession(ctx, a, ref)
		}
		orphans = append(orphans, sid)
	}
	if !schedule {
		return nil
	}
	rearmed, err := rearmPersistedCloses(ctx, a)
	if err != nil {
		return err
	}
	for _, sid := range rearmed {
		scheduleSession(ctx, a, SessionRef{ProfileID: a.ProfileID, SessionID: sid})
		rows, err := a.DB.ListSessionAdmissions(ctx, sid, true)
		if err != nil {
			return err
		}
		if anyUnknown(rows) {
			trackMutation(a, func(taskCtx context.Context) (any, error) {
				return nil, settleViewerClose(taskCtx, a, sid)
			})
		}
	}
	return armStartupOrphanCloses(ctx, a, orphans)
}
