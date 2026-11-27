package contexts

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	contextsconfig "github.com/nickheyer/icebath/internal/contexts/config"
	"github.com/nickheyer/icebath/internal/tools"
)

// Toolset of the tools context engines give the model
const Toolset = "context_engine"

// Tools of every engine kind, each offered while the profile's engine is its kind and handled by the engine of the calling session
type engineTools struct {
	host *Contexts
}

func (t engineTools) Tools() []tools.Def {
	var defs []tools.Def
	for _, kind := range t.host.Kinds() {
		for _, def := range kind.Tools {
			check, name := def.Check, kind.Name
			def.Toolset = Toolset
			def.Group = cmp.Or(def.Group, "Long sessions")
			def.Check = func(env *tools.Env) bool {
				selected, _ := t.host.Selected(env.Config)
				return selected.Name == name && env.Config.GetString(contextsconfig.Engine) != "" && (check == nil || check(env))
			}
			def.Handler = t.handle(kind)
			defs = append(defs, def)
		}
	}
	return defs
}

// Hands a call of the kind's tool to the calling session's engine
func (t engineTools) handle(kind Kind) tools.Handler {
	return func(ctx context.Context, call *tools.Call) string {
		if !kind.offers(call.Name) {
			return tools.ErrorResult(fmt.Errorf("Unknown context engine tool: %s", call.Name))
		}
		engine := t.host.Engine(call.SessionID)
		if engine == nil || t.host.kindOf(call.SessionID) != kind.Name {
			return tools.ErrorResult(errors.New("No context engine of this kind runs this session, so the tool can't be used here."))
		}
		handler, ok := engine.(Handler)
		if !ok {
			return tools.ErrorResult(fmt.Errorf("Unknown context engine tool: %s", call.Name))
		}
		return handler.Handle(ctx, call)
	}
}
