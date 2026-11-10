package cron

import (
	"context"
	"errors"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Config reads and the runtime provider resolver the pin goes through, swapped by tests
var (
	pinConfigExists      = config.ConfigExists
	pinLoadUserEffective = config.LoadUserEffective
	pinRuntimeProvider   = resolveMainRuntimeProvider
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

// Main agent provider and model a pinned job locks onto, both nil when no model is configured
func mainModelPin(s *Store) (*string, *string, error) {
	cfgPath := home.ConfigPath(s.home)
	cfg := map[string]any{}
	exists, err := pinConfigExists(context.Background(), cfgPath)
	if err == nil && exists {
		cfg, err = pinLoadUserEffective(cfgPath, os.LookupEnv)
	}
	if err != nil {
		var unavailable *config.ConfigBackendUnavailable
		if errors.As(err, &unavailable) {
			return nil, nil, &PinRefusedError{Code: unavailable.SystemExitCode(), cause: err}
		}
		return nil, nil, err
	}
	modelCfg := cfg["model"]
	if !pyrepr.Truthy(modelCfg) {
		modelCfg = map[string]any{}
	}
	if m, ok := modelCfg.(map[string]any); ok {
		modelCfg = m["default"]
		if !pyrepr.Truthy(modelCfg) {
			modelCfg = m["model"]
		}
	}
	model := normalizeOptionalText(modelCfg, false)
	if model == nil {
		return nil, nil, nil
	}
	prov := normalizeOptionalText(pinRuntimeProvider(s), false)
	if prov != nil {
		lowered := pystr.Lower(*prov)
		prov = &lowered
	}
	return prov, model, nil
}
