package pm

import "context"

// Relative binary path of a binary package for a target, empty when the package ships none there
func (b *BinaryPackage) Rel(target string) string { return b.rel(target) }

// Interpreter the tool store of a tree records for its python entry, empty when none is recorded or present
func ResolveStorePython(ctx context.Context, root string) (string, error) {
	store, err := EnvironmentStoreRoot(ctx, root, true)
	if err != nil {
		return "", err
	}
	return storePython(store)
}
