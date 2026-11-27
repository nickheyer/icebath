package prompt

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/git"
)

// Git here only looks at a project, which may be anyone's: it runs in the environments of internal/git, which never prompt, page, open an
// editor, run hooks or filter drivers, or read the user's config beyond the directories it says are safe

const gitTimeout = 2500 * time.Millisecond

// Output of a short git run that succeeded, trimmed, empty for any failure
func gitProbe(ctx context.Context, env map[string]string, args ...string) string {
	env = maps.Clone(env)
	env["GIT_NO_LAZY_FETCH"] = "1"
	result, err := git.Run(ctx, "", env, gitTimeout, nil, args...)
	if err != nil || result.Code != 0 {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}
