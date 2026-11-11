package plugins

import "context"

// Records consent for a hook pair in the allowlist of the home in ctx with the script mtime
func RecordShellHookApproval(ctx context.Context, event, command string) error {
	return recordShellHookApproval(ctx, event, command)
}
