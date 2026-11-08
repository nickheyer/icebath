package agent

import (
	"context"
	"weak"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Binds the agent as the weakly held host parent of the turn so context snapshots never pin it
func bindSubagentParent(ctx context.Context, a *AIAgent) context.Context {
	ref := weak.Make(a)
	return plugins.BindSubagentParentRef(ctx, func() any {
		if p := ref.Value(); p != nil {
			return p
		}
		return nil
	})
}
