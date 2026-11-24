package gateway

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

var queuedTextOnly = map[string]bool{"text": true, "surface_v1": true, "display_v1": true, "local_operator_v1": true}

var queuedInteractiveSources = map[string]bool{"cli": true, "tui": true, "gui": true, state.ClientHistorySource: true, "acp": true}

// Connection surface the queued row verbs read, its authority, actor, operator flag and submission parser
type QueuedControlConnection interface {
	SessionAuthority() *SessionAuthority
	Actor() Principal
	Operator() bool
	Submission(ctx context.Context, ref SessionRef, params *pyjson.Object) (Submission, error)
}

// Queued admissions handed to one running claim, each with whether its text reached the claim's agent
type steerTransfer struct {
	generation int64
	order      []string
	delivered  map[string]bool
}

// Agent a turn adopted with the generation that adopted it
type adoptedAgent struct {
	generation int64
	agent      any
}

// Agent seam taking a mid turn steer
type SteerReceiver interface {
	Steer(text string) bool
}

// Agent seam taking a ticketed steer its settlement can withdraw
type ticketedSteerReceiver interface {
	SteerTicket(text string, ticket any) bool
	WithdrawSteer(ticket any) bool
}

// Queued row verbs of a connection keyed by method name
func queuedControlHandlers(connection QueuedControlConnection) map[string]func(ctx context.Context, ref SessionRef, params *pyjson.Object) (*pyjson.Object, error) {
	return map[string]func(ctx context.Context, ref SessionRef, params *pyjson.Object) (*pyjson.Object, error){
		"prompt.replace_queued": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (*pyjson.Object, error) {
			return replaceQueued(ctx, connection, ref, params)
		},
	}
}

// True when the session's frozen launch source names an interactive local surface, read once per session
func queuedInteractive(ctx context.Context, a *SessionAuthority, sessionID string) (bool, error) {
	a.mu.Lock()
	source, cached := a.launchSources[sessionID]
	a.mu.Unlock()
	if !cached {
		found := false
		err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			rows, err := conn.QueryContext(ctx, "SELECT source FROM sessions WHERE id=?", sessionID)
			if err != nil {
				return err
			}
			defer rows.Close()
			if rows.Next() {
				found = true
				var value any
				if err := rows.Scan(&value); err != nil {
					return err
				}
				switch v := value.(type) {
				case string:
					source = v
				case []byte:
					source = string(v)
				default:
					source = value
				}
			}
			return rows.Err()
		})
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		a.mu.Lock()
		if a.launchSources == nil {
			a.launchSources = map[string]any{}
		}
		a.launchSources[sessionID] = source
		a.mu.Unlock()
	}
	name, _ := source.(string)
	return queuedInteractiveSources[name], nil
}

// Queued row verbs this owner serves for a row, none for anything but interactive plain text
func queuedControls(ctx context.Context, a *SessionAuthority, row *pyjson.Object) ([]string, error) {
	payloadValue, _ := row.Get("payload")
	payload, _ := payloadValue.(*pyjson.Object)
	if payload == nil {
		payload = pyjson.NewObject()
	}
	text, _ := payload.Get("text")
	textStr, isText := text.(string)
	if rowString(row, "status") != AdmissionQueued || !isText || strings.TrimSpace(textStr) == "" {
		return []string{}, nil
	}
	for _, key := range payload.Keys {
		if !queuedTextOnly[key] {
			return []string{}, nil
		}
	}
	if display, _ := payload.Get("display_v1"); pyTruthy(display) {
		if obj, ok := display.(*pyjson.Object); ok {
			if kind, _ := obj.Get("kind"); kind == "hidden" {
				return []string{}, nil
			}
		}
	}
	interactive, err := queuedInteractive(ctx, a, rowString(row, "target_session_id"))
	if err != nil {
		return nil, err
	}
	if !interactive {
		return []string{}, nil
	}
	return []string{"steer", "replace"}, nil
}

// Pending rows a viewer is shown plus each queued admission in transfer to the running claim
func pendingRows(ctx context.Context, a *SessionAuthority, ref SessionRef) ([]*pyjson.Object, error) {
	masked, err := a.pendingRowsMasked(ctx, ref)
	if err != nil {
		return nil, err
	}
	byID := map[string]*pyjson.Object{}
	var order []string
	for _, row := range masked {
		id := rowString(row, "admission_id")
		if _, seen := byID[id]; !seen {
			order = append(order, id)
		}
		byID[id] = row
	}
	a.mu.Lock()
	var transfers []string
	if entry := a.steerTransfers[ref.SessionID]; entry != nil {
		transfers = append(transfers, entry.order...)
	}
	a.mu.Unlock()
	for _, admissionID := range transfers {
		row, err := a.DB.GetSessionAdmission(ctx, admissionID)
		if err != nil {
			return nil, err
		}
		if outcome, _ := rowGet(row, "outcome"); row != nil && rowString(row, "status") == AdmissionTerminal && outcome == state.Steering {
			if _, seen := byID[admissionID]; !seen {
				order = append(order, admissionID)
			}
			byID[admissionID] = row
		}
	}
	out := make([]*pyjson.Object, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	sortRowsBySeq(out)
	return out, nil
}

// Column of a row that may be absent, nil for a missing row
func rowGet(row *pyjson.Object, key string) (any, bool) {
	if row == nil {
		return nil, false
	}
	return row.Get(key)
}

// Receipt and stored row of an admission the connection's actor may read
func queuedRow(ctx context.Context, connection QueuedControlConnection, ref SessionRef, admissionID string) (AdmissionReceipt, *pyjson.Object, error) {
	a := connection.SessionAuthority()
	receipt, err := a.Receipt(ctx, connection.Actor(), ref, admissionID)
	if err != nil {
		return AdmissionReceipt{}, nil, err
	}
	row, err := a.DB.GetSessionAdmission(ctx, admissionID)
	return receipt, row, err
}

// Transfers handed to the claim with their delivery, a delivered one the agent withdraws reading undelivered
func steerTransfers(a *SessionAuthority, sessionID string, generation int64) map[string]bool {
	a.mu.Lock()
	entry := a.steerTransfers[sessionID]
	adopted, hasAdopted := a.adopted[sessionID]
	a.mu.Unlock()
	if entry == nil || entry.generation != generation {
		return nil
	}
	var withdraw func(ticket any) bool
	if hasAdopted && adopted.generation == generation {
		if agent, ok := adopted.agent.(ticketedSteerReceiver); ok {
			withdraw = agent.WithdrawSteer
		}
	}
	out := map[string]bool{}
	for _, admissionID := range entry.order {
		a.mu.Lock()
		delivered := entry.delivered[admissionID]
		a.mu.Unlock()
		if delivered && withdraw != nil && withdraw(admissionID) {
			delivered = false
			a.mu.Lock()
			entry.delivered[admissionID] = false
			a.mu.Unlock()
		}
		out[admissionID] = delivered
	}
	return out
}

// Publishes the outcome each admission a settled claim carried settled to, an undelivered one queued again
func publishTransfers(ctx context.Context, a *SessionAuthority, ref SessionRef, generation int64) error {
	a.mu.Lock()
	live := a.Sessions[ref.SessionID]
	entry := a.steerTransfers[ref.SessionID]
	a.mu.Unlock()
	if live == nil || entry == nil || entry.generation != generation {
		return nil
	}
	live.EventStream.Lock.Lock()
	a.mu.Lock()
	delete(a.steerTransfers, ref.SessionID)
	order := append([]string(nil), entry.order...)
	a.mu.Unlock()
	live.EventStream.Lock.Unlock()
	for _, admissionID := range order {
		row, err := a.DB.GetSessionAdmission(ctx, admissionID)
		if err != nil {
			return err
		}
		outcome, _ := rowGet(row, "outcome")
		if row != nil && rowString(row, "status") == AdmissionTerminal && (outcome == "steered" || outcome == "interrupted") {
			published := "cancelled"
			if outcome == "steered" {
				published = "steered"
			}
			if err := a.settleCancelled(ctx, ref, admissionID, published); err != nil {
				return err
			}
		}
	}
	return a.publishPending(ctx, ref)
}

// Cancels the queued admission and admits its edit at the FIFO tail under a new identity in one write
func replaceQueued(ctx context.Context, connection QueuedControlConnection, ref SessionRef, params *pyjson.Object) (result *pyjson.Object, err error) {
	a, actor, oldID := connection.SessionAuthority(), connection.Actor(), rowString(params, "admission_id")
	rest := pyjson.NewObject()
	for _, key := range params.Keys {
		if key != "admission_id" {
			value, _ := params.Get(key)
			rest.Set(key, value)
		}
	}
	request, err := connection.Submission(ctx, ref, rest)
	if err != nil {
		return nil, err
	}
	receipt, row, err := queuedRow(ctx, connection, ref, oldID)
	if err != nil {
		return nil, err
	}
	replacement, err := a.DB.FindSessionAdmission(ctx, actor.Subject, ref.SessionID, request.RequestID)
	if err != nil {
		return nil, err
	}
	if replacement == nil {
		if receipt.Status != AdmissionQueued {
			return nil, runtimeRefusal("admission_started")
		}
		controls, err := queuedControls(ctx, a, row)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(controls, "replace") {
			return nil, runtimeRefusal("unsupported_control")
		}
		a.mu.Lock()
		a.cancelObligations[oldID] = true
		a.mu.Unlock()
	} else if receipt.Status != AdmissionTerminal || receipt.Outcome == nil || *receipt.Outcome != "cancelled" {
		return nil, runtimeRefusal("admission_conflict")
	}
	binding, err := state.ReplacementBinding(actor.Subject, ref.SessionID, request.RequestID)
	if err != nil {
		return nil, err
	}
	admitted, err := func() (admitted AdmissionReceipt, err error) {
		defer func() {
			old, readErr := a.DB.GetSessionAdmission(ctx, oldID)
			if readErr != nil {
				err = readErr
				return
			}
			outcome, _ := old.Get("outcome")
			a.mu.Lock()
			owed := a.cancelObligations[oldID]
			a.mu.Unlock()
			if owed && rowString(old, "status") == AdmissionTerminal && outcome == "cancelled" {
				if settleErr := a.settleCancelled(ctx, ref, oldID, "cancelled"); settleErr != nil {
					err = settleErr
				}
			} else if rowString(old, "status") == AdmissionQueued {
				a.mu.Lock()
				delete(a.cancelObligations, oldID)
				a.mu.Unlock()
			}
		}()
		return a.Submit(ctx, actor, request, SubmitOptions{QueueWhileRestarting: connection.Operator(),
			AuthorizeWrite: func(wctx context.Context, conn state.SQLiteConn) error {
				return state.RetireReplacedInput(wctx, conn, oldID, binding)
			},
			ReplayGuard: func(wctx context.Context, conn state.SQLiteConn) error {
				return state.CheckReplacedInput(wctx, conn, oldID, binding)
			}})
	}()
	if err != nil {
		return nil, err
	}
	if err := releaseAdmissionMedia(ctx, a.DB, oldID); err != nil {
		return nil, err
	}
	return admissionReceiptDict(admitted), nil
}
