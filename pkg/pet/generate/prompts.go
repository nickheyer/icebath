package generate

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// What each state depicts, phrased to avoid detached effects, motion lines and shadows
var StateActions = map[string]string{
	"idle": "a calm idle loop: subtle breathing, a tiny blink or gentle bob, no big gestures",
	"running-right": "a sideways walk/run locomotion cycle moving to the RIGHT: the character " +
		"faces and travels right with clear directional steps, a smooth gait loop",
	"running-left": "a sideways walk/run locomotion cycle moving to the LEFT: the character " +
		"faces and travels left with clear directional steps (the mirror of the " +
		"right-facing run)",
	"waving":  "a friendly greeting: raising a paw/hand/limb to wave, clear up-and-down gesture",
	"jumping": "a happy celebration jump: anticipation, lift off the ground, peak, and land",
	"failed":  "a sad or deflated reaction: slumped, dejected, small frown — readable but not noisy",
	"waiting": "an expectant 'waiting on you' pose: looking up/out as if asking for input " +
		"or approval — distinct from idle and review",
	"running": "focused active work, staying IN PLACE (NOT walking or foot-running): " +
		"leaning in, concentrating, busy 'thinking / processing / typing' energy",
	"review": "careful inspection: a focused lean, head tilt, studying something intently",
}

var styleHints = map[string]string{
	"auto": " Style: crisp 16-bit PIXEL-ART game sprite — visible square pixels, a small " +
		"limited palette, clean dark outline, flat cel shading, chunky chibi " +
		"proportions, like a classic SNES/JRPG party member or a petdex.dev mascot. " +
		"Absolutely NOT 3D-rendered, NOT a smooth painted or vector illustration, " +
		"NOT photorealistic — no soft gradients, no realistic lighting, no figurine look.",
	"pixel":       " Render in clean 16-bit pixel-art style with visible square pixels and a limited palette.",
	"plush":       " Render as a soft plush toy.",
	"clay":        " Render as a claymation / soft 3D clay figure.",
	"sticker":     " Render as a glossy die-cut sticker.",
	"flat-vector": " Render in flat vector mascot style.",
	"3d-toy":      " Render as a glossy 3D toy.",
	"painterly":   " Render in a soft painterly style.",
}

const promptBackground = "Center the character on a SINGLE flat, uniform, high-contrast chroma-key " +
	"background — pure hot magenta #FF00FF (only if magenta appears on the " +
	"character, use pure green #00FF00 instead). The background is ONE continuous " +
	"even color that completely surrounds the character with NO gradient, " +
	"vignette, texture, pattern, scenery, shadow, ground line, frame, border, " +
	"panel, comic cell, gutter line, grid, or divider of any kind, so it keys out " +
	"cleanly. The background color must not appear anywhere on the character. " +
	"No text, no labels, no speech bubbles, no UI."

// Style suffix of a named style, auto when empty, nothing for an unknown one
func StyleHint(style string) string {
	if style == "" {
		style = "auto"
	}
	return styleHints[pystr.Lower(pystr.Strip(style))]
}

const assumedStripWidth = 1536

// Per pose width and gap in pixels for a row of the frame count
func spacingSpec(frameCount int) (int, int) {
	slots := max(1, frameCount)
	slotW := float64(assumedStripWidth) / float64(slots)
	return pyRound(slotW * 0.7), max(48, pyRound(slotW*0.3))
}

// Per draft nudges varying the look but not the pose
var BaseVariations = []string{
	"",
	"a distinctly different colour palette and markings",
	"a heavier, broader silhouette with sturdier proportions",
	"a different facial structure and expression matching the concept tone, with unique accent/accessory details",
	"a leaner, taller build and an alternate colour scheme",
	"bolder, more saturated colours and a stronger expression matching the concept tone",
}

// Base look prompt, a single centred full body mascot, the variation differentiating drafts
func BuildBasePrompt(concept, style, variation string) string {
	concept = pystr.Strip(concept)
	if concept == "" {
		concept = "a distinctive mascot creature"
	}
	nudge := ""
	if variation != "" {
		nudge = " Make this design distinct: " + variation + "."
	}
	return "A stylized mascot pet character: " + concept + ". " +
		"Honor the requested tone and mood exactly (cute, eerie, scary, menacing, whimsical, etc.) " +
		"while staying non-graphic. " +
		"Compact, whole-body silhouette that reads clearly at small size, " +
		"clear readable facial features, simple consistent palette. " +
		"Neutral front-facing standing pose, upright and symmetric, arms/limbs " +
		"relaxed at the sides, feet together on the ground, any cape/accessories " +
		"hanging straight and still." +
		nudge + " " +
		promptBackground + StyleHint(style)
}

// Row strip prompt of the frame count poses left to right, identity locked to the attached base
func BuildRowPrompt(state string, frameCount int, style string) string {
	action, ok := StateActions[state]
	if !ok {
		action = "a simple idle pose"
	}
	poseW, gap := spacingSpec(frameCount)
	var b strings.Builder
	b.WriteString("Using the attached reference image as the exact same character " +
		"(same species, face, colors, markings, proportions, and props), " +
		"preserving the same emotional tone/mood (e.g., scary stays scary, cute stays cute), ")
	fmt.Fprintf(&b, "draw a single WIDE horizontal strip of %d animation frames showing %s. ", frameCount, action)
	fmt.Fprintf(&b, "LAYOUT: arrange %d poses in ONE horizontal row at equal spacing, ", frameCount)
	b.WriteString("each pose centered in its own imaginary equal region. Draw NO panel borders, " +
		"NO comic cells, NO boxes, NO vertical divider/gutter lines, NO grid, NO frame " +
		"outlines between poses — the backdrop is one unbroken flat field behind all of them. " +
		"Fill the WHOLE strip with the SAME single flat chroma-key color as the attached " +
		"reference image's background (identical hue in every frame, no per-pose color shifts). ")
	fmt.Fprintf(&b, "SPACING (critical): draw each pose at a consistent, healthy, clearly "+
		"visible size (roughly %dpx wide on a %dpx "+
		"strip) — do NOT shrink it tiny — but keep its ENTIRE silhouette "+
		"(wings, tail, halo, horns, cape, every appendage) fully INSIDE its own "+
		"cell. Leave at least %dpx of empty chroma-key background between "+
		"neighboring silhouettes at their closest point (wingtip to wingtip), and "+
		"the same empty margin before the first pose and after the last. If a wing, "+
		"cape, or tail would reach into a neighbor, FOLD or angle it inward rather "+
		"than letting it cross the gap. Silhouettes must NEVER touch, overlap, "+
		"share a shadow, share a ground line, share motion trails, or merge into "+
		"one connected shape. ", poseW, assumedStripWidth, gap)
	b.WriteString("REGISTRATION (critical): the character is the SAME height and SAME width " +
		"in every frame, drawn at the SAME scale, centered over the SAME point, " +
		"with all feet aligned to the SAME invisible horizontal baseline across the " +
		"whole strip — this baseline is conceptual ONLY: draw NO ground line, floor, " +
		"platform, horizon, or contact shadow beneath the feet. Keep the body's center, size, and stance fixed frame to " +
		"frame — ONLY the limbs/features the action needs may move. Capes, cloaks, " +
		"bags, and scarves stay in the SAME place and shape every frame (no " +
		"swinging, flowing, or drifting) unless the action itself requires it. No " +
		"pose is cropped at the strip edges. ")
	b.WriteString(promptBackground + StyleHint(style))
	return b.String()
}
