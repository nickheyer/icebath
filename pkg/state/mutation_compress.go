package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Commits a prepared summary, its physical target, generation and receipt in one transaction
func compressInTransaction(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload, prepared *pyjson.Object) ([]string, *pyjson.Object, error) {
	snapshot, err := d.ValidatePrepared(ctx, conn, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	policyValue, err := receiptPath(snapshot, "receipt", "policy")
	if err != nil {
		return nil, nil, err
	}
	target := snapshotTarget(snapshot)
	inPlace, err := receiptItem(prepared, "in_place")
	if err != nil {
		return nil, nil, err
	}
	rawMessages, err := receiptItem(prepared, "messages")
	if err != nil {
		return nil, nil, err
	}
	messages, ok := plainMessages(rawMessages)
	if !ok {
		return nil, nil, &stateTypedError{"TypeError", "prepared messages must be a list of dicts"}
	}
	var child string
	var affected []string
	if pyTruthy(inPlace) {
		child = target
		if _, err := archiveOnConnection(d, ctx, conn, target, messages, ArchiveCompactOptions{}); err != nil {
			return nil, nil, err
		}
		affected = distinctSessionIDs(sessionID, target)
	} else {
		child = NewUID()
		source, err := receiptItem(policyValue, "source")
		if err != nil {
			return nil, nil, err
		}
		model, err := receiptItem(policyValue, "model")
		if err != nil {
			return nil, nil, err
		}
		cwd, err := receiptItem(policyValue, "cwd")
		if err != nil {
			return nil, nil, err
		}
		sourceText, _ := source.(string)
		publish := CompressionChildPublish{ParentSessionID: target, ChildSessionID: child, Source: sourceText, Model: optStringOf(model),
			Messages: messages, ModelConfig: pyjson.ObjectOf("_compressed_from", target), Cwd: optStringOf(cwd), LeaseTTLSeconds: 300}
		if err := publishOnConnection(d, ctx, conn, publish); err != nil {
			return nil, nil, err
		}
		affected = []string{}
	}
	generation, err := bumpRuntimeGeneration(ctx, conn, sessionID)
	if err != nil {
		return nil, nil, err
	}
	summary, _ := prepared.Get("summary")
	return affected, pyjson.ObjectOf("target_session_id", child, "previous_target_session_id", target,
		"execution_generation", generation, "message_count", int64(len(messages)), "summary", summary), nil
}
