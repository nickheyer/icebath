package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
)

// Scaffold markers shared byte for byte with the session store preview SQL
var (
	skillInvocationPrefix = strings.TrimSuffix(state.SkillScaffoldSQLLike, "%")
	autoLoadPrefix        = strings.TrimSuffix(state.AutoLoadScaffoldSQLLike, "%")
)

const (
	singleSkillMarker      = "The full skill content is loaded below.]"
	singleSkillInstruction = "The user has provided the following instruction alongside the skill invocation: "
	bundleMarker           = " skill bundle,"
)

var (
	skillInvalidChars  = regexp.MustCompile(`[^\p{L}\p{N}_-]`)
	skillMultiHyphen   = regexp.MustCompile(`-{2,}`)
	skillNameRe        = regexp.MustCompile(`^` + regexp.QuoteMeta(skillInvocationPrefix) + `"([^"]*)"`)
	autoLoadSuffix     = `" skill is auto-loaded\. Follow its instructions for this session\.\]`
	autoLoadHeaderRe   = regexp.MustCompile(regexp.QuoteMeta(autoLoadPrefix) + `([^"]*)` + autoLoadSuffix)
	autoLoadHeaderAtRe = regexp.MustCompile(`^` + regexp.QuoteMeta(autoLoadPrefix) + `([^"]*)` + autoLoadSuffix)
	bundleLoadedRe     = regexp.MustCompile(`(?m)^Skills loaded: (.+)$`)
)

// Slash command form of a skill name with spaces and underscores hyphenated and invalid characters stripped
func SlugifySkillName(name string) string {
	cmd := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(name), " ", "-"), "_", "-")
	cmd = skillInvalidChars.ReplaceAllString(cmd, "")
	return strings.Trim(skillMultiHyphen.ReplaceAllString(cmd, "-"), "-")
}

// Appends the volatile user instruction and returns the stable scaffold prefix ending at its marker
func AppendUserInstruction(parts *[]string, instruction string) string {
	stable := strings.Join(*parts, "\n") + "\n" + singleSkillInstruction
	*parts = append(*parts, singleSkillInstruction+instruction)
	return stable
}

// User instruction of a skill invocation turn, false when the text carries no instruction
func ExtractUserInstructionFromSkillMessage(content string) (string, bool) {
	return state.ExtractUserInstructionFromSkillMessage(content)
}

// Short description of a skill invocation or auto load turn, false for any other text
func DescribeSkillInvocation(content, separator string) (string, bool) {
	return state.DescribeSkillInvocation(content, separator)
}

// Skill names a complete invocation, bundle or auto load scaffold activated, empty for other text
func SkillsLoadedByScaffold(content string) []string {
	if strings.HasPrefix(content, autoLoadPrefix) {
		if !autoLoadHeaderAtRe.MatchString(content) {
			return nil
		}
		var out []string
		for _, m := range autoLoadHeaderRe.FindAllStringSubmatch(content, -1) {
			out = append(out, m[1])
		}
		return out
	}
	if !strings.HasPrefix(content, skillInvocationPrefix) {
		return nil
	}
	header, _, _ := strings.Cut(content, "\n")
	if strings.Contains(header, bundleMarker) {
		m := bundleLoadedRe.FindStringSubmatch(content)
		if m == nil {
			return nil
		}
		var out []string
		for _, n := range strings.Split(m[1], ",") {
			if n = strings.TrimSpace(n); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	if m := skillNameRe.FindStringSubmatch(content); strings.HasSuffix(header, singleSkillMarker) && m != nil && m[1] != "" {
		return []string{m[1]}
	}
	return nil
}

// Loaded skill payload, its directory and resolved name, false when the skill cannot be served
func (s *Scope) loadSkillPayload(ctx context.Context, identifier, taskID string) (map[string]any, string, string, bool) {
	if config.SafeWorkerEnabled() {
		return nil, "", "", false
	}
	raw := strings.TrimSpace(identifier)
	if raw == "" {
		return nil, "", "", false
	}
	normalized := s.normalizeSkillLookupName(raw)
	var loaded map[string]any
	if err := json.Unmarshal([]byte(s.SkillView(ctx, normalized, "", taskID, false)), &loaded); err != nil || !fmTruthy(loaded["success"]) {
		return nil, "", "", false
	}
	skillPath := pyStrOr(firstTruthy(loaded["path"]))
	skillDir := ""
	if d := loaded["skill_dir"]; fmTruthy(d) {
		skillDir = pyStrValue(d)
	} else if skillPath != "" {
		skillDir = filepath.Join(s.SkillsDir(), filepath.Dir(skillPath))
	}
	name := normalized
	if n := loaded["name"]; fmTruthy(n) {
		name = pyStrValue(n)
	}
	return loaded, skillDir, name, true
}

// Ambiguity label naming the exact load names of a same tier collision, false otherwise
func AmbiguousSkillLabel(identifier string, payload map[string]any) (string, bool) {
	names, _ := payload["load_names"].([]any)
	if len(names) == 0 {
		return "", false
	}
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = pyStrValue(n)
	}
	return fmt.Sprintf("%s%s: use one of %s", AmbiguousSkillPrefix, identifier, strings.Join(parts, ", ")), true
}

// Identifier of a missing skill, or its ambiguity label when the name collides
func (s *Scope) missingSkillLabel(ctx context.Context, identifier string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(s.SkillView(ctx, s.normalizeSkillLookupName(identifier), "", "", false)), &payload); err != nil {
		return identifier
	}
	if label, ok := AmbiguousSkillLabel(identifier, payload); ok {
		return label
	}
	return identifier
}

// Ambiguity labels then one unknown skills line
func FormatMissingSkills(missing []string) string {
	var ambiguous, unknown []string
	for _, m := range missing {
		if strings.HasPrefix(m, AmbiguousSkillPrefix) {
			ambiguous = append(ambiguous, m)
		} else {
			unknown = append(unknown, m)
		}
	}
	if len(unknown) > 0 {
		ambiguous = append(ambiguous, "Unknown skill(s): "+strings.Join(unknown, ", "))
	}
	return strings.Join(ambiguous, "; ")
}

// Appends the skill's resolved config values block, nothing when it declares none
func (s *Scope) injectSkillConfig(loaded map[string]any, parts *[]string) {
	raw := pyStrOr(firstTruthy(loaded["raw_content"], loaded["content"]))
	fm, _ := ParseFrontmatter(raw)
	resolved := s.ResolveSkillConfigValues(extractSkillConfigVars(fm))
	if len(resolved) == 0 {
		return
	}
	*parts = append(*parts, "", fmt.Sprintf("[Skill config (from %s/config.yaml):", s.displayHome()))
	for _, v := range extractSkillConfigVars(fm) {
		value, ok := resolved[v.Key]
		if !ok {
			continue
		}
		shown := "(not set)"
		if config.PyTruthy(value) {
			shown = config.PyStr(value)
		}
		*parts = append(*parts, fmt.Sprintf("  %s = %s", v.Key, shown))
	}
	*parts = append(*parts, "]")
}
