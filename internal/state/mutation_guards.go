package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

const (
	ReasonRuntimeCoordinationRequired = "runtime_coordination_required"
	historyBindingPrefix              = "gateway.history_control.v1."
)

var ErrNoGatewayHooks = errors.New("store was opened without the gateway identity and routing hooks")

type GatewayHooks struct {
	SameOwner   func(stored, subject string) bool
	ResetEntry  func(previous json.RawMessage, sessionID string) (json.RawMessage, error)
	BranchEntry func(parent json.RawMessage, sessionID string) (BranchRoute, error)
}

type BranchRoute struct {
	Route     string
	Origin    json.RawMessage
	Entry     json.RawMessage
	UpdatedAt float64
}

type Actor struct {
	ProfileID    string
	Subject      string
	Capabilities []string
}

func (s *Store) gatewayHooks() GatewayHooks { return s.tables.state.options.Gateway }

func (h GatewayHooks) sameOwner(stored, subject sql.NullString) (bool, error) {
	if stored == subject {
		return true, nil
	}
	if !stored.Valid || stored.String == "" || !subject.Valid || subject.String == "" {
		return false, nil
	}
	if h.SameOwner == nil {
		return false, ErrNoGatewayHooks
	}
	return h.SameOwner(stored.String, subject.String), nil
}

func (h GatewayHooks) ownersAgree(first, second sql.NullString) (bool, error) {
	if same, err := h.sameOwner(first, second); err != nil || same {
		return same, err
	}
	return h.sameOwner(second, first)
}

func BusyRefusal(err error) (error, bool) {
	var lease *TurnLeaseLostError
	if errors.As(err, &lease) {
		return runtimeErr(ReasonSessionBusy), true
	}
	var busy *CompressionBusyError
	if errors.As(err, &busy) && busy.Live {
		return runtimeErrAt(ReasonSessionBusy, "compression", ""), true
	}
	return nil, false
}

func liveStates(ctx context.Context, c Conn, sessionID, admissionStatuses string) ([]string, error) {
	admissions, err := queryStrings(ctx, c, "SELECT status FROM session_admissions WHERE target_session_id=? AND status IN ("+admissionStatuses+")", sessionID)
	if err != nil {
		return nil, err
	}
	workers, err := queryStrings(ctx, c, "SELECT status FROM worker_executions WHERE session_id=? AND status IN ('registered','running','unknown')", sessionID)
	return append(admissions, workers...), err
}

func refuseLiveStates(states []string) error {
	if slices.Contains(states, "unknown") {
		return runtimeErr(ReasonUnknownExecution)
	}
	if len(states) > 0 {
		return runtimeErr(ReasonSessionBusy)
	}
	return nil
}

func requireNotExecuting(ctx context.Context, c Conn, sessionIDs []string) error {
	for _, sid := range sessionIDs {
		states, err := liveStates(ctx, c, sid, "'started','unknown'")
		if err != nil {
			return err
		}
		if err := refuseLiveStates(states); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) requireTranscriptUnleased(ctx context.Context, c Conn, sessionID string) error {
	closed, err := endedByCompression(ctx, c, sessionID)
	if err != nil || closed {
		return err
	}
	return s.checkTranscriptWriteGuards(ctx, c, sessionID, writeGuards{rejectActiveTurnLease: true, rejectActiveCompressionLock: true})
}

func (s *Store) requireTargetAdvanceable(ctx context.Context, c Conn, sessionIDs []string) error {
	if err := requireNotExecuting(ctx, c, sessionIDs); err != nil {
		return err
	}
	for _, sid := range sessionIDs {
		if err := s.requireTranscriptUnleased(ctx, c, sid); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) requireIdle(ctx context.Context, c Conn, sessionIDs []string) error {
	for _, sid := range sessionIDs {
		states, err := liveStates(ctx, c, sid, "'queued','started','unknown'")
		if err != nil {
			return err
		}
		if err := refuseLiveStates(states); err != nil {
			return err
		}
		if err := s.requireTranscriptUnleased(ctx, c, sid); err != nil {
			return err
		}
	}
	return nil
}

func uniqueIDs(ids ...string) []string {
	var out []string
	for _, id := range ids {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func continuationEdges(ctx context.Context, c Conn, sessionID string) ([]string, error) {
	edge := nonContinuationChildSQL("child.", "parent.id")
	return queryStrings(ctx, c, "SELECT child.id FROM sessions parent JOIN sessions child ON child.parent_session_id=parent.id "+
		"WHERE parent.id=? AND parent.end_reason='compression'\n"+edge+
		" UNION SELECT parent.id FROM sessions child JOIN sessions parent ON parent.id=child.parent_session_id "+
		"WHERE child.id=? AND parent.end_reason='compression'\n"+edge, sessionID, sessionID)
}

func (s *Store) deleteTargets(ctx context.Context, c Conn, sessionID string) ([]string, error) {
	targets := map[string]bool{sessionID: true}
	owner, err := LocalLineageOwnerIn(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	targets[owner] = true
	receipt, err := storedLocalReceipt(ctx, c, owner)
	if err != nil {
		return nil, err
	}
	if receipt != nil {
		if _, err := validateLocalLineage(ctx, c, receipt); err != nil {
			return nil, err
		}
		lineage := []string{sessionID}
		if raw, has := memberValue(receipt, "lineage"); has {
			lineage, _ = stringList(raw)
		}
		for _, id := range lineage {
			targets[id] = true
		}
	}
	anchor := map[string]string{}
	var frontier []string
	for id := range targets {
		anchor[id] = sessionID
		frontier = append(frontier, id)
	}
	slices.Sort(frontier)
	for range chainCap {
		found := map[string]string{}
		var order []string
		for _, sid := range frontier {
			edges, err := continuationEdges(ctx, c, sid)
			if err != nil {
				return nil, err
			}
			for _, next := range edges {
				if targets[next] || found[next] != "" {
					continue
				}
				owned, err := s.sameHistoryOwner(ctx, c, anchor[sid], next)
				if err != nil {
					return nil, err
				}
				if owned {
					found[next] = anchor[sid]
					order = append(order, next)
				}
			}
		}
		delegates, err := collectDelegateChildIDs(ctx, c, frontier)
		if err != nil {
			return nil, err
		}
		for _, sid := range delegates {
			if !targets[sid] && found[sid] == "" {
				found[sid] = sid
				order = append(order, sid)
			}
		}
		if len(found) == 0 {
			break
		}
		for id, a := range found {
			anchor[id] = a
			targets[id] = true
		}
		frontier = order
	}
	rest := make([]string, 0, len(targets))
	for id := range targets {
		if id != sessionID {
			rest = append(rest, id)
		}
	}
	slices.Sort(rest)
	return append([]string{sessionID}, rest...), nil
}

type historyRow struct {
	row     Session
	present bool
}

func readHistoryRow(ctx context.Context, c Conn, sessionID string) (historyRow, error) {
	rows, err := querySessions(c.QueryContext(ctx, "SELECT * FROM sessions WHERE id=?", sessionID))
	if err != nil || len(rows) == 0 {
		return historyRow{}, err
	}
	return historyRow{row: rows[0], present: true}, nil
}

func routed(row Session) bool {
	return row.SessionKey.V != "" || row.ChatID.V != "" || row.OriginJSON.V != ""
}

func historySource(row Session) bool {
	return row.Source == "import" || legacyHistoryRow(row)
}

func ownerUser(row Session) sql.NullString {
	if legacyAccountLabel(row) {
		return sql.NullString{}
	}
	return sql.NullString{String: row.UserID.V, Valid: row.UserID.Valid}
}

func historyBindingValue(profileID, subject string) string {
	enc, _ := json.Marshal([]string{profileID, subject})
	return string(enc)
}

func (s *Store) bindingNames(saved, profileID, subject string) (bool, error) {
	var pair []json.RawMessage
	if json.Unmarshal([]byte(saved), &pair) != nil || len(pair) != 2 {
		return false, nil
	}
	if !jsonIsString(pair[0]) || jsonString(pair[0]) != profileID {
		return false, nil
	}
	if !jsonIsString(pair[1]) {
		return false, nil
	}
	return s.gatewayHooks().sameOwner(sql.NullString{String: jsonString(pair[1]), Valid: true}, sql.NullString{String: subject, Valid: true})
}

func (s *Store) BindingNames(saved, profileID, subject string) (bool, error) {
	return s.bindingNames(saved, profileID, subject)
}

func (s *Store) bindingsAgree(first, second sql.NullString) (bool, error) {
	if first == second || !first.Valid || !second.Valid {
		return first == second, nil
	}
	var a, b []json.RawMessage
	if json.Unmarshal([]byte(first.String), &a) != nil || len(a) != 2 || json.Unmarshal([]byte(second.String), &b) != nil || len(b) != 2 {
		return false, nil
	}
	if jsonCompareKey(a[0]) != jsonCompareKey(b[0]) || !jsonIsString(a[1]) || !jsonIsString(b[1]) {
		return false, nil
	}
	return s.gatewayHooks().ownersAgree(sql.NullString{String: jsonString(a[1]), Valid: true}, sql.NullString{String: jsonString(b[1]), Valid: true})
}

func ImportHistoryControlIn(ctx context.Context, c Conn, actor Actor, sessionIDs []string) error {
	binding := historyBindingValue(actor.ProfileID, actor.Subject)
	for _, sid := range sessionIDs {
		var one int
		err := c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id=?", sid).Scan(&one)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", historyBindingPrefix+sid, binding); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) AuthorizeHistoryIn(ctx context.Context, c Conn, actor Actor, sessionID string, claim bool) (bool, error) {
	h, err := readHistoryRow(ctx, c, sessionID)
	if err != nil || !h.present || routed(h.row) {
		return false, err
	}
	key := historyBindingPrefix + sessionID
	saved, found, err := metaValue(ctx, c, key)
	if err != nil {
		return false, err
	}
	if found {
		bound, err := s.bindingNames(saved, actor.ProfileID, actor.Subject)
		if err != nil {
			return false, err
		}
		if !bound {
			return false, runtimeErr(ReasonPermissionDenied)
		}
		return true, nil
	}
	if !historySource(h.row) {
		return false, nil
	}
	if owner := ownerUser(h.row); owner.String != "" {
		same, err := s.gatewayHooks().sameOwner(owner, sql.NullString{String: actor.Subject, Valid: true})
		if err != nil {
			return false, err
		}
		if !same {
			return false, runtimeErr(ReasonPermissionDenied)
		}
	}
	if !slices.Contains(actor.Capabilities, "session:create") {
		return false, runtimeErr(ReasonPermissionDenied)
	}
	if claim {
		if _, err := c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, historyBindingValue(actor.ProfileID, actor.Subject)); err != nil {
			return false, err
		}
	}
	return true, nil
}

func historyBinding(ctx context.Context, c Conn, sessionID string) (sql.NullString, error) {
	saved, found, err := metaValue(ctx, c, historyBindingPrefix+sessionID)
	return sql.NullString{String: saved, Valid: found}, err
}

func (s *Store) sameHistoryOwner(ctx context.Context, c Conn, anchorID, sessionID string) (bool, error) {
	anchor, err := readHistoryRow(ctx, c, anchorID)
	if err != nil {
		return false, err
	}
	row, err := readHistoryRow(ctx, c, sessionID)
	if err != nil || !anchor.present || !row.present {
		return false, err
	}
	a, r := anchor.row, row.row
	if routed(r) {
		return r.UserID == a.UserID && r.SessionKey == a.SessionKey && r.ChatID == a.ChatID, nil
	}
	binding, err := historyBinding(ctx, c, sessionID)
	if err != nil {
		return false, err
	}
	anchorBinding, err := historyBinding(ctx, c, anchorID)
	if err != nil {
		return false, err
	}
	agree, err := s.bindingsAgree(binding, anchorBinding)
	if err != nil || !agree {
		return false, err
	}
	if binding.Valid {
		return true, nil
	}
	routelessPeer := r.Source == a.Source && a.SessionKey.V == "" && a.ChatID.V == "" && a.OriginJSON.V == ""
	if !historySource(r) && !routelessPeer {
		return false, nil
	}
	owner := ownerUser(r)
	if owner.String == "" {
		return true, nil
	}
	return s.gatewayHooks().ownersAgree(owner, sql.NullString{String: a.UserID.V, Valid: a.UserID.Valid})
}
