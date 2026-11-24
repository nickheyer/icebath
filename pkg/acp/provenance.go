package acp

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const maxProvenanceWalk = 100

// Session row reader the provenance walk needs
type SessionGetter interface {
	GetSession(ctx context.Context, sessionID string) (*jsonutil.Object, error)
}

func provenanceRow(ctx context.Context, db SessionGetter, sessionID string) *jsonutil.Object {
	row, err := db.GetSession(ctx, sessionID)
	if err != nil || row == nil || row.Len() == 0 {
		return nil
	}
	return row
}

func isCompressionEnd(row *jsonutil.Object) bool {
	if row == nil {
		return false
	}
	v, _ := row.Get("end_reason")
	return v == "compression"
}

func rowString(row *jsonutil.Object, key string) string {
	if row == nil {
		return ""
	}
	v, _ := row.Get(key)
	s, _ := v.(string)
	return s
}

// Session provenance of an ACP session walked from the compression chain, nil when unreadable
func BuildSessionProvenance(ctx context.Context, db SessionGetter, acpSessionID, currentID, previousID string) *jsonutil.Object {
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
	prov := jsonutil.NewObject()
	prov.Set("acpSessionId", acpSessionID)
	prov.Set("currentIcebathSessionId", currentID)
	prov.Set("rootIcebathSessionId", rootID)
	prov.Set("parentIcebathSessionId", parentRaw)
	kind := "root"
	if continuation {
		kind = "continuation"
	}
	prov.Set("sessionKind", kind)
	prov.Set("compressionDepth", int64(depth))
	if previousID != "" {
		prov.Set("previousIcebathSessionId", previousID)
		if previousID != currentID {
			prov.Set("reason", "compression")
			prov.Set("creatorKind", "compression")
		}
	}
	return prov
}

// Meta payload wrapping the provenance under icebath.sessionProvenance, nil when unreadable
func SessionProvenanceMeta(ctx context.Context, db SessionGetter, acpSessionID, currentID, previousID string) *jsonutil.Object {
	prov := BuildSessionProvenance(ctx, db, acpSessionID, currentID, previousID)
	if prov == nil {
		return nil
	}
	inner := jsonutil.NewObject()
	inner.Set("sessionProvenance", prov)
	meta := jsonutil.NewObject()
	meta.Set("icebath", inner)
	return meta
}
