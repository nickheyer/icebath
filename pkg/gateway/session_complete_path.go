package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Path completion listing of the TUI server, assigned from pkg/tuiserver in cmd/icebath/wiring.go
var PathCompletionItems func(ctx context.Context, word, root, sessionKey string, local bool) ([]map[string]any, error)

// Path and context ref completions rooted in one session's frozen folder, nothing for a remote backend
func completePath(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	invalid := paramsOutside(params, "session_id", "profile", "word", "cwd")
	for _, key := range params.Keys {
		if _, isStr := params.GetOr(key, nil).(string); !isStr {
			invalid = true
		}
	}
	if invalid {
		return nil, runtimeRefusal("invalid_params")
	}
	word, _ := params.GetOr("word", "").(string)
	if word == "" || ref.SessionID == "" {
		return pyjson.ObjectOf("items", []any{}), nil
	}
	a := c.authority
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, runtimeRefusal("not_found")
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:read"); err != nil {
		return nil, err
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return pyjson.ObjectOf("items", []any{}), nil
	}
	terminal, err := loadsPolicyMap(policy.TerminalJSON)
	if err != nil {
		return nil, err
	}
	if !terminalUsesHostCwd(terminal) {
		return pyjson.ObjectOf("items", []any{}), nil
	}
	items, err := forwardOffThread(ctx, func() ([]map[string]any, error) {
		return PathCompletionItems(profileRuntimeScope(ctx, a.ProfileID, true), word, policy.Cwd, "", true)
	})
	if err != nil {
		return nil, err
	}
	rows := make([]any, len(items))
	for i, item := range items {
		rows[i] = item
	}
	return pyjson.ObjectOf("items", rows), nil
}
