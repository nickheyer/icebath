package kanban

import "context"

// PermissionError when a delegate_task child context would mutate the kanban database at path
func AssertNotDelegatedChildMutation(ctx context.Context, path string) error {
	return assertNotDelegatedChildMutation(ctx, path)
}
