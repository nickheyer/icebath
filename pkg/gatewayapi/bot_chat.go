package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session store the title fallback reads, the title lookup and the local receipt of a lineage
type titleOwnerStore interface {
	GetSessionByTitle(ctx context.Context, title string) (*pyjson.Object, error)
	LocalReceipt(ctx context.Context, sessionID string) (*pyjson.Object, error)
}

// The titled creation row of a reset local conversation for an exact title lookup, empty otherwise
func resetLocalTitleOwner(ctx context.Context, db titleOwnerStore, title string, includeHidden bool, source string) ([]*pyjson.Object, error) {
	if title == "" {
		return nil, nil
	}
	row, err := db.GetSessionByTitle(ctx, title)
	if err != nil {
		return nil, err
	}
	if row == nil || row.GetOr("end_reason", nil) != "session_reset" || pyrepr.Truthy(row.GetOr("archived", nil)) ||
		(pyrepr.Truthy(row.GetOr("hidden", nil)) && !includeHidden) || (source != "" && row.GetOr("source", nil) != source) {
		return nil, nil
	}
	receipt, err := db.LocalReceipt(ctx, pyrepr.Str(row.GetOr("id", "")))
	if err != nil {
		var runtimeErr *state.RuntimeStoreError
		if errors.As(err, &runtimeErr) {
			return nil, nil
		}
		return nil, err
	}
	lineage, _ := receipt.GetOr("lineage", nil).([]any)
	if len(lineage) > 1 {
		return []*pyjson.Object{row}, nil
	}
	return nil, nil
}

// Receipt statuses a peer waiter keeps waiting through
var peerPending = map[string]bool{"queued": true, "claimed": true}

// Owner of a Bot Chat the gateway mints for a peer's create if missing
const peerSubject = "api-peer"

// The whole body a peer dm create sends
var peerCreateFields = map[string]bool{"title": true, "source": true}

// Active authority of a runner holding a session authority, nil for one without
func botChatAuthority(ctx context.Context, runner any) (*gateway.SessionAuthority, error) {
	holder, ok := runner.(interface {
		SessionAuthority() *gateway.SessionAuthority
	})
	if !ok || holder == nil || holder.SessionAuthority() == nil {
		return nil, nil
	}
	return gateway.ActiveAuthority(ctx, runner)
}

// Peer principal that may mint the Bot Chat as well as submit to it
func peerCreator(profileID string) gateway.Principal {
	return gateway.Principal{Subject: peerSubject, ProfileID: profileID,
		Capabilities: capabilities("session:create", "session:read", "session:submit"), TransportID: "api-peer-dm"}
}

// Bot Chat minted by the owner for a peer's titled create under session authority, nil when the ordinary handler answers
func createPeerBotChat(ctx context.Context, a *APIServerAdapter, body *pyjson.Object) (*gateway.WebResponse, error) {
	authority, err := botChatAuthority(ctx, a.GatewayRunner)
	if err != nil {
		return nil, err
	}
	title := ""
	if raw := body.GetOr("title", nil); pyrepr.Truthy(raw) {
		title = pystr.Strip(pyrepr.Str(raw))
	}
	if authority == nil || title != botmode.BotChatTitle || pyrepr.Truthy(body.GetOr("id", nil)) || pyrepr.Truthy(body.GetOr("session_id", nil)) {
		return nil, nil
	}
	var unsupported []string
	for _, key := range body.Keys {
		if !peerCreateFields[key] {
			unsupported = append(unsupported, key)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return errorResponse("Bot Chat creation refused: bot_chat_parameters_unsupported ("+strings.Join(unsupported, ", ")+"); "+
			"the Bot Chat is created with the profile's own model and prompt", http.StatusBadRequest,
			errOpts{Code: "bot_chat_parameters_unsupported"}), nil
	}
	ref, err := gateway.CreatePeerBotChat(ctx, authority, peerCreator(authority.ProfileID))
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return errorResponse("Bot Chat creation refused: "+refusal.Reason, http.StatusServiceUnavailable, errOpts{Code: refusal.Reason}), nil
		}
		return nil, err
	}
	if ref == nil {
		return nil, nil
	}
	row, err := authority.DB.GetSession(ctx, ref.SessionID)
	if err != nil {
		return nil, err
	}
	return jsonResponse(pyjson.ObjectOf("object", "hermes.session", "session", sessionResponse(row)), http.StatusCreated, nil), nil
}

// Python text of an optional refusal detail, None for an absent one
func optionalDetail(value *string) string {
	if value == nil {
		return "None"
	}
	return *value
}

// Reason and error text of a refused peer turn, naming a Bot Chat that is no local chat
func peerRefusal(ctx context.Context, authority *gateway.SessionAuthority, tip *pyjson.Object, refusal *state.RuntimeStoreError) (string, string, error) {
	reason := refusal.Reason
	chatID := ""
	if raw := tip.GetOr("chat_id", nil); pyrepr.Truthy(raw) {
		chatID = pyrepr.Str(raw)
	}
	if reason != "not_found" || strings.HasPrefix(chatID, "local-") {
		return reason, "Bot Chat admission refused: " + reason, nil
	}
	var hint string
	if err := authority.DB.ValidateLegacyRow(tip, nil); err == nil {
		reason, hint = "bot_chat_unbound", "open it once with `icebath chat -c \"Bot Chat\"` on that host"
	} else {
		var legacy *state.RuntimeStoreError
		if !errors.As(err, &legacy) {
			return "", "", err
		}
		reason = "bot_chat_not_local"
		displaced, err := gateway.DisplaceableBotChat(ctx, authority, tip)
		if err != nil {
			return "", "", err
		}
		if displaced != "" {
			hint = "it is a conversation the gateway cannot own; it is moved aside as 'Bot Chat (<source> <id>)' on the next delivery"
		} else {
			hint = "it is local history owned by another login or profile; open or rename it from the client that owns it"
		}
	}
	slog.Warn(fmt.Sprintf("Peer DM into Bot Chat %s refused: %s (not_found: field=%s detail=%s)", pyrepr.Str(tip.GetOr("id", nil)),
		reason, optionalDetail(refusal.Field), optionalDetail(refusal.Detail)))
	return reason, "Bot Chat admission refused: " + reason + " (this profile's 'Bot Chat' is not a local chat; " + hint + ")", nil
}

// Canonical Bot Chat's tip row when the session is in its lineage, nil otherwise
func botChatTip(ctx context.Context, authority *gateway.SessionAuthority, sessionID string) (*pyjson.Object, error) {
	db := authority.DB
	row, err := db.GetSessionByTitle(ctx, "Bot Chat")
	if err != nil || row == nil {
		return nil, err
	}
	tip, err := db.GetCompressionTip(ctx, pyrepr.Str(row.GetOr("id", nil)))
	if err != nil || tip == "" {
		return nil, err
	}
	sessionTip, err := db.GetCompressionTip(ctx, sessionID)
	if err != nil || sessionTip != tip {
		return nil, err
	}
	return db.GetSession(ctx, tip)
}

// Profile home and receipt of a peer turn the Bot Chat FIFO admitted, empty for any other session
func admitPeerTurn(ctx context.Context, runner any, sessionID, message string, author *pyjson.Object) (string, map[string]any, error) {
	authority, err := botChatAuthority(ctx, runner)
	if err != nil || authority == nil {
		return "", nil, err
	}
	tip, err := botChatTip(ctx, authority, sessionID)
	if err != nil || tip == nil {
		return "", nil, err
	}
	profileHome, err := pypath.Resolve(filepath.Dir(authority.DB.Path()))
	if err != nil {
		return "", nil, err
	}
	titled, err := authority.DB.GetSessionByTitle(ctx, "Bot Chat")
	if err != nil {
		return "", nil, err
	}
	if titled == nil {
		titled = tip
	}
	displaced, err := gateway.DisplaceableBotChat(ctx, authority, titled)
	if err != nil {
		return "", nil, err
	}
	actor := peerCreator(authority.ProfileID)
	if displaced == "" {
		subject, _ := tip.GetOr("user_id", nil).(string)
		actor = gateway.Principal{Subject: subject, ProfileID: authority.ProfileID,
			Capabilities: capabilities("session:submit", "session:read"), TransportID: "api-peer-dm"}
	}
	key := hexUUID()
	params := map[string]any{"id": key, "profile": gateway.ServedProfileName(profileHome), "message": message}
	if author != nil {
		params["author"] = author.Copy()
	}
	record, err := gateway.BotDeliver(ctx, authority, actor, params)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if !errors.As(err, &refusal) {
			return "", nil, err
		}
		reason, text, err := peerRefusal(ctx, authority, tip, refusal)
		if err != nil {
			return "", nil, err
		}
		record = map[string]any{"status": "failed", "delivery_id": key, "reason": reason, "error": text}
	}
	return profileHome, record, nil
}

// Seconds as a duration pointer, nil for no timeout
func secondsDuration(seconds *float64) *time.Duration {
	if seconds == nil {
		return nil
	}
	d := time.Duration(*seconds * float64(time.Second))
	return &d
}

// Live Bot Chat receipt once it settles or the budget lapses, through whichever door admitted it
func awaitLiveDelivery(ctx context.Context, a *APIServerAdapter, profileHome string, record map[string]any, budget float64) (map[string]any, error) {
	return awaitLiveDeliveryUntil(ctx, a, profileHome, record, &budget, nil)
}

// Live Bot Chat receipt once it settles, the timeout lapses or shouldStop says so
func awaitLiveDeliveryUntil(ctx context.Context, a *APIServerAdapter, profileHome string, record map[string]any, timeout *float64,
	shouldStop func() bool) (map[string]any, error) {
	authority, err := botChatAuthority(ctx, a.GatewayRunner)
	if err != nil {
		return nil, err
	}
	if authority != nil && pyrepr.Truthy(record["admission_id"]) {
		return awaitPeerReceipt(ctx, authority, record, timeout, shouldStop)
	}
	deliveryID, ok := record["delivery_id"]
	if !ok {
		return nil, &gateway.PyError{Kind: "KeyError", Msg: "'delivery_id'"}
	}
	settled, err := gateway.AwaitDelivery(ctx, profileHome, pyrepr.Str(deliveryID), secondsDuration(timeout), shouldStop)
	if err != nil {
		return nil, err
	}
	if len(settled) == 0 {
		return record, nil
	}
	return settled, nil
}

// Delivery receipt merged over the record it was read for
func peerReceipt(ctx context.Context, authority *gateway.SessionAuthority, record map[string]any) (map[string]any, error) {
	result, err := gateway.BotResult(ctx, authority, record)
	if err != nil {
		return nil, err
	}
	current := make(map[string]any, len(record)+len(result))
	for key, value := range record {
		current[key] = value
	}
	for key, value := range result {
		current[key] = value
	}
	return current, nil
}

// Delivery receipt after it settles for good, the timeout lapses or shouldStop says so, following the one retry
func awaitPeerReceipt(ctx context.Context, authority *gateway.SessionAuthority, record map[string]any, timeout *float64,
	shouldStop func() bool) (map[string]any, error) {
	if !peerPending[pyrepr.Str(record["status"])] {
		return record, nil
	}
	var deadline *time.Time
	if timeout != nil {
		at := time.Now().Add(time.Duration(*timeout * float64(time.Second)))
		deadline = &at
	}
	for {
		live, interim, err := gateway.PeerWaitAdmission(ctx, authority, record)
		if err != nil {
			return nil, err
		}
		var waiter <-chan struct{}
		if live != "" && !interim {
			waiter = authority.AdmissionWaiterDone(live)
		}
		current, err := peerReceipt(ctx, authority, record)
		if err != nil {
			return nil, err
		}
		status := pyrepr.Str(current["status"])
		if live == "" && peerPending[status] {
			continue
		}
		if live == pyrepr.Str(record["admission_id"]) && !interim && status == "failed" && !pyrepr.Truthy(current["retry_admission_id"]) {
			continue
		}
		if interim {
			current["status"], status = "claimed", "claimed"
		}
		if live == "" || !peerPending[status] {
			if err := ownerPublished(ctx, authority, record, deadline); err != nil {
				return nil, err
			}
			return current, nil
		}
		if shouldStop != nil && shouldStop() {
			return current, nil
		}
		var remaining *time.Duration
		if deadline != nil {
			left := time.Until(*deadline)
			if left <= 0 {
				return current, nil
			}
			remaining = &left
		}
		var tick *time.Duration
		switch {
		case interim:
			t := 50 * time.Millisecond
			tick = &t
		case shouldStop != nil:
			t := 500 * time.Millisecond
			tick = &t
		default:
			tick = remaining
		}
		if tick != nil && remaining != nil {
			bounded := min(*tick, *remaining)
			tick = &bounded
		}
		var expired <-chan time.Time
		var timer *time.Timer
		if tick != nil {
			timer = time.NewTimer(*tick)
			expired = timer.C
		}
		select {
		case <-waiter:
		case <-expired:
		case <-ctx.Done():
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

// Waits until the owner's reply watcher published the outcome this waiter read, bounded by the deadline
func ownerPublished(ctx context.Context, authority *gateway.SessionAuthority, record map[string]any, deadline *time.Time) error {
	var remaining *time.Duration
	if deadline != nil {
		left := max(0, time.Until(*deadline))
		remaining = &left
	}
	return authority.AwaitBotReplyPublished(ctx, pyrepr.Str(record["delivery_id"]), remaining)
}
