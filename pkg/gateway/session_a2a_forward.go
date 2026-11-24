package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Fields a forwarded A2A turn carries
var a2aForwardFields = []string{"agent", "tenant", "peer", "context_id", "input_id", "text"}

// Runs of characters a context slug replaces with one dash
var unsafeSlugRun = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// Context id sanitized for a session title, ctx when nothing survives, at most 96 characters
func safeContextSlug(value any) string {
	text := ""
	if anyutil.IsTrue(value) {
		text = anyutil.String(value)
	}
	slug := strings.Trim(unsafeSlugRun.ReplaceAllString(text, "-"), "-._")
	if slug == "" {
		slug = "ctx"
	}
	if runes := []rune(slug); len(runes) > 96 {
		slug = string(runes[:96])
	}
	return slug
}

// Valid forward params, exactly the six string fields, non empty but tenant and within their length caps
func validForwardParams(params *jsonutil.Object) bool {
	if params.Len() != len(a2aForwardFields) {
		return false
	}
	for _, key := range a2aForwardFields {
		value, ok := params.Get(key)
		text, isStr := value.(string)
		if !ok || !isStr {
			return false
		}
		if key != "tenant" && text == "" {
			return false
		}
		if key != "text" && pyLen(text) > 1024 {
			return false
		}
		if key == "text" && pyLen(text) > 1_048_576 {
			return false
		}
	}
	return true
}

// A2A forward verb, the peer's turn admitted on the canonical session its four part identity owns
func (c *AuthorityConnection) a2aForward(ctx context.Context, ref SessionRef, params *jsonutil.Object) (any, error) {
	a, actor := c.authority, c.actor
	if actor.ProfileID != a.ProfileID {
		return nil, runtimeRefusal("profile_mismatch")
	}
	if !actor.Capabilities["session:create"] || !actor.Capabilities["session:submit"] || !actor.Capabilities["session:read"] {
		return nil, runtimeRefusal("permission_denied")
	}
	if !validForwardParams(params) {
		return nil, runtimeRefusal("invalid_params")
	}
	identity := []string{}
	identityList := []any{}
	for _, key := range []string{"agent", "tenant", "peer", "context_id"} {
		text := params.ValueOr(key, "").(string)
		identity = append(identity, text)
		identityList = append(identityList, text)
	}
	encoded, err := jsonutil.Encode(identityList, jsonutil.ASCII())
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(encoded))
	base := "a2a:" + hex.EncodeToString(digest[:])
	requestID := func(n int) string {
		if n == 0 {
			return base
		}
		return fmt.Sprintf("%s:%d", base, n)
	}
	inputID := params.ValueOr("input_id", "").(string)
	var deriveErr error
	derive := func(n int) string {
		sid, err := state.LocalIdentity(a.ProfileID, actor.Subject, requestID(n))
		if err != nil && deriveErr == nil {
			deriveErr = err
		}
		return sid
	}
	pinned := func(ctx context.Context, conn state.SQLiteConn, sid string) (bool, error) {
		key, err := identityMetaKey(actor.Subject, sid, inputID)
		if err != nil {
			return false, err
		}
		rows, err := conn.QueryContext(ctx, "SELECT 1 FROM state_meta WHERE key=?", key)
		if err != nil {
			return false, err
		}
		defer rows.Close()
		found := rows.Next()
		return found, rows.Err()
	}
	generation, sid, err := a.DB.FirstGeneration(ctx, derive, pinned, nil)
	if err != nil {
		return nil, err
	}
	if deriveErr != nil {
		return nil, deriveErr
	}
	key := requestID(generation)
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	row, err := a.DB.GetSession(ctx, sid)
	if err != nil {
		return nil, err
	}
	var target SessionRef
	if row != nil {
		if err := checkForwardIdentity(ctx, a, sid, identityList); err != nil {
			return nil, err
		}
		if target, err = restoreLocalSession(ctx, a, sid); err != nil {
			return nil, err
		}
	} else {
		title := fmt.Sprintf("a2a-%s-%s", params.ValueOr("agent", ""), safeContextSlug(params.ValueOr("context_id", nil)))
		legacy, err := a.DB.GetSessionByTitle(ctx, title)
		if err != nil {
			return nil, err
		}
		if legacy != nil {
			pkgLog().Warn(fmt.Sprintf("A2A: pre-authority session %s titled %s has no peer identity; "+
				"forwarding into a new session instead of adopting it", anyutil.String(legacy.ValueOr("id", nil)), anyutil.Quote(title)))
		}
		existing, staged, err := stageLocalSession(withForwardIdentity(ctx, identity), a, actor,
			map[string]any{"source": "a2a", "request_id": key}, localCreateOptions{})
		if err != nil {
			return nil, err
		}
		if staged != nil {
			receipt := *staged
			if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
				return a.DB.CommitLocalSession(taskCtx, a.Epoch, receipt)
			}, nil, false, nil); err != nil {
				return nil, err
			}
			created, err := receiptField(receipt.Receipt, "session_id")
			if err != nil {
				return nil, err
			}
			if target, err = restoreLocalSession(ctx, a, pyStrOf(created)); err != nil {
				return nil, err
			}
		} else {
			target = *existing
		}
	}
	if !c.subscribed(target.SessionID) {
		if _, err := c.resume(ctx, target, jsonutil.NewObject()); err != nil {
			return nil, err
		}
	}
	receipt, err := a.Submit(ctx, actor, Submission{RequestID: inputID, Ref: target,
		Payload: map[string]any{"text": params.ValueOr("text", "")}, Intent: "queue"}, SubmitOptions{})
	if err != nil {
		return nil, err
	}
	saved, err := admissionResult(ctx, a.DB, receipt.AdmissionID)
	if err != nil {
		return nil, err
	}
	var result any
	if saved != nil {
		savedObj, ok := saved.(*jsonutil.Object)
		if !ok {
			return nil, &PyError{Kind: "TypeError", Msg: "the saved A2A forward is " + errchain.Kind(saved) + ", expected a mapping"}
		}
		if result, err = receiptField(savedObj, "result"); err != nil {
			return nil, err
		}
	}
	answer := admissionReceiptDict(receipt)
	answer.Set("session_id", target.SessionID)
	answer.Set("result", result)
	return answer, nil
}

// Terminal identity meta key of one principal request on a session
func identityMetaKey(principalID, sessionID, requestID string) (string, error) {
	fp, err := state.AdmissionFingerprint(sessionID, map[string]any{"principal": principalID, "request": requestID})
	if err != nil {
		return "", err
	}
	return state.TerminalIdentityPrefix + fp, nil
}

// Refuses a forward onto an existing session whose frozen policy names another peer identity
func checkForwardIdentity(ctx context.Context, a *SessionAuthority, sid string, identity []any) error {
	receipt, err := a.DB.LocalReceipt(ctx, sid)
	if err != nil {
		return err
	}
	if receipt == nil {
		return &PyError{Kind: "TypeError", Msg: "the forwarded session row is missing"}
	}
	rawPolicy, err := receiptField(receipt, "policy")
	if err != nil {
		return err
	}
	policy, ok := rawPolicy.(*jsonutil.Object)
	if !ok {
		return &PyError{Kind: "TypeError", Msg: "the forwarded session policy is " + errchain.Kind(rawPolicy) + ", expected a mapping"}
	}
	rawRequest, err := receiptField(policy, "request_json")
	if err != nil {
		return err
	}
	text, isStr := rawRequest.(string)
	if !isStr {
		return &PyError{Kind: "TypeError", Msg: "the forwarded request is " + errchain.Kind(rawRequest) + ", expected JSON text"}
	}
	parsed, err := jsonutil.Decode(text)
	if err != nil {
		return &PyError{Kind: "JSONDecodeError", Msg: err.Error()}
	}
	request, ok := parsed.(*jsonutil.Object)
	if !ok {
		return &PyError{Kind: "AttributeError", Msg: "the forwarded request is " + errchain.Kind(parsed) + ", expected a mapping"}
	}
	if !anyutil.Equal(request.ValueOr("a2a_identity", nil), identity) {
		return runtimeRefusal("admission_conflict")
	}
	return nil
}
