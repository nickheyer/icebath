package dashboard

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Spawns a background CLI action under a profile, a spawn failure logged and answered 500 with the prefix
func spawnProfileAction(ctx context.Context, profile string, argv []string, name, logMsg, prefix string) (*pyjson.Object, error) {
	proc, err := func() (actionProc, error) {
		args, err := profileCLIArgs(profile)
		if err != nil {
			return nil, err
		}
		return spawnHermesAction(ctx, append(args, argv...), name, nil)
	}()
	if err != nil {
		return nil, httpFailure(err, logMsg, 500, prefix, nil)
	}
	return pyjson.ObjectOf("ok", true, "pid", proc.Pid(), "name", name), nil
}
