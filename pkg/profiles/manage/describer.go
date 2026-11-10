package manage

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/skills"
)

// MaxSkillsForPrompt bounds the skill names a description prompt lists
const MaxSkillsForPrompt = 60

const describerSystemPrompt = `You are a profile-describer for the Hermes Agent kanban board.

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
  - Never write "Hermes Agent profile" or other meta-narration.
  - No code fences, no preamble, no closing remarks. Output only JSON.
`

const describerUserTemplate = "Profile name: %s\nDefault model: %s\nProvider: %s\nInstalled skill count: %d\nNotable skills (up to %d):\n%s\n"

var describerFenceRe = regexp.MustCompile("(?mi)^```(?:json)?" + pyWS + "*|" + pyWS + "*```$")

// Auxiliary completion seam, the auxiliary client call_llm in production
var describeCallLLM = auxCallLLM

// DescribeOutcome is the result of describing one profile
type DescribeOutcome struct {
	ProfileName string
	OK          bool
	Reason      string
	Description *string
}

// Sorted non excluded skill names as category/skill or a bare name for skills directly under skills
func collectSkills(profileDir string) []string {
	skillsDir := filepath.Join(profileDir, "skills")
	if !isDir(skillsDir) {
		return nil
	}
	var names []string
	filepath.WalkDir(skillsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "SKILL.md" || skills.IsExcludedSkillPath(p, "") {
			return nil
		}
		rel, err := filepath.Rel(skillsDir, p)
		if err != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		parts = parts[:len(parts)-1]
		switch len(parts) {
		case 0:
		case 1:
			names = append(names, parts[0])
		default:
			names = append(names, parts[0]+"/"+parts[len(parts)-1])
		}
		return nil
	})
	sort.Strings(names)
	return names
}

// Evenly spaced picks capping names to the prompt budget
func sampleSkills(names []string) []string {
	if len(names) <= MaxSkillsForPrompt {
		return names
	}
	step := float64(len(names)) / MaxSkillsForPrompt
	out := make([]string, MaxSkillsForPrompt)
	for i := range out {
		out[i] = names[int(float64(i)*step)]
	}
	return out
}

// First JSON object between the outer braces after fence stripping, nil when none parses to an object
func extractJSONBlob(raw string, fence *regexp.Regexp) *pyjson.Object {
	if raw == "" {
		return nil
	}
	stripped := fence.ReplaceAllString(pystr.Strip(raw), "")
	first, last := strings.Index(stripped, "{"), strings.LastIndex(stripped, "}")
	if first == -1 || last == -1 || last <= first {
		return nil
	}
	val, err := pyjson.Loads(stripped[first : last+1])
	if err != nil {
		return nil
	}
	obj, _ := val.(*pyjson.Object)
	return obj
}

// First n code points of s
func headRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) > n {
		rs = rs[:n]
	}
	return string(rs)
}

// Generates a profile description through the auxiliary model, expected failures returned as not ok
func DescribeProfile(ctx context.Context, profileName string, overwrite bool, timeout time.Duration) (DescribeOutcome, error) {
	canon, err := profiles.NormalizeProfileName(profileName)
	if err != nil {
		return DescribeOutcome{}, err
	}
	fail := func(reason string) (DescribeOutcome, error) {
		return DescribeOutcome{ProfileName: canon, Reason: reason}, nil
	}
	if !profiles.ProfileExists(canon) {
		return fail("profile not found")
	}
	profileDir := home.Dir(ctx)
	if canon != "default" {
		if profileDir, err = profiles.GetProfileDir(canon); err != nil {
			return fail("cannot resolve profile dir: " + err.Error())
		}
	}
	existing := profiles.ReadProfileMeta(profileDir)
	if existing.Description != "" && !existing.DescriptionAuto && !overwrite {
		return fail("profile already has a user-authored description (use --overwrite to replace)")
	}
	all := collectSkills(profileDir)
	var lines []string
	for _, n := range sampleSkills(all) {
		lines = append(lines, "  - "+n)
	}
	skillList := strings.Join(lines, "\n")
	if skillList == "" {
		skillList = "  (no skills installed)"
	}
	model, provider, err := profiles.ReadConfigModel(ctx, profileDir)
	if err != nil {
		model, provider = nil, nil
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	userMsg := fmt.Sprintf(describerUserTemplate, canon, unsetOr(model), unsetOr(provider), len(all), MaxSkillsForPrompt, skillList)
	raw, errClass, err := describeCallLLM(ctx, "profile_describer", []map[string]any{
		{"role": "system", "content": describerSystemPrompt},
		{"role": "user", "content": userMsg},
	}, 0.3, 400, timeout)
	if err != nil {
		slog.Default().Info(fmt.Sprintf("describe: API call failed for %s (%v)", canon, err))
		return fail("LLM error: " + errClass)
	}
	var description string
	if parsed := extractJSONBlob(raw, describerFenceRe); parsed == nil {
		stripped := describerFenceRe.ReplaceAllString(pystr.Strip(raw), "")
		if strings.HasPrefix(stripped, "{") {
			slog.Default().Info(fmt.Sprintf("describe: %s aux response looked JSON-shaped but failed to parse "+
				"(likely truncated) -- refusing to persist the raw fragment", canon))
			return fail("LLM returned malformed/truncated JSON response")
		}
		text, _, _ := strings.Cut(pystr.Strip(raw), "\n\n")
		if text == "" {
			return fail("LLM returned an empty response")
		}
		description = headRunes(text, 280)
	} else {
		val, _ := parsed.Get("description")
		s, ok := val.(string)
		if !ok || pystr.Strip(s) == "" {
			return fail("LLM response missing 'description' field")
		}
		description = headRunes(pystr.Strip(s), 280)
	}
	auto := true
	if err := profiles.WriteProfileMeta(ctx, profileDir, profiles.MetaUpdate{Description: &description, DescriptionAuto: &auto}); err != nil {
		return fail("failed to write profile.yaml: " + err.Error())
	}
	return DescribeOutcome{ProfileName: canon, OK: true, Reason: "described", Description: &description}, nil
}

// Python str of a config value or the unset marker when it is falsy
func unsetOr(v any) string {
	if !pyrepr.Truthy(v) {
		return "(unset)"
	}
	return pyrepr.Str(v)
}

// Profile names that can be described, only those lacking a user authored description when missingOnly
func ListDescribableProfiles(ctx context.Context, missingOnly bool) ([]string, error) {
	infos, err := ListProfiles(ctx, false)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range infos {
		if !(missingOnly && pystr.Strip(p.Description) != "" && !p.DescriptionAuto) {
			out = append(out, p.Name)
		}
	}
	return out, nil
}
