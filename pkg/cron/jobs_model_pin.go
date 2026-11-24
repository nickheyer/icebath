package cron

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Config backend probe and the runtime provider resolver the pin goes through, swapped by tests
var (
	pinConfigExists    = config.ConfigExists
	pinRuntimeProvider = resolveMainRuntimeProvider
)

// Pin refusal while the config backend cannot serve the profile, unwrapping to the outage
type PinRefusedError struct {
	Code  string
	cause error
}

func (e *PinRefusedError) Error() string { return "cannot pin the job to the main model: " + e.Code }
func (e *PinRefusedError) Unwrap() error { return e.cause }

// Provider the main agent resolves to with no explicit request, any failure or panic yielding empty
func resolveMainRuntimeProvider(s *Store) (name string) {
	defer func() {
		if recover() != nil {
			name = ""
		}
	}()
	rt, err := provider.ResolveRuntimeProvider(context.Background(), s.home, "", "", "", "")
	if err != nil || rt == nil {
		return ""
	}
	return rt.Provider
}

// Main agent provider and model a pinned job locks onto, both nil when no model is configured, refused while the config backend cannot serve the profile
func mainModelPin(s *Store) (*string, *string, error) {
	if _, err := pinConfigExists(context.Background(), home.ConfigPath(s.home)); err != nil {
		var unavailable *config.ConfigBackendUnavailable
		if errors.As(err, &unavailable) {
			return nil, nil, &PinRefusedError{Code: unavailable.SystemExitCode(), cause: err}
		}
		return nil, nil, err
	}
	model := normalizeOptionalText(settings.Model.Get(s.settingsCtx()), false)
	if model == nil {
		return nil, nil, nil
	}
	prov := normalizeOptionalText(pinRuntimeProvider(s), false)
	if prov != nil {
		lowered := strings.ToLower(*prov)
		prov = &lowered
	}
	return prov, model, nil
}
