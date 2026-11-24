package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/memory"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

var toolsConfigRegistry = &handlerRegistry{}

func init() {
	guardedRPC(toolsConfigRegistry, "learning.frames", 5000, rpcShape{prefix: "learning.frames failed: "}, learningFrames)
	toolsConfigRegistry.install()
}

// Pre rendered journey timeline frames with their legend and summary metadata
func learningFrames(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	sizes := []int64{80, 24, 48}
	for i, key := range []string{"cols", "rows", "frames"} {
		raw, present := params[key]
		if !present || !anyutil.IsTrue(raw) {
			continue
		}
		n, err := anyutil.ToInt(raw)
		if err != nil {
			sizes = []int64{80, 24, 48}
			break
		}
		sizes[i] = n
	}
	graph, err := memory.BuildLearningGraph(ctx, configEnv(ctx))
	if err != nil {
		return nil, err
	}
	rendered, err := memory.RenderFrames(graph, int(max(20, sizes[0])), int(max(10, sizes[1])), int(sizes[2]))
	if err != nil {
		return nil, err
	}
	return ok(rid, rendered), nil
}
