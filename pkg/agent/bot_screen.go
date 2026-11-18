//go:build !icebath_core && !icebath_noextras

package agent

import (
	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the bot desktop published env and profile lease reads behind the Bot Screen line
func init() {
	botScreenPublishedEnvFn = botdesktop.PublishedEnv
	botScreenLeaseHolderFn = func(env *tools.Env) string { return botdesktop.LeaseGet(botdesktop.ProfileKey(env)).Holder }
}
