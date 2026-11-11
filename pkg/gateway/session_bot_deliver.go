package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"path/filepath"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	botDMMaxChars       = 16200
	botProducerMaxChars = 1024 * 1024
	botPinGraceSeconds  = 300
)

// Admission refusals about the owner's state, after which a same id resend admits once they clear
var botOwnerStateRefusals = map[string]bool{"queue_full": true, "runtime_draining": true, "runtime_retired": true,
	"stale_epoch": true, "stale_generation": true, "storage_unavailable": true,
	"runtime_coordination_required": true, "session_busy": true, "not_found": true}

// FIFO key of a legacy ticket, its integer order and file name, or the lowest key for a pin without one
type botLegacyOrder struct {
	lowest bool
	order  *big.Int
	name   string
}

// Whether o sorts before other as the Python tuple comparison orders them
func (o botLegacyOrder) less(other botLegacyOrder) bool {
	if o.lowest != other.lowest {
		return o.lowest
	}
	if o.lowest {
		return false
	}
	if c := o.order.Cmp(other.order); c != 0 {
		return c < 0
	}
	return o.name < other.name
}

// Legacy FIFO key of a ticket or of the pin laid over one
func legacyOrderOf(path string, record map[string]any) botLegacyOrder {
	order, has := record["sequence"]
	if !has {
		order = record["created_at"]
	}
	if _, isBool := order.(bool); !isBool {
		if n, ok := roomInt(order); ok {
			return botLegacyOrder{order: n, name: filepath.Base(path)}
		}
	}
	return botLegacyOrder{lowest: true}
}

// Logs a malformed mailbox receipt the scan skipped
func botWarnMalformed(path, problem string) {
	if problem == "" {
		problem = "None"
	}
	slog.Warn(fmt.Sprintf("Skipping malformed Bot mailbox receipt %s (%s)", path, problem))
}

// Owner bulk scan withdrawing or requeueing stale pins, with the FIFO barrier of unresolved legacy pins
func botScanOwnerReceipts(ctx context.Context, a *SessionAuthority, home string) (string, []botMailboxRecord, *botLegacyOrder, error) {
	var pins []botMailboxPin
	var barrier *botLegacyOrder
	root, records, err := scanReceipts(ctx, home, &pins)
	if err != nil {
		return "", nil, nil, err
	}
	for _, pin := range pins {
		key := pyrepr.Str(pin.Record["delivery_id"])
		age := float64(time.Now().UnixNano())/1e9 - pin.MTime
		_, legacy := pin.Record["owner"]
		committed := false
		if pyjson.Equal(pin.Record["profile_home"], home) {
			if committed, err = botPinCommitted(ctx, a, pin.Record, "bot:"+key); err != nil {
				return "", nil, nil, err
			}
		}
		if !pyjson.Equal(pin.Record["profile_home"], home) || committed {
			botWarnMalformed(pin.Path, recordShapeError(pin.Path, pin.Record))
			continue
		}
		cleared := false
		if age >= botPinGraceSeconds && legacy {
			ticket, err := requeuePin(ctx, home, key)
			if err != nil {
				return "", nil, nil, err
			}
			cleared = ticket != nil && recordShapeError(pin.Path, ticket) == ""
			if cleared {
				records = append(records, botMailboxRecord{Path: pin.Path, Record: ticket})
			}
		} else if age >= botPinGraceSeconds {
			if cleared, err = withdrawPin(ctx, home, key); err != nil {
				return "", nil, nil, err
			}
		}
		if !cleared {
			if legacy {
				order := legacyOrderOf(pin.Path, pin.Record)
				if barrier == nil || order.less(*barrier) {
					barrier = &order
				}
			}
			botWarnMalformed(pin.Path, recordShapeError(pin.Path, pin.Record))
			continue
		}
		verb := "Withdrew"
		if legacy {
			verb = "Requeued"
		}
		slog.Info(fmt.Sprintf("%s Bot delivery %s pre-admission pin (age %ds): nothing was admitted", verb, key, int64(age)))
	}
	return root, records, barrier, nil
}

// Whether a pin's admission may have committed, a live row or a deleted target
func botPinCommitted(ctx context.Context, a *SessionAuthority, record map[string]any, requestID string) (bool, error) {
	sessionID, err := receiptText(record, "session_id")
	if err != nil {
		return false, err
	}
	admitted, err := botAdmissionAt(ctx, a, sessionID, requestID)
	if err != nil || admitted != "" {
		return admitted != "", err
	}
	retired := false
	err = a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var one int
		scanErr := conn.QueryRowContext(ctx, "SELECT 1 FROM state_meta WHERE key=?", state.RetiredPrefix+sessionID).Scan(&one)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil
		}
		retired = scanErr == nil
		return scanErr
	})
	return retired, err
}

// Resolved profile home a delivery for profile lands in, refusing another profile or a principal without submit
func botHome(a *SessionAuthority, actor Principal, profile any) (string, error) {
	homeDir, err := pypath.Resolve(filepath.Dir(a.DB.Path()))
	if err != nil {
		return "", err
	}
	name := servedProfileName(homeDir)
	handle := name
	if name == "default" {
		var names []string
		for _, entry := range botmode.Roster(botmode.InstallRoot(homeDir)) {
			names = append(names, entry.Name)
		}
		handle = botmode.Handle(name, names)
	}
	if actor.ProfileID != a.ProfileID || !(profile == name || profile == handle) {
		return "", runtimeRefusal("profile_mismatch")
	}
	if !actor.Capabilities["session:submit"] {
		return "", runtimeRefusal("permission_denied")
	}
	return homeDir, nil
}

// Admits queued legacy tickets of this home in FIFO order, a claimed one settling as ambiguous
func botMigrate(ctx context.Context, a *SessionAuthority, actor Principal, home string) error {
	root, records, barrier, err := botScanOwnerReceipts(ctx, a, home)
	if err != nil {
		return err
	}
	var legacy []botMailboxRecord
	for _, item := range records {
		record := item.Record
		_, owned := record["owner"]
		status, _ := record["status"].(string)
		if record != nil && owned && !pyTruthy(record["admission_id"]) && (status == "queued" || status == "claimed") &&
			(barrier == nil || legacyOrderOf(item.Path, record).less(*barrier)) {
			legacy = append(legacy, item)
		}
	}
	if len(legacy) == 0 {
		return nil
	}
	ref, live, entry, err := botTarget(ctx, a, actor)
	if err != nil {
		return err
	}
	sort.SliceStable(legacy, func(i, j int) bool {
		return legacyOrderOf(legacy[i].Path, legacy[i].Record).less(legacyOrderOf(legacy[j].Path, legacy[j].Record))
	})
	for _, item := range legacy {
		record := item.Record
		owner, ok := hostedMapping(record["owner"])
		if !ok {
			return &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(record["owner"]) + "' object is not subscriptable"}
		}
		if !pyjson.Equal(owner["profile_home"], home) {
			continue
		}
		ownerSession := pyrepr.Str(owner["session_id"])
		tip, err := a.DB.GetCompressionTip(ctx, ownerSession)
		if err != nil {
			return err
		}
		if tip != entry.SessionID {
			continue
		}
		if err := activesessions.LivenessGuard(ctx, ownerSession, home, nil, func(active bool) error {
			if active {
				return runtimeRefusal("runtime_coordination_required")
			}
			return nil
		}); err != nil {
			return err
		}
		if record["status"] == "claimed" {
			record["status"], record["reason"] = "ambiguous", "unknown_execution"
			if _, err := writeReceipt(ctx, home, record, &receiptExpect{status: "claimed"}); err != nil {
				return err
			}
			continue
		}
		rawID, _ := record["delivery_id"].(string)
		key, err := checkDeliveryID(rawID)
		if err != nil {
			return err
		}
		message, err := receiptText(record, "message")
		if err != nil {
			return err
		}
		category, has := record["notification_category"]
		if !has {
			category = "result"
		}
		if _, err := botAdmit(ctx, a, actor, home, root, key, message, ref, live, entry,
			provider.ParseTurnAuthor(record["author"]), record, category); err != nil {
			return err
		}
	}
	return nil
}

// Rebuilds derivative replies and queued legacy admissions at owner startup
func recoverBotDeliveries(ctx context.Context, a *SessionAuthority) error {
	homeDir, err := pypath.Resolve(filepath.Dir(a.DB.Path()))
	if err != nil {
		return err
	}
	return mailboxLock(ctx, a, func(ctx context.Context) error {
		_, records, _, err := botScanOwnerReceipts(ctx, a, homeDir)
		if err != nil {
			return err
		}
		for _, item := range records {
			record := item.Record
			if record == nil || !pyjson.Equal(record["profile_home"], homeDir) || !pyTruthy(record["admission_id"]) {
				continue
			}
			err := botRecoverReceipt(ctx, a, homeDir, record)
			var refusal *state.RuntimeStoreError
			if errors.As(err, &refusal) {
				slog.Error(fmt.Sprintf("Bot delivery %s not recovered: %s (%s)", pyrepr.Str(record["delivery_id"]), refusal.Reason, item.Path), "error", err)
				continue
			}
			if err != nil {
				return err
			}
			if _, err := writeReceipt(ctx, homeDir, record, nil); err != nil {
				return err
			}
			rememberReceipt(a, homeDir, record)
		}
		row, err := a.DB.GetSessionByTitle(ctx, "Bot Chat")
		if err != nil || row == nil {
			return err
		}
		tip, err := a.DB.GetCompressionTip(ctx, routeStr(rowField(row, "id")))
		if err != nil {
			return err
		}
		target, err := a.DB.GetSession(ctx, tip)
		if err != nil || target == nil {
			return err
		}
		key := routeStr(rowField(target, "chat_id"))
		if key == "" {
			key = routeStr(rowField(target, "id"))
		}
		bound := false
		err = a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			var one int
			scanErr := conn.QueryRowContext(ctx, "SELECT 1 FROM state_meta WHERE key=?", state.LocalPolicyPrefix+key).Scan(&one)
			if errors.Is(scanErr, sql.ErrNoRows) {
				return nil
			}
			bound = scanErr == nil
			return scanErr
		})
		if err != nil || !bound {
			return err
		}
		actor := Principal{Subject: routeStr(rowField(target, "user_id")), ProfileID: a.ProfileID,
			Capabilities: map[string]bool{"session:submit": true, "session:read": true}, TransportID: "bot-owner-recovery"}
		return botMigrate(ctx, a, actor, homeDir)
	})
}

// Projects one recovered receipt, re-arming its reply watch or admitting its owed retry
func botRecoverReceipt(ctx context.Context, a *SessionAuthority, home string, record map[string]any) error {
	result, err := botResult(ctx, a, record)
	if err != nil {
		return err
	}
	maps.Copy(record, result)
	if record["status"] == "queued" || record["status"] == "claimed" {
		following := record["retry_admission_id"]
		if !pyTruthy(following) {
			following = record["admission_id"]
		}
		botWatchReply(ctx, a, home, pyrepr.Str(record["delivery_id"]), pyrepr.Str(following))
		return nil
	}
	return botMaybeRetry(ctx, a, home, record)
}

// Delivers one Bot Chat message as tracked owner work a cancelled caller cannot abandon midway
func BotDeliver(ctx context.Context, a *SessionAuthority, actor Principal, params map[string]any) (map[string]any, error) {
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	task := trackMutation(a, func(taskCtx context.Context) (any, error) { return botDeliverLocked(taskCtx, a, actor, params) })
	return botTaskAnswer(ctx, task)
}

// Answer of a shielded tracked task, the caller's cancellation leaving the task running
func botTaskAnswer(ctx context.Context, task *trackedTask) (map[string]any, error) {
	select {
	case <-task.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	result, err := task.wait()
	answer, _ := result.(map[string]any)
	return answer, err
}

// Refuses a delivery id reused for another message, principal, author or category
func requireSameDelivery(record map[string]any, actor Principal, message string, author any, category any) error {
	stored, err := botReceiptField(record, "message")
	if err != nil {
		return err
	}
	principal, err := botReceiptField(record, "principal_id")
	if err != nil {
		return err
	}
	storedCategory, has := record["notification_category"]
	if !has {
		storedCategory = "result"
	}
	if !pyjson.Equal(stored, message) || !SameOwner(principal, actor.Subject) || !pyjson.Equal(record["author"], author) ||
		!pyjson.Equal(storedCategory, category) {
		return runtimeRefusal("admission_conflict")
	}
	return nil
}

// Validated identity of one deliver or settle request
type botDeliveryRequest struct {
	home     string
	key      string
	message  string
	author   any
	category any
}

// Home, delivery id, message, author and category of one deliver or settle request
func botDeliveryParams(a *SessionAuthority, actor Principal, params map[string]any) (botDeliveryRequest, error) {
	homeDir, err := botHome(a, actor, params["profile"])
	if err != nil {
		return botDeliveryRequest{}, err
	}
	for key := range params {
		switch key {
		case "id", "profile", "message", "session_id", "author", "notification_category":
		default:
			return botDeliveryRequest{}, runtimeRefusal("invalid_params")
		}
	}
	notification, err := automationNotificationMetadata(func(key string) (any, bool) {
		value, ok := params[key]
		return value, ok
	})
	if err != nil {
		return botDeliveryRequest{}, err
	}
	category, has := notification.Get("notification_category")
	if !has {
		category = "result"
	}
	rawID, _ := params["id"].(string)
	key, err := checkDeliveryID(rawID)
	if err != nil {
		return botDeliveryRequest{}, runtimeRefusal("invalid_params")
	}
	limit := botProducerMaxChars
	if params["author"] != nil {
		limit = botDMMaxChars
	}
	message, ok := params["message"].(string)
	if !ok || pystr.Strip(message) == "" || utf8.RuneCountInString(message) > limit {
		return botDeliveryRequest{}, runtimeRefusal("invalid_params")
	}
	var author any
	if parsed := provider.ParseTurnAuthor(params["author"]); parsed != nil {
		author = parsed
	}
	if raw := params["author"]; raw != nil {
		if _, isDict := hostedMapping(raw); !isDict || author == nil {
			return botDeliveryRequest{}, runtimeRefusal("invalid_params")
		}
	}
	return botDeliveryRequest{home: homeDir, key: key, message: message, author: author, category: category}, nil
}

// Answer of a terminal refusal receipt
func refusedAnswer(record map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, key := range []string{"status", "refused", "delivery_id", "profile_home", "session_id", "message", "reply", "error", "reason"} {
		value, err := botReceiptField(record, key)
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

// Answer an id's existing receipt gives, nil when it has none yet
func receiptAnswer(ctx context.Context, a *SessionAuthority, actor Principal, home string, record map[string]any, req botDeliveryRequest, retry bool) (map[string]any, error) {
	if record == nil || !(pyTruthy(record["admission_id"]) || record["refused"] == true) {
		return nil, nil
	}
	if err := requireSameDelivery(record, actor, req.message, req.author, req.category); err != nil {
		return nil, err
	}
	if !pyTruthy(record["admission_id"]) {
		return refusedAnswer(record)
	}
	sessionID, err := receiptText(record, "session_id")
	if err != nil {
		return nil, err
	}
	if err := a.Authorize(ctx, actor, SessionRef{ProfileID: a.ProfileID, SessionID: sessionID}, "session:submit"); err != nil {
		return nil, err
	}
	if retry {
		if err := botMaybeRetry(ctx, a, home, record); err != nil {
			return nil, err
		}
	}
	return publishBotResult(ctx, a, home, record)
}

// Delivers under the mailbox lock, answering from an existing receipt or admitting the message once
func botDeliverLocked(ctx context.Context, a *SessionAuthority, actor Principal, params map[string]any) (map[string]any, error) {
	req, err := botDeliveryParams(a, actor, params)
	if err != nil {
		return nil, err
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	var answer map[string]any
	err = mailboxLock(ctx, a, func(ctx context.Context) error {
		root, record, err := readReceipt(ctx, req.home, req.key)
		if err != nil {
			return err
		}
		if answer, err = receiptAnswer(ctx, a, actor, req.home, record, req, true); err != nil || answer != nil {
			return err
		}
		err = botDeliverFresh(ctx, a, actor, params, req, root, &answer)
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			held, heldErr := heldByAnotherWriter(ctx, req.home, req.key, refusal)
			if heldErr != nil {
				return heldErr
			}
			if held != nil {
				return held
			}
		}
		return err
	})
	return answer, err
}

// Migrates legacy tickets, then admits a fresh delivery onto the Bot Chat the producer pinned
func botDeliverFresh(ctx context.Context, a *SessionAuthority, actor Principal, params map[string]any, req botDeliveryRequest, root string, answer *map[string]any) error {
	if err := botMigrate(ctx, a, actor, req.home); err != nil {
		return err
	}
	_, record, err := readReceipt(ctx, req.home, req.key)
	if err != nil {
		return err
	}
	if *answer, err = receiptAnswer(ctx, a, actor, req.home, record, req, true); err != nil || *answer != nil {
		return err
	}
	if record != nil {
		return runtimeRefusal("unknown_execution")
	}
	holder, err := a.DB.GetSessionByTitle(ctx, "Bot Chat")
	if err != nil {
		return err
	}
	ref, live, entry, err := botTarget(ctx, a, actor)
	if err != nil {
		return err
	}
	pinned, has := params["session_id"]
	if !has {
		pinned = entry.SessionID
	}
	if !pyjson.Equal(pinned, entry.SessionID) {
		moved, err := movedAsideByThisDoor(ctx, a, holder, pinned)
		if err != nil {
			return err
		}
		if !moved {
			return runtimeRefusal("admission_conflict")
		}
	}
	*answer, err = botAdmit(ctx, a, actor, req.home, root, req.key, req.message, ref, live, entry, req.author, nil, req.category)
	return err
}

// Whether the pinned Bot Chat is the title holder this delivery's target just moved aside
func movedAsideByThisDoor(ctx context.Context, a *SessionAuthority, holder *pyjson.Object, pinned any) (bool, error) {
	if holder == nil {
		return false, nil
	}
	holderID := routeStr(rowField(holder, "id"))
	tip, err := a.DB.GetCompressionTip(ctx, holderID)
	if err != nil {
		return false, err
	}
	if !pyjson.Equal(pinned, holderID) && !pyjson.Equal(pinned, tip) {
		return false, nil
	}
	row, err := a.DB.GetSession(ctx, holderID)
	if err != nil {
		return false, err
	}
	title := routeStr(rowField(row, "title"))
	return len(title) >= len("Bot Chat (") && title[:len("Bot Chat (")] == "Bot Chat (", nil
}

// Settles a delivery the sender was told may not have been admitted, as never sent or with what it already is
func BotSettle(ctx context.Context, a *SessionAuthority, actor Principal, params map[string]any) (map[string]any, error) {
	if err := requireServing(a); err != nil {
		return nil, err
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	task := trackMutation(a, func(taskCtx context.Context) (any, error) { return botSettleLocked(taskCtx, a, actor, params) })
	return botTaskAnswer(ctx, task)
}

// Settle under the mailbox lock every admission holds
func botSettleLocked(ctx context.Context, a *SessionAuthority, actor Principal, params map[string]any) (map[string]any, error) {
	req, err := botDeliveryParams(a, actor, params)
	if err != nil {
		return nil, err
	}
	var answer map[string]any
	err = mailboxLock(ctx, a, func(ctx context.Context) error {
		_, record, err := readReceipt(ctx, req.home, req.key)
		if err != nil {
			return err
		}
		if record == nil {
			if record, err = notSentRecord(ctx, a, actor, req); err != nil {
				return err
			}
			if _, err := writeReceipt(ctx, req.home, record, nil); err != nil {
				return err
			}
			slog.Info(fmt.Sprintf("Bot Chat delivery %s settled as not sent (%s)", req.key, pyrepr.Str(record["reason"])))
		}
		if err := requireSameDelivery(record, actor, req.message, req.author, req.category); err != nil {
			return err
		}
		if answer, err = receiptAnswer(ctx, a, actor, req.home, record, req, false); err != nil {
			return err
		}
		if answer == nil {
			return runtimeRefusal("unknown_execution")
		}
		return nil
	})
	return answer, err
}

// Terminal receipt of a never admitted id, unknown when the FIFO holds its row anyway or no Bot Chat exists
func notSentRecord(ctx context.Context, a *SessionAuthority, actor Principal, req botDeliveryRequest) (map[string]any, error) {
	admitted := false
	err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var one int
		scanErr := conn.QueryRowContext(ctx, "SELECT 1 FROM session_admissions WHERE request_id=?", "bot:"+req.key).Scan(&one)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil
		}
		admitted = scanErr == nil
		return scanErr
	})
	if err != nil {
		return nil, err
	}
	row, err := a.DB.GetSessionByTitle(ctx, "Bot Chat")
	if err != nil {
		return nil, err
	}
	if admitted || row == nil {
		return nil, runtimeRefusal("unknown_execution")
	}
	tip, err := a.DB.GetCompressionTip(ctx, routeStr(rowField(row, "id")))
	if err != nil {
		return nil, err
	}
	record := map[string]any{"delivery_id": req.key, "profile_home": req.home, "session_id": tip,
		"principal_id": actor.Subject, "message": req.message, "status": "failed", "refused": true, "reply": "",
		"error": "Settled as not sent after a session-writer refusal", "reason": tools.HeldNotSent}
	notification, err := automationNotificationMetadata(func(key string) (any, bool) {
		if key == "notification_category" {
			return req.category, true
		}
		return nil, false
	})
	if err != nil {
		return nil, err
	}
	for _, key := range notification.Keys {
		record[key], _ = notification.Get(key)
	}
	if req.author != nil {
		fields, _ := hostedMapping(req.author)
		record["author"] = maps.Clone(fields)
	}
	return record, nil
}

// Settleable runtime_coordination_required refusal that admitted nothing yet
type HeldNotAdmittedError struct{ *state.RuntimeStoreError }

func (e *HeldNotAdmittedError) Unwrap() error { return e.RuntimeStoreError }

// False, the admitted attribute the sender reads before it asks settle
func (e *HeldNotAdmittedError) Admitted() bool { return false }

// The settleable form of a session writer refusal when the id still has no receipt, else nil
func heldByAnotherWriter(ctx context.Context, home, key string, refusal *state.RuntimeStoreError) (error, error) {
	if refusal.Reason != tools.HeldNotSent {
		return nil, nil
	}
	_, record, err := readReceipt(ctx, home, key)
	if err != nil {
		return nil, err
	}
	if record != nil {
		return nil, nil
	}
	field, detail := "session_writer", "another session writer holds the Bot Chat; nothing was admitted yet"
	return &HeldNotAdmittedError{state.NewRuntimeStoreError(refusal.Reason, &field, &detail)}, nil
}

// Pins the physical target, admits the delivery and publishes its projection
func botAdmit(ctx context.Context, a *SessionAuthority, actor Principal, home, root, key, message string, ref SessionRef,
	live *LiveSession, entry *SessionEntry, author any, legacy map[string]any, category any) (map[string]any, error) {
	notification, err := automationNotificationMetadata(func(name string) (any, bool) {
		if name == "notification_category" {
			return category, true
		}
		return nil, false
	})
	if err != nil {
		return nil, err
	}
	identity := "bot:" + key
	event := NewMessageEvent(message)
	event.Source, event.Internal, event.MessageID = live.Source, true, &identity
	event.Metadata = map[string]any{"gateway_session_key": live.Route, "gateway_session_id": entry.SessionID}
	for _, name := range notification.Keys {
		event.Metadata[name], _ = notification.Get(name)
	}
	var authorFields map[string]any
	if author != nil {
		authorFields, _ = hostedMapping(author)
		event.Metadata["turn_author"] = maps.Clone(authorFields)
	}
	record := maps.Clone(legacy)
	if record == nil {
		record = map[string]any{}
	}
	maps.Copy(record, map[string]any{"delivery_id": key, "profile_home": home, "session_id": ref.SessionID,
		"principal_id": actor.Subject, "message": message, "status": "ambiguous"})
	for _, name := range notification.Keys {
		record[name], _ = notification.Get(name)
	}
	if actor.Capabilities["session:operator"] {
		record["operator"] = true
	}
	if author != nil {
		record["author"] = maps.Clone(authorFields)
	}
	expect := &receiptExpect{status: nil}
	if legacy != nil {
		expect.status = legacy["status"]
	}
	written, err := writeReceipt(ctx, home, record, expect)
	if err != nil {
		return nil, err
	}
	if !written {
		if legacy != nil {
			return nil, nil
		}
		return nil, runtimeRefusal("admission_conflict")
	}
	adapter, err := a.Runner.AdapterForSource(ctx, live.Source)
	var receipt AdmissionReceipt
	if err == nil {
		receipt, err = a.AdmitAutomation(ctx, adapter, event, identity, nil, nil)
	}
	if err != nil {
		var refusal *state.RuntimeStoreError
		if !errors.As(err, &refusal) {
			return nil, err
		}
		committed, atErr := botAdmissionAt(ctx, a, ref.SessionID, identity)
		if atErr != nil {
			return nil, atErr
		}
		if committed != "" {
			return nil, err
		}
		if botOwnerStateRefusals[refusal.Reason] {
			if legacy == nil {
				if _, werr := withdrawPin(ctx, home, key); werr != nil {
					return nil, werr
				}
			}
		} else {
			slog.Info(fmt.Sprintf("Bot Chat delivery %s refused (%s)", key, refusal.Reason))
			if rerr := refusePin(ctx, home, key); rerr != nil {
				return nil, rerr
			}
		}
		return nil, err
	}
	record["status"], record["admission_id"] = "canonical", receipt.AdmissionID
	result, err := botResult(ctx, a, record)
	if err != nil {
		return nil, err
	}
	maps.Copy(record, result)
	if record["status"] == "queued" || record["status"] == "claimed" {
		botWatchReply(ctx, a, home, key, receipt.AdmissionID)
	} else if err := botMaybeRetry(ctx, a, home, record); err != nil {
		return nil, err
	}
	return publishBotResult(ctx, a, home, record)
}

// Publishes the delivery's projection and answers with exactly that projection
func publishBotResult(ctx context.Context, a *SessionAuthority, home string, record map[string]any) (map[string]any, error) {
	result, err := botDeliveryResult(ctx, a, record)
	if err != nil {
		return nil, err
	}
	maps.Copy(record, result)
	if _, err := writeReceipt(ctx, home, record, nil); err != nil {
		return nil, err
	}
	rememberReceipt(a, home, record)
	return result, nil
}

// Admission a live waiter must follow and whether it is interim, empty once the delivery settled for good
func PeerWaitAdmission(ctx context.Context, a *SessionAuthority, record map[string]any) (string, bool, error) {
	retryID, err := botRetryAdmission(ctx, a, record)
	if err != nil {
		return "", false, err
	}
	if retryID != "" {
		status, _, err := botAdmissionOutcome(ctx, a, retryID, nil)
		if err != nil || !(status == "queued" || status == "claimed") {
			return "", false, err
		}
		return retryID, false, nil
	}
	admission, err := receiptText(record, "admission_id")
	if err != nil {
		return "", false, err
	}
	status, _, err := botAdmissionOutcome(ctx, a, admission, record)
	if err != nil {
		return "", false, err
	}
	if status == "queued" || status == "claimed" {
		return admission, false, nil
	}
	if status == "failed" {
		home, err := receiptText(record, "profile_home")
		if err != nil {
			return "", false, err
		}
		deliveryID, err := receiptText(record, "delivery_id")
		if err != nil {
			return "", false, err
		}
		saved, err := ReadDeliveryResult(ctx, home, deliveryID)
		if err != nil {
			return "", false, err
		}
		if saved == nil {
			saved = record
		}
		probe := maps.Clone(record)
		probe["retry"] = saved["retry"]
		eligible, err := botRetryEligible(ctx, a, probe)
		if err != nil {
			return "", false, err
		}
		if eligible {
			return admission, true, nil
		}
	}
	return "", false, nil
}
