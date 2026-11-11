package builtin

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// The ECMAScript undefined value where a JSON document has no such property
type jsUndefinedType struct{}

var jsUndefined = jsUndefinedType{}

// Manifest keys confirmed to load in a real Claude Code install
var knownLoaderKeys = []string{"name", "description", "version", "author", "homepage", "repository", "skills"}

const jsLineBreaks = "\n\r\u2028\u2029"

var (
	skillFrontmatterRe = regexp.MustCompile(`^---[ \t]*\r?\n([\s\S]*?)\r?\n---(?:[ \t]*\r?\n|[ \t]*$)`)
	versionLineRe      = regexp.MustCompile(`^version:[` + jsSpaceClass + `]*([^` + jsLineBreaks + `]+?)[` + jsSpaceClass + `]*$`)
	metadataLineRe     = regexp.MustCompile(`^metadata:[` + jsSpaceClass + `]*(?:#[^` + jsLineBreaks + `]*)?$`)
)

// One version bearing file and the value read from it
type pluginVersionCheck struct {
	RelPath string
	Found   any
}

// One file whose version disagrees with the root manifest
type pluginVersionMismatch struct {
	RelPath  string
	Found    any
	Expected any
}

// One file with its reason, a read or parse failure or a manifest shape finding
type pluginFinding struct {
	RelPath string
	Reason  string
}

// Outcome of comparing every version bearing file with the root manifest, Source nil when there is none
type pluginVersions struct {
	Source     any
	Checked    []pluginVersionCheck
	Mismatches []pluginVersionMismatch
	Errors     []pluginFinding
}

// Strict JSON.parse of a document into ordered values
func jsonParse(raw string) (any, error) {
	var probe any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil, err
	}
	return pyjson.Loads(raw)
}

// Property read on a parsed JSON value, an error where ECMAScript throws on null
func jsonProp(v any, key string) (any, error) {
	if v == nil {
		return nil, errors.New("Cannot read properties of null (reading '" + key + "')")
	}
	if obj, ok := v.(*pyjson.Object); ok {
		if got, has := obj.Get(key); has {
			return got, nil
		}
	}
	return jsUndefined, nil
}

// Optional chained property read, undefined for null, undefined and missing properties
func jsonOptionalProp(v any, key string) any {
	switch x := v.(type) {
	case *pyjson.Object:
		if got, has := x.Get(key); has {
			return got
		}
	case []any:
		if i, err := strconv.Atoi(key); err == nil && strconv.Itoa(i) == key && i >= 0 && i < len(x) {
			return x[i]
		}
	case string:
		units := utf16Units(x)
		if i, err := strconv.Atoi(key); err == nil && strconv.Itoa(i) == key && i >= 0 && i < len(units) {
			return string(utf16.Decode(units[i : i+1]))
		}
	}
	return jsUndefined
}

// Template literal text of a parsed JSON value
func jsonValueText(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case jsUndefinedType:
		return "undefined"
	case int64:
		return jsNumber(x).String()
	case float64:
		return jsNumber(x).String()
	case *big.Int:
		f, _ := new(big.Float).SetInt(x).Float64()
		return jsNumber(f).String()
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			if item != nil && item != jsUndefined {
				parts[i] = jsonValueText(item)
			}
		}
		return strings.Join(parts, ",")
	case *pyjson.Object:
		return "[object Object]"
	}
	return jsString(v)
}

func jsonNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case *big.Int:
		f, _ := new(big.Float).SetInt(x).Float64()
		return f, true
	}
	return 0, false
}

// Strict equality of two parsed JSON values, objects and arrays never equal across documents
func jsonStrictEqual(a, b any) bool {
	if fa, ok := jsonNumber(a); ok {
		fb, isNum := jsonNumber(b)
		return isNum && fa == fb
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case jsUndefinedType:
		return b == jsUndefined
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	}
	return false
}

// Version of a SKILL.md frontmatter block, metadata.version before the top level key, false without one
func readSkillFrontmatterVersion(content string) (string, bool) {
	fm := skillFrontmatterRe.FindStringSubmatch(content)
	if fm == nil {
		return "", false
	}
	metadataVersion, topLevelVersion := "", ""
	inMetadata := false
	metadataIndent := -1
	for _, line := range lineSplitRe.Split(fm[1], -1) {
		trimmedStart := strings.TrimLeft(line, jsSpaceCutset)
		if jsTrim(line) == "" || strings.HasPrefix(trimmedStart, "#") {
			continue
		}
		indentText := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		indent := len(strings.ReplaceAll(indentText, "\t", "  "))
		if indent == 0 {
			inMetadata = metadataLineRe.MatchString(line)
			metadataIndent = -1
			if m := versionLineRe.FindStringSubmatch(line); m != nil {
				topLevelVersion = m[1]
			}
			continue
		}
		if !inMetadata {
			continue
		}
		if metadataIndent < 0 {
			metadataIndent = indent
		}
		if indent != metadataIndent {
			continue
		}
		if m := versionLineRe.FindStringSubmatch(jsTrim(line)); m != nil {
			metadataVersion = m[1]
		}
	}
	value := metadataVersion
	if value == "" {
		value = topLevelVersion
	}
	if value == "" {
		return "", false
	}
	value = jsTrim(value)
	if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
		value = value[1 : n-1]
	}
	return value, true
}

// Value extracted from a file, with the reason text when the read or the extraction fails
func extractFromFile(absPath string, extract func(raw string) (any, error)) (any, string) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		code, ok := pypath.ErrnoName(err)
		if !ok {
			code = err.Error()
		}
		return nil, "could not read file (" + code + ")"
	}
	value, err := extract(decodeUTF8Lossy(data))
	if err != nil {
		return nil, "could not parse (" + err.Error() + ")"
	}
	return value, ""
}

// Reader of the version property of a JSON manifest
func jsonVersion(raw string) (any, error) {
	parsed, err := jsonParse(raw)
	if err != nil {
		return nil, err
	}
	return jsonProp(parsed, "version")
}

// Marks a file that is not the artifact its reader checks
type pluginVersionSkip struct{}

// Compares every version bearing plugin and skill file with root .claude-plugin/plugin.json
func collectPluginVersions(rootDir string) pluginVersions {
	const rootRel = ".claude-plugin/plugin.json"
	rootManifestPath := filepath.Join(rootDir, filepath.FromSlash(rootRel))
	if !pathExists(rootManifestPath) {
		return pluginVersions{}
	}
	source, reason := extractFromFile(rootManifestPath, jsonVersion)
	if reason != "" {
		return pluginVersions{Errors: []pluginFinding{{rootRel, reason}}}
	}
	if source == nil || source == jsUndefined {
		return pluginVersions{Errors: []pluginFinding{{rootRel, `missing "version" field`}}}
	}
	skillVersion := func(raw string) (any, error) {
		if v, ok := readSkillFrontmatterVersion(raw); ok {
			return v, nil
		}
		return nil, nil
	}
	type check struct {
		relPath string
		read    func(raw string) (any, error)
	}
	checks := []check{
		{"package.json", func(raw string) (any, error) {
			pkg, err := jsonParse(raw)
			if err != nil {
				return nil, err
			}
			deps, err := jsonProp(pkg, "optionalDependencies")
			if err != nil {
				return nil, err
			}
			if obj, ok := deps.(*pyjson.Object); ok {
				for _, k := range obj.Keys {
					if strings.HasPrefix(k, "@impeccable/cli-") {
						return jsonProp(pkg, "version")
					}
				}
			}
			return pluginVersionSkip{}, nil
		}},
		{".claude-plugin/marketplace.json", func(raw string) (any, error) {
			parsed, err := jsonParse(raw)
			if err != nil {
				return nil, err
			}
			plugins, err := jsonProp(parsed, "plugins")
			if err != nil {
				return nil, err
			}
			return jsonOptionalProp(jsonOptionalProp(plugins, "0"), "version"), nil
		}},
		{"plugin/.claude-plugin/plugin.json", jsonVersion},
		{"dist/openai/impeccable/.codex-plugin/plugin.json", jsonVersion},
	}
	for _, prefix := range []string{"cursor-plugin", "dist/cursor-plugin"} {
		checks = append(checks, check{prefix + "/.cursor-plugin/plugin.json", jsonVersion}, check{prefix + "/skills/impeccable/SKILL.md", skillVersion})
	}
	checks = append(checks, check{"plugin/skills/impeccable/SKILL.md", skillVersion})

	out := pluginVersions{Source: source}
	for _, c := range checks {
		absPath := filepath.Join(rootDir, filepath.FromSlash(c.relPath))
		if !pathExists(absPath) {
			continue
		}
		found, reason := extractFromFile(absPath, c.read)
		if reason != "" {
			out.Errors = append(out.Errors, pluginFinding{c.relPath, reason})
			continue
		}
		if _, skip := found.(pluginVersionSkip); skip {
			continue
		}
		out.Checked = append(out.Checked, pluginVersionCheck{c.relPath, found})
		if !jsonStrictEqual(found, source) {
			out.Mismatches = append(out.Mismatches, pluginVersionMismatch{c.relPath, found, source})
		}
	}
	return out
}

// Sorted names of the markdown files directly inside a directory, none when it is missing
func listMarkdown(dir string) ([]string, error) {
	if !pathExists(dir) {
		return nil, nil
	}
	names, err := markdownNames(dir)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// Source agent file and the file name the claude-code build ships it under
type claudeAgentFile struct {
	SourceFile  string
	ShippedFile string
}

// Agent files the claude-code build emits from skill/agents, honouring name, claude-name and providers
func expectedClaudeAgentFiles(rootDir string) ([]claudeAgentFile, error) {
	agentsDir := filepath.Join(rootDir, "skill", "agents")
	sources, err := listMarkdown(agentsDir)
	if err != nil {
		return nil, err
	}
	var expected []claudeAgentFile
	for _, sourceFile := range sources {
		text, err := readTextFile(filepath.Join(agentsDir, sourceFile))
		if err != nil {
			return nil, err
		}
		fm, _ := parseImpeccableFrontmatter(text)
		if providers := agentProviders(fmLookup(fm, "providers")); providers != nil && !slices.Contains(providers, "claude-code") {
			continue
		}
		name := fmOr(fm, "name", strings.TrimSuffix(sourceFile, ".md"))
		expected = append(expected, claudeAgentFile{sourceFile, jsString(fmOr(fm, "claude-name", name)) + ".md"})
	}
	return expected, nil
}

// Findings where plugin/.claude-plugin/plugin.json or plugin/agents breaks the verified loader contract
func collectPluginManifestFindings(rootDir string) ([]pluginFinding, error) {
	const manifestRel = "plugin/.claude-plugin/plugin.json"
	manifestPath := filepath.Join(rootDir, filepath.FromSlash(manifestRel))
	if !pathExists(manifestPath) {
		return nil, nil
	}
	var parsed any
	data, err := os.ReadFile(manifestPath)
	if err == nil {
		parsed, err = jsonParse(decodeUTF8Lossy(data))
	}
	if err != nil {
		return []pluginFinding{{manifestRel, "could not parse (" + err.Error() + ")"}}, nil
	}
	manifest, ok := parsed.(*pyjson.Object)
	if !ok {
		kind := "a number"
		switch parsed.(type) {
		case []any:
			kind = "an array"
		case nil:
			kind = "null"
		case string:
			kind = "a string"
		case bool:
			kind = "a boolean"
		}
		return []pluginFinding{{manifestRel, "manifest is " + kind + ", not a JSON object"}}, nil
	}

	var findings []pluginFinding
	if _, has := manifest.Get("agents"); has {
		findings = append(findings, pluginFinding{manifestRel, `declares an "agents" key; Claude Code loads zero agents when it is present ` +
			"as file paths and fails the whole plugin on other shapes. Omit it and let " +
			"the loader auto-discover plugin/agents/*.md (PR #494)"})
	}
	for _, key := range jsObjectKeys(manifest.Keys) {
		if key == "agents" {
			continue
		}
		if !slices.Contains(knownLoaderKeys, key) {
			findings = append(findings, pluginFinding{manifestRel, `unverified manifest key "` + key + `"; confirm it loads in a real Claude Code ` +
				"install, then add it to KNOWN_LOADER_KEYS in scripts/lib/validate-plugin-manifest.js"})
		}
	}
	if skills, has := manifest.Get("skills"); !has || skills != "./skills/" {
		shown := "undefined"
		if has {
			shown = jsonStringify(skills, "")
		}
		findings = append(findings, pluginFinding{manifestRel, `"skills" is ` + shown + `; must be "./skills/" ` +
			"(trailing-slash form, issue #86)"})
	}

	shippedAgents, err := listMarkdown(filepath.Join(rootDir, "plugin", "agents"))
	if err != nil {
		return nil, err
	}
	expected, err := expectedClaudeAgentFiles(rootDir)
	if err != nil {
		return nil, err
	}
	for _, agent := range expected {
		if !slices.Contains(shippedAgents, agent.ShippedFile) {
			findings = append(findings, pluginFinding{"plugin/agents/" + agent.ShippedFile, "missing from the plugin subtree; auto-discovery relies on the shipped " +
				"file, so skill/agents/" + agent.SourceFile + " would never load. Run `bun run build:release`"})
		}
	}
	return findings, nil
}
