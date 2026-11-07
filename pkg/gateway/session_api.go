package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Stored creation label of an API server binding, the fallback for every other transport
func apiStorageSource(ctx context.Context, db SessionStoreDB, sessionID, fallback string) (string, error) {
	if fallback != "api_server" {
		return fallback, nil
	}
	saved, ok, err := db.GetMeta(ctx, state.LocalAPIBindingPrefix+sessionID)
	if err != nil || !ok {
		return fallback, err
	}
	decoded, err := pyjson.Loads(saved)
	if err != nil {
		return "", err
	}
	binding, isObj := decoded.(*pyjson.Object)
	if !isObj {
		return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyTypeName(decoded))
	}
	value, has := binding.Get("storage_source")
	if !has {
		return fallback, nil
	}
	label, isText := value.(string)
	if !isText {
		return "", fmt.Errorf("TypeError: storage_source is %s, not str", pyTypeName(value))
	}
	return label, nil
}

// Session a declared API conversation key names, the registry first then the legacy peer row
func declaredAPISession(ctx context.Context, db *state.SessionDB, key string) (string, bool, error) {
	saved, ok, err := db.GetMeta(ctx, state.LocalAPIDeclaredPrefix+key)
	if err != nil {
		return "", false, err
	}
	if ok {
		return saved, true, nil
	}
	legacy, err := db.FindLatestGatewaySessionForPeer(ctx, state.GatewayPeerQuery{Source: "api_server", SessionKey: &key})
	if err != nil || legacy == nil {
		return "", false, err
	}
	id, _ := legacy.Get("id")
	text, _ := id.(string)
	return text, true, nil
}

// Session a header less completions turn lands on, the first unretired generation of its opening
func implicitAPISession(ctx context.Context, db *state.SessionDB, sessionID, requestID string) (string, error) {
	var pinned state.GenerationProbe
	if requestID != "" {
		pinned = func(ctx context.Context, conn state.SQLiteConn, sid string) (bool, error) {
			key, err := apiIdentityKey(sid, requestID)
			if err != nil {
				return false, err
			}
			var one int
			err = conn.QueryRowContext(ctx, "SELECT 1 FROM state_meta WHERE key=?", key).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return err == nil, err
		}
	}
	_, sid, err := db.FirstGeneration(ctx, func(n int) string { return apiGenerationID(sessionID, n) }, pinned, nil)
	return sid, err
}

// Terminal identity meta key of one API request on a session, hermes_state_terminal identity_key
func apiIdentityKey(sessionID, requestID string) (string, error) {
	fp, err := state.AdmissionFingerprint(sessionID, map[string]any{"principal": "api", "request": requestID})
	if err != nil {
		return "", err
	}
	return state.TerminalIdentityPrefix + fp, nil
}

// The id itself for generation zero, then api- and sixteen hex digits of the id and generation digest
func apiGenerationID(sessionID string, generation int) string {
	if generation == 0 {
		return sessionID
	}
	sum := sha256.Sum256([]byte(sessionID + "\x00" + strconv.Itoa(generation)))
	return "api-" + hex.EncodeToString(sum[:])[:16]
}

// room_ and 32 hex digits of a hosted member identity digest, a later generation folded in
func roomSessionID(identity []string, generation int) string {
	seed := strings.Join(identity, "\x00")
	if generation != 0 {
		seed += "\x00" + strconv.Itoa(generation)
	}
	sum := sha256.Sum256([]byte(seed))
	return "room_" + hex.EncodeToString(sum[:])[:32]
}

// Generation and session id of the hidden session a hosted room member dispatches land on
func hostedMemberSession(ctx context.Context, db *state.SessionDB, identity []string) (int, string, error) {
	return db.FirstGeneration(ctx, func(n int) string { return roomSessionID(identity, n) }, nil, nil)
}
