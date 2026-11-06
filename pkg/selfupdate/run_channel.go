package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Channel this run updates from, main for a pinned version, then the transient flag, then the install record
func SourceUpdateChannel(ctx context.Context, pinned bool, transient *string, projectRoot string) (string, error) {
	if pinned {
		return ChannelMain, nil
	}
	if transient != nil {
		return ValidateChannelName(*transient)
	}
	cfg, err := config.RequireReadableConfigBeforeWrite(ctx, home.ConfigPath(home.Dir(ctx)))
	if err != nil {
		return "", err
	}
	return ResolveUpdateChannel(cfg, rootOrRepo(projectRoot))
}
