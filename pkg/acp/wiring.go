package acp

import (
	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/state"
)

// Binds the gateway client wiring of the core packages, set wherever the adapter is linked
func init() {
	NewSurfaceLabelReader = func(db *state.SessionDB) RichSessionLister { return gatewaycli.NewSurfaceLabelReader(db) }
	NewPreparedMutations = func() PreparedMutations { return app.NewACPMutations() }
	SlashMutation, ModelRefusalText, ConfirmationTitle = app.ACPSlashMutation, app.ACPModelRefusalText, app.ACPConfirmationTitle
	CommandRefusal, RewindTo = app.ACPCommandRefusal, app.ACPRewindTo
	ReplayGapOwedReply, ReplayGapPendingStatus, ReplayGapSettled = app.ACPReplayGapOwedReply, app.ACPReplayGapPendingStatus, app.ACPReplayGapSettled
	EnvLaunchFlags = gatewaycli.EnvLaunchFlags
	ResolveCommand = func(name string) (string, bool) {
		if d := commands.Resolve(name); d != nil {
			return d.Name, true
		}
		return "", false
	}
}
