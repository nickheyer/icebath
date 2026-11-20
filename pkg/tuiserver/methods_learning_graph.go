package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var learningGraphRegistry = &handlerRegistry{}

func init() {
	guardedRPC(learningGraphRegistry, "learning.graph", 5000, rpcShape{prefix: "learning.graph failed: "}, learningGraph)
	learningGraphRegistry.install()
}

// Learning graph of the profile, its learned skills and memories with their links and stats
func learningGraph(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	graph, err := memory.BuildLearningGraph(ctx, configEnv(ctx))
	if err != nil {
		return nil, err
	}
	return ok(rid, graph), nil
}
