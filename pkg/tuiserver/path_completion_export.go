package tuiserver

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Path and context ref rows for a word rooted in a folder, an empty root meaning the terminal workspace
func PathCompletionItems(ctx context.Context, word, root, sessionKey string, local bool) ([]tuigateway.CompletionItem, error) {
	if root == "" {
		local = effectiveTerminalBackend(ctx) == "local"
		if local {
			cwd, err := completionCwd(ctx, nil)
			if err != nil {
				return nil, err
			}
			root = cwd
		} else if root = strings.TrimSpace(workdirTerminalCfg(ctx, "cwd")); root == "" || cwdPlaceholders[root] {
			root = "~"
		}
	}
	items, err := pathCompletionItems(ctx, word, root, sessionKey, local)
	if err != nil {
		return nil, err
	}
	return completionRows("complete.path", items), nil
}
