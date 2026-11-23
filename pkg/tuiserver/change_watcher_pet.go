//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Puts the active pet watch ahead of the session and skill watches
func init() {
	changeWatches = append([]changeWatch{{"pet.changed", 2.0, petSig, petChangedPayload}}, changeWatches...)
}

// Enabled pet with an existing sheet and its scale, nil when none renders
func activePet(ctx context.Context) (*pet.InstalledPet, float64, error) {
	enabled, selected, scale, err := petActiveSelection(ctx)
	if err != nil || !enabled || selected == nil || !selected.Exists() {
		return nil, 0, err
	}
	return selected, scale, nil
}

// Slug, spritesheet revision and scale of the active pet, the off signature when none renders
func petSig(ctx context.Context) (sig any) {
	off := []any{"off"}
	if !settings.Pet.Get(ctx) {
		return off
	}
	defer func() {
		if recover() != nil {
			sig = off
		}
	}()
	selected, scale, err := activePet(ctx)
	if err != nil || selected == nil {
		return off
	}
	return []any{selected.Slug, petSheetRevision(selected.Spritesheet), scale}
}

// is_truthy_value with an explicit default for an absent value
func isTruthyValueDefault(v any, def bool) bool {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return truthy(v)
}

// pet.info.meta shaped payload so the renderer decides whether to refetch sprites
func petChangedPayload(ctx context.Context) (payload map[string]any) {
	defer func() {
		if recover() != nil {
			payload = map[string]any{"enabled": false}
		}
	}()
	selected, scale, err := activePet(ctx)
	if err != nil || selected == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "slug": selected.Slug, "displayName": selected.DisplayName,
		"scale": scale, "spritesheetRevision": petSheetRevision(selected.Spritesheet)}
}
