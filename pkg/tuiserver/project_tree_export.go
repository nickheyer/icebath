package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// BuildProjectTree is the project tree of a session store with its active project id, the dashboard fan out builder
func BuildProjectTree(ctx context.Context, db *state.SessionDB, previewLimit, sessionLimit int, hydrate, includeDiscovered bool) (map[string]any, any, error) {
	return buildProjectTree(ctx, db, projectTreeBuild{previewLimit: previewLimit, hydrate: hydrate,
		sessionLimit: sessionLimit, includeDiscovered: includeDiscovered})
}

// TreePathKey is the separator and trailing slash agnostic folder identity of a path
func TreePathKey(path string) string { return treePathKey(path) }
