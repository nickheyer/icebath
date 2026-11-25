package provider

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
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
	return MoASlot{Provider: strings.TrimSpace(anyutil.StringOr(slot["provider"], "")), Model: strings.TrimSpace(anyutil.StringOr(slot["model"], "")),
		ReasoningEffort: strings.TrimSpace(anyutil.StringOr(slot["reasoning_effort"], "")), Enabled: !has || anyutil.IsTrue(enabled)}
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

// Presets of the profile of ctx as written, the built-in ones while the person stores none, so a change keeps the list the person sees
func moaWritablePresets(ctx context.Context) map[string]any {
	if stored, ok := settings.Stored(ctx, settings.MoaPresets.Key); ok {
		if presets := config.AsMap(stored); presets != nil {
			return presets
		}
	}
	return config.AsMap(settings.MoaPresets.DefaultValue())
}

// Puts the reference models and the aggregator in the named preset through the settings writer, a new preset starting from the default one; while only the built-in presets apply they are written with it so they stay listed
func moaStorePreset(ctx context.Context, name string, refs []any, aggregator map[string]any) error {
	presets := moaWritablePresets(ctx)
	current, ok := presets[name].(map[string]any)
	if !ok {
		current = anyMap(presets[anyutil.String(moaView(ctx)["default_preset"])])
	}
	updated := make(map[string]any, len(current)+2)
	for k, v := range current {
		updated[k] = v
	}
	updated["reference_models"] = refs
	updated["aggregator"] = aggregator
	if settings.MoaPresets.Source(ctx) != settings.SourceDefault {
		_, err := settings.MoaPresets.SetEntry(ctx, name, updated)
		return err
	}
	if !ok {
		config.AppendMappingKey(presets, name)
	}
	presets[name] = updated
	_, err := settings.Set(ctx, settings.MoaPresets.Key, presets, settings.Global)
	return err
}

// Takes the named preset out of the stored presets through the settings writer, refusing an unknown one and the only one, the default moving on and the active name cleared when they named it
func moaDeletePreset(ctx context.Context, name string) error {
	moa := moaView(ctx)
	presets := anyMap(moa["presets"])
	if _, ok := presets[name]; !ok {
		return &MoAExitError{Message: "Unknown MoA preset: " + name}
	}
	if len(presets) <= 1 {
		return &MoAExitError{Message: "Cannot delete the only MoA preset"}
	}
	if _, err := settings.MoaPresets.DeleteEntry(ctx, name); err != nil {
		return err
	}
	delete(presets, name)
	var changes []config.KeyValue
	if moa["default_preset"] == name {
		changes = append(changes, config.KeyValue{Key: settings.MoaDefaultPreset.Key, Value: moaPresetOrder(presets)[0]})
	}
	if moa["active_preset"] == name {
		changes = append(changes, config.KeyValue{Key: settings.MoaActivePreset.Key})
	}
	return setProfileSettings(ctx, changes...)
}

// MoAPresets is every saved mix of the home in the context, in stored order
func MoAPresets(ctx context.Context) ([]MoAPreset, error) {
	moa := moaView(ctx)
	presets := anyMap(moa["presets"])
	out := make([]MoAPreset, 0, len(presets))
	for _, name := range moaPresetOrder(presets) {
		stored := anyMap(presets[name])
		aggregator := anyMap(stored["aggregator"])
		preset := MoAPreset{Name: name, Default: moa["default_preset"] == name, Active: moa["active_preset"] == name,
			Aggregator: moaSlotOf(aggregator), Notice: moaProviderMismatchNotice(ctx, aggregator)}
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
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return &MoAExitError{Message: "A mix needs a name"}
	case len(references) == 0:
		return &MoAExitError{Message: "A mix needs at least one reference model"}
	case aggregator.Provider == "" || aggregator.Model == "":
		return &MoAExitError{Message: "A mix needs an aggregator model"}
	}
	refs := make([]any, 0, len(references))
	for _, slot := range references {
		if slot.Provider == "" || slot.Model == "" {
			return &MoAExitError{Message: "A reference model needs a provider and a model"}
		}
		refs = append(refs, moaSlotMapping(slot, true))
	}
	return moaStorePreset(ctx, name, refs, moaSlotMapping(aggregator, false))
}

// DeleteMoAPreset removes the named mix, refusing the only one
func DeleteMoAPreset(ctx context.Context, name string) error {
	return moaDeletePreset(ctx, strings.TrimSpace(name))
}

// SetDefaultMoAPreset makes the named mix the one a run uses when it names none
func SetDefaultMoAPreset(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if _, ok := anyMap(moaView(ctx)["presets"])[name]; !ok {
		return &MoAExitError{Message: "Unknown MoA preset: " + name}
	}
	_, err := settings.Set(ctx, settings.MoaDefaultPreset.Key, name, settings.Global)
	return err
}
