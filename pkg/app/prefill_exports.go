package app

import "context"

// Prefill file path for ctx: the session's client env, then the prefill messages setting
func ResolvePrefillMessagesFile(ctx context.Context) string {
	return resolvePrefillMessagesFile(ctx)
}

// Prefill messages of a JSON array file, relative paths under baseDir or the home, missing or invalid giving none
func LoadPrefillMessages(ctx context.Context, filePath, baseDir string) []any {
	return loadPrefillMessages(ctx, filePath, baseDir)
}
