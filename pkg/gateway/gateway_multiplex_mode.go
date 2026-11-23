package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Operator's explicit multiplex choice for the default profile, nil when the setting is left at its default
func ExplicitMultiplexFlag(ctx context.Context, defaultHome string) (*bool, error) {
	sctx := home.WithOverride(ctx, defaultHome)
	v, err := settings.Resolve(sctx, settings.GatewayMultiplexProfiles.Key)
	if err != nil || v.Source == settings.SourceDefault {
		return nil, err
	}
	on := settings.GatewayMultiplexProfiles.Get(sctx)
	return &on, nil
}

// Whether the default profile's gateway serves every profile, the live record first then any explicit flag
func DefaultGatewayMultiplexes(ctx context.Context, defaultHome string) (bool, error) {
	if defaultHome == "" {
		defaultHome = home.Root()
	}
	recorded, known, err := RecordedServedProfiles(defaultHome)
	if err != nil {
		return false, err
	}
	if known {
		return len(recorded) > 0, nil
	}
	flag, err := ExplicitMultiplexFlag(ctx, defaultHome)
	if err != nil {
		return false, err
	}
	return flag != nil, nil
}

// How boot reads an explicit flag, true final and the retired false settling like unset
func SettledExplicitFlag(value *bool) *bool {
	if value != nil && *value {
		t := true
		return &t
	}
	return nil
}

// Offline boot verdict for named profile home, false for a standalone secondary and unknown when boot settles it
func HostServesNamedProfile(ctx context.Context, defaultHome, h string) (verdict, known bool, err error) {
	standalone, err := profiles.ProfileIsStandalone(ctx, h)
	if err != nil {
		return false, false, err
	}
	if standalone {
		return false, true, nil
	}
	flag, err := ExplicitMultiplexFlag(ctx, defaultHome)
	if err != nil {
		return false, false, err
	}
	if settled := SettledExplicitFlag(flag); settled != nil {
		return *settled, true, nil
	}
	return false, false, nil
}

// Guard refusal a live standalone gateway published in its runtime status, empty for history or none
func RecordedStandaloneReason(statePath string) string {
	record := ReadRuntimeStatus(statePath)
	state := recGet(record, "gateway_state")
	if state == nil || state == "stopped" || state == "startup_failed" {
		return ""
	}
	if !RuntimeStatusPidIsLive(record) {
		return ""
	}
	reason, _ := recGet(record, "multiplex_standalone_reason").(string)
	return reason
}
