//go:build !windows

package provider

import (
	"context"
	"os/exec"

	"github.com/nickheyer/icebath/pkg/tools"
)

// WindowsJob is the owner held containment handle, never created outside Windows
type WindowsJob struct{}

// Releases nothing because POSIX spawns carry no job object
func (*WindowsJob) Close() error { return nil }

var spawnOwnedFn = tools.SpawnOwned

// Starts cmd, an owned router leads its own group handed to the owner death supervisor
func SpawnServer(ctx context.Context, cmd *exec.Cmd, owned bool) (*WindowsJob, error) {
	if !owned {
		return nil, cmd.Start()
	}
	return nil, spawnOwnedFn(ctx, cmd, "llama-server")
}
