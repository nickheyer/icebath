package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reply watcher of one delivery, the admission it follows with its task and waiter
type botReplyWatch struct {
	admissionID string
	task        *adapterTask
	future      *admissionWaiter
}

// Value of a receipt field, the KeyError Python raises when it is absent
func botReceiptField(record map[string]any, key string) (any, error) {
	value, ok := record[key]
	if !ok {
		return nil, &pyKeyError{key}
	}
	return value, nil
}

// Text of a receipt field as Python passes it to SQL and formatting
func receiptText(record map[string]any, key string) (string, error) {
	value, err := botReceiptField(record, key)
	if err != nil {
		return "", err
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	return pyrepr.Str(value), nil
}

// Request id of a delivery's one retry admission
func botRetryIdentity(key string) string { return "bot:" + key + ":retry" }

// Admission id holding request id on the session, empty when none
func botAdmissionAt(ctx context.Context, a *SessionAuthority, sessionID, requestID string) (string, error) {
	var admissionID string
	err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		err := conn.QueryRowContext(ctx, "SELECT admission_id FROM session_admissions WHERE target_session_id=? AND request_id=?",
			sessionID, requestID).Scan(&admissionID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return admissionID, err
}

// The delivery's one retry admission id, read from the FIFO by its derived identity
func botRetryAdmission(ctx context.Context, a *SessionAuthority, record map[string]any) (string, error) {
	sessionID, err := receiptText(record, "session_id")
	if err != nil {
		return "", err
	}
	deliveryID, err := receiptText(record, "delivery_id")
	if err != nil {
		return "", err
	}
	return botAdmissionAt(ctx, a, sessionID, botRetryIdentity(deliveryID))
}

// Reply of a fallback receipt, empty when it carries none
func receiptReply(record map[string]any) any {
	if reply, ok := record["reply"]; ok {
		return reply
	}
	return ""
}

// Final answer of a receipt that outlived its admission, ambiguous once its deleted session can never answer
func botExpiredOutcome(ctx context.Context, a *SessionAuthority, record map[string]any) (string, any, error) {
	if record == nil {
		record = map[string]any{}
	}
	if status, ok := record["status"].(string); ok && deliveryTerminal[status] {
		return status, receiptReply(record), nil
	}
	if sessionID := record["session_id"]; pyTruthy(sessionID) {
		sid := pyrepr.Str(sessionID)
		retired, err := a.DB.RetiredSession(context.WithoutCancel(ctx), sid)
		if err != nil {
			return "", nil, err
		}
		if retired {
			return "ambiguous", receiptReply(record), nil
		}
		row, err := a.DB.GetSession(ctx, sid)
		if err != nil {
			return "", nil, err
		}
		if row == nil {
			slog.Warn(fmt.Sprintf("Bot delivery %s: session %s is gone with no deletion marker; settled as ambiguous",
				pyrepr.Str(record["delivery_id"]), sid))
			return "ambiguous", receiptReply(record), nil
		}
	}
	return "", nil, runtimeRefusal("storage_unavailable")
}

// Status and reply of one admission read from its committed result, never transcript recency
func botAdmissionOutcome(ctx context.Context, a *SessionAuthority, admissionID string, fallback map[string]any) (string, any, error) {
	row, err := a.DB.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return "", nil, err
	}
	if row == nil {
		return botExpiredOutcome(ctx, a, fallback)
	}
	saved, err := admissionResult(ctx, a.DB, admissionID)
	if err != nil {
		return "", nil, err
	}
	if saved != nil && rowString(row, "status") != AdmissionTerminal {
		if row, err = a.DB.GetSessionAdmission(ctx, admissionID); err != nil {
			return "", nil, err
		}
		if row == nil {
			return "", nil, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
		}
	}
	outcome, present := row.Get("outcome")
	if !present {
		return "", nil, &pyKeyError{"outcome"}
	}
	rawStatus, _ := row.Get("status")
	var status string
	switch rawStatus {
	case "queued":
		status = "queued"
	case "started":
		status = "claimed"
	case "unknown":
		status = "ambiguous"
	case "terminal":
		switch outcome {
		case "cancelled":
			status = "cancelled"
		case "rejected", "failed":
			status = "failed"
		default:
			status = "ambiguous"
		}
	default:
		return "", nil, &pyKeyError{pyrepr.Str(rawStatus)}
	}
	var reply any = ""
	if fallback != nil {
		reply = receiptReply(fallback)
	}
	if saved != nil {
		result, err := botSavedResult(saved)
		if err != nil {
			return "", nil, err
		}
		if reply, err = pyDictGet(result, "final_response", ""); err != nil {
			return "", nil, err
		}
		status = "failed"
		if outcome == "completed" {
			status = "settled"
			if IsIntentionalSilenceResponse(reply) {
				reply = ""
			} else {
				reply = StripTrailingSilenceMarker(reply)
			}
		}
	} else if fallback != nil {
		if fallbackStatus, ok := fallback["status"].(string); ok && (fallbackStatus == "settled" || fallbackStatus == "failed") {
			status = fallbackStatus
		}
	}
	return status, reply, nil
}

// Result mapping of a committed admission result, the KeyError Python raises without one
func botSavedResult(saved any) (any, error) {
	obj, ok := saved.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(saved) + "' object is not subscriptable"}
	}
	result, present := obj.Get("result")
	if !present {
		return nil, &pyKeyError{"result"}
	}
	return result, nil
}

// Error text and typed reason of a failed admission from its committed result
func botFailure(ctx context.Context, a *SessionAuthority, admissionID string) (any, string, error) {
	saved, err := admissionResult(ctx, a.DB, admissionID)
	if err != nil {
		return nil, "", err
	}
	var result any = pyjson.NewObject()
	if saved != nil {
		if got, err := pyDictGet(saved, "result", nil); err != nil {
			return nil, "", err
		} else if pyTruthy(got) {
			result = got
		}
	}
	rawError, err := pyDictGet(result, "error", nil)
	if err != nil {
		return nil, "", err
	}
	failureReason, _ := pyDictGet(result, "failure_reason", nil)
	errorText := rawError
	if !pyTruthy(errorText) {
		errorText, _ = pyDictGet(result, "final_response", nil)
	}
	if !pyTruthy(errorText) {
		errorText = "Bot Chat delivery failed"
	}
	text, err := tools.TurnFailureText(rawError, failureReason)
	if err != nil {
		return nil, "", err
	}
	var classified any = text
	if text == "" {
		classified = errorText
	}
	return errorText, tools.ClassifyAgentError(classified), nil
}

// The delivery's receipt, its status and reply following the one retry once that was admitted
func botResult(ctx context.Context, a *SessionAuthority, record map[string]any) (map[string]any, error) {
	retryID, err := botRetryAdmission(ctx, a, record)
	if err != nil {
		return nil, err
	}
	admission, err := botReceiptField(record, "admission_id")
	if err != nil {
		return nil, err
	}
	current := retryID
	if current == "" {
		current = pyrepr.Str(admission)
	}
	var status string
	var reply any
	if retryID != "" {
		status, reply, err = botAdmissionOutcome(ctx, a, retryID, nil)
	} else {
		status, reply, err = botAdmissionOutcome(ctx, a, pyrepr.Str(admission), record)
	}
	if err != nil {
		return nil, err
	}
	var errorText, reason any
	if status == "failed" {
		text, typed, err := botFailure(ctx, a, current)
		switch {
		case err == nil:
			errorText, reason = text, typed
		case isRuntimeRefusal(err, "not_found"):
			errorText, reason = record["error"], record["reason"]
			if !pyTruthy(errorText) {
				errorText = "Bot Chat delivery failed"
			}
			if !pyTruthy(reason) {
				reason = tools.ReasonUnknown
			}
		default:
			return nil, err
		}
	}
	out := map[string]any{"status": status}
	for _, key := range []string{"delivery_id", "profile_home", "session_id", "admission_id", "message"} {
		value, err := botReceiptField(record, key)
		if err != nil {
			return nil, err
		}
		if value != nil {
			out[key] = value
		}
	}
	if reply != nil {
		out["reply"] = reply
	}
	if retryID != "" {
		out["retry_admission_id"] = retryID
	}
	if errorText != nil {
		out["error"] = errorText
	}
	if reason != nil {
		out["reason"] = reason
	}
	return out, nil
}

// Receipt a sender sees, a failure still owed its one retry projected as claimed
func botDeliveryResult(ctx context.Context, a *SessionAuthority, record map[string]any) (map[string]any, error) {
	result, err := botResult(ctx, a, record)
	if err != nil {
		return nil, err
	}
	if result["status"] == "failed" {
		eligible, err := botRetryEligible(ctx, a, record)
		if err != nil {
			return nil, err
		}
		if eligible {
			result["status"] = "claimed"
		}
	}
	return result, nil
}

// Whether the original admission settled failed with a committed result whose error classifies transient
func botRetryEligible(ctx context.Context, a *SessionAuthority, record map[string]any) (bool, error) {
	if retry, _ := record["retry"].(map[string]any); retry != nil && pyTruthy(retry["refused"]) {
		return false, nil
	} else if retry == nil && pyTruthy(record["retry"]) {
		if _, err := pyDictGet(record["retry"], "refused", nil); err != nil {
			return false, err
		}
	}
	retryID, err := botRetryAdmission(ctx, a, record)
	if err != nil || retryID != "" {
		return false, err
	}
	admission, err := receiptText(record, "admission_id")
	if err != nil {
		return false, err
	}
	row, err := a.DB.GetSessionAdmission(ctx, admission)
	if err != nil {
		return false, err
	}
	if row == nil || rowString(row, "status") != AdmissionTerminal || rowString(row, "outcome") != "failed" {
		return false, nil
	}
	saved, err := admissionResult(ctx, a.DB, admission)
	if err != nil || saved == nil {
		return false, err
	}
	result, err := botSavedResult(saved)
	if err != nil {
		return false, err
	}
	action, err := tools.ResultRetryAction(result)
	if err != nil {
		return false, err
	}
	return action != tools.RetryNone, nil
}

// Admits the delivery's one retry under its derived identity recorded on the same receipt first
func botMaybeRetry(ctx context.Context, a *SessionAuthority, home string, record map[string]any) error {
	eligible, err := botRetryEligible(ctx, a, record)
	if err != nil || !eligible {
		return err
	}
	deliveryID, err := receiptText(record, "delivery_id")
	if err != nil {
		return err
	}
	identity := botRetryIdentity(deliveryID)
	capabilities := map[string]bool{"session:submit": true, "session:read": true}
	if record["operator"] == true {
		capabilities["session:operator"] = true
	}
	principal, err := receiptText(record, "principal_id")
	if err != nil {
		return err
	}
	actor := Principal{Subject: principal, ProfileID: a.ProfileID, Capabilities: capabilities, TransportID: "bot-delivery-retry"}
	live, entry, err := botRetryTarget(ctx, a, actor, record)
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		if refusal.Reason == "runtime_draining" {
			record["status"], record["reply"] = "claimed", ""
			return nil
		}
		record["retry"] = map[string]any{"identity": identity, "refused": refusal.Reason}
		_, err = writeReceipt(ctx, home, record, nil)
		return err
	}
	if err != nil {
		return err
	}
	text, err := receiptText(record, "message")
	if err != nil {
		return err
	}
	event := NewMessageEvent(text)
	event.Source, event.Internal, event.MessageID = live.Source, true, &identity
	event.Metadata = map[string]any{"gateway_session_key": live.Route, "gateway_session_id": entry.SessionID}
	notification, err := automationNotificationMetadata(func(key string) (any, bool) {
		value, ok := record[key]
		return value, ok
	})
	if err != nil {
		return err
	}
	for _, key := range notification.Keys {
		event.Metadata[key], _ = notification.Get(key)
	}
	if author := record["author"]; author != nil {
		fields, ok := hostedMapping(author)
		if !ok {
			return &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(author) + "' object is not iterable"}
		}
		event.Metadata["turn_author"] = maps.Clone(fields)
	}
	record["retry"] = map[string]any{"identity": identity}
	record["status"], record["reply"] = "queued", ""
	if _, err := writeReceipt(ctx, home, record, nil); err != nil {
		return err
	}
	adapter, err := a.Runner.AdapterForSource(ctx, live.Source)
	var receipt AdmissionReceipt
	if err == nil {
		receipt, err = a.AdmitAutomation(ctx, adapter, event, identity, nil, nil)
	}
	if err != nil {
		if !isRuntimeRefusal(err, "queue_full") {
			return err
		}
		record["status"], record["reply"] = "claimed", ""
		return nil
	}
	result, err := botResult(ctx, a, record)
	if err != nil {
		return err
	}
	maps.Copy(record, result)
	if record["status"] == "queued" || record["status"] == "claimed" {
		botWatchReply(ctx, a, home, deliveryID, receipt.AdmissionID)
	}
	if _, err := writeReceipt(ctx, home, record, nil); err != nil {
		return err
	}
	rememberReceipt(a, home, record)
	return nil
}

// Retry target of a delivery, refused when the Bot Chat moved or its transcript ends on an open user turn
func botRetryTarget(ctx context.Context, a *SessionAuthority, actor Principal, record map[string]any) (*LiveSession, *SessionEntry, error) {
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, nil, err
	}
	ref, live, entry, err := botTarget(ctx, a, actor)
	if err != nil {
		return nil, nil, err
	}
	if !pyjson.Equal(ref.SessionID, record["session_id"]) {
		return nil, nil, runtimeRefusal("admission_conflict")
	}
	role, err := a.DB.LatestConversationRole(ctx, entry.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if role == "user" {
		return nil, nil, runtimeRefusal("open_user_tail")
	}
	return live, entry, nil
}

// Projects the settled outcome of a watched admission onto its receipt, admitting the one retry when owed
func botRecordReply(ctx context.Context, a *SessionAuthority, home, key string, future *admissionWaiter) error {
	paused := false
	if _, err := future.wait(ctx); err != nil {
		var refusal *state.RuntimeStoreError
		if !errors.As(err, &refusal) {
			return err
		}
		paused = true
	}
	return mailboxLock(ctx, a, func(ctx context.Context) error {
		_, record, err := readReceipt(ctx, home, key)
		if err != nil || record == nil {
			return err
		}
		result, err := botResult(ctx, a, record)
		if err != nil {
			return err
		}
		maps.Copy(record, result)
		if !paused {
			if err := botMaybeRetry(ctx, a, home, record); err != nil {
				return err
			}
		}
		delivery, err := botDeliveryResult(ctx, a, record)
		if err != nil {
			return err
		}
		maps.Copy(record, delivery)
		if _, err := writeReceipt(ctx, home, record, nil); err != nil {
			return err
		}
		rememberReceipt(a, home, record)
		return nil
	})
}

// Re-reads one stored receipt against the FIFO, re-arms its reply watch and admits its retry when owed
func refreshBotReceipt(ctx context.Context, a *SessionAuthority, home, key string) error {
	return mailboxLock(ctx, a, func(ctx context.Context) error {
		_, record, err := readReceipt(ctx, home, key)
		if err != nil || record == nil {
			return err
		}
		result, err := botResult(ctx, a, record)
		if err != nil {
			return err
		}
		maps.Copy(record, result)
		if record["status"] == "queued" || record["status"] == "claimed" {
			following := record["retry_admission_id"]
			if !pyTruthy(following) {
				if following, err = botReceiptField(record, "admission_id"); err != nil {
					return err
				}
			}
			botWatchReply(ctx, a, home, key, pyrepr.Str(following))
		}
		if err := botMaybeRetry(ctx, a, home, record); err != nil {
			return err
		}
		if _, err := writeReceipt(ctx, home, record, nil); err != nil {
			return err
		}
		rememberReceipt(a, home, record)
		return nil
	})
}

// Watches one admission's waiter for the delivery, keeping a live watch on the same admission
func botWatchReply(ctx context.Context, a *SessionAuthority, home, key, admissionID string) {
	a.mu.Lock()
	if a.botReplyWatchers == nil {
		a.botReplyWatchers = map[string]*botReplyWatch{}
	}
	if previous := a.botReplyWatchers[key]; previous != nil && previous.admissionID == admissionID && !previous.future.isDone() {
		a.mu.Unlock()
		return
	}
	future := a.waiters[admissionID]
	if future == nil {
		future = newAdmissionWaiter()
		a.waiters[admissionID] = future
	}
	a.mu.Unlock()
	registered := make(chan struct{})
	task := spawnAdapterTask(context.WithoutCancel(ctx), func(taskCtx context.Context) error {
		<-registered
		return botRecordReply(taskCtx, a, home, key, future)
	})
	watch := &botReplyWatch{admissionID: admissionID, task: task, future: future}
	a.mu.Lock()
	a.botReplyWatchers[key] = watch
	a.mu.Unlock()
	task.addDoneCallback(func() {
		a.mu.Lock()
		if a.botReplyWatchers[key] == watch {
			delete(a.botReplyWatchers, key)
		}
		a.mu.Unlock()
	})
	close(registered)
	trackReceiptTask(a, task)
}

// Live session, its route entry and ref a bot delivery lands on, creating or moving aside the Bot Chat
func botTarget(ctx context.Context, a *SessionAuthority, actor Principal) (SessionRef, *LiveSession, *SessionEntry, error) {
	var ref *SessionRef
	var lane *SessionRef
	row, err := a.DB.GetSessionByTitle(ctx, "Bot Chat")
	if err != nil {
		return SessionRef{}, nil, nil, err
	}
	if row == nil {
		if ref, err = createBotChat(ctx, a, actor, ""); err != nil {
			return SessionRef{}, nil, nil, err
		}
	} else {
		displaced, err := displaceableBotChat(ctx, a, row, actor.Subject)
		if err != nil {
			return SessionRef{}, nil, nil, err
		}
		if displaced != "" {
			if ref, err = createBotChat(ctx, a, actor, displaced); err != nil {
				return SessionRef{}, nil, nil, err
			}
		}
	}
	if ref == nil {
		if row, err = a.DB.GetSessionByTitle(ctx, "Bot Chat"); err != nil {
			return SessionRef{}, nil, nil, err
		}
		if row == nil {
			if ref, err = createBotChat(ctx, a, actor, ""); err != nil {
				return SessionRef{}, nil, nil, err
			}
			if ref == nil {
				if row, err = a.DB.GetSessionByTitle(ctx, "Bot Chat"); err != nil {
					return SessionRef{}, nil, nil, err
				}
			}
		}
	}
	if ref == nil {
		if row == nil {
			return SessionRef{}, nil, nil, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
		}
		tip, err := a.DB.GetCompressionTip(ctx, routeStr(rowField(row, "id")))
		if err != nil {
			return SessionRef{}, nil, nil, err
		}
		target, err := a.DB.GetSession(ctx, tip)
		if err != nil {
			return SessionRef{}, nil, nil, err
		}
		if target == nil {
			return SessionRef{}, nil, nil, runtimeRefusal("not_found")
		}
		targetID := routeStr(rowField(target, "id"))
		if lane, err = handedOffRef(ctx, a, targetID); err != nil {
			return SessionRef{}, nil, nil, err
		}
		ref = lane
		if ref == nil {
			resolved, err := resolveLocalTarget(ctx, a, actor, targetID)
			if err != nil {
				return SessionRef{}, nil, nil, err
			}
			ref = &resolved
		}
	}
	if err := a.Authorize(ctx, actor, *ref, "session:submit"); err != nil {
		return SessionRef{}, nil, nil, err
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return SessionRef{}, nil, nil, &pyKeyError{ref.SessionID}
	}
	entry := a.Runner.SessionStore().LookupBySessionKey(ctx, live.Route)
	if (live.Source.Platform != PlatformLocal && lane == nil) || entry == nil {
		return SessionRef{}, nil, nil, runtimeRefusal("admission_conflict")
	}
	return *ref, live, entry, nil
}

// Id holding the canonical title when the Bot Chat is a conversation no delivery can adopt, else empty
func displaceableBotChat(ctx context.Context, a *SessionAuthority, row *pyjson.Object, subject any) (string, error) {
	if strings.HasPrefix(routeStr(rowField(row, "chat_id")), "local-") {
		return "", nil
	}
	rowID := routeStr(rowField(row, "id"))
	tipID, err := a.DB.GetCompressionTip(ctx, rowID)
	if err != nil {
		return "", err
	}
	if tipID == "" {
		tipID = rowID
	}
	tip, err := a.DB.GetSession(ctx, tipID)
	if err != nil {
		return "", err
	}
	if tip != nil {
		lane, err := handedOffRef(ctx, a, routeStr(rowField(tip, "id")))
		if err != nil {
			return "", err
		}
		if lane != nil {
			superseded, err := supersededHandOff(ctx, a, *lane)
			if err != nil || !superseded {
				return "", err
			}
			return rowID, nil
		}
	}
	for _, candidate := range []*pyjson.Object{row, tip} {
		if candidate == nil || strings.HasPrefix(routeStr(rowField(candidate, "chat_id")), "local-") {
			return "", nil
		}
		key := routeStr(rowField(candidate, "chat_id"))
		if key == "" {
			key = routeStr(rowField(candidate, "id"))
		}
		bound := false
		err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			var one int
			err := conn.QueryRowContext(ctx, "SELECT 1 FROM state_meta WHERE key IN (?,?)",
				state.LocalPolicyPrefix+key, state.LocalLegacyPrefix+key).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			bound = err == nil
			return err
		})
		if err != nil {
			return "", err
		}
		if bound {
			return "", nil
		}
	}
	kept, err := keptLocalHistory(a.DB, tip, subject)
	if err != nil || kept {
		return "", err
	}
	return rowID, nil
}

// Whether an unbound local source row is history the Bot Chat door must leave in place
func keptLocalHistory(db *state.SessionDB, row *pyjson.Object, subject any) (bool, error) {
	source := rowField(row, "source")
	local := false
	for _, name := range state.LegacyLocalSources() {
		local = local || source == name
	}
	if !local {
		return false, nil
	}
	userID := rowField(row, "user_id")
	owner := HostOwnerSubject()
	if pyTruthy(userID) {
		labelled, err := state.LegacyAccountLabel(row)
		if err != nil {
			return false, err
		}
		_, authenticated := ParseAuthenticatedSubject(userID)
		if !labelled && !SameOwner(userID, owner) && !(pyTruthy(subject) && SameOwner(userID, subject)) && !authenticated {
			return source == "desktop", nil
		}
	}
	first := subject
	if subject == nil {
		first = userID
	}
	adopters := []any{first}
	if !pyjson.Equal(first, owner) {
		adopters = append(adopters, owner)
	}
	for _, adopter := range adopters {
		err := db.ValidateLegacyRow(row, adopter)
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			continue
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// Mints the Bot Chat under the titled create lock, moving aside a displaceable title holder first
func createBotChat(ctx context.Context, a *SessionAuthority, actor Principal, displace string) (*SessionRef, error) {
	if !actor.Capabilities["session:create"] {
		return nil, runtimeRefusal("not_found")
	}
	if err := a.titledCreateLock.Lock(ctx); err != nil {
		return nil, err
	}
	defer a.titledCreateLock.Unlock()
	held, err := a.DB.GetSessionByTitle(ctx, "Bot Chat")
	if err != nil {
		return nil, err
	}
	if held != nil {
		if displace == "" || routeStr(rowField(held, "id")) != displace {
			return nil, nil
		}
		again, err := displaceableBotChat(ctx, a, held, actor.Subject)
		if err != nil {
			return nil, err
		}
		if again != displace {
			return nil, nil
		}
		slog.Warn(fmt.Sprintf("Bot Chat title held by %s, a conversation no FIFO can own: moving it aside", displace))
		moved, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
			return displaceBotChat(taskCtx, a.DB, displace)
		}, nil, false, nil)
		if err != nil {
			return nil, err
		}
		if moved != true {
			stillHeld, err := a.DB.GetSessionByTitle(ctx, "Bot Chat")
			if err != nil {
				return nil, err
			}
			if stillHeld != nil {
				return nil, nil
			}
		}
	}
	requestID, err := botChatCreationID(ctx, a, actor)
	if err != nil {
		return nil, err
	}
	userHome, err := home.UserHome()
	if err != nil {
		return nil, err
	}
	params := map[string]any{"request_id": requestID, "source": "cli", "cwd": userHome, "follow_profile_config": true}
	follows, err := stagedCreationFollows(ctx, a, actor, requestID)
	if err != nil {
		return nil, err
	}
	if !follows {
		delete(params, "follow_profile_config")
	}
	ref, staged, err := stageLocalSession(ctx, a, actor, params, localCreateOptions{})
	if err != nil {
		return nil, err
	}
	if staged != nil {
		if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
			return a.DB.CommitLocalSession(taskCtx, a.Epoch, *staged)
		}, nil, false, nil); err != nil {
			return nil, err
		}
		sid, _ := staged.Receipt.Get("session_id")
		restored, err := restoreLocalSession(ctx, a, pyrepr.Str(sid))
		if err != nil {
			return nil, err
		}
		ref = &restored
	}
	created := *ref
	if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		return nil, titleNewSession(taskCtx, a, created, "Bot Chat")
	}, nil, false, nil); err != nil {
		return nil, err
	}
	return &created, nil
}

// First Bot Chat creation identity whose session is not spent, derived from durable rows only
func botChatCreationID(ctx context.Context, a *SessionAuthority, actor Principal) (string, error) {
	base := "bot-chat:" + a.ProfileID
	requestID := func(generation int) string {
		if generation == 0 {
			return base
		}
		return fmt.Sprintf("%s:%d", base, generation)
	}
	spent := func(ctx context.Context, conn state.SQLiteConn, sid string) (bool, error) {
		var title, source sql.NullString
		err := conn.QueryRowContext(ctx, "SELECT title, source FROM sessions WHERE id=?", sid).Scan(&title, &source)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if title.Valid && title.String != "" {
			return true, nil
		}
		var label any
		if source.Valid {
			label = source.String
		}
		_, messaging := messagingPlatform(ctx, label)
		return messaging, nil
	}
	var deriveErr error
	generation, _, err := a.DB.FirstGeneration(ctx, func(n int) string {
		identity, err := state.LocalIdentity(a.ProfileID, actor.Subject, requestID(n))
		if err != nil && deriveErr == nil {
			deriveErr = err
		}
		return identity
	}, nil, spent)
	if deriveErr != nil {
		return "", deriveErr
	}
	if err != nil {
		return "", err
	}
	return requestID(generation), nil
}
