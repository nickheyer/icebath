package state

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var legacyLocalSources = map[string]bool{"cli": true, "tui": true, "gui": true, "desktop": true, "oneshot": true, "tool": true, "acp": true}

var legacyCreationSources = map[string]string{"desktop": "gui"}

var notClientLabelSet = map[string]bool{"cron": true, "subagent": true, "kanban": true, "bot_room": true, "a2a": true, "webhook": true,
	"api_server": true, "import": true, "recovered": true, "curator": true, "batch": true, "homeassistant": true, "webui": true,
	"local": true, "unknown": true}

// Every shipped platform value plus bundled plugin platforms and aliases, assigned from pkg/gateway by the process wiring
var MessagingPlatformLabels func() []string

var messagingLabels struct {
	once   sync.Once
	labels map[string]bool
}

// Shipped messaging platform labels computed once, the lru_cache of one entry
func messagingLabelSet() map[string]bool {
	messagingLabels.once.Do(func() {
		if MessagingPlatformLabels == nil {
			panic("state.MessagingPlatformLabels unassigned: the process wiring installs the gateway Platform values and bundled plugin platforms")
		}
		labels := map[string]bool{}
		for _, label := range MessagingPlatformLabels() {
			labels[label] = true
		}
		messagingLabels.labels = labels
	})
	return messagingLabels.labels
}

// Labels a client app row can never carry, for queries that pre filter rows
func NotClientLabels() map[string]bool {
	out := map[string]bool{}
	for _, set := range []map[string]bool{legacyLocalSources, notClientLabelSet, messagingLabelSet()} {
		for label := range set {
			out[label] = true
		}
	}
	return out
}

// Whether the source is a client app's own label rather than a surface, platform or automation label
func ClientLabel(source any) bool {
	text, ok := source.(string)
	return ok && pystr.Strip(text) != "" && !NotClientLabels()[text]
}

func rowGet(row *pyjson.Object, key string) any {
	v, _ := row.Get(key)
	return v
}

func routedRow(row *pyjson.Object) bool {
	for _, key := range []string{"session_key", "chat_id", "origin_json"} {
		if pyTruthy(rowGet(row, key)) {
			return true
		}
	}
	return false
}

// Whether a routeless row is a chat a client app wrote under its own label, adopted as gui
func ClientHistoryRow(row *pyjson.Object) bool {
	return ClientLabel(rowGet(row, "source")) && !routedRow(row)
}

// Whether resume adopts the row as old local history, a local surface label or a client app label
func LegacyHistoryRow(row *pyjson.Object) bool {
	source, ok := rowGet(row, "source").(string)
	return (ok && legacyLocalSources[source]) || ClientHistoryRow(row)
}

func rowSource(row *pyjson.Object) (any, error) {
	source, has := row.Get("source")
	if !has {
		return nil, &stateTypedError{"KeyError", "'source'"}
	}
	return source, nil
}

// Creation source an adopted legacy row resumes on
func LegacyCreationSource(row *pyjson.Object) (any, error) {
	if ClientHistoryRow(row) {
		return "gui", nil
	}
	source, err := rowSource(row)
	if err != nil {
		return nil, err
	}
	if text, ok := source.(string); ok {
		if mapped, has := legacyCreationSources[text]; has {
			return mapped, nil
		}
	}
	return source, nil
}

// Whether the row's user_id is the Nous account label a Desktop chat carried, not an owner
func LegacyAccountLabel(row *pyjson.Object) (bool, error) {
	source, err := rowSource(row)
	if err != nil {
		return false, err
	}
	userID, ok := rowGet(row, "user_id").(string)
	return source == "desktop" && ok && strings.HasPrefix(userID, "nous:"), nil
}

// Whether the subject owns the dashboard login stamp a served build put on the row
func LoginOwnedBy(row *pyjson.Object, subject any) bool {
	userID, ok := rowGet(row, "user_id").(string)
	if !ok || !pyTruthy(subject) {
		return false
	}
	return sameOwner(userID, subject)
}

// Refuses as not_found every row but unrouted old local history of this profile
func (d *SessionDB) ValidateLegacyRow(row *pyjson.Object, subject any) error {
	if !LegacyHistoryRow(row) || routedRow(row) {
		return runtimeRefusal("not_found")
	}
	if userID := rowGet(row, "user_id"); pyTruthy(userID) {
		label, err := LegacyAccountLabel(row)
		if err != nil {
			return err
		}
		if !label && !LoginOwnedBy(row, subject) {
			return runtimeRefusal("not_found")
		}
	}
	own := d.ownProfileName()
	if own == "" {
		own = "default"
	}
	if profile := rowGet(row, "profile_name"); profile != nil && profile != "" && profile != own {
		return runtimeRefusal("not_found")
	}
	return nil
}

// Claims an idle unbound legacy row for the receipt's route and records the legacy binding
func (d *SessionDB) bindLegacyTarget(ctx context.Context, conn SQLiteConn, receipt *pyjson.Object) error {
	target, err := receiptItem(receipt, "legacy_session_id")
	if err != nil {
		return err
	}
	sessionID, err := receiptItem(receipt, "session_id")
	if err != nil {
		return err
	}
	if !pyjson.Equal(target, sessionID) {
		return runtimeRefusal("storage_unavailable")
	}
	targetID, _ := target.(string)
	rows, err := queryObjects(ctx, conn, "SELECT * FROM sessions WHERE id=?", target)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return runtimeRefusal("not_found")
	}
	row := rows[0]
	principal, err := receiptItem(receipt, "principal_id")
	if err != nil {
		return err
	}
	if err := d.ValidateLegacyRow(row, principal); err != nil {
		return err
	}
	imported, ok, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", BindingPrefix+targetID)
	if err != nil {
		return err
	}
	if ok {
		profileID, err := receiptItem(receipt, "profile_id")
		if err != nil {
			return err
		}
		if !BindingNames(imported, profileID, principal) {
			return runtimeRefusal("permission_denied")
		}
	}
	if _, stepped, err := fetchScalar(ctx, conn, chainStepSQL, targetID); err != nil {
		return err
	} else if stepped {
		return runtimeRefusal("admission_conflict")
	}
	if err := d.RequireIdle(ctx, conn, []string{targetID}); err != nil {
		return err
	}
	root, err := d.sessionTurnLeaseKeyOnConn(ctx, conn, targetID)
	if err != nil {
		return err
	}
	if _, leased, err := fetchScalar(ctx, conn, "SELECT 1 FROM session_turn_leases WHERE conversation_id=? AND expires_at>?", root, nowSeconds()); err != nil {
		return err
	} else if leased {
		return runtimeRefusal("runtime_coordination_required")
	}
	key := LocalLegacyPrefix + targetID
	if _, bound, err := fetchScalar(ctx, conn, "SELECT 1 FROM state_meta WHERE key=?", key); err != nil {
		return err
	} else if bound {
		return runtimeRefusal("storage_unavailable")
	}
	origin, err := receiptPath(receipt, "entry", "origin")
	if err != nil {
		return err
	}
	label := rowGet(row, "source")
	if ClientHistoryRow(row) {
		label = "desktop"
	}
	route, err := receiptItem(receipt, "route")
	if err != nil {
		return err
	}
	originJSON, err := runtimeJSON(origin)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET source=?,session_key=?,chat_id=?,user_id=?,chat_type=?,origin_json=? WHERE id=?",
		label, route, sessionID, principal, "dm", originJSON, targetID); err != nil {
		return err
	}
	profileID, err := receiptItem(receipt, "profile_id")
	if err != nil {
		return err
	}
	text, err := runtimeJSON(pyjson.ObjectOf("session_id", sessionID, "profile_id", profileID, "principal_id", principal, "legacy_session_id", target))
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, text)
	return err
}

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }
