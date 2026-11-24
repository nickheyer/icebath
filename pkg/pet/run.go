package pet

import (
	"context"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// One pet operation with its profile home, output writer and tool environment
type Run struct {
	Ctx  context.Context
	Home string
	W    io.Writer
	Env  *tools.Env
}

// display.pet config dict, empty when absent or not a dict
func (r *Run) PetConfig() (map[string]any, error) {
	cfg, err := config.LoadContext(r.Ctx, r.Home, nil)
	if err != nil {
		return nil, err
	}
	display, _ := cfg["display"].(map[string]any)
	petCfg, _ := display["pet"].(map[string]any)
	if petCfg == nil {
		petCfg = map[string]any{}
	}
	return petCfg, nil
}

// Bool coercion over the shared truthy strings, the default for None
func truthy(v any, def bool) bool {
	switch x := v.(type) {
	case nil:
		return def
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return anyutil.IsTrue(v)
}

// Whether display.pet.enabled is truthy
func Enabled(cfg map[string]any) bool { return truthy(cfg["enabled"], false) }

// str of a config value or empty when falsy
func CfgText(cfg map[string]any, key string) string {
	if v := cfg[key]; anyutil.IsTrue(v) {
		return anyutil.String(v)
	}
	return ""
}

// Writes display.pet keys and saves config, only when the configured slug matches whenSlug if given
func (r *Run) UpdatePetConfig(whenSlug *string, values ...any) (bool, error) {
	cfg, err := config.LoadContext(r.Ctx, r.Home, nil)
	if err != nil {
		return false, err
	}
	display, ok := cfg["display"].(map[string]any)
	if _, present := cfg["display"]; !present {
		display, ok = map[string]any{}, true
		cfg["display"] = display
	}
	if !ok {
		return false, &pyrepr.FormatError{Class: "AttributeError", Msg: "'" + anyutil.Kind(cfg["display"]) + "' object has no attribute 'setdefault'"}
	}
	petVal, present := display["pet"]
	if !present {
		petVal = map[string]any{}
		display["pet"] = petVal
	}
	petCfg, isDict := petVal.(map[string]any)
	if whenSlug != nil && (!isDict || CfgText(petCfg, "slug") != *whenSlug) {
		return false, nil
	}
	if !isDict {
		return false, &pyrepr.FormatError{Class: "AttributeError", Msg: "'" + anyutil.Kind(petVal) + "' object has no attribute 'update'"}
	}
	for i := 0; i+1 < len(values); i += 2 {
		petCfg[values[i].(string)] = values[i+1]
	}
	if err := config.SaveConfig(r.Ctx, r.Home, cfg, config.SaveOptions{}); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Run) SetActive(slug string) error {
	_, err := r.UpdatePetConfig(nil, "slug", slug, "enabled", true)
	return err
}

// Whether a pet is enabled and a slug is configured
func (r *Run) HasActivePet() (bool, error) {
	cfg, err := r.PetConfig()
	if err != nil {
		return false, err
	}
	return Enabled(cfg) && anyutil.IsTrue(cfg["slug"]), nil
}

// Persists the clamped display.pet.scale, a message and nothing written when the value is not a number
func (r *Run) SetPetScale(value any) (float64, string, error) {
	f, err := anyutil.ToFloat(value)
	if err != nil {
		return 0, "not a number: " + anyutil.Quote(value) + " — try a value like 0.5", nil
	}
	scale := ClampScale(f)
	if _, err := r.UpdatePetConfig(nil, "scale", scale); err != nil {
		return 0, "", err
	}
	return scale, "", nil
}
