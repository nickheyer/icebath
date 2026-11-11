package tuiserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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
	guardedRPC(toolsLearningRegistry, "skills.reload", 5025, rpcShape{}, skillsReload)
	toolsLearningRegistry.install()
}

// Registers a learning node mutation fed the str of each named param, failures answered 5000 with its label
func learningMutation(name string, argKeys []string, fn func(ctx context.Context, args []string) (map[string]any, error)) {
	guardedRPC(toolsLearningRegistry, "learning."+name, 5000, rpcShape{prefix: "learning." + name + " failed: "},
		func(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
			args := make([]string, len(argKeys))
			for i, k := range argKeys {
				raw, present := params[k]
				if !present {
					raw = ""
				}
				args[i] = pyrepr.Str(raw)
			}
			out, err := fn(ctx, args)
			if err != nil {
				return nil, err
			}
			return ok(rid, out), nil
		})
}

// Rescans skills in the calling session's profile and workspace and reports what was added and removed
func skillsReload(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	cwd, err := completionCwd(ctx, params)
	if err != nil {
		return nil, err
	}
	scoped, err := sessionHomeScope(tuigateway.SessionRecord(strOr(params["session_id"])), cwd, strOr(params["profile"]))(ctx)
	if err != nil {
		return nil, err
	}
	scope, err := boundSkillScope(scoped)
	if err != nil {
		return nil, err
	}
	result, err := scope.ReloadSkills(scoped)
	if err != nil {
		return nil, err
	}
	rows := func(key string) []map[string]any {
		items, _ := result[key].([]map[string]any)
		return items
	}
	added, removed := rows("added"), rows("removed")
	lines := []string{"Reloading skills..."}
	if len(added) == 0 && len(removed) == 0 {
		lines = append(lines, "No new skills detected.")
	}
	for _, group := range []struct {
		label string
		items []map[string]any
	}{{"Added skills:", added}, {"Removed skills:", removed}} {
		if len(group.items) == 0 {
			continue
		}
		lines = append(lines, group.label)
		for _, item := range group.items {
			name, present := item["name"]
			if !present {
				name = ""
			}
			lines = append(lines, "  - "+pyrepr.Str(name))
		}
	}
	total := int64(0)
	if truthy(result["total"]) {
		n, err := pyint.Int64(result["total"])
		if err != nil {
			return nil, err
		}
		total = n
	}
	lines = append(lines, fmt.Sprintf("%d skill(s) available", total))
	return ok(rid, map[string]any{"output": strings.Join(lines, "\n"), "result": result}), nil
}
