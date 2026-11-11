package acp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const maxProvenanceWalk = 100

// Session row reader the provenance walk needs
type SessionGetter interface {
	GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error)
}

func provenanceRow(ctx context.Context, db SessionGetter, sessionID string) *pyjson.Object {
	row, err := db.GetSession(ctx, sessionID)
	if err != nil || row == nil || row.Len() == 0 {
		return nil
	}
	return row
}

func isCompressionEnd(row *pyjson.Object) bool {
	if row == nil {
		return false
	}
	v, _ := row.Get("end_reason")
	return v == "compression"
}

func rowString(row *pyjson.Object, key string) string {
	if row == nil {
		return ""
	}
	v, _ := row.Get(key)
	s, _ := v.(string)
	return s
}

// Session provenance of an ACP session walked from the compression chain, nil when unreadable
func BuildSessionProvenance(ctx context.Context, db SessionGetter, acpSessionID, currentID, previousID string) *pyjson.Object {
	row := provenanceRow(ctx, db, currentID)
	if row == nil {
		return nil
	}
	parentRaw, _ := row.Get("parent_session_id")
	parentID, _ := parentRaw.(string)
	rootID, depth, cursor := currentID, 0, parentID
	seen := map[string]bool{currentID: true}
	for range maxProvenanceWalk {
		if cursor == "" || seen[cursor] {
			break
		}
		seen[cursor] = true
		prow := provenanceRow(ctx, db, cursor)
		if prow == nil {
			break
		}
		rootID = cursor
		if isCompressionEnd(prow) {
			depth++
		}
		cursor = rowString(prow, "parent_session_id")
	}
	continuation := parentID != "" && isCompressionEnd(provenanceRow(ctx, db, parentID))
	prov := pyjson.NewObject()
	prov.Set("acpSessionId", acpSessionID)
	prov.Set("currentHermesSessionId", currentID)
	prov.Set("rootHermesSessionId", rootID)
	prov.Set("parentHermesSessionId", parentRaw)
	kind := "root"
	if continuation {
		kind = "continuation"
	}
	prov.Set("sessionKind", kind)
	prov.Set("compressionDepth", int64(depth))
	if previousID != "" {
		prov.Set("previousHermesSessionId", previousID)
		if previousID != currentID {
			prov.Set("reason", "compression")
			prov.Set("creatorKind", "compression")
		}
	}
	return prov
}

// Meta payload wrapping the provenance under hermes.sessionProvenance, nil when unreadable
func SessionProvenanceMeta(ctx context.Context, db SessionGetter, acpSessionID, currentID, previousID string) *pyjson.Object {
	prov := BuildSessionProvenance(ctx, db, acpSessionID, currentID, previousID)
	if prov == nil {
		return nil
	}
	inner := pyjson.NewObject()
	inner.Set("sessionProvenance", prov)
	meta := pyjson.NewObject()
	meta.Set("hermes", inner)
	return meta
}
