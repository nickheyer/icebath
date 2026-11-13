package provider

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Bot author of a relayed DM's recipient turn from the reported sender fields, nil when no sender profile is named
func DeliveryTurnAuthor(fromProfile, fromHandle, fromConnection any) map[string]any {
	profile := pystr.Strip(relayText(fromProfile))
	if profile == "" {
		return nil
	}
	name := pystr.Strip(relayText(fromHandle))
	if name == "" {
		name = profile
	}
	return map[string]any{"id": BotAuthorID(profile, relayText(fromConnection)), "name": name, "is_bot": true}
}

// Bot author of a relayed DM whose sender fields came from a logged in client, built from its minted principal
func RelayingPrincipalAuthor(principal string) map[string]any {
	return map[string]any{"id": BotAuthorID("relay", pystr.Strip(principal)), "name": "relayed teammate", "is_bot": true}
}

// Python str of a value or empty string for a falsy one
func relayText(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

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
