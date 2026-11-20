package provider

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Session bound env names stripped from a delivery child, the canonical session context surface
func deliveryChildSessionEnvNames() []string {
	return append([]string(nil), tools.SessionContextEnvVars...)
}

// Env of one delivery turn child built from the target profile env, dispatcher author and session identity dropped
func DeliveryEnv(ctx context.Context, env *tools.Env, author *pyjson.Object, profileHome string) (map[string]string, error) {
	target := profileHome
	overridden := home.Override(ctx) != "" || (env != nil && env.Home != "" && env.Home != home.Process())
	if target == "" && IsMultiplexActive(ctx) && !overridden && boundScope(ctx) == nil {
		target = RoutingProcessHome()
	}
	base := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			base[k] = v
		}
	}
	child, err := tools.ServedProfileChildEnv(ctx, env, base, target, true)
	if err != nil {
		return nil, err
	}
	delete(child, TurnAuthorEnvName)
	for _, name := range deliveryChildSessionEnvNames() {
		delete(child, name)
	}
	if author != nil && author.Len() > 0 {
		entry, err := TurnAuthorEnv(author)
		if err != nil {
			return nil, err
		}
		for k, v := range entry {
			child[k] = v
		}
	}
	return child, nil
}
