package state

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Turn result keeping replay, accounting and this turn's API suffix, never the cumulative history
func CompactResult(result *pyjson.Object, userMessage any) (*pyjson.Object, error) {
	result = result.Copy()
	raw, _ := result.Get("result")
	inner, ok := raw.(*pyjson.Object)
	if !ok {
		return result, nil
	}
	value := inner.Copy()
	value.Delete("tools")
	rawMessages, _ := value.Get("messages")
	if messages, isList := rawMessages.([]any); isList {
		compressed, _ := value.Get("_compressed")
		suffix, _ := value.Get("_messages_are_turn_suffix")
		if !pyTruthy(compressed) && !pyTruthy(suffix) {
			rawAnchor, _ := value.Get("current_turn_user_idx")
			anchor, exact := exactInt(rawAnchor)
			if !exact || anchor < 0 || anchor >= int64(len(messages)) || !userRow(messages[anchor]) {
				anchor = int64(reanchorCurrentTurnUserIdx(messages, userMessage))
			}
			value.Set("messages", append([]any{}, messages[anchor+1:]...))
			if anchor >= 0 {
				if row, isObj := messages[anchor].(*pyjson.Object); isObj {
					apiContent, _ := row.Get("api_content")
					if pyTruthy(apiContent) {
						turnRow := pyjson.NewObject()
						for _, k := range []string{"role", "content", "api_content"} {
							v, has := row.Get(k)
							if !has {
								return nil, &stateTypedError{"KeyError", k}
							}
							turnRow.Set(k, v)
						}
						value.Set("_turn_user_row", turnRow)
					}
				}
			}
			value.Set("_messages_are_turn_suffix", true)
			value.Delete("current_turn_user_idx")
		}
	}
	result.Set("result", value)
	return result, nil
}

// True for a dict message whose role is user
func userRow(message any) bool {
	row, ok := pyjson.AsObject(message)
	if !ok {
		return false
	}
	role, _ := row.Get("role")
	return pyjson.Equal(role, "user")
}

// Index of this turn's user message after compaction rebuilt the list, minus one when no user turn remains
func reanchorCurrentTurnUserIdx(messages []any, userMessage any) int {
	fallback := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if !userRow(messages[i]) {
			continue
		}
		row, _ := pyjson.AsObject(messages[i])
		content, _ := row.Get("content")
		if pyjson.Equal(content, userMessage) {
			return i
		}
		_, live := SplitUserOriginatedTurn(objMap(row))
		if live == nil {
			continue
		}
		if pyjson.Equal(live["content"], userMessage) {
			return i
		}
		if fallback < 0 {
			fallback = i
		}
	}
	return fallback
}

// Retired API admission records carrying the request id, which stay spent for every other target
func TerminalAPIAdmissions(ctx context.Context, conn SQLiteConn, requestID string) ([]any, error) {
	values, err := queryStrings(ctx, conn, "SELECT value FROM state_meta WHERE key GLOB ? "+
		"AND json_extract(value, '$.principal_id')='api' AND json_extract(value, '$.request_id')=?",
		TerminalAdmissionPrefix+"*", requestID)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(values))
	for _, text := range values {
		decoded, err := pyjson.Loads(text)
		if err != nil {
			return nil, pyValueError(err.Error())
		}
		out = append(out, decoded)
	}
	return out, nil
}

// Stored tombstone of one retired admission, nil when none was kept
func terminalAdmission(ctx context.Context, conn SQLiteConn, admissionID any) (*pyjson.Object, error) {
	id, ok := admissionID.(string)
	if !ok {
		return nil, &stateTypedError{"TypeError", "can only concatenate str to str"}
	}
	saved, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", TerminalAdmissionPrefix+id)
	if err != nil || saved == nil {
		return nil, err
	}
	text, _ := saved["value"].(string)
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	row, isObj := decoded.(*pyjson.Object)
	if !isObj {
		return nil, &stateTypedError{"TypeError", "terminal admission is not a dict"}
	}
	return row, nil
}

// Exact retry of an admission on a retired target, nil for a live target and a refusal for a retired miss or conflict
func (d *SessionDB) retryTerminalAdmission(ctx context.Context, in SessionInput) (*pyjson.Object, error) {
	for _, v := range []string{in.PrincipalID, in.SessionID, in.RequestID} {
		if err := runtimeText(v); err != nil {
			return nil, err
		}
	}
	digest, err := admissionDigest(in)
	if err != nil {
		return nil, err
	}
	var out *pyjson.Object
	err = d.readCtx(ctx, false, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, in.Epoch); err != nil {
			return err
		}
		retired, err := connRow(ctx, conn, "SELECT 1 AS one FROM state_meta WHERE key=?", RetiredPrefix+in.SessionID)
		if err != nil || retired == nil {
			return err
		}
		key, err := terminalIdentityKey(in.PrincipalID, in.SessionID, in.RequestID)
		if err != nil {
			return err
		}
		saved, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", key)
		if err != nil {
			return err
		}
		if saved == nil {
			return runtimeRefusal("not_found")
		}
		text, _ := saved["value"].(string)
		admissionID, err := pyjson.Loads(text)
		if err != nil {
			return pyValueError(err.Error())
		}
		row, err := terminalAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		if row == nil {
			return &stateTypedError{"TypeError", "'NoneType' object is not subscriptable"}
		}
		stored, _ := row.Get("payload_digest")
		if !pyjson.Equal(stored, digest) {
			return admittedRequestConflict()
		}
		out, err = admissionRow(row)
		return err
	})
	return out, err
}

// Hex sha256 of an adoption secret, refused like any other invalid runtime text
func secretDigest(secret string) (string, error) {
	if err := runtimeText(secret); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:]), nil
}

// Retired producer evidence for one exact worker receipt, possession never reactivating the worker
func (d *SessionDB) TerminalWorkerReceipt(ctx context.Context, executionID, sessionID string, generation, sequence int64, adoptionSecret string, payloadDigest any) (any, error) {
	proof, err := secretDigest(adoptionSecret)
	if err != nil {
		return nil, err
	}
	var out any
	err = d.readCtx(ctx, false, func(conn SQLiteConn) error {
		saved, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", TerminalWorkerPrefix+executionID)
		if err != nil {
			return err
		}
		if saved == nil {
			return runtimeRefusal("not_found")
		}
		text, _ := saved["value"].(string)
		decoded, err := pyjson.Loads(text)
		if err != nil {
			return pyValueError(err.Error())
		}
		worker, ok := decoded.(*pyjson.Object)
		if !ok {
			return &stateTypedError{"TypeError", "terminal worker is not a dict"}
		}
		owner, _ := worker.Get("session_id")
		adoption, _ := worker.Get("adoption_digest")
		adoptionText, _ := adoption.(string)
		if !pyjson.Equal(owner, sessionID) || subtle.ConstantTimeCompare([]byte(adoptionText), []byte(proof)) != 1 {
			return runtimeRefusal("permission_denied")
		}
		stored, _ := worker.Get("generation")
		if n, exact := exactInt(stored); !exact || n != generation {
			return runtimeRefusal("stale_generation")
		}
		receipts, _ := worker.Get("receipts")
		list, _ := receipts.([]any)
		var receipt *pyjson.Object
		for _, item := range list {
			r, isObj := item.(*pyjson.Object)
			if !isObj {
				continue
			}
			seq, _ := r.Get("sequence")
			if n, exact := exactInt(seq); exact && n == sequence {
				receipt = r
				break
			}
		}
		if receipt == nil {
			return runtimeRefusal("stale_generation")
		}
		digest, _ := receipt.Get("payload_digest")
		if !pyjson.Equal(digest, payloadDigest) {
			return runtimeRefusal("admission_conflict")
		}
		resultJSON, _ := receipt.Get("result_json")
		if resultJSON == nil {
			return runtimeRefusal("stale_generation")
		}
		resultText, isStr := resultJSON.(string)
		if !isStr {
			return &stateTypedError{"TypeError", "the JSON object must be str"}
		}
		out, err = pyjson.Loads(resultText)
		if err != nil {
			return pyValueError(err.Error())
		}
		return nil
	})
	return out, err
}
