package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var compressionTipKeys = []string{"id", "ended_at", "end_reason", "message_count", "tool_call_count", "title", "last_active",
	"preview", "model", "system_prompt", "cwd", "git_branch", "git_repo_root"}

// Replaces each compression root's surfaced fields with its live tip's, keeping the root started_at and naming the chain
func (d *SessionDB) projectCompressionTips(ctx context.Context, sessions []*pyjson.Object, compactRows bool) ([]*pyjson.Object, error) {
	chainByRoot := map[string][]string{}
	var tipIDs []string
	for _, s := range sessions {
		if rowValue(s, "end_reason") != "compression" {
			continue
		}
		id := pyStr(rowValue(s, "id"))
		chain, err := d.GetCompressionChain(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(chain) > 0 && chain[len(chain)-1] != id {
			chainByRoot[id] = chain
			tipIDs = append(tipIDs, chain[len(chain)-1])
		}
	}
	tipRows := map[string]*pyjson.Object{}
	if len(chainByRoot) > 0 {
		var err error
		if tipRows, err = d.getSessionRichRowsBatch(ctx, tipIDs, compactRows); err != nil {
			return nil, err
		}
	}
	projected := make([]*pyjson.Object, 0, len(sessions))
	for _, s := range sessions {
		chain := chainByRoot[pyStr(rowValue(s, "id"))]
		var tipRow *pyjson.Object
		if chain != nil {
			tipRow = tipRows[chain[len(chain)-1]]
		}
		if tipRow == nil || tipRow.Len() == 0 {
			projected = append(projected, s)
			continue
		}
		merged := s.Copy()
		for _, key := range compressionTipKeys {
			if v, ok := tipRow.Get(key); ok {
				merged.Set(key, v)
			}
		}
		if rowValue(merged, "title") == nil {
			merged.Set("title", rowValue(s, "title"))
		}
		merged.Set("_lineage_root_id", rowValue(s, "id"))
		merged.Set("_lineage_ids", stringsToAny(chain))
		merged.Set("continuation_kind", "compression")
		projected = append(projected, merged)
	}
	return projected, nil
}
