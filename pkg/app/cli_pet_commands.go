package app

import (
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/pet/generate"
)

// Pets runner bound to this session's profile writing to the console
func (c *CLI) petsRunner() *petsRun {
	return &petsRun{ctx: c.ctx, h: home.Dir(c.ctx), w: console().Out, env: c.env}
}

// Handles /pet toggle, list, scale, off or adopting a petdex pet by slug
func (c *CLI) handlePetCommand(cmd string) {
	arg := commandArg(cmd, false)
	low := pystr.Lower(arg)
	r := c.petsRunner()
	switch {
	case arg == "" || low == "toggle":
		enabled, name, errMsg, err := r.togglePetDisplay()
		if err != nil {
			panic(err)
		}
		switch {
		case errMsg != nil && *errMsg != "":
			printLines(c.commandsT("pet.error", map[string]any{"error": *errMsg}))
		case enabled:
			printLines(c.commandsT("pet.is_out", map[string]any{"name": derefOrNone(name)}))
		case name != nil && *name != "":
			printLines(c.commandsT("pet.put_away_named", map[string]any{"name": *name}))
		default:
			printLines(c.commandsT("pet.put_away", nil))
		}
	case low == "list" || low == "gallery" || low == "browse" || low == "all":
		if err := r.printPetGallery(20); err != nil {
			panic(err)
		}
	case low == "scale" || strings.HasPrefix(low, "scale "):
		value := pystr.Strip(pystr.Slice(arg, len("scale"), pystr.Len(arg)))
		if value == "" {
			printLines(c.commandsT("pet.usage_scale", nil))
			return
		}
		scale, errMsg, err := r.setPetScale(value)
		if err != nil {
			panic(err)
		}
		if errMsg != "" {
			printLines(c.commandsT("pet.error", map[string]any{"error": errMsg}))
		} else {
			printLines(c.commandsT("pet.scale_set", map[string]any{"scale": pyFmt("{v:g}", map[string]any{"v": scale})}))
		}
	case low == "off":
		if _, err := r.updatePetConfig(nil, "enabled", false); err != nil {
			panic(err)
		}
		printLines(c.commandsT("pet.put_away", nil))
	default:
		printLines(c.commandsT("pet.fetching", map[string]any{"name": arg}))
		installed, err := pet.InstallPet(c.ctx, c.env, r.h, arg, false)
		if err != nil {
			var se *pet.StoreError
			var me *pet.ManifestError
			if errors.As(err, &se) || errors.As(err, &me) {
				printLines(c.commandsT("pet.adopt_failed", map[string]any{"name": arg, "error": pyrepr.ExceptionMessage(err)}))
				return
			}
			panic(err)
		}
		if err := r.setActive(arg); err != nil {
			panic(err)
		}
		printLines(c.commandsT("pet.is_out", map[string]any{"name": installed.DisplayName}))
	}
}

// Pointed to string or None for formatting
func derefOrNone(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Progress event keys of /hatch other than row
var hatchProgressKeys = map[string]string{"compose": "hatch.progress_compose", "save": "hatch.progress_save"}

// Handles /hatch, generating a base look then every animation row and adopting the pet
func (c *CLI) handleHatchCommand(cmd string) {
	concept := commandArg(cmd, false)
	if concept == "" {
		raw, _ := c.promptTextInput(c.commandsT("hatch.describe_prompt", nil))
		concept = pystr.Strip(raw)
	}
	if concept == "" {
		printLines(c.commandsT("hatch.usage", nil))
		return
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
		displayName = c.commandsT("hatch.default_name", nil)
	}
	slug := pet.Slugify(displayName)
	printLines(c.commandsT("hatch.designing", map[string]any{"concept": concept}))
	drafts, err := generate.GenerateBaseDrafts(c.ctx, c.env, concept, generate.DraftOptions{N: 1})
	if err != nil {
		var ge *generate.GenerationError
		if !errors.As(err, &ge) {
			panic(err)
		}
		printLines(c.commandsT("hatch.base_failed", map[string]any{"error": ge.Error()}))
		return
	}
	if len(drafts) == 0 {
		printLines(c.commandsT("hatch.no_base_draft", nil))
		return
	}
	progress := func(event, detail string) {
		if event == "row" {
			state, _, _ := strings.Cut(detail, ":")
			printLines("  " + c.commandsT("hatch.progress_drawing", map[string]any{"state": state}))
		} else if key, ok := hatchProgressKeys[event]; ok {
			printLines("  " + c.commandsT(key, nil))
		}
	}
	result, err := generate.HatchPet(c.ctx, c.env, generate.HatchOptions{BaseImage: drafts[0], Slug: slug, DisplayName: displayName,
		Concept: concept, OnProgress: progress})
	if err != nil {
		var ge *generate.GenerationError
		if !errors.As(err, &ge) {
			panic(err)
		}
		printLines(c.commandsT("hatch.failed", map[string]any{"error": ge.Error()}))
		return
	}
	if err := c.petsRunner().setActive(result.Slug); err != nil {
		panic(err)
	}
	printLines(c.commandsT("hatch.hatched", map[string]any{"name": result.DisplayName}))
}
