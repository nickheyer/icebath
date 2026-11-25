package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Whether db holds an active loop row or one that cannot be parsed, read errors propagating
func StoreHasActiveLoop(ctx context.Context, db *state.SessionDB) (bool, error) {
	entries, err := db.ListMetaPrefix(ctx, loopMetaPrefix)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		var st *SessionLoopState
		if anyutil.IsTrue(e.Value) {
			st = parseLoopState(e.Value, e.Key[len(loopMetaPrefix):])
		}
		if st == nil || st.Status == "active" {
			return true, nil
		}
	}
	return false, nil
}
