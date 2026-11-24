package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Directive answer of command.dispatch
func directive(rid any, d tuigateway.CommandDispatchResult) map[string]any {
	return ok(rid, tuigateway.Plain(d))
}

func init() {
}

var toolsDispatchRegistry = &handlerRegistry{}

func init() { toolsDispatchRegistry.install() }
