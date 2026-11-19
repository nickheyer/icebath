package gateway

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Path completion listing of the TUI server, assigned from pkg/tuiserver in cmd/icebath/wiring.go
var PathCompletionItems func(ctx context.Context, word, root, sessionKey string, local bool) ([]tuigateway.CompletionItem, error)

// No rows, the answer to an empty word
func noCompletions() any {
	return tuigateway.Plain(tuigateway.CompletionItemsResult{Items: []tuigateway.CompletionItem{}})
}

// Path and context ref completions of one session, its host folder or its sandbox listed through its terminal
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
		return noCompletions(), nil
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
	local, root, sessionKey := true, "", ""
	if policy != nil {
		terminal, err := loadsPolicyMap(policy.TerminalJSON)
		if err != nil {
			return nil, err
		}
		local, root = terminalUsesHostCwd(terminal), policy.Cwd
		if !local {
			root = sandboxCompletionRoot(terminal)
		}
	}
	if !local || policy == nil {
		if sessionKey, err = physicalTarget(ctx, a, ref); err != nil {
			return nil, err
		}
	}
	items, err := forwardOffThread(ctx, func() ([]tuigateway.CompletionItem, error) {
		return PathCompletionItems(profileRuntimeScope(ctx, a.ProfileID, true), word, root, sessionKey, local)
	})
	if err != nil {
		return nil, err
	}
	return tuigateway.Plain(tuigateway.CompletionItemsResult{Items: items}), nil
}

// Folder a sandboxed session's terminal starts in, its home when the policy names none
func sandboxCompletionRoot(terminal map[string]any) string {
	switch cwd := strings.TrimSpace(routeStr(terminal["TERMINAL_CWD"])); cwd {
	case "", ".", "auto", "cwd":
		return "~"
	default:
		return cwd
	}
}
