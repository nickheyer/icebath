package pm

import "context"

// Replaces the venv sync the extras installer runs and returns the previous one
func SwapClientSyncVenv(fn func(ctx context.Context, opts SyncVenvOptions) error) func(ctx context.Context, opts SyncVenvOptions) error {
	saved := clientSyncVenv
	clientSyncVenv = fn
	return saved
}
