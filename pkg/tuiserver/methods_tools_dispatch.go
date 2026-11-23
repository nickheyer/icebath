package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Registry command a typed name resolves to with its localized description and category
type ResolvedCommand struct {
	Name, Category string
	Describe       func(ctx context.Context) string
}

// Registry resolver of pkg/commands assigned at startup, false for a name no command or alias carries
var ResolveCommand func(name string) (ResolvedCommand, bool)

// Slash commands whose success republishes the session control snapshot
var sessionControlSlashes = map[string]bool{"goal": true, "heartbeat": true, "loop": true, "subgoal": true}

// One command.dispatch stage, a nil response falling through to the next stage
type dispatchStage func(ctx context.Context, rid any, params map[string]any, session *tuihost.Session, name, arg string) map[string]any

// Directive answer of command.dispatch
func directive(rid any, d tuigateway.CommandDispatchResult) map[string]any {
	return ok(rid, tuigateway.Plain(d))
}

// Display only command.dispatch result
func execOut(rid any, output string) map[string]any {
	return directive(rid, tuigateway.CommandDispatchResult{Type: "exec", Output: output})
}

func init() {
}

var toolsDispatchRegistry = &handlerRegistry{}

func init() { toolsDispatchRegistry.install() }
