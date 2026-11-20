package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Receipt key and digest of a messaging chat pick on the mount the fences name
func ChatPickKey(principalID, sessionID string, params *pyjson.Object) (string, string, error) {
	requestID, err := receiptItem(params, "request_id")
	if err != nil {
		return "", "", err
	}
	for _, value := range []any{principalID, sessionID, requestID} {
		text, ok := value.(string)
		if !ok {
			return "", "", runtimeRefusal("invalid_params")
		}
		if err := runtimeText(text); err != nil {
			return "", "", err
		}
	}
	revisionValue, err := receiptItem(params, "expected_revision")
	if err != nil {
		return "", "", err
	}
	generationValue, _ := params.Get("expected_generation")
	revision, revOK := exactInt(revisionValue)
	generation, genOK := exactInt(generationValue)
	if !revOK || revision < 0 || !genOK || generation < 0 {
		return "", "", runtimeRefusal("invalid_params")
	}
	rawPayload, err := receiptItem(params, "payload")
	if err != nil {
		return "", "", err
	}
	text, err := runtimeJSON(rawPayload)
	if err != nil {
		return "", "", err
	}
	payload, err := pyjson.Loads(text)
	if err != nil {
		return "", "", pyValueError(err.Error())
	}
	operation, err := receiptItem(params, "operation")
	if err != nil {
		return "", "", err
	}
	name, ok := operation.(string)
	if !ok {
		return "", "", runtimeRefusal("invalid_params")
	}
	return MutationReceiptKey(principalID, sessionID, requestID.(string), name, payload, revision, &generation)
}

// Mount row whose revision and generation still match the fences the pick was made against
func chatPickFencedRow(ctx context.Context, conn SQLiteConn, sessionID string, params *pyjson.Object) (*pyjson.Object, error) {
	rows, err := queryObjects(ctx, conn, "SELECT * FROM sessions WHERE id=?", sessionID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, runtimeRefusal("not_found")
	}
	row := rows[0]
	revision, err := receiptItem(params, "expected_revision")
	if err != nil {
		return nil, err
	}
	if !pyjson.Equal(rowValue(row, "runtime_revision"), revision) {
		return nil, runtimeRefusal("revision_conflict")
	}
	generation, err := receiptItem(params, "expected_generation")
	if err != nil {
		return nil, err
	}
	if !pyjson.Equal(rowValue(row, "runtime_generation"), generation) {
		return nil, runtimeRefusal("stale_generation")
	}
	return row, nil
}

// Stored receipt of an exact retry under the receipt key, else the fenced mount row under the row key
func (d *SessionDB) PrepareChatPick(ctx context.Context, epoch int64, key, digest, sessionID string, params *pyjson.Object) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		replay, err := ReplayMutationReceipt(ctx, conn, key, digest)
		if err != nil {
			return err
		}
		if replay != nil {
			out.Set("receipt", replay)
			return nil
		}
		row, err := chatPickFencedRow(ctx, conn, sessionID, params)
		if err != nil {
			return err
		}
		out.Set("row", row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Bumps the mount revision and stores the result with that revision as the receipt in one fenced write
func (d *SessionDB) CommitChatPick(ctx context.Context, epoch int64, key, digest, sessionID string, params, result *pyjson.Object) (any, error) {
	var committed any
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		replay, err := ReplayMutationReceipt(ctx, conn, key, digest)
		if err != nil {
			return err
		}
		if replay != nil {
			committed = replay
			return nil
		}
		if _, err := chatPickFencedRow(ctx, conn, sessionID, params); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", sessionID); err != nil {
			return err
		}
		revision, err := requiredScalar(ctx, conn, "SELECT runtime_revision FROM sessions WHERE id=?", sessionID)
		if err != nil {
			return err
		}
		stored := result.Copy()
		stored.Set("revision", revision)
		receipt := pyjson.NewObject()
		receipt.Set("digest", digest)
		receipt.Set("result", stored)
		text, err := runtimeJSON(receipt)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, text); err != nil {
			return err
		}
		committed = stored
		return nil
	}, nil)
	return committed, err
}
