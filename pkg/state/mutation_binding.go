package state

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const BindingPrefix = "gateway.history_control.v1."

// Gateway owner identity rules and subject parser, assigned from pkg/gateway by the process wiring
var (
	SameOwner                 func(stored, subject any) bool
	OwnersAgree               func(first, second any) bool
	ParseAuthenticatedSubject func(subject any) ([3]string, bool)
)

func sameOwner(stored, subject any) bool {
	if SameOwner == nil {
		panic("state.SameOwner unassigned: the process wiring installs gateway owner_identity same_owner")
	}
	return SameOwner(stored, subject)
}

func ownersAgree(first, second any) bool {
	if OwnersAgree == nil {
		panic("state.OwnersAgree unassigned: the process wiring installs gateway owner_identity owners_agree")
	}
	return OwnersAgree(first, second)
}

func parseAuthenticatedSubject(subject any) ([3]string, bool) {
	if ParseAuthenticatedSubject == nil {
		panic("state.ParseAuthenticatedSubject unassigned: the process wiring installs gateway ParseAuthenticatedSubject")
	}
	return ParseAuthenticatedSubject(subject)
}

// Principal a history control check acts for
type HistoryActor struct {
	ProfileID    string
	Subject      string
	Capabilities []string
}

// Whether resume or import controls the row as owner history
func historyRow(row *pyjson.Object) bool {
	return rowGet(row, "source") == "import" || LegacyHistoryRow(row)
}

// Owner identity of the row, None for a Desktop Nous account label
func ownerUser(row *pyjson.Object) (any, error) {
	label, err := LegacyAccountLabel(row)
	if err != nil || label {
		return nil, err
	}
	return rowGet(row, "user_id"), nil
}

// Provider and user label Desktop stamps for an issuer less dashboard login subject
func LoginLabel(subject any) (string, bool) {
	parts, ok := parseAuthenticatedSubject(subject)
	if !ok {
		return "", false
	}
	provider, issuer, user := parts[0], parts[1], parts[2]
	if issuer != "" || strings.Contains(provider, ":") || pystr.Strip(provider) == "" || pystr.Strip(user) == "" {
		return "", false
	}
	label := pystr.Strip(provider) + ":" + pystr.Strip(user)
	if strings.HasPrefix(label, "auth:") {
		return "", false
	}
	return label, true
}

// Two items of a decoded JSON value as Python tuple unpacking yields them
func unpackPair(v any) (any, any, bool) {
	switch x := v.(type) {
	case []any:
		if len(x) == 2 {
			return x[0], x[1], true
		}
	case *pyjson.Object:
		if x.Len() == 2 {
			return x.Keys[0], x.Keys[1], true
		}
	case string:
		if runes := []rune(x); len(runes) == 2 {
			return string(runes[0]), string(runes[1]), true
		}
	}
	return nil, nil, false
}

// Stored profile and subject pair of a binding value, false when it does not decode to two items
func bindingPair(saved any) (any, any, bool) {
	text, ok := saved.(string)
	if !ok {
		return nil, nil, false
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil, false
	}
	return unpackPair(decoded)
}

// Whether a stored binding names the subject in the profile under the same owner rule
func BindingNames(saved, profileID, subject any) bool {
	savedProfile, savedSubject, ok := bindingPair(saved)
	if !ok {
		return false
	}
	return pyjson.Equal(savedProfile, profileID) && sameOwner(savedSubject, subject)
}

// Whether two stored bindings name the same owner, both absent agreeing and one absent not
func bindingsAgree(first, second any) bool {
	if pyjson.Equal(first, second) || first == nil || second == nil {
		return pyjson.Equal(first, second)
	}
	firstProfile, firstSubject, ok := bindingPair(first)
	if !ok {
		return false
	}
	profileID, subject, ok := bindingPair(second)
	if !ok {
		return false
	}
	return pyjson.Equal(firstProfile, profileID) && ownersAgree(firstSubject, subject)
}

func actorBinding(actor HistoryActor) (string, error) {
	return pyjson.Dumps([]any{actor.ProfileID, actor.Subject}, pyjson.Separators(",", ":"))
}

// Binds each session id the import is about to create to the importing actor
func ImportHistoryControl(ctx context.Context, conn SQLiteConn, actor HistoryActor, sessionIDs []string) error {
	binding, err := actorBinding(actor)
	if err != nil {
		return err
	}
	for _, sid := range sessionIDs {
		_, exists, err := fetchScalar(ctx, conn, "SELECT 1 FROM sessions WHERE id=?", sid)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", BindingPrefix+sid, binding); err != nil {
				return err
			}
		}
	}
	return nil
}

// Whether the actor controls the unrouted history row, claiming it when asked, permission_denied for a foreign owner
func AuthorizeHistory(ctx context.Context, conn SQLiteConn, actor HistoryActor, sessionID string, claim bool) (bool, error) {
	rows, err := queryObjects(ctx, conn, "SELECT * FROM sessions WHERE id=?", sessionID)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	row := rows[0]
	if routedRow(row) {
		return false, nil
	}
	key := BindingPrefix + sessionID
	binding, err := actorBinding(actor)
	if err != nil {
		return false, err
	}
	saved, ok, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", key)
	if err != nil {
		return false, err
	}
	if ok {
		if !BindingNames(saved, actor.ProfileID, actor.Subject) {
			return false, runtimeRefusal("permission_denied")
		}
		return true, nil
	}
	if !historyRow(row) {
		return false, nil
	}
	owner, err := ownerUser(row)
	if err != nil {
		return false, err
	}
	if pyTruthy(owner) && !sameOwner(rowGet(row, "user_id"), actor.Subject) {
		return false, runtimeRefusal("permission_denied")
	}
	canCreate := false
	for _, capability := range actor.Capabilities {
		canCreate = canCreate || capability == "session:create"
	}
	if !canCreate {
		return false, runtimeRefusal("permission_denied")
	}
	if claim {
		if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, binding); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Stored import binding of the session, nil when unbound
func HistoryBinding(ctx context.Context, conn SQLiteConn, sessionID string) (any, error) {
	saved, _, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", BindingPrefix+sessionID)
	return saved, err
}

// Whether a chain member is history the authorizer of the anchor controls
func SameHistoryOwner(ctx context.Context, conn SQLiteConn, anchorID, sessionID string) (bool, error) {
	const columns = "SELECT source,user_id,session_key,chat_id,origin_json FROM sessions WHERE id=?"
	anchor, ok, err := fetchRow(ctx, conn, columns, anchorID)
	if err != nil || !ok {
		return false, err
	}
	row, ok, err := fetchRow(ctx, conn, columns, sessionID)
	if err != nil || !ok {
		return false, err
	}
	if pyTruthy(row[2]) || pyTruthy(row[3]) || pyTruthy(row[4]) {
		for i := 1; i < 4; i++ {
			if !pyjson.Equal(row[i], anchor[i]) {
				return false, nil
			}
		}
		return true, nil
	}
	binding, err := HistoryBinding(ctx, conn, sessionID)
	if err != nil {
		return false, err
	}
	anchorBinding, err := HistoryBinding(ctx, conn, anchorID)
	if err != nil {
		return false, err
	}
	if !bindingsAgree(binding, anchorBinding) {
		return false, nil
	}
	if binding != nil {
		return true, nil
	}
	routelessPeer := pyjson.Equal(row[0], anchor[0]) && !pyTruthy(anchor[2]) && !pyTruthy(anchor[3]) && !pyTruthy(anchor[4])
	member := pyjson.ObjectOf("source", row[0], "user_id", row[1], "session_key", row[2], "chat_id", row[3], "origin_json", row[4])
	if !historyRow(member) && !routelessPeer {
		return false, nil
	}
	owner, err := ownerUser(member)
	if err != nil {
		return false, err
	}
	return owner == nil || owner == "" || ownersAgree(owner, anchor[1]), nil
}
