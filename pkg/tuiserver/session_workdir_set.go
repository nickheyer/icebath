package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Pins an explicit user chosen cwd on the session, persisting it and recycling the session's terminal sandbox
func setSessionCwd(ctx context.Context, session *tuigateway.Session, cwd string) (string, error) {
	resolved, err := workspaceCwd(ctx, strOr(session.Get("profile_home")), home.TranslateCwdForWSLBackend(cwd))
	if err != nil {
		return "", err
	}
	session.Set("cwd", resolved)
	session.Set("explicit_cwd", true)
	session.Set("cwd_pinned", true)
	session.Set("cwd_from_settle", false)
	registerSessionCwd(ctx, session)
	persistSessionCwdAndScheduleGitMeta(ctx, session, resolved, nil)
	suppress(func() { tools.CleanupVM(configEnv(ctx), strOr(session.Get("session_key")), false) })
	return resolved, nil
}
