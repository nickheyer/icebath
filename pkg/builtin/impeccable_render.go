package builtin

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"
)

const impeccableSkillRoot = "impeccable/skill"

// Hermes provider token values from PROVIDER_PLACEHOLDERS
var impeccableProviderTokens = map[string]string{
	"model":           "the model",
	"config_file":     "AGENTS.md",
	"ask_instruction": "Ask the user directly to clarify what you cannot infer.",
	"command_prefix":  "/",
}

// Markdown block tags compileProviderBlocks recognises
var providerBlockTags = map[string]bool{"agents": true, "antigravity": true, "claude": true, "claude-code": true, "codex": true,
	"cursor": true, "devin": true, "dsh": true, "gemini": true, "github": true, "grok": true, "hermes": true, "kiro": true,
	"opencode": true, "pi": true, "omp": true, "qoder": true, "rovo-dev": true, "trae": true, "trae-cn": true, "vibe": true, "veto": true}

var excludedFromSuggestions = map[string]bool{"impeccable": true, "teach-impeccable": true, "frontend-design": true}

var impeccableSubCommands = []string{"adapt", "animate", "audit", "bolder", "clarify", "colorize", "critique", "delight", "distill",
	"document", "harden", "layout", "onboard", "optimize", "overdrive", "polish", "quieter", "shape", "typeset"}

// Category of each command from SKILL_CATEGORIES and the order the hint groups them in
var skillCategories = map[string]string{"impeccable": "create", "shape": "create", "critique": "evaluate", "audit": "evaluate",
	"typeset": "refine", "layout": "refine", "colorize": "refine", "animate": "refine", "delight": "refine", "bolder": "refine",
	"quieter": "refine", "overdrive": "refine", "distill": "simplify", "clarify": "simplify", "adapt": "simplify",
	"polish": "harden", "optimize": "harden", "harden": "harden", "onboard": "harden", "init": "system", "document": "system",
	"extract": "system", "live": "system", "generate": "system"}
var categoryOrder = []string{"create", "evaluate", "refine", "simplify", "harden", "system"}

const degradedPreamble = "<!-- Generated from skill/agents/ at build time. Do not edit; edit the agent definition. -->\n" +
	"This harness has no subagent capability, so you are running this role inline. Step fully out of the work you just finished, adopt only this file's instructions for the pass, and disclose the substitution in one line of your closing message to the user, outside the output contract. Where the text below addresses a parent agent, you are both parties: produce the full output contract first, then act on it yourself."

var (
	providerBlockRe = regexp2.MustCompile(`(^|\r?\n)[ \t]*<([a-z][a-z0-9-]*)>[ \t]*\r?\n([\s\S]*?)\r?\n[ \t]*</\2>[ \t]*(?=\r?\n|\z)`, regexp2.None)
	blankRunRe      = regexp.MustCompile(`(?:\r?\n){3,}`)
	ruleMarkerRe    = regexp.MustCompile(`[ \t]*<!--\s*rule:[a-z0-9-]+\s*-->`)
	fmBlockRe       = regexp.MustCompile(`^---\r?\n([\s\S]*?)\r?\n---\r?\n([\s\S]*)$`)
	quotedValueRe   = regexp.MustCompile(`^(".*"|'.*')$`)
	lineSplitRe     = regexp.MustCompile(`\r?\n`)
	leadingWSRe     = regexp.MustCompile(`^\s+`)
	crlfRe          = regexp.MustCompile(`\r?\n`)
	yamlEdgeWSRe    = regexp.MustCompile(`^\s|\s$`)
	yamlIndicatorRe = regexp.MustCompile("^[\\[\\]{},&*!|>'\"%@`#]")
	yamlDashRe      = regexp.MustCompile(`^[?:-](\s|$)`)
	yamlColonRe     = regexp.MustCompile(`: |\s#|:$`)
	yamlKeywordRe   = regexp.MustCompile(`(?i)^(true|false|null|yes|no|on|off|~)$`)
	yamlNumberRe    = regexp.MustCompile(`^-?\d+(\.\d+)?([eE][+-]?\d+)?$`)
)

// Keeps provider blocks tagged for the active harness, drops other known blocks and squeezes blank runs
func compileProviderBlocks(content string, active []string) string {
	activeSet := map[string]bool{}
	for _, t := range active {
		activeSet[t] = true
	}
	compiled := false
	out, err := providerBlockRe.ReplaceFunc(content, func(m regexp2.Match) string {
		groups := m.Groups()
		prefix, tag, body := groups[1].String(), groups[2].String(), groups[3].String()
		if !providerBlockTags[tag] {
			return m.String()
		}
		compiled = true
		if activeSet[tag] {
			return prefix + body
		}
		return prefix
	}, -1, -1)
	if err != nil {
		return content
	}
	if !compiled {
		return content
	}
	return blankRunRe.ReplaceAllString(out, "\n\n")
}

// Removes rule id comment markers with their leading spaces
func stripRuleMarkers(content string) string {
	return ruleMarkerRe.ReplaceAllString(content, "")
}

// Substitutes the hermes command prefix and model name tokens and the available commands list
func replacePlaceholders(content string, commandNames []string) string {
	prefix := impeccableProviderTokens["command_prefix"]
	var nonExcluded []string
	for _, n := range commandNames {
		if !excludedFromSuggestions[n] {
			nonExcluded = append(nonExcluded, n)
		}
	}
	var list []string
	if len(nonExcluded) == 0 {
		for _, n := range impeccableSubCommands {
			list = append(list, prefix+"impeccable "+n)
		}
	} else {
		for _, n := range nonExcluded {
			list = append(list, prefix+n)
		}
	}
	r := strings.NewReplacer("{{model}}", impeccableProviderTokens["model"], "{{config_file}}", impeccableProviderTokens["config_file"],
		"{{ask_instruction}}", impeccableProviderTokens["ask_instruction"], "{{command_prefix}}", prefix,
		"{{available_commands}}", strings.Join(list, ", "))
	return r.Replace(content)
}

// Renders the provider markers in an executable script for the hermes build
func replaceScriptProviderMarker(content string) string {
	content = strings.Replace(content, "export const IMPECCABLE_COMMAND_PREFIX = '/'; // @impeccable-provider-command-prefix", `export const IMPECCABLE_COMMAND_PREFIX = "/";`, 1)
	return strings.Replace(content, "export const IMPECCABLE_PROVIDER_ID = 'source'; // @impeccable-provider-id", `export const IMPECCABLE_PROVIDER_ID = "hermes";`, 1)
}

// One ordered frontmatter entry, a string, bool, list or nested ordered map
type fmPair struct {
	Key   string
	Value any
}

// Frontmatter and trimmed body using the impeccable build parser rules
func parseImpeccableFrontmatter(content string) ([]fmPair, string) {
	m := fmBlockRe.FindStringSubmatch(content)
	if m == nil {
		return nil, content
	}
	var pairs []fmPair
	set := func(k string, v any) {
		for i := range pairs {
			if pairs[i].Key == k {
				pairs[i].Value = v
				return
			}
		}
		pairs = append(pairs, fmPair{k, v})
	}
	var arrayKey string
	var array []any
	inArray := false
	for _, line := range lineSplitRe.Split(m[1], -1) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		trimmedStart := strings.TrimLeft(line, " \t\r\n\f\v")
		leading := len(line) - len(trimmedStart)
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") && leading >= 2 {
			if inArray {
				if strings.HasPrefix(trimmed, "- name:") {
					array = append(array, []fmPair{{"name", strings.TrimSpace(trimmed[7:])}})
				} else {
					array = append(array, trimmed[2:])
				}
				set(arrayKey, array)
			}
			continue
		}
		if leading >= 4 && inArray && len(array) > 0 {
			if i := strings.Index(trimmed, ":"); i > 0 {
				if obj, ok := array[len(array)-1].([]fmPair); ok {
					v := strings.TrimSpace(trimmed[i+1:])
					obj = append(obj, fmPair{strings.TrimSpace(trimmed[:i]), coerceBool(v)})
					array[len(array)-1] = obj
					set(arrayKey, array)
				}
			}
			continue
		}
		if leading == 0 {
			i := strings.Index(trimmed, ":")
			if i <= 0 {
				continue
			}
			key, value := strings.TrimSpace(trimmed[:i]), strings.TrimSpace(trimmed[i+1:])
			quoted := quotedValueRe.MatchString(value)
			unquoted := value
			if quoted {
				unquoted = value[1 : len(value)-1]
			}
			if value != "" {
				if key == "user-invocable" || key == "user-invokable" || !quoted {
					set(key, coerceBool(unquoted))
				} else {
					set(key, unquoted)
				}
				inArray = false
			} else {
				arrayKey, array, inArray = key, []any{}, true
				set(key, array)
			}
		}
	}
	return pairs, strings.TrimSpace(m[2])
}

func coerceBool(v string) any {
	switch v {
	case "true":
		return true
	case "false":
		return false
	}
	return v
}

func fmLookup(pairs []fmPair, key string) any {
	for _, p := range pairs {
		if p.Key == key {
			return p.Value
		}
	}
	return nil
}

func fmString(pairs []fmPair, key string) string {
	s, _ := fmLookup(pairs, key).(string)
	return s
}

// Whether a YAML scalar must be quoted to survive a parse
func yamlNeedsQuoting(v string) bool {
	return v == "" || yamlEdgeWSRe.MatchString(v) || yamlIndicatorRe.MatchString(v) || yamlDashRe.MatchString(v) ||
		yamlColonRe.MatchString(v) || yamlKeywordRe.MatchString(v) || yamlNumberRe.MatchString(v)
}

func formatYamlScalar(v any) string {
	s, ok := v.(string)
	if !ok {
		return jsString(v)
	}
	if yamlNeedsQuoting(s) {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return s
}

// YAML frontmatter block in the impeccable build layout
func generateYamlFrontmatter(pairs []fmPair) string {
	lines := []string{"---"}
	for _, p := range pairs {
		switch v := p.Value.(type) {
		case []any:
			lines = append(lines, p.Key+":")
			for _, item := range v {
				if obj, ok := item.([]fmPair); ok {
					lines = append(lines, "  - name: "+formatYamlScalar(fmLookup(obj, "name")))
					if d := fmLookup(obj, "description"); d != nil && d != "" {
						lines = append(lines, "    description: "+formatYamlScalar(d))
					}
					if r := fmLookup(obj, "required"); r != nil {
						lines = append(lines, fmt.Sprintf("    required: %v", r))
					}
				} else {
					lines = append(lines, "  - "+formatYamlScalar(item))
				}
			}
		case []fmPair:
			lines = append(lines, p.Key+":")
			lines = appendYamlObject(lines, v, 2)
		case bool:
			lines = append(lines, fmt.Sprintf("%s: %v", p.Key, v))
		default:
			lines = append(lines, p.Key+": "+formatYamlScalar(v))
		}
	}
	return strings.Join(append(lines, "---"), "\n")
}

func appendYamlObject(lines []string, pairs []fmPair, indent int) []string {
	space := strings.Repeat(" ", indent)
	for _, p := range pairs {
		switch v := p.Value.(type) {
		case []any:
			lines = append(lines, space+p.Key+":")
			for _, item := range v {
				if obj, ok := item.([]fmPair); ok {
					lines = append(lines, space+"  -")
					lines = appendYamlObject(lines, obj, indent+4)
				} else {
					lines = append(lines, space+"  - "+formatYamlScalar(item))
				}
			}
		case []fmPair:
			lines = append(lines, space+p.Key+":")
			lines = appendYamlObject(lines, v, indent+2)
		case bool:
			lines = append(lines, fmt.Sprintf("%s%s: %v", space, p.Key, v))
		default:
			lines = append(lines, space+p.Key+": "+formatYamlScalar(v))
		}
	}
	return lines
}

// Version the build stamps into the skill, read from the Claude plugin manifest
func impeccableVersion() (string, error) {
	data, err := payloadFile("impeccable/plugin/.claude-plugin/plugin.json")
	if err != nil {
		return "", err
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", err
	}
	return m.Version, nil
}

// Command names of command-metadata.json in file order
func impeccableCommandNames() ([]string, error) {
	data, err := payloadFile(impeccableSkillRoot + "/scripts/command-metadata.json")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var names []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		names = append(names, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// Command hint of the argument hint, commands grouped by category with middle dots between groups
func impeccableCommandHint() (string, error) {
	names, err := impeccableCommandNames()
	if err != nil {
		return "", err
	}
	var groups []string
	for _, cat := range categoryOrder {
		var in []string
		for _, n := range names {
			if skillCategories[n] == cat {
				in = append(in, n)
			}
		}
		if len(in) > 0 {
			groups = append(groups, strings.Join(in, "|"))
		}
	}
	return strings.Join(groups, " · "), nil
}

// Hermes build of skill/ keyed by path below the skill directory, scripts path substituted
func renderImpeccableSkill(scriptsPath string) (map[string]treeFile, error) {
	src, err := payloadFile(impeccableSkillRoot + "/SKILL.src.md")
	if err != nil {
		return nil, err
	}
	version, err := impeccableVersion()
	if err != nil {
		return nil, err
	}
	fm, body := parseImpeccableFrontmatter(string(src))
	name := fmString(fm, "name")
	if name == "" {
		name = "impeccable"
	}
	var commandNames []string
	if inv := fmLookup(fm, "user-invocable"); inv == true || inv == "true" {
		commandNames = []string{name}
	}
	out := []fmPair{{"name", name}, {"description", fmString(fm, "description")}}
	if version != "" {
		out = append(out, fmPair{"version", version})
	}
	for _, key := range []string{"license", "compatibility", "metadata"} {
		if v := fmLookup(fm, key); v != nil && v != "" && v != false {
			out = append(out, fmPair{key, v})
		}
	}
	render := func(text string, names []string) string {
		text = compileProviderBlocks(text, []string{"hermes"})
		text = replacePlaceholders(text, names)
		return stripRuleMarkers(text)
	}
	sp := strings.NewReplacer("{{scripts_path}}", scriptsPath)
	files := map[string]treeFile{}
	skillBody := sp.Replace(render(body, commandNames))
	files["SKILL.md"] = treeFile{Data: []byte(sp.Replace(generateYamlFrontmatter(out) + "\n\n" + skillBody)), Mode: 0o644}
	for _, rel := range payloadUnder(impeccableSkillRoot + "/reference") {
		if path.Ext(rel) != ".md" || path.Dir(rel) != impeccableSkillRoot+"/reference" {
			continue
		}
		data, err := payloadFile(rel)
		if err != nil {
			return nil, err
		}
		files["reference/"+path.Base(rel)] = treeFile{Data: []byte(sp.Replace(render(string(data), nil))), Mode: 0o644}
	}
	for _, rel := range payloadUnder(impeccableSkillRoot + "/agents") {
		if path.Ext(rel) != ".md" {
			continue
		}
		data, err := payloadFile(rel)
		if err != nil {
			return nil, err
		}
		afm, abody := parseImpeccableFrontmatter(string(data))
		agent := fmString(afm, "name")
		if agent == "" {
			agent = strings.TrimSuffix(path.Base(rel), ".md")
		}
		rendered := strings.ReplaceAll(render(abody, nil), "{{reference_path}}", "..")
		rendered = sp.Replace(rendered)
		role := strings.TrimPrefix(agent, "impeccable-")
		files["reference/degraded/"+role+".md"] = treeFile{Data: []byte(degradedPreamble + "\n\n" + leadingWSRe.ReplaceAllString(rendered, "")), Mode: 0o644}
	}
	entries, err := manifestEntries()
	if err != nil {
		return nil, err
	}
	modes := map[string]treeFile{}
	for _, e := range entries {
		modes[e.Path] = treeFile{Mode: e.Mode}
	}
	scriptsRoot := impeccableSkillRoot + "/scripts/"
	for _, rel := range payloadUnder(impeccableSkillRoot + "/scripts") {
		name := strings.TrimPrefix(rel, scriptsRoot)
		if strings.HasPrefix(name, "bin/") || path.Base(name) == "config.json" {
			continue
		}
		data, err := payloadFile(rel)
		if err != nil {
			return nil, err
		}
		text := string(data)
		if lower := strings.ToLower(name); strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat") {
			text = crlfRe.ReplaceAllString(text, "\r\n")
		}
		files["scripts/"+name] = treeFile{Data: []byte(replaceScriptProviderMarker(text)), Mode: modes[rel].Mode}
	}
	return files, nil
}

// Rendered impeccable skill written under host/impeccable/skills/impeccable with its own scripts path
func impeccableHostLayout(t *Tree, files map[string]treeFile) error {
	scripts := t.Host("impeccable/skills/impeccable/scripts")
	rendered, err := renderImpeccableSkill(scripts)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(rendered))
	for name := range rendered {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		files["host/impeccable/skills/impeccable/"+name] = rendered[name]
	}
	return nil
}
