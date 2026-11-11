package gatewayapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Body fields only a verified room grant may supply
var roomOnlyFields = []string{"hosted_room_dispatch", "_room_execution_policy"}

// Creates or verifies the target's canonical hidden group session for one room member
func (a *APIServerAdapter) ensureHostedMemberSession(ctx context.Context, dispatch *gateway.HostedMemberDispatch) (string, error) {
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return "", &gateway.PyError{Kind: "RuntimeError", Msg: "session database unavailable"}
	}
	title := "Group: " + dispatch.RoomID
	seed := dispatch.HomeInstallID + "\x00" + dispatch.RoomID + "\x00" + dispatch.MemberID + "\x00" + dispatch.TargetProfile
	sum := sha256.Sum256([]byte(seed))
	sessionID := "room_" + hex.EncodeToString(sum[:])[:32]
	authority, err := gateway.ActiveAuthority(ctx, a.GatewayRunner)
	if err != nil {
		return "", err
	}
	if authority != nil {
		if db != authority.DB {
			return "", &gateway.PyError{Kind: "RuntimeError", Msg: "profile_mismatch"}
		}
		identity := []string{dispatch.HomeInstallID, dispatch.RoomID, dispatch.MemberID, dispatch.TargetProfile}
		if _, sessionID, err = gateway.HostedMemberSession(ctx, authority.DB, identity); err != nil {
			return "", err
		}
		hosted, _ := pyjson.Plain(dispatch.AsMapping()).(map[string]any)
		bound, err := gateway.RunStepsOffLoop(ctx, authority, gateway.BindAPISteps(ctx, authority, sessionID, hosted, "", ""))
		if err != nil {
			return "", err
		}
		return bound.(gateway.SessionRef).SessionID, nil
	}
	err = db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		row, err := state.QueryRowObject(ctx, conn, "SELECT id, title, source FROM sessions WHERE id=?", sessionID)
		if err != nil {
			return err
		}
		if row != nil {
			if !pyjson.Equal(row.GetOr("title", nil), title) || !pyjson.Equal(row.GetOr("source", nil), "bot_room") {
				return &gateway.PyError{Kind: "RuntimeError", Msg: "room session identity conflicts with existing data"}
			}
			return nil
		}
		cleanTitle, err := state.SanitizeTitle(title)
		if err != nil {
			return err
		}
		conflict, err := state.QueryRowObject(ctx, conn, "SELECT id FROM sessions WHERE title=? AND id!=?", cleanTitle, sessionID)
		if err != nil {
			return err
		}
		if conflict != nil {
			return &gateway.PyError{Kind: "RuntimeError", Msg: "Another group already uses this room title on the target gateway. " +
				"Rename or migrate that group before retrying."}
		}
		_, err = conn.ExecContext(ctx, "INSERT INTO sessions(id, source, title, hidden, started_at) VALUES(?, 'bot_room', ?, 1, ?)",
			sessionID, cleanTitle, wallClock())
		return err
	})
	if err != nil {
		return "", err
	}
	return sessionID, nil
}

// 403 for a refused room dispatch, naming a changed execution policy or capability catalog
func roomDispatchError(err error) *gateway.WebResponse {
	message, code := err.Error(), "invalid_room_dispatch"
	lowered := strings.ToLower(message)
	switch {
	case strings.Contains(lowered, "execution policy") || strings.Contains(lowered, "remote room execution requires"):
		message, code = "Room execution policy changed; reauthorization is required.", "room_execution_policy_changed"
	case strings.Contains(lowered, "capability catalog changed"):
		message, code = "Room capability catalog changed; reauthorization is required.", "room_capability_catalog_changed"
	}
	return jsonError(message, code, http.StatusForbidden)
}

// Validated and normalized body of a scoped RoomLink dispatch, the body unchanged without a room grant
func (a *APIServerAdapter) normalizeRoomDispatch(ctx context.Context, request *gateway.WebRequest, body any) (any, *gateway.WebResponse, error) {
	roomToken := roomGrantToken(request)
	dict, isDict := body.(*pyjson.Object)
	if roomToken == "" {
		if isDict {
			for _, field := range roomOnlyFields {
				if _, present := dict.Get(field); present {
					return body, jsonError("hosted_room_dispatch and _room_execution_policy require a room grant.", "room_grant_required",
						http.StatusForbidden), nil
				}
			}
		}
		return body, nil, nil
	}
	if !isDict {
		return body, jsonError("Room dispatch accepts only input and hosted_room_dispatch.", "invalid_room_dispatch", http.StatusBadRequest), nil
	}
	for _, key := range dict.Keys {
		if key != "input" && key != "hosted_room_dispatch" {
			return body, jsonError("Room dispatch accepts only input and hosted_room_dispatch.", "invalid_room_dispatch", http.StatusBadRequest), nil
		}
	}
	normalized, err := func() (*pyjson.Object, error) {
		dispatch, err := gateway.HostedMemberDispatchFromMapping(dict.GetOr("hosted_room_dispatch", nil))
		if err != nil {
			return nil, err
		}
		secret, err := a.roomGrantSecret(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := gateway.VerifyRoomGrant(secret, roomToken, dispatch, "dispatch", nil); err != nil {
			return nil, err
		}
		activeProfile := requestProfile(ctx)
		if activeProfile == "" {
			activeProfile = "default"
		}
		localInstall, err := gateway.LocalAuthorityGatewayID(ctx)
		if err != nil {
			return nil, err
		}
		if dispatch.TargetProfile != activeProfile || dispatch.TargetInstallID != localInstall {
			return nil, &gateway.PyError{Kind: "ValueError", Msg: "room dispatch target does not match this profile"}
		}
		_, catalogMap, err := gateway.APILocalRoomCatalog(ctx, a, activeProfile, localInstall)
		if err != nil {
			return nil, err
		}
		catalog, err := gateway.GatewayRoomCatalogFromMapping(catalogMap)
		if err != nil {
			return nil, err
		}
		policy, err := gateway.RoomExecutionPolicyFromMapping(catalog.ExecutionPolicy.AsMapping())
		if err != nil {
			return nil, err
		}
		if !hmac.Equal([]byte(policy.PolicyDigest), []byte(dispatch.ExecutionPolicyDigest)) {
			return nil, &gateway.PyError{Kind: "ValueError", Msg: "room execution policy changed"}
		}
		if !hmac.Equal([]byte(catalog.CatalogDigest), []byte(dispatch.CapabilityDigest)) {
			return nil, &gateway.PyError{Kind: "ValueError", Msg: "room capability catalog changed"}
		}
		if input := dict.GetOr("input", nil); input != nil && !pyjson.Equal(input, dispatch.Prompt) {
			return nil, &gateway.PyError{Kind: "ValueError", Msg: "room dispatch input does not match its prompt"}
		}
		expectedKey := "room:" + dispatch.TaskID + ":" + dispatch.ExecutionGeneration.String()
		if strings.TrimSpace(request.Header.Get("Idempotency-Key")) != expectedKey {
			return nil, &gateway.PyError{Kind: "ValueError", Msg: "room dispatch idempotency key is invalid"}
		}
		sessionID, err := a.ensureHostedMemberSession(ctx, dispatch)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("input", dispatch.Prompt, "session_id", sessionID, "hosted_room_dispatch", dispatch.AsMapping(),
			"_room_execution_policy", policy.AsMapping()), nil
	}()
	if err != nil {
		return body, roomDispatchError(err), nil
	}
	return normalized, nil, nil
}
