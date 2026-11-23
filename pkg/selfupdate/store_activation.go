//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Puts the tools of this install's store first on PATH for the process, the startup step of every command and of the background service
func ActivateRuntime(ctx context.Context) error { return pm.ActivatePath(ctx) }
