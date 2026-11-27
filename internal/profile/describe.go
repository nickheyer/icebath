package profile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/tools/redact"
)

// Skill names a description prompt lists at most
const promptSkills = 60

// What the helper model is told when it describes a profile
const DescribeSystem = `You are a profile-describer for the Icebath Agent kanban board.

A user runs multiple "profiles" — distinct agent identities, each with their
own skills, model, and configuration. The kanban board's orchestrator routes
work to whichever profile best fits each task. To do that well, every
profile needs a short, concrete description of what it's good at.

You are given a profile's:
  - Name
  - Model / provider
  - List of installed skill names (a strong signal of role / domain)

Produce a single JSON object with exactly one key:

  {
    "description": "<1-2 sentence description, plain prose, no preamble>"
  }

Rules:
  - The description is what an orchestrator will read to decide whether to
    route a task here. Lead with the profile's strongest capability.
  - Stay concrete. Bad: "an AI agent that helps users."
                  Good: "Reads and modifies Python codebases — runs tests,
                         refactors functions, opens GitHub PRs."
  - 1-2 sentences, <= 280 characters total.
  - Never invent capabilities the skills don't suggest.
  - Never write "Icebath profile" or other meta-narration.
  - No code fences, no preamble, no closing remarks. Output only JSON.
`

// Fences a model may wrap its JSON in
var fence = regexp.MustCompile("(?mi)^```(?:json)?\\s*|\\s*```$")

// How describing one profile went
type Outcome struct {
	Name        string
	OK          bool
	Reason      string
	Description string
}

// Skill names spread evenly over the prompt's budget
func sample(names []string) []string {
	if len(names) <= promptSkills {
		return names
	}
	step := float64(len(names)) / promptSkills
	out := make([]string, promptSkills)
	for i := range out {
		out[i] = names[int(float64(i)*step)]
	}
	return out
}

func unset(text string) string {
	if text == "" {
		return "(unset)"
	}
	return text
}

func firstRunes(text string, n int) string {
	runes := []rune(text)
	return string(runes[:min(n, len(runes))])
}

// Has the helper model write what a profile is good at from its skills and model, through complete, and keeps it as written by the model;
// a description a person wrote is kept unless overwrite says otherwise. What the model could not do is an outcome that is not ok
func (p *Profiles) Describe(ctx context.Context, name string, overwrite bool, complete func(ctx context.Context, system, user string) (string, error)) (Outcome, error) {
	canon, err := Normalize(name)
	if err != nil {
		return Outcome{}, err
	}
	fail := func(reason string) (Outcome, error) { return Outcome{Name: canon, Reason: reason}, nil }
	if !p.Exists(canon) {
		return fail("profile not found")
	}
	dir := p.Dir(canon)
	if meta := ReadMeta(dir); meta.Description != "" && !meta.DescriptionAuto && !overwrite {
		return fail("profile already has a user-authored description (use --overwrite to replace)")
	}
	summary := p.Summarize(p.config.In(canon))
	skills := slices.Clone(summary.Skills)
	slices.Sort(skills)
	var lines []string
	for _, skill := range sample(skills) {
		lines = append(lines, "  - "+skill)
	}
	listed := strings.Join(lines, "\n")
	if listed == "" {
		listed = "  (no skills installed)"
	}
	prompt := fmt.Sprintf("Profile name: %s\nDefault model: %s\nProvider: %s\nInstalled skill count: %d\nNotable skills (up to %d):\n%s\n", canon,
		unset(summary.Model), unset(summary.Provider), len(skills), promptSkills, listed)
	raw, err := complete(ctx, DescribeSystem, prompt)
	if err != nil {
		slog.Info(fmt.Sprintf("describe: API call failed for %s (%v)", canon, err))
		// The whole failure, not only its kind, with what looks like a secret masked
		return fail("LLM error: " + redact.File(err.Error(), true))
	}
	description := ""
	stripped := fence.ReplaceAllString(strings.TrimSpace(raw), "")
	first, last := strings.Index(stripped, "{"), strings.LastIndex(stripped, "}")
	var parsed map[string]any
	if first >= 0 && last > first && json.Unmarshal([]byte(stripped[first:last+1]), &parsed) == nil {
		value, ok := parsed["description"].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return fail("LLM response missing 'description' field")
		}
		description = firstRunes(strings.TrimSpace(value), 280)
	} else {
		if strings.HasPrefix(stripped, "{") {
			slog.Info(fmt.Sprintf("describe: %s aux response looked JSON-shaped but failed to parse (likely truncated) -- refusing to persist the raw "+
				"fragment", canon))
			return fail("LLM returned malformed/truncated JSON response")
		}
		paragraph, _, _ := strings.Cut(strings.TrimSpace(raw), "\n\n")
		if paragraph == "" {
			return fail("LLM returned an empty response")
		}
		description = firstRunes(paragraph, 280)
	}
	auto := true
	if err := WriteMeta(dir, MetaUpdate{Description: &description, DescriptionAuto: &auto}); err != nil {
		return fail("failed to write profile.yaml: " + err.Error())
	}
	return Outcome{Name: canon, OK: true, Reason: "described", Description: description}, nil
}

// Profiles that can be described, those without a description a person wrote when missingOnly
func (p *Profiles) Describable(missingOnly bool) ([]string, error) {
	names, err := p.List()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		meta := ReadMeta(p.Dir(name))
		if !(missingOnly && strings.TrimSpace(meta.Description) != "" && !meta.DescriptionAuto) {
			out = append(out, name)
		}
	}
	return out, nil
}
