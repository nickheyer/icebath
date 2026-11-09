package gateway

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Operator's explicit multiplex choice for the default profile, the env override first then its raw config.yaml
func ExplicitMultiplexFlag(ctx context.Context, defaultHome string) (*bool, error) {
	if env := envMultiplexProfilesOverride(); env != nil {
		return env, nil
	}
	cfgPath := filepath.Join(defaultHome, "config.yaml")
	exists, err := config.ConfigExists(ctx, cfgPath)
	if err != nil || !exists {
		return nil, err
	}
	cfg, err := config.ReadUserConfigRaw(ctx, cfgPath)
	if err != nil {
		return nil, err
	}
	section, _ := cfg["gateway"].(map[string]any)
	value, ok := cfg["multiplex_profiles"]
	if !ok || value == nil {
		value = section["multiplex_profiles"]
	}
	if value == nil {
		return nil, nil
	}
	parsed := true
	if s, isStr := value.(string); isStr {
		if v, known := pystr.BoolToken(s); known {
			parsed = v
		}
	} else {
		parsed = pyrepr.Truthy(value)
	}
	return &parsed, nil
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
