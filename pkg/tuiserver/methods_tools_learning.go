package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/memory"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

var toolsLearningRegistry = &handlerRegistry{}

func init() {
	learningMutation("detail", []string{"id"}, func(ctx context.Context, args []string) (map[string]any, error) {
		return memory.NodeDetail(ctx, configEnv(ctx), args[0])
	})
	learningMutation("delete", []string{"id"}, func(ctx context.Context, args []string) (map[string]any, error) {
		return memory.DeleteNode(ctx, configEnv(ctx), args[0])
	})
	learningMutation("edit", []string{"id", "content"}, func(ctx context.Context, args []string) (map[string]any, error) {
		return memory.EditNode(ctx, configEnv(ctx), args[0], args[1])
	})
	toolsLearningRegistry.install()
}

// Registers a learning node mutation fed the str of each named param, failures answered 5000 with its label
func learningMutation(name string, argKeys []string, fn func(ctx context.Context, args []string) (map[string]any, error)) {
	guardedRPC(toolsLearningRegistry, "learning."+name, 5000, rpcShape{prefix: "learning." + name + " failed: "},
		func(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
			args := make([]string, len(argKeys))
			for i, k := range argKeys {
				raw, present := params[k]
				if !present {
					raw = ""
				}
				args[i] = anyutil.String(raw)
			}
			out, err := fn(ctx, args)
			if err != nil {
				return nil, err
			}
			return ok(rid, out), nil
		})
}
