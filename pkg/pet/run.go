package pet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One pet operation with its profile home, output writer and tool environment
type Run struct {
	Ctx  context.Context
	Home string
	W    io.Writer
	Env  *tools.Env
}

func (r *Run) printf(format string, a ...any) { fmt.Fprintf(r.W, format+"\n", a...) }

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
		switch pystr.Lower(pystr.Strip(x)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyrepr.Truthy(v)
}

// Whether display.pet.enabled is truthy
func Enabled(cfg map[string]any) bool { return truthy(cfg["enabled"], false) }

// str of a config value or empty when falsy
func CfgText(cfg map[string]any, key string) string {
	if v := cfg[key]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
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
		return false, &pyrepr.FormatError{Class: "AttributeError", Msg: "'" + pyrepr.TypeName(cfg["display"]) + "' object has no attribute 'setdefault'"}
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
		return false, &pyrepr.FormatError{Class: "AttributeError", Msg: "'" + pyrepr.TypeName(petVal) + "' object has no attribute 'update'"}
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
	return Enabled(cfg) && pyrepr.Truthy(cfg["slug"]), nil
}

// Persists the clamped display.pet.scale, a message and nothing written when the value is not a number
func (r *Run) SetPetScale(value any) (float64, string, error) {
	f, err := pyint.Float(value)
	if err != nil {
		return 0, "not a number: " + pyrepr.Repr(value) + " — try a value like 0.5", nil
	}
	scale := ClampScale(f)
	if _, err := r.UpdatePetConfig(nil, "scale", scale); err != nil {
		return 0, "", err
	}
	return scale, "", nil
}

// Flips display.pet.enabled, adopting the first installed pet when none resolves
func (r *Run) ToggleDisplay() (bool, *string, *string, error) {
	cfg, err := r.PetConfig()
	if err != nil {
		return false, nil, nil, err
	}
	p, err := ResolveActivePet(r.Home, CfgText(cfg, "slug"))
	if err != nil {
		return false, nil, nil, err
	}
	if Enabled(cfg) {
		if _, err := r.UpdatePetConfig(nil, "enabled", false); err != nil {
			return false, nil, nil, err
		}
		if p != nil {
			return false, &p.DisplayName, nil, nil
		}
		return false, nil, nil, nil
	}
	if p == nil {
		installed, err := InstalledPets(r.Home)
		if err != nil {
			return false, nil, nil, err
		}
		if len(installed) == 0 {
			msg := "no pets installed — /pet list to browse, or /pet <slug> to adopt"
			return false, nil, &msg, nil
		}
		p = installed[0]
		if err := r.SetActive(p.Slug); err != nil {
			return false, nil, nil, err
		}
	} else if _, err := r.UpdatePetConfig(nil, "enabled", true); err != nil {
		return false, nil, nil, err
	}
	return true, &p.DisplayName, nil, nil
}

// Prints a slice of the public petdex gallery as the CLI and TUI text fallback
func (r *Run) PrintGallery(limit int) error {
	entries, err := FetchManifest(r.Ctx, r.Env, false)
	if err != nil {
		var me *ManifestError
		if errors.As(err, &me) {
			r.printf("◈ Couldn't reach the petdex gallery: %s", me.Msg)
			return nil
		}
		return err
	}
	installedPets, err := InstalledPets(r.Home)
	if err != nil {
		return err
	}
	installed := map[string]bool{}
	for _, p := range installedPets {
		installed[p.Slug] = true
	}
	shown := entries
	if limit > 0 && len(entries) > limit {
		shown = entries[:limit]
	}
	r.printf("❆ petdex gallery — first %d of %d:", len(shown), len(entries))
	for _, e := range shown {
		mark := "○"
		if installed[e.Slug] {
			mark = "●"
		}
		r.printf("  %s %-24s %s", mark, e.Slug, e.DisplayName)
	}
	r.printf("  /pet <slug> to adopt · /pet to toggle")
	return nil
}

// Pointed to string or None for formatting
func derefOrNone(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Runs slash pet toggle, list, scale, off or adoption by slug, every outcome said as translated text
func (r *Run) Slash(arg string, t func(string, map[string]any) string, say func(string)) error {
	low := pystr.Lower(arg)
	switch {
	case arg == "" || low == "toggle":
		enabled, name, errMsg, err := r.ToggleDisplay()
		if err != nil {
			return err
		}
		switch {
		case errMsg != nil && *errMsg != "":
			say(t("pet.error", map[string]any{"error": *errMsg}))
		case enabled:
			say(t("pet.is_out", map[string]any{"name": derefOrNone(name)}))
		case name != nil && *name != "":
			say(t("pet.put_away_named", map[string]any{"name": *name}))
		default:
			say(t("pet.put_away", nil))
		}
	case low == "list" || low == "gallery" || low == "browse" || low == "all":
		return r.PrintGallery(20)
	case low == "scale" || strings.HasPrefix(low, "scale "):
		value := pystr.Strip(pystr.Slice(arg, len("scale"), pystr.Len(arg)))
		if value == "" {
			say(t("pet.usage_scale", nil))
			return nil
		}
		scale, errMsg, err := r.SetPetScale(value)
		if err != nil {
			return err
		}
		if errMsg != "" {
			say(t("pet.error", map[string]any{"error": errMsg}))
			return nil
		}
		shown, err := pyrepr.Format(scale, "g")
		if err != nil {
			return err
		}
		say(t("pet.scale_set", map[string]any{"scale": shown}))
	case low == "off":
		if _, err := r.UpdatePetConfig(nil, "enabled", false); err != nil {
			return err
		}
		say(t("pet.put_away", nil))
	default:
		say(t("pet.fetching", map[string]any{"name": arg}))
		installed, err := InstallPet(r.Ctx, r.Env, r.Home, arg, false)
		if err != nil {
			var se *StoreError
			var me *ManifestError
			if errors.As(err, &se) || errors.As(err, &me) {
				say(t("pet.adopt_failed", map[string]any{"name": arg, "error": pyrepr.ExceptionMessage(err)}))
				return nil
			}
			return err
		}
		if err := r.SetActive(arg); err != nil {
			return err
		}
		say(t("pet.is_out", map[string]any{"name": installed.DisplayName}))
	}
	return nil
}
