//go:build !icebath_core && !icebath_noextras

package manage

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() { stopBotDesktop = stopProfileBotDesktop }

// Stops the profile's Bot Desktop under its own home before the directory moves or goes, logging failures
func stopProfileBotDesktop(ctx context.Context, profileDir string) {
	if !botDesktopSupportedHost() {
		return
	}
	pctx := home.WithOverride(ctx, profileDir)
	stopped, err := botDesktopStop(pctx)
	if err == nil && stopped {
		fmt.Fprintln(stdout, "✓ Bot Desktop stopped")
		err = botDesktopLeaseRelease(pctx)
	}
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Could not stop the Bot Desktop of %s: %v", profileDir, err))
	}
}

func botDesktopSupportedHost() bool { return botdesktop.IsSupportedHost() }

// Tool env of the profile home the context overrides, with that profile's read only config
func botDesktopEnv(ctx context.Context) (*tools.Env, error) {
	h := home.Dir(ctx)
	cfg, err := config.LoadReadonly(ctx, h, nil)
	if err != nil {
		return nil, err
	}
	return &tools.Env{Home: h, Config: cfg}, nil
}

func botDesktopStop(ctx context.Context) (bool, error) {
	env, err := botDesktopEnv(ctx)
	if err != nil {
		return false, err
	}
	return botdesktop.Stop(env, nil)
}

func botDesktopLeaseRelease(ctx context.Context) error {
	_, err := botdesktop.LeaseRelease(botdesktop.LeaseReleaseOptions{ProfileKey: home.Dir(ctx)})
	return err
}
