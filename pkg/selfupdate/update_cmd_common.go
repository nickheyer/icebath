//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"fmt"
)

// Runs a non critical update step, a failure logged at debug and swallowed, an exit or interrupt returned
func bestEffort(ctx context.Context, message string, step func() error) error {
	err := step()
	if err == nil || isUpdateExit(err) {
		return err
	}
	pkgLog().DebugContext(ctx, fmt.Sprintf(message, launchErrorText(err)))
	return nil
}
