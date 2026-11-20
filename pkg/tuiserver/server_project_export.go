package tuiserver

import "context"

// Project of a working directory with its id, slug, name and primary path, nil when none
func ProjectInfoForCwd(ctx context.Context, cwd string) any { return projectInfoForCwd(ctx, cwd) }
