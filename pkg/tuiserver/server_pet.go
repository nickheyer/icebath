//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// display.pet of the canonical config, empty on any failure
func petCfg(ctx context.Context) (out map[string]any) {
	defer func() {
		if recover() != nil {
			out = map[string]any{}
		}
	}()
	cfg, err := loadConfig(ctx)
	if err != nil {
		return map[string]any{}
	}
	display, _ := cfg["display"].(map[string]any)
	if petConf, ok := display["pet"].(map[string]any); ok {
		return petConf
	}
	return map[string]any{}
}

// Configured pet selection resolver, swapped where a case installs a fake selection
var petActiveSelection = configuredPetSelection

// Configured pet selection, whether it is enabled, the resolved pet and its scale
func configuredPetSelection(ctx context.Context) (bool, *pet.InstalledPet, float64, error) {
	conf := petCfg(ctx)
	enabled := provider.IsTruthyValue(conf["enabled"])
	var active *pet.InstalledPet
	if enabled {
		slug := ""
		if value := conf["slug"]; anyutil.IsTrue(value) {
			slug = anyutil.String(value)
		}
		resolved, err := pet.ResolveActivePet(home.Dir(ctx), slug)
		if err != nil {
			return false, nil, 0, err
		}
		active = resolved
	}
	rawScale, present := conf["scale"]
	if !present || !anyutil.IsTrue(rawScale) {
		return enabled, active, pet.DefaultScale, nil
	}
	scale, err := pyFloat(rawScale)
	if err != nil {
		return false, nil, 0, err
	}
	return enabled, active, scale, nil
}
