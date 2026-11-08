package state

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Owner memory snapshot a staged create receipt publishes with the transaction that creates its row
type OnCreateSnapshot interface {
	Read() error
	Publish() error
	Withdraw()
}

// Staged local create receipt with the owner only parts never stored
type LocalCreateReceipt struct {
	Receipt *pyjson.Object
	// The staged request's own client env, nil when the receipt carries none
	ClientRequest *pyjson.Object
	OnCreate      OnCreateSnapshot
}

// Epoch seconds of a naive ISO datetime read as local time, the fromisoformat timestamp rule
func isoTimestamp(value any) (float64, error) {
	text, ok := value.(string)
	if !ok {
		return 0, &stateTypedError{"TypeError", "fromisoformat: argument must be str"}
	}
	t, aware, err := pydatetime.ParseISOFormat(text)
	if err != nil {
		return 0, pyValueError(err.Error())
	}
	if !aware {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
	}
	return float64(t.UnixNano()) / 1e9, nil
}

// The sessions row cwd, the create request's row_cwd when it sent one else the execution cwd
func rowCwd(policy *pyjson.Object) (any, error) {
	requestJSON, _ := policy.Get("request_json")
	var request any
	if text, ok := requestJSON.(string); ok && text != "" {
		decoded, err := pyjson.Loads(text)
		if err == nil {
			request = decoded
		}
	} else if requestJSON != nil && pyTruthy(requestJSON) {
		request = nil
	} else {
		request = pyjson.NewObject()
	}
	var value any
	if obj, ok := request.(*pyjson.Object); ok {
		value, _ = obj.Get("row_cwd")
	}
	text, isStr := value.(string)
	if !isStr {
		return receiptItem(policy, "cwd")
	}
	if text == "" {
		return nil, nil
	}
	return text, nil
}

// Reserves the row, route and immutable policy of a local session together before publication
func (d *SessionDB) CommitLocalSession(ctx context.Context, epoch int64, staged LocalCreateReceipt) (*pyjson.Object, error) {
	request := staged.ClientRequest
	if request == nil {
		policy, err := receiptItem(staged.Receipt, "policy")
		if err != nil {
			return nil, err
		}
		policyObj, ok := policy.(*pyjson.Object)
		if !ok {
			return nil, &stateTypedError{"TypeError", "'" + pyTypeName(policy) + "' object has no attribute 'get'"}
		}
		if clientEnv, _ := policyObj.Get("client_env_json"); clientEnv != nil {
			return nil, runtimeRefusal("invalid_params")
		}
		request = pyjson.NewObject()
	}
	if staged.OnCreate != nil {
		if err := staged.OnCreate.Read(); err != nil {
			return nil, err
		}
	}
	encoded, err := runtimeJSON(staged.Receipt)
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(encoded)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	receipt := decoded.(*pyjson.Object)
	sidValue, err := receiptItem(receipt, "session_id")
	if err != nil {
		return nil, err
	}
	routeValue, err := receiptItem(receipt, "route")
	if err != nil {
		return nil, err
	}
	sid, route := pyStr(sidValue), pyStr(routeValue)
	key := LocalPolicyPrefix + sid
	var out *pyjson.Object
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		out = nil
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		old, has, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", key)
		if err != nil {
			return err
		}
		if has {
			saved, same, err := sameLocalCreate(old, receipt, request)
			if err != nil {
				return runtimeRefusal("storage_unavailable")
			}
			if !same {
				return runtimeRefusal("invalid_params")
			}
			out = saved
			return nil
		}
		retired, err := RetiredIn(ctx, conn, sid)
		if err != nil {
			return err
		}
		if retired {
			return runtimeRefusal("not_found")
		}
		_, legacy := receipt.Get("legacy_session_id")
		if !legacy {
			exists, err := connRow(ctx, conn, "SELECT 1 FROM sessions WHERE id=?", sid)
			if err != nil {
				return err
			}
			if exists != nil {
				return runtimeRefusal("storage_unavailable")
			}
		}
		routed, err := connRow(ctx, conn, "SELECT 1 FROM gateway_routing WHERE scope='' AND session_key=?", route)
		if err != nil {
			return err
		}
		if routed != nil {
			return runtimeRefusal("admission_conflict")
		}
		if legacy {
			if err := d.bindLegacyTarget(ctx, conn, receipt); err != nil {
				return err
			}
		}
		policyValue, err := receiptItem(receipt, "policy")
		if err != nil {
			return err
		}
		entryValue, err := receiptItem(receipt, "entry")
		if err != nil {
			return err
		}
		policy, ok := policyValue.(*pyjson.Object)
		if !ok {
			return &stateTypedError{"TypeError", "policy must be a dict"}
		}
		entry, ok := entryValue.(*pyjson.Object)
		if !ok {
			return &stateTypedError{"TypeError", "entry must be a dict"}
		}
		createdAt, err := receiptItem(entry, "created_at")
		if err != nil {
			return err
		}
		started, err := isoTimestamp(createdAt)
		if err != nil {
			return err
		}
		source, err := receiptItem(policy, "source")
		if err != nil {
			return err
		}
		model, err := receiptItem(policy, "model")
		if err != nil {
			return err
		}
		principal, err := receiptItem(receipt, "principal_id")
		if err != nil {
			return err
		}
		origin, err := receiptItem(entry, "origin")
		if err != nil {
			return err
		}
		chatID, err := receiptItem(origin, "chat_id")
		if err != nil {
			return err
		}
		cwd, err := rowCwd(policy)
		if err != nil {
			return err
		}
		originJSON, err := runtimeJSON(origin)
		if err != nil {
			return err
		}
		var ownProfile any
		if name := d.ownProfileName(); name != "" {
			ownProfile = name
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO sessions(id,source,created_source,user_id,session_key,chat_id,chat_type,
            model,cwd,profile_name,origin_json,started_at) VALUES(?,?,?,?,?,?,'dm',?,?,?,?,?)
            ON CONFLICT(id) DO NOTHING`, sid, source, CreatedSurface(source), principal, route, chatID, model, cwd, ownProfile, originJSON, started); err != nil {
			return err
		}
		entryJSON, err := runtimeJSON(entry)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO gateway_routing(scope,session_key,entry_json,updated_at) VALUES('',?,?,?)", route, entryJSON, started); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, encoded); err != nil {
			return err
		}
		if staged.OnCreate != nil {
			if err := staged.OnCreate.Publish(); err != nil {
				return err
			}
		}
		out = receipt
		return nil
	}, nil)
	if err != nil {
		if staged.OnCreate != nil {
			staged.OnCreate.Withdraw()
		}
		return nil, err
	}
	return out, nil
}

// Whether a stored create receipt is the exact retry of this one, the saved receipt returned
func sameLocalCreate(old any, receipt, request *pyjson.Object) (*pyjson.Object, bool, error) {
	text, ok := old.(string)
	if !ok {
		return nil, false, &stateTypedError{"TypeError", "the JSON object must be str, bytes or bytearray, not " + pyTypeName(old)}
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, false, pyValueError(err.Error())
	}
	saved, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, false, &stateTypedError{"TypeError", "saved receipt is not a dict"}
	}
	for _, k := range []string{"profile_id", "principal_id", "request_id", "session_id", "route"} {
		savedValue, err := receiptItem(saved, k)
		if err != nil {
			return nil, false, err
		}
		value, err := receiptItem(receipt, k)
		if err != nil {
			return nil, false, err
		}
		if !pyjson.Equal(savedValue, value) {
			return saved, false, nil
		}
	}
	savedPolicy, err := receiptItem(saved, "policy")
	if err != nil {
		return nil, false, err
	}
	savedPolicyObj, ok := savedPolicy.(*pyjson.Object)
	if !ok {
		return nil, false, &stateTypedError{"TypeError", "policy must be a dict"}
	}
	retryJSON, err := receiptPath(receipt, "policy", "request_json")
	if err != nil {
		return nil, false, err
	}
	retryText, ok := retryJSON.(string)
	if !ok {
		return nil, false, &stateTypedError{"TypeError", "the JSON object must be str, bytes or bytearray, not " + pyTypeName(retryJSON)}
	}
	same, err := SameCreateRetry(savedPolicyObj, retryText, request)
	if err != nil {
		return nil, false, err
	}
	return saved, same, nil
}

// Stamps ended_at on the physical target only while the logical session's FIFO and workers are idle
func (d *SessionDB) EndIdleLocalSession(ctx context.Context, epoch int64, sessionID, targetID, reason string) (int64, error) {
	var changed int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		changed = 0
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		pending, err := connRow(ctx, conn, "SELECT 1 FROM session_admissions WHERE target_session_id=? AND status IN ('queued','started','unknown')", sessionID)
		if err != nil || pending != nil {
			return err
		}
		live, err := connRow(ctx, conn, "SELECT 1 FROM worker_executions WHERE session_id IN (?,?) AND status IN ('registered','running','unknown')", sessionID, targetID)
		if err != nil || live != nil {
			return err
		}
		changed, err = d.endAndBump(ctx, conn, "UPDATE sessions SET ended_at=?, end_reason=? WHERE id=? AND ended_at IS NULL",
			[]any{now(), reason, targetID}, targetID, reason)
		return err
	}, nil)
	return changed, err
}
