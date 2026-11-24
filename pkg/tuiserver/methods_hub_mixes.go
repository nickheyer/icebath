package tuiserver

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var hubMixesRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubMixesRegistry, tuigateway.MixesList.Name(), 5192, rpcShape{}, mixesList)
	scopedRPC(hubMixesRegistry, tuigateway.MixesChange.Name(), 5192, rpcShape{}, mixesChange)
	hubMixesRegistry.install()
}

// One model of a stored mix as the screen shows it
func mixSlot(slot provider.MoASlot) tuigateway.MixSlot {
	return tuigateway.MixSlot{Provider: slot.Provider, Model: slot.Model, ReasoningEffort: slot.ReasoningEffort, Off: !slot.Enabled}
}

// One model the screen sent, as the mix stores it
func mixStored(slot tuigateway.MixSlot) provider.MoASlot {
	return provider.MoASlot{Provider: slot.Provider, Model: slot.Model, ReasoningEffort: slot.ReasoningEffort, Enabled: !slot.Off}
}

// Every saved mix of the profile as the model mixes screen shows it
func mixListing(ctx context.Context) (tuigateway.MixesListResult, error) {
	presets, err := provider.MoAPresets(ctx)
	if err != nil {
		return tuigateway.MixesListResult{}, err
	}
	out := tuigateway.MixesListResult{Mixes: make([]tuigateway.MixRow, 0, len(presets))}
	for _, preset := range presets {
		row := tuigateway.MixRow{Name: preset.Name, Default: preset.Default, Active: preset.Active, Acting: mixSlot(preset.Aggregator), Notice: preset.Notice,
			Advisers: make([]tuigateway.MixSlot, 0, len(preset.References))}
		row.Acting.Off = false
		for _, slot := range preset.References {
			row.Advisers = append(row.Advisers, mixSlot(slot))
		}
		out.Mixes = append(out.Mixes, row)
	}
	return out, nil
}

// Saved mixes of the profile for the model mixes screen
func mixesList(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	if _, err := tuigateway.MixesList.Params(raw); err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	out, err := mixListing(ctx)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// One change to the saved mixes, answered with the mixes after it
func mixesChange(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.MixesChange.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	switch p.Verb {
	case "save":
		advisers := make([]provider.MoASlot, 0, len(p.Advisers))
		for _, slot := range p.Advisers {
			advisers = append(advisers, mixStored(slot))
		}
		var acting provider.MoASlot
		if p.Acting != nil {
			acting = mixStored(*p.Acting)
		}
		err = provider.SaveMoAPreset(ctx, p.Name, advisers, acting)
	case "default":
		err = provider.SetDefaultMoAPreset(ctx, p.Name)
	case "remove":
		err = provider.DeleteMoAPreset(ctx, p.Name)
	default:
		return rpcErr(rid, 4192, "unknown mix change: "+p.Verb, nil), nil
	}
	var refused *provider.MoAExitError
	switch {
	case errors.As(err, &refused):
		return rpcErr(rid, 4192, refused.Message, nil), nil
	case err != nil:
		return nil, err
	}
	out, err := mixListing(ctx)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}
