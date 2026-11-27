package launch

import (
	"context"
	"os"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/launch"
)

// Snapshots on for every session of a launch, whatever the profile's settings say
type flags struct{}

func Register(registry *launch.Registry) {
	registry.Register(flags{})
}

func (flags) Flags() []launch.Flag {
	return []launch.Flag{{Names: []string{"--checkpoints"}, Help: "Keep checkpoints of files before risky changes, restored with /rollback"}}
}

// Turns snapshots on through the variable File checkpoints follows, for the sessions this process serves
func (flags) Around(ctx context.Context, _ *connect.Client, given launch.Given, run launch.Run) error {
	if given.Has("--checkpoints") {
		os.Setenv("ICEBATH_TUI_CHECKPOINTS", "1")
	}
	return run(ctx)
}
