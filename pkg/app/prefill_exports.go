package app

import "context"

// Prefill file path from the session env, then the top level key, then the legacy agent key
func ResolvePrefillMessagesFile(ctx context.Context, cfg map[string]any) string {
	return resolvePrefillMessagesFile(ctx, cfg)
}

// Prefill messages of a JSON array file, relative paths under baseDir or the home, missing or invalid giving none
func LoadPrefillMessages(ctx context.Context, filePath, baseDir string) []any {
	return loadPrefillMessages(ctx, filePath, baseDir)
}
