package tuiserver

import "context"

// Path and context ref completion items for a word rooted in a folder, for the shared owner's complete.path
func PathCompletionItems(ctx context.Context, word, root, sessionKey string, local bool) ([]map[string]any, error) {
	return pathCompletionItems(ctx, word, root, sessionKey, local)
}
