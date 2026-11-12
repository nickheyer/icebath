package acp

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Blocks external HTTP, installs the wiring hooks, keeps the model inventory offline and isolates the home for the suite
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
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
	RunModelSetup = func(context.Context) int { return 0 }
	loadPickerContext = func(context.Context) (provider.PickerConfigContext, error) {
		return provider.PickerConfigContext{}, nil
	}
	buildModelsPayload = func(context.Context, provider.PickerConfigContext, provider.ModelsPayloadOptions) (map[string]any, error) {
		return map[string]any{"providers": []any{}}, nil
	}
	dir, err := os.MkdirTemp("", "acp-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("ICEBATH_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
