package generate

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Progress event keys of slash hatch other than row
var hatchProgressKeys = map[string]string{"compose": "hatch.progress_compose", "save": "hatch.progress_save"}

// Runs slash hatch for a concept, a base look then every animation row, and adopts the pet
func HatchCommand(ctx context.Context, env *tools.Env, home, concept string, t func(string, map[string]any) string, say func(string)) error {
	if concept == "" {
		say(t("hatch.usage", nil))
		return nil
	}
	words := pystr.SplitWhitespace(concept, -1)
	if len(words) > 3 {
		words = words[:3]
	}
	for i, w := range words {
		words[i] = pystr.Capitalize(w)
	}
	displayName := pystr.Strip(pystr.Slice(strings.Join(words, " "), 0, 28))
	if displayName == "" {
		displayName = t("hatch.default_name", nil)
	}
	slug := pet.Slugify(displayName)
	say(t("hatch.designing", map[string]any{"concept": concept}))
	drafts, err := GenerateBaseDrafts(ctx, env, concept, DraftOptions{N: 1})
	if err != nil {
		var ge *GenerationError
		if !errors.As(err, &ge) {
			return err
		}
		say(t("hatch.base_failed", map[string]any{"error": ge.Error()}))
		return nil
	}
	if len(drafts) == 0 {
		say(t("hatch.no_base_draft", nil))
		return nil
	}
	progress := func(event, detail string) {
		if event == "row" {
			state, _, _ := strings.Cut(detail, ":")
			say("  " + t("hatch.progress_drawing", map[string]any{"state": state}))
		} else if key, ok := hatchProgressKeys[event]; ok {
			say("  " + t(key, nil))
		}
	}
	result, err := HatchPet(ctx, env, HatchOptions{BaseImage: drafts[0], Slug: slug, DisplayName: displayName,
		Concept: concept, OnProgress: progress})
	if err != nil {
		var ge *GenerationError
		if !errors.As(err, &ge) {
			return err
		}
		say(t("hatch.failed", map[string]any{"error": ge.Error()}))
		return nil
	}
	if err := (&pet.Run{Ctx: ctx, Home: home, Env: env}).SetActive(result.Slug); err != nil {
		return err
	}
	say(t("hatch.hatched", map[string]any{"name": result.DisplayName}))
	return nil
}
