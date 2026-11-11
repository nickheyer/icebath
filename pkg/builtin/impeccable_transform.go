package builtin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Source directory beside the skill and destination under the config directory of a provider plugin
type impeccablePluginPayload struct {
	Source string
	Dest   string
}

// One entry of PROVIDERS, the settings a provider build is rendered with
type impeccableProvider struct {
	Provider            string
	ProviderTags        []string
	ConfigDir           string
	DisplayName         string
	PlaceholderProvider string
	FrontmatterFields   []string
	AgentFormat         string
	EmitHooks           string
	HooksManifestRel    string
	VersionInMetadata   bool
	WriteOpenAIMetadata bool
	OmitVersion         bool
	BodyTransform       func(body string, skill impeccableSkill) string
	PluginPayload       *impeccablePluginPayload
}

var (
	fmFull        = []string{"user-invocable", "argument-hint", "license", "compatibility", "metadata"}
	fmFullTools   = []string{"user-invocable", "argument-hint", "license", "compatibility", "metadata", "allowed-tools"}
	fmPlain       = []string{"license", "compatibility", "metadata"}
	fmPlainTools  = []string{"license", "compatibility", "metadata", "allowed-tools"}
	fmInvocable   = []string{"user-invocable", "license", "compatibility", "metadata"}
	fmInvocTools  = []string{"user-invocable", "license", "compatibility", "metadata", "allowed-tools"}
	fmHintedPlain = []string{"argument-hint", "license", "compatibility", "metadata"}
)

// PROVIDERS in declaration order
var impeccableProviders = []impeccableProvider{
	{Provider: "cursor", ProviderTags: []string{"cursor"}, ConfigDir: ".cursor", DisplayName: "Cursor", FrontmatterFields: fmPlain, AgentFormat: "cursor-md", EmitHooks: "cursor", HooksManifestRel: "hooks.json"},
	{Provider: "claude-code", ProviderTags: []string{"claude-code", "claude"}, ConfigDir: ".claude", DisplayName: "Claude Code", FrontmatterFields: fmFull, AgentFormat: "claude-md", EmitHooks: "claude", HooksManifestRel: "settings.json"},
	{Provider: "gemini", ProviderTags: []string{"gemini"}, ConfigDir: ".gemini", DisplayName: "Gemini", FrontmatterFields: []string{}, EmitHooks: "gemini", HooksManifestRel: "settings.json"},
	{Provider: "dsh", ProviderTags: []string{"dsh"}, ConfigDir: ".dsh", DisplayName: "DeepSeek Harness", FrontmatterFields: fmInvocable},
	{Provider: "codex", ProviderTags: []string{"codex"}, ConfigDir: ".codex", DisplayName: "Codex", FrontmatterFields: []string{}, VersionInMetadata: true, WriteOpenAIMetadata: true, EmitHooks: "codex", HooksManifestRel: "hooks.json"},
	{Provider: "agents", ProviderTags: []string{"agents", "codex"}, ConfigDir: ".agents", DisplayName: "Codex Repo Skills", PlaceholderProvider: "codex", FrontmatterFields: []string{}, VersionInMetadata: true, WriteOpenAIMetadata: true},
	{Provider: "github", ProviderTags: []string{"github"}, ConfigDir: ".github", DisplayName: "GitHub Copilot", PlaceholderProvider: "agents", FrontmatterFields: fmFull, AgentFormat: "copilot-agent-md", EmitHooks: "github", HooksManifestRel: "hooks/impeccable.json"},
	{Provider: "kiro", ProviderTags: []string{"kiro"}, ConfigDir: ".kiro", DisplayName: "Kiro", FrontmatterFields: fmPlain},
	{Provider: "opencode", ProviderTags: []string{"opencode"}, ConfigDir: ".opencode", DisplayName: "OpenCode", FrontmatterFields: fmFullTools},
	{Provider: "pi", ProviderTags: []string{"pi"}, ConfigDir: ".pi", DisplayName: "Pi", FrontmatterFields: fmPlainTools},
	{Provider: "omp", ProviderTags: []string{"omp"}, ConfigDir: ".omp", DisplayName: "Oh My Pi", FrontmatterFields: fmPlainTools},
	{Provider: "qoder", ProviderTags: []string{"qoder"}, ConfigDir: ".qoder", DisplayName: "Qoder", FrontmatterFields: fmFullTools},
	{Provider: "trae-cn", ProviderTags: []string{"trae-cn", "trae"}, ConfigDir: ".trae-cn", DisplayName: "Trae China", PlaceholderProvider: "trae", FrontmatterFields: fmFull},
	{Provider: "trae", ProviderTags: []string{"trae"}, ConfigDir: ".trae", DisplayName: "Trae", FrontmatterFields: fmFull},
	{Provider: "rovo-dev", ProviderTags: []string{"rovo-dev"}, ConfigDir: ".rovodev", DisplayName: "Rovo Dev", FrontmatterFields: fmFullTools},
	{Provider: "vibe", ProviderTags: []string{"vibe"}, ConfigDir: ".vibe", DisplayName: "Mistral Vibe", FrontmatterFields: fmInvocTools},
	{Provider: "veto", ProviderTags: []string{"veto"}, ConfigDir: ".veto", DisplayName: "Veto", FrontmatterFields: fmPlain},
	{Provider: "grok", ProviderTags: []string{"grok"}, ConfigDir: ".grok", DisplayName: "Grok Build", FrontmatterFields: fmFullTools, AgentFormat: "claude-md", EmitHooks: "grok", HooksManifestRel: "hooks/impeccable.json"},
	{Provider: "antigravity", ProviderTags: []string{"antigravity"}, ConfigDir: ".agent", DisplayName: "Antigravity", FrontmatterFields: fmPlainTools},
	{Provider: "hermes", ProviderTags: []string{"hermes"}, ConfigDir: ".hermes", DisplayName: "Hermes Agent", FrontmatterFields: fmPlain, PluginPayload: &impeccablePluginPayload{Source: "hermes-plugin", Dest: "plugins/impeccable-design-hook"}},
	{Provider: "devin", ProviderTags: []string{"devin"}, ConfigDir: ".devin", DisplayName: "Devin", FrontmatterFields: fmHintedPlain},
}

var codexSkillProviders = map[string]bool{"agents": true, "codex": true}

// Provider entry by key, false when PROVIDERS has no such key
func impeccableProviderByName(name string) (impeccableProvider, bool) {
	for _, config := range impeccableProviders {
		if config.Provider == name {
			return config, true
		}
	}
	return impeccableProvider{}, false
}

// String with the ECMAScript whitespace class trimmed from both ends
func jsTrim(s string) string {
	return strings.Trim(s, jsSpaceCutset)
}

// String.prototype.replace or replaceAll with a string pattern and its dollar substitution patterns
func jsReplaceLiteral(s, pattern, replacement string, all bool) string {
	if pattern == "" {
		return s
	}
	var b strings.Builder
	rest, offset := s, 0
	for {
		i := strings.Index(rest, pattern)
		if i < 0 {
			break
		}
		b.WriteString(rest[:i])
		at := offset + i
		for j := 0; j < len(replacement); j++ {
			if replacement[j] != '$' || j+1 == len(replacement) {
				b.WriteByte(replacement[j])
				continue
			}
			switch replacement[j+1] {
			case '$':
				b.WriteByte('$')
			case '&':
				b.WriteString(pattern)
			case '`':
				b.WriteString(s[:at])
			case '\'':
				b.WriteString(s[at+len(pattern):])
			default:
				b.WriteByte('$')
				continue
			}
			j++
		}
		rest = rest[i+len(pattern):]
		offset = at + len(pattern)
		if !all {
			break
		}
	}
	b.WriteString(rest)
	return b.String()
}

// Object.keys order, canonical array index keys ascending before the other keys in insertion order
func jsObjectKeys(keys []string) []string {
	var index []uint64
	var named []string
	for _, k := range keys {
		n, err := strconv.ParseUint(k, 10, 64)
		if err == nil && n < 1<<32-1 && strconv.FormatUint(n, 10) == k {
			index = append(index, n)
			continue
		}
		named = append(named, k)
	}
	sort.Slice(index, func(i, j int) bool { return index[i] < index[j] })
	out := make([]string, 0, len(keys))
	for _, n := range index {
		out = append(out, strconv.FormatUint(n, 10))
	}
	return append(out, named...)
}

// Skill name with each dash separated part capitalised and joined by spaces
func humanizeSkillName(name string) string {
	parts := strings.Split(name, "-")
	upper := cases.Upper(language.Und)
	for i, part := range parts {
		if part == "" {
			continue
		}
		first := []rune(part)[0]
		if first > 0xFFFF {
			continue
		}
		parts[i] = upper.String(string(first)) + part[len(string(first)):]
	}
	return strings.Join(parts, " ")
}

// Description clipped to the maximum UTF-16 length at a word boundary with an ellipsis
func summarizeDescription(description any, maxLength int) (any, error) {
	if !jsTruthy(description) {
		return description, nil
	}
	text, ok := description.(string)
	if !ok {
		if list, isList := description.([]any); isList && len(list) <= maxLength {
			return description, nil
		}
		return nil, errors.New("TypeError: description.slice(...).trimEnd is not a function")
	}
	units := utf16Units(text)
	if len(units) <= maxLength {
		return text, nil
	}
	clipped := units[:maxLength-1]
	lastSpace := -1
	for i := len(clipped) - 1; i >= 0; i-- {
		if clipped[i] == ' ' {
			lastSpace = i
			break
		}
	}
	if lastSpace > 48 {
		clipped = clipped[:lastSpace]
	}
	return strings.TrimRight(string(utf16.Decode(clipped)), jsSpaceCutset) + "...", nil
}

// agents/openai.yaml interface block of a skill
func buildOpenAIMetadata(skill impeccableSkill) ([]fmPair, error) {
	displayName := humanizeSkillName(skill.Name)
	short, err := summarizeDescription(skill.Description, 88)
	if err != nil {
		return nil, err
	}
	return []fmPair{{"interface", []fmPair{
		{"display_name", displayName},
		{"short_description", short},
		{"default_prompt", "Use " + displayName + " to redesign, critique, audit, or polish this frontend."},
	}}}, nil
}

// Plain YAML document of an ordered object
func generateYamlDocument(data []fmPair) string {
	return strings.Join(appendYamlObject(nil, data, 0), "\n")
}

func formatTomlString(value any) string {
	return jsonQuote(jsString(value))
}

// TOML multiline string, literal unless the text holds three single quotes
func formatTomlMultiline(value string) string {
	normalized := strings.ReplaceAll(jsTrim(value), "\r\n", "\n")
	if !strings.Contains(normalized, "'''") {
		return "'''\n" + normalized + "\n'''"
	}
	return "\"\"\"\n" + strings.ReplaceAll(strings.ReplaceAll(normalized, `\`, `\\`), `"""`, `\"""`) + "\n\"\"\""
}

func codexAgentName(agent impeccableAgent) string {
	if agent.CodexName != "" {
		return agent.CodexName
	}
	return strings.ReplaceAll(agent.Name, "-", "_")
}

func claudeAgentName(agent impeccableAgent) string {
	if agent.ClaudeName != "" {
		return agent.ClaudeName
	}
	return agent.Name
}

// Codex subagent TOML with name, description, effort, nicknames and developer instructions
func buildCodexAgent(agent impeccableAgent, body string) (string, error) {
	lines := []string{
		"name = " + formatTomlString(codexAgentName(agent)),
		"description = " + formatTomlString(agent.Description),
	}
	if jsTruthy(agent.Effort) {
		lines = append(lines, "model_reasoning_effort = "+formatTomlString(agent.Effort))
	}
	switch nick := agent.NicknameCandidates.(type) {
	case []any:
		if len(nick) > 0 {
			quoted := make([]string, len(nick))
			for i, v := range nick {
				quoted[i] = formatTomlString(v)
			}
			lines = append(lines, "nickname_candidates = ["+strings.Join(quoted, ", ")+"]")
		}
	case string:
		if nick != "" {
			return "", errors.New("TypeError: values.map is not a function")
		}
	}
	lines = append(lines, "developer_instructions = "+formatTomlMultiline(body))
	return strings.Join(lines, "\n") + "\n", nil
}

// Claude subagent markdown with name, description, tools, model, effort and maxTurns frontmatter
func buildClaudeAgent(agent impeccableAgent, body string) string {
	fm := []fmPair{{"name", claudeAgentName(agent)}, {"description", agent.Description}}
	for _, opt := range []fmPair{{"tools", agent.Tools}, {"model", agent.Model}, {"effort", agent.Effort}, {"maxTurns", agent.MaxTurns}} {
		if jsTruthy(opt.Value) {
			fm = append(fm, opt)
		}
	}
	return generateYamlFrontmatter(fm) + "\n" + jsTrim(body) + "\n"
}

// Copilot custom agent markdown carrying only name and description
func buildCopilotAgent(agent impeccableAgent, body string) string {
	return generateYamlFrontmatter([]fmPair{{"name", agent.Name}, {"description", agent.Description}}) + "\n" + jsTrim(body) + "\n"
}

// Cursor subagent markdown with model, readonly for roles without Write or Edit, and is_background false
func buildCursorAgent(agent impeccableAgent, body string) string {
	model := agent.Model
	if !jsTruthy(model) {
		model = "inherit"
	}
	fm := []fmPair{{"name", agent.Name}, {"description", agent.Description}, {"model", model}}
	toolText := ""
	if jsTruthy(agent.Tools) {
		toolText = jsString(agent.Tools)
	}
	count, writes := 0, false
	for _, tool := range strings.Split(toolText, ",") {
		if tool = jsTrim(tool); tool != "" {
			count++
			writes = writes || tool == "Write" || tool == "Edit"
		}
	}
	if count > 0 && !writes {
		fm = append(fm, fmPair{"readonly", true})
	}
	fm = append(fm, fmPair{"is_background", false})
	return generateYamlFrontmatter(fm) + "\n" + jsTrim(body) + "\n"
}

// Settings an agent body is rendered with for one emitted surface
type agentRender struct {
	ProviderTags  []string
	TokenKey      string
	AllSkillNames []string
	ScriptsPath   string
	EmitDir       string
	SkillDir      string
}

// Agent body with provider blocks, tokens, rule markers, reference path and scripts path resolved
func renderAgentBody(agent impeccableAgent, r agentRender) (string, error) {
	body := compileProviderBlocks(agent.Body, r.ProviderTags)
	body = replaceProviderTokens(body, r.TokenKey, nil, r.AllSkillNames)
	body = stripRuleMarkers(body)
	rel, err := filepath.Rel(r.EmitDir, filepath.Join(r.SkillDir, "reference"))
	if err != nil {
		return "", err
	}
	body = jsReplaceLiteral(body, "{{reference_path}}", filepath.ToSlash(rel), true)
	return jsReplaceLiteral(body, "{{scripts_path}}", r.ScriptsPath, true), nil
}

// File name and content of an agent in the provider agent format, false for an unknown format
func buildAgentFile(config impeccableProvider, agent impeccableAgent, body string) (string, string, bool, error) {
	switch config.AgentFormat {
	case "codex-toml":
		content, err := buildCodexAgent(agent, body)
		return codexAgentName(agent) + ".toml", content, true, err
	case "claude-md":
		return claudeAgentName(agent) + ".md", buildClaudeAgent(agent, body), true, nil
	case "copilot-agent-md":
		return agent.Name + ".agent.md", buildCopilotAgent(agent, body), true, nil
	case "cursor-md":
		return agent.Name + ".md", buildCursorAgent(agent, body), true, nil
	}
	return "", "", false, nil
}

// Creates a directory with its parents when it does not exist yet
func ensureDir(dirPath string) error {
	if pathExists(dirPath) {
		return nil
	}
	return os.MkdirAll(dirPath, 0o777)
}

// Removes a directory with everything in it when it exists
func cleanDir(dirPath string) error {
	if !pathExists(dirPath) {
		return nil
	}
	return os.RemoveAll(dirPath)
}

// Writes a text file, creating its parent directories
func writeBuildFile(filePath, content string) error {
	if err := ensureDir(filepath.Dir(filePath)); err != nil {
		return err
	}
	return os.WriteFile(filePath, []byte(content), 0o666)
}

// Frontmatter key and value a provider field contributes for a skill, false when it contributes none
func frontmatterField(field string, skill impeccableSkill) (string, any, bool) {
	switch field {
	case "user-invocable":
		return field, true, skill.UserInvocable
	case "argument-hint":
		return field, skill.ArgumentHint, skill.UserInvocable && jsTruthy(skill.ArgumentHint)
	case "license":
		return field, skill.License, true
	case "compatibility":
		return field, skill.Compatibility, true
	case "metadata":
		return field, skill.Metadata, true
	case "allowed-tools":
		return field, skill.AllowedTools, true
	}
	return "", nil, false
}

func fmSet(pairs []fmPair, key string, value any) []fmPair {
	for i := range pairs {
		if pairs[i].Key == key {
			pairs[i].Value = value
			return pairs
		}
	}
	return append(pairs, fmPair{key, value})
}

// Object spread of a frontmatter value, index keys for lists and strings
func jsSpread(v any) []fmPair {
	var out []fmPair
	switch x := v.(type) {
	case []fmPair:
		out = append(out, x...)
	case []any:
		for i, item := range x {
			out = append(out, fmPair{strconv.Itoa(i), item})
		}
	case string:
		for i, unit := range utf16Units(x) {
			out = append(out, fmPair{strconv.Itoa(i), string(utf16.Decode([]uint16{unit}))})
		}
	}
	return out
}

// Command names of command-metadata.json grouped by category for the argument hint
func commandHintGroups(metadata string) (string, error) {
	parsed, err := pyjson.Loads(metadata)
	if err != nil {
		return "", err
	}
	var commands []string
	switch x := parsed.(type) {
	case *pyjson.Object:
		commands = jsObjectKeys(x.Keys)
	case []any:
		for i := range x {
			commands = append(commands, strconv.Itoa(i))
		}
	case string:
		for i := range utf16Units(x) {
			commands = append(commands, strconv.Itoa(i))
		}
	case nil:
		return "", errors.New("TypeError: Cannot convert undefined or null to object")
	}
	var groups []string
	for _, cat := range categoryOrder {
		var in []string
		for _, c := range commands {
			if skillCategories[c] == cat {
				in = append(in, c)
			}
		}
		if len(in) > 0 {
			groups = append(groups, strings.Join(in, "|"))
		}
	}
	return strings.Join(groups, " · "), nil
}

// Writes the build of every skill for this provider under dist/<provider> and logs one summary line
func (config impeccableProvider) transform(skills []impeccableSkill, distDir string, skillsVersion any, log io.Writer) error {
	provider, configDir := config.Provider, config.ConfigDir
	tokenKey := config.PlaceholderProvider
	if tokenKey == "" {
		tokenKey = provider
	}
	providerTags := config.ProviderTags
	if providerTags == nil {
		providerTags = []string{provider}
	}
	providerDir := filepath.Join(distDir, provider)
	configRoot := filepath.Join(providerDir, filepath.FromSlash(configDir))
	skillsDir := filepath.Join(configRoot, "skills")
	if err := cleanDir(providerDir); err != nil {
		return err
	}
	if err := ensureDir(skillsDir); err != nil {
		return err
	}
	var allSkillNames, commandNames []string
	for _, s := range skills {
		allSkillNames = append(allSkillNames, s.Name)
		if s.UserInvocable {
			commandNames = append(commandNames, s.Name)
		}
	}
	refCount, scriptCount, agentCount := 0, 0, 0
	withVersion := jsTruthy(skillsVersion) && !config.OmitVersion

	for _, skill := range skills {
		skillDir := filepath.Join(skillsDir, skill.Name)
		fm := []fmPair{{"name", skill.Name}, {"description", skill.Description}}
		if withVersion && !config.VersionInMetadata {
			fm = fmSet(fm, "version", skillsVersion)
		}
		for _, field := range config.FrontmatterFields {
			key, val, ok := frontmatterField(field, skill)
			if ok && jsTruthy(val) {
				fm = fmSet(fm, key, val)
			}
		}
		if withVersion && config.VersionInMetadata {
			fm = fmSet(fm, "metadata", fmSet(jsSpread(fmLookup(fm, "metadata")), "version", skillsVersion))
		}
		switch hint := fmLookup(fm, "argument-hint").(type) {
		case string:
			if !strings.Contains(hint, "{{command_hint}}") {
				break
			}
			for _, script := range skill.Scripts {
				if script.Name != "command-metadata.json" {
					continue
				}
				grouped, err := commandHintGroups(script.Content)
				if err != nil {
					return err
				}
				fm = fmSet(fm, "argument-hint", jsReplaceLiteral(hint, "{{command_hint}}", grouped, false))
				break
			}
		case []any:
			for _, item := range hint {
				if item == "{{command_hint}}" {
					return errors.New("TypeError: frontmatterObj['argument-hint'].replace is not a function")
				}
			}
		case nil:
		default:
			return errors.New("TypeError: frontmatterObj['argument-hint']?.includes is not a function")
		}
		frontmatter := generateYamlFrontmatter(fm)

		scriptsPath := configDir + "/skills/" + skill.Name + "/scripts"
		skillBody := compileProviderBlocks(skill.Body, providerTags)
		skillBody = replaceProviderTokens(skillBody, tokenKey, commandNames, allSkillNames)
		skillBody = stripRuleMarkers(skillBody)
		skillBody = jsReplaceLiteral(skillBody, "{{scripts_path}}", scriptsPath, true)
		if config.BodyTransform != nil {
			skillBody = config.BodyTransform(skillBody, skill)
		}
		content := jsReplaceLiteral(frontmatter+"\n\n"+skillBody, "{{scripts_path}}", scriptsPath, true)
		if err := writeBuildFile(filepath.Join(skillDir, "SKILL.md"), content); err != nil {
			return err
		}

		if config.WriteOpenAIMetadata {
			meta, err := buildOpenAIMetadata(skill)
			if err != nil {
				return err
			}
			if err := writeBuildFile(filepath.Join(skillDir, "agents", "openai.yaml"), generateYamlDocument(meta)); err != nil {
				return err
			}
		}

		if len(skill.References) > 0 {
			refDir := filepath.Join(skillDir, "reference")
			if err := ensureDir(refDir); err != nil {
				return err
			}
			for _, ref := range skill.References {
				refContent := compileProviderBlocks(ref.Content, providerTags)
				refContent = replaceProviderTokens(refContent, tokenKey, nil, allSkillNames)
				refContent = stripRuleMarkers(refContent)
				refContent = jsReplaceLiteral(refContent, "{{scripts_path}}", scriptsPath, true)
				if err := writeBuildFile(filepath.Join(refDir, ref.Name+".md"), refContent); err != nil {
					return err
				}
				refCount++
			}
		}

		if len(skill.Agents) > 0 {
			degradedDir := filepath.Join(skillDir, "reference", "degraded")
			if err := ensureDir(degradedDir); err != nil {
				return err
			}
			for _, agent := range skill.Agents {
				role := strings.TrimPrefix(agent.Name, "impeccable-")
				body, err := renderAgentBody(agent, agentRender{providerTags, tokenKey, allSkillNames, scriptsPath, degradedDir, skillDir})
				if err != nil {
					return err
				}
				degraded := degradedPreamble + "\n\n" + strings.TrimLeft(body, jsSpaceCutset)
				if err := writeBuildFile(filepath.Join(degradedDir, role+".md"), degraded); err != nil {
					return err
				}
				refCount++
			}
		}

		if len(skill.Scripts) > 0 {
			scriptsOutDir := filepath.Join(skillDir, "scripts")
			if err := ensureDir(scriptsOutDir); err != nil {
				return err
			}
			for _, script := range skill.Scripts {
				outPath := filepath.Join(scriptsOutDir, filepath.FromSlash(script.Name))
				if err := writeBuildFile(outPath, replaceProviderScriptMarker(script.Content, tokenKey, provider)); err != nil {
					return err
				}
				if script.Mode != 0 {
					if err := os.Chmod(outPath, script.Mode); err != nil {
						return err
					}
				}
				scriptCount++
			}
		}

		if codexSkillProviders[provider] {
			for _, agent := range skill.Agents {
				if agent.Providers != nil && !slices.Contains(agent.Providers, "codex") {
					continue
				}
				agentsOut := filepath.Join(skillDir, "agents")
				body, err := renderAgentBody(agent, agentRender{providerTags, tokenKey, allSkillNames, scriptsPath, agentsOut, skillDir})
				if err != nil {
					return err
				}
				toml, err := buildCodexAgent(agent, body)
				if err != nil {
					return err
				}
				if err := writeBuildFile(filepath.Join(agentsOut, codexAgentName(agent)+".toml"), toml); err != nil {
					return err
				}
			}
		}
	}

	if provider == "opencode" && len(skills) > 0 {
		commandsDir := filepath.Join(configRoot, "commands")
		if err := ensureDir(commandsDir); err != nil {
			return err
		}
		for _, skill := range skills {
			bridgeBody := "Call skill({ name: \"" + skill.Name + "\" }) and follow its `Setup` and `Commands` sections to handle $ARGUMENTS.\n"
			bridge := generateYamlFrontmatter([]fmPair{{"description", skill.Description}, {"agent", "build"}, {"subtask", true}})
			text := strings.TrimRight(bridge+"\n"+bridgeBody, "\n") + "\n"
			if err := writeBuildFile(filepath.Join(commandsDir, skill.Name+".md"), text); err != nil {
				return err
			}
		}
	}

	if config.AgentFormat != "" {
		agentsDir := filepath.Join(configRoot, "agents")
		for _, skill := range skills {
			scriptsPath := configDir + "/skills/" + skill.Name + "/scripts"
			for _, agent := range skill.Agents {
				if agent.Providers != nil && !slices.Contains(agent.Providers, provider) {
					continue
				}
				body, err := renderAgentBody(agent, agentRender{providerTags, tokenKey, allSkillNames, scriptsPath, agentsDir, filepath.Join(skillsDir, skill.Name)})
				if err != nil {
					return err
				}
				filename, content, ok, err := buildAgentFile(config, agent, body)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				if err := writeBuildFile(filepath.Join(agentsDir, filename), content); err != nil {
					return err
				}
				agentCount++
			}
		}
	}

	hooksRel := config.HooksManifestRel
	if hooksRel == "" {
		hooksRel = filepath.Join("hooks", "hooks.json")
	}
	hooksEmitted := false
	if config.EmitHooks != "" {
		if manifest := hooksJSONFor(config.EmitHooks, configDir); manifest != nil {
			if err := writeBuildFile(filepath.Join(configRoot, filepath.FromSlash(hooksRel)), jsonStringify(manifest, "  ")+"\n"); err != nil {
				return err
			}
			hooksEmitted = true
		}
	}

	pluginInfo := ""
	if config.PluginPayload != nil {
		wrote := false
		for _, skill := range skills {
			for _, file := range skill.Payloads[config.PluginPayload.Source] {
				dest := filepath.Join(configRoot, filepath.FromSlash(config.PluginPayload.Dest), file.Name)
				if err := writeBuildFile(dest, file.Content); err != nil {
					return err
				}
				wrote = true
			}
		}
		if wrote {
			pluginInfo = " (" + config.PluginPayload.Dest + ")"
		}
	}

	summary := fmt.Sprintf("✓ %s: %d skill", config.DisplayName, len(skills))
	if len(skills) != 1 {
		summary += "s"
	}
	if refCount > 0 {
		summary += fmt.Sprintf(" (%d reference files)", refCount)
	}
	if scriptCount > 0 {
		summary += fmt.Sprintf(" (%d script files)", scriptCount)
	}
	if agentCount > 0 {
		summary += fmt.Sprintf(" (%d agent files)", agentCount)
	}
	if hooksEmitted {
		summary += " (" + hooksRel + ")"
	}
	_, err := fmt.Fprintln(log, summary+pluginInfo)
	return err
}
