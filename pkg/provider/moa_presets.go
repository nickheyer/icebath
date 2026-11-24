package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// MoASlot is one model of a mix
type MoASlot struct {
	Provider        string
	Model           string
	ReasoningEffort string
	// Whether a reference model takes part, always true for an aggregator
	Enabled bool
}

// MoAPreset is one saved mix: the models that advise and the one that acts
type MoAPreset struct {
	Name string
	// The mix a run uses when it names none
	Default bool
	// The mix the config has switched on
	Active     bool
	References []MoASlot
	Aggregator MoASlot
	// Billing note when the aggregator runs on another provider than the main model, empty otherwise
	Notice string
}

// Slot of a stored mapping, a reference model without an enabled key taking part
func moaSlotOf(slot map[string]any) MoASlot {
	enabled, has := slot["enabled"]
	return MoASlot{Provider: pyStripSpace(pyOrStr(slot["provider"])), Model: pyStripSpace(pyOrStr(slot["model"])),
		ReasoningEffort: pyStripSpace(pyOrStr(slot["reasoning_effort"])), Enabled: !has || config.PyTruthy(enabled)}
}

// Stored mapping of a slot, the enabled key written for a reference model only
func moaSlotMapping(slot MoASlot, reference bool) map[string]any {
	out := map[string]any{"provider": slot.Provider, "model": slot.Model}
	if slot.ReasoningEffort != "" {
		out["reasoning_effort"] = slot.ReasoningEffort
	}
	if reference {
		out["enabled"] = slot.Enabled
	}
	return out
}

// Puts the reference models and the aggregator in the named preset of the loaded MoA section and saves the config, a new preset starting from the default one and the first preset becoming the default
func moaStorePreset(ctx context.Context, cfg config.Tree, moa map[string]any, name string, refs []any, aggregator map[string]any) error {
	presets := anyMap(moa["presets"])
	current, ok := presets[name].(map[string]any)
	if !ok {
		current = anyMap(presets[pyStr(moa["default_preset"])])
	}
	updated := make(map[string]any, len(current)+2)
	for k, v := range current {
		updated[k] = v
	}
	updated["reference_models"] = refs
	updated["aggregator"] = aggregator
	presets[name] = updated
	if _, has := moa["default_preset"]; !has {
		moa["default_preset"] = name
	}
	return moaSave(ctx, cfg, moa)
}

// Takes the named preset out of the loaded MoA section and saves the config, refusing an unknown one and the only one, the default moving on and the active name cleared when they named it
func moaDeletePreset(ctx context.Context, cfg config.Tree, moa map[string]any, name string) error {
	presets := anyMap(moa["presets"])
	if _, ok := presets[name]; !ok {
		return &MoAExitError{Message: "Unknown MoA preset: " + name}
	}
	if len(presets) <= 1 {
		return &MoAExitError{Message: "Cannot delete the only MoA preset"}
	}
	delete(presets, name)
	if moa["default_preset"] == name {
		moa["default_preset"] = moaPresetOrder(presets)[0]
	}
	if moa["active_preset"] == name {
		moa["active_preset"] = ""
	}
	return moaSave(ctx, cfg, moa)
}

// Config of the home in the context with its MoA section
func moaLoad(ctx context.Context) (config.Tree, map[string]any, error) {
	h := home.Dir(ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, nil, err
	}
	return cfg, moaSection(cfg), nil
}

// MoAPresets is every saved mix of the home in the context, in stored order
func MoAPresets(ctx context.Context) ([]MoAPreset, error) {
	cfg, moa, err := moaLoad(ctx)
	if err != nil {
		return nil, err
	}
	presets := anyMap(moa["presets"])
	out := make([]MoAPreset, 0, len(presets))
	for _, name := range moaPresetOrder(presets) {
		stored := anyMap(presets[name])
		aggregator := anyMap(stored["aggregator"])
		preset := MoAPreset{Name: name, Default: moa["default_preset"] == name, Active: moa["active_preset"] == name,
			Aggregator: moaSlotOf(aggregator), Notice: moaProviderMismatchNotice(cfg, aggregator)}
		refs, _ := stored["reference_models"].([]any)
		for _, slot := range refs {
			preset.References = append(preset.References, moaSlotOf(anyMap(slot)))
		}
		out = append(out, preset)
	}
	return out, nil
}

// SaveMoAPreset stores the reference models and the aggregator of the named mix, making the mix when it is new
func SaveMoAPreset(ctx context.Context, name string, references []MoASlot, aggregator MoASlot) error {
	name = pyStripSpace(name)
	switch {
	case name == "":
		return &MoAExitError{Message: "A mix needs a name"}
	case len(references) == 0:
		return &MoAExitError{Message: "A mix needs at least one reference model"}
	case aggregator.Provider == "" || aggregator.Model == "":
		return &MoAExitError{Message: "A mix needs an aggregator model"}
	}
	cfg, moa, err := moaLoad(ctx)
	if err != nil {
		return err
	}
	refs := make([]any, 0, len(references))
	for _, slot := range references {
		if slot.Provider == "" || slot.Model == "" {
			return &MoAExitError{Message: "A reference model needs a provider and a model"}
		}
		refs = append(refs, moaSlotMapping(slot, true))
	}
	return moaStorePreset(ctx, cfg, moa, name, refs, moaSlotMapping(aggregator, false))
}

// DeleteMoAPreset removes the named mix, refusing the only one
func DeleteMoAPreset(ctx context.Context, name string) error {
	cfg, moa, err := moaLoad(ctx)
	if err != nil {
		return err
	}
	return moaDeletePreset(ctx, cfg, moa, pyStripSpace(name))
}

// SetDefaultMoAPreset makes the named mix the one a run uses when it names none
func SetDefaultMoAPreset(ctx context.Context, name string) error {
	cfg, moa, err := moaLoad(ctx)
	if err != nil {
		return err
	}
	name = pyStripSpace(name)
	if _, ok := anyMap(moa["presets"])[name]; !ok {
		return &MoAExitError{Message: "Unknown MoA preset: " + name}
	}
	moa["default_preset"] = name
	return moaSave(ctx, cfg, moa)
}
