package builtin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var engineVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

// JSON.stringify of a JSON value with the given indentation unit, undefined members left out
func jsonStringify(v any, gap string) string {
	return jsonStringifyAt(v, gap, "")
}

func jsonStringifyAt(v any, gap, indent string) string {
	inner := indent + gap
	open, sep, closing, colon := "", ",", "", ":"
	if gap != "" {
		open, sep, closing, colon = "\n"+inner, ",\n"+inner, "\n"+indent, ": "
	}
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(x)
	case string:
		return jsonQuote(x)
	case int:
		return strconv.Itoa(x)
	case []any:
		if len(x) == 0 {
			return "[]"
		}
		parts := make([]string, len(x))
		for i, item := range x {
			if item == jsUndefined {
				item = nil
			}
			parts[i] = jsonStringifyAt(item, gap, inner)
		}
		return "[" + open + strings.Join(parts, sep) + closing + "]"
	case *pyjson.Object:
		var parts []string
		for _, k := range jsObjectKeys(x.Keys) {
			item, _ := x.Get(k)
			if item == jsUndefined {
				continue
			}
			parts = append(parts, jsonQuote(k)+colon+jsonStringifyAt(item, gap, inner))
		}
		if len(parts) == 0 {
			return "{}"
		}
		return "{" + open + strings.Join(parts, sep) + closing + "}"
	}
	return jsonValueText(v)
}

// ECMAScript truthiness of a parsed JSON value
func jsonTruthy(v any) bool {
	if f, ok := jsonNumber(v); ok {
		return f != 0
	}
	switch x := v.(type) {
	case nil, jsUndefinedType:
		return false
	case string:
		return x != ""
	case bool:
		return x
	}
	return true
}

// Object spread of a parsed JSON value into a fresh ordered object
func jsonSpread(v any) *pyjson.Object {
	out := pyjson.NewObject()
	switch x := v.(type) {
	case *pyjson.Object:
		for _, k := range jsObjectKeys(x.Keys) {
			item, _ := x.Get(k)
			out.Set(k, item)
		}
	case []any:
		for i, item := range x {
			out.Set(strconv.Itoa(i), item)
		}
	case string:
		for i, unit := range utf16Units(x) {
			out.Set(strconv.Itoa(i), string(utf16.Decode([]uint16{unit})))
		}
	}
	return out
}

// Parsed JSON value as the frontmatter value the transformer prints, undefined as the empty default
func jsonToFrontmatter(v any) any {
	if f, ok := jsonNumber(v); ok {
		return jsNumber(f)
	}
	switch x := v.(type) {
	case jsUndefinedType:
		return ""
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = jsonToFrontmatter(item)
		}
		return out
	case *pyjson.Object:
		out := []fmPair{}
		for _, k := range jsObjectKeys(x.Keys) {
			item, _ := x.Get(k)
			out = append(out, fmPair{k, jsonToFrontmatter(item)})
		}
		return out
	}
	return v
}

// Copies a directory tree, each file with its permission bits
func copyDirSync(src, dest string) error {
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath, destPath := filepath.Join(src, entry.Name()), filepath.Join(dest, entry.Name())
		if entry.IsDir() {
			if err := copyDirSync(srcPath, destPath); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		info, err := os.Stat(srcPath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(destPath, data, info.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Chmod(destPath, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// Trimmed text of the root ENGINE_VERSION file
func readEngineVersion(rootDir string) (string, error) {
	text, err := readTextFile(filepath.Join(rootDir, "ENGINE_VERSION"))
	return jsTrim(text), err
}

// Writes skill/scripts/VERSION from ENGINE_VERSION when it differs, refusing a non semver pin
func syncEngineVersionFile(rootDir string, out io.Writer) error {
	version, err := readEngineVersion(rootDir)
	if err != nil {
		return err
	}
	if !engineVersionRe.MatchString(version) {
		return fmt.Errorf("ENGINE_VERSION must be a semver string, got \"%s\"", version)
	}
	dest := filepath.Join(rootDir, "skill", "scripts", "VERSION")
	write := true
	if pathExists(dest) {
		current, err := readTextFile(dest)
		if err != nil {
			return err
		}
		write = current != version+"\n"
	}
	if write {
		if err := os.WriteFile(dest, []byte(version+"\n"), 0o666); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(out, "✓ Engine pinned at v%s (skill/scripts/VERSION)\n", version)
	return err
}

// Count of skills whose description exceeds 1024 UTF-16 units, each reported on the error stream
func validateSkillFrontmatter(skills []impeccableSkill, errOut io.Writer) int {
	count := 0
	for _, skill := range skills {
		if !jsTruthy(skill.Description) {
			continue
		}
		length := -1
		switch d := skill.Description.(type) {
		case string:
			length = len(utf16Units(d))
		case []any:
			length = len(d)
		}
		if length > 1024 {
			fmt.Fprintf(errOut, "❌ %s: invalid description: exceeds maximum length of 1024 characters (%d)\n", skill.FilePath, length)
			count++
		}
	}
	return count
}

// Reports plugin and skill version drift and returns the number of problems
func validatePluginVersions(rootDir string, out, errOut io.Writer) int {
	versions := collectPluginVersions(rootDir)
	if versions.Source == nil && len(versions.Errors) == 0 {
		return 0
	}
	for _, e := range versions.Errors {
		fmt.Fprintf(errOut, "  ❌ %s: %s\n", e.RelPath, e.Reason)
	}
	for _, m := range versions.Mismatches {
		fmt.Fprintf(errOut, "  ❌ %s: version \"%s\" disagrees with .claude-plugin/plugin.json \"%s\"\n", m.RelPath, jsonValueText(m.Found), jsonValueText(m.Expected))
	}
	total := len(versions.Errors) + len(versions.Mismatches)
	if total > 0 {
		fmt.Fprintf(errOut, "\n❌ %d plugin/skill version problem(s). Bump every version together and run "+
			"`bun run build:release` to regenerate the ./plugin subtree (issue #274).\n", total)
	} else {
		fmt.Fprintf(out, "✓ Plugin/skill versions agree: %s\n", jsonValueText(versions.Source))
	}
	return total
}

// Reports plugin manifest shape findings and returns their number
func validatePluginManifestShape(rootDir string, out, errOut io.Writer) (int, error) {
	findings, err := collectPluginManifestFindings(rootDir)
	if err != nil {
		return 0, err
	}
	for _, f := range findings {
		fmt.Fprintf(errOut, "  ❌ %s: %s\n", f.RelPath, f.Reason)
	}
	if len(findings) > 0 {
		fmt.Fprintf(errOut, "\n❌ %d plugin manifest shape problem(s). Every manifest key is a "+
			"claim about the Claude Code loader; see scripts/lib/validate-plugin-manifest.js (PR #494).\n", len(findings))
	} else {
		fmt.Fprintln(out, "✓ Plugin manifest shape matches the verified loader contract")
	}
	return len(findings), nil
}

// Reports batch files that hold a bare LF under dist and the synced root outputs
func validateBatchLineEndings(rootDir, distDir string, syncRootOutputs bool, out, errOut io.Writer) (int, error) {
	dirs := []string{distDir}
	add := func(dir string) {
		for _, d := range dirs {
			if d == dir {
				return
			}
		}
		dirs = append(dirs, dir)
	}
	if syncRootOutputs {
		for _, config := range impeccableProviders {
			add(filepath.Join(rootDir, filepath.FromSlash(config.ConfigDir), "skills", "impeccable"))
		}
		add(filepath.Join(rootDir, "plugin"))
		add(filepath.Join(rootDir, "cursor-plugin"))
	}
	bad, err := findBatchFilesWithBareLf(dirs)
	if err != nil {
		return 0, err
	}
	for _, file := range bad {
		rel, err := filepath.Rel(rootDir, file)
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(errOut, "❌ %s has LF line endings; cmd.exe needs CRLF in a batch file (#1098).\n", rel)
	}
	if len(bad) == 0 {
		fmt.Fprintln(out, "✓ Batch launchers ship with CRLF line endings")
	}
	return len(bad), nil
}

// Rebuilds root/plugin from dist/claude-code with both manifests, rewritten markdown and the hooks manifest
func buildPluginSubtree(rootDir, distDir string, out io.Writer) error {
	pluginRoot := filepath.Join(rootDir, "plugin")
	pluginManifestDir := filepath.Join(pluginRoot, ".claude-plugin")
	grokPluginManifestDir := filepath.Join(pluginRoot, ".grok-plugin")
	pluginSkillsDir := filepath.Join(pluginRoot, "skills")
	pluginAgentsDir := filepath.Join(pluginRoot, "agents")
	pluginHooksDir := filepath.Join(pluginRoot, "hooks")
	for _, dir := range []string{pluginManifestDir, grokPluginManifestDir, pluginSkillsDir, pluginAgentsDir, pluginHooksDir,
		filepath.Join(pluginRoot, ".codex-plugin"), filepath.Join(pluginRoot, "assets")} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}

	rootText, err := readTextFile(filepath.Join(rootDir, ".claude-plugin", "plugin.json"))
	if err != nil {
		return err
	}
	rootManifest, err := jsonParse(rootText)
	if err != nil {
		return err
	}
	claudeAgentsSrc := filepath.Join(distDir, "claude-code", ".claude", "agents")
	pluginManifest := jsonSpread(rootManifest)
	pluginManifest.Set("skills", "./skills/")
	pluginManifest.Delete("agents")
	if err := writeBuildFile(filepath.Join(pluginManifestDir, "plugin.json"), jsonStringify(pluginManifest, "  ")+"\n"); err != nil {
		return err
	}

	grokPluginManifest := pyjson.NewObject()
	for _, key := range []string{"name", "version", "description", "author", "homepage", "repository"} {
		if v, has := pluginManifest.Get(key); has {
			grokPluginManifest.Set(key, v)
		}
	}
	license, _ := pluginManifest.Get("license")
	if !jsonTruthy(license) {
		license = "MIT"
	}
	grokPluginManifest.Set("license", license)
	grokPluginManifest.Set("keywords", []any{"design", "frontend", "ui", "ux", "skills", "hooks"})
	if err := writeBuildFile(filepath.Join(grokPluginManifestDir, "plugin.json"), jsonStringify(grokPluginManifest, "  ")+"\n"); err != nil {
		return err
	}

	claudeSkillsSrc := filepath.Join(distDir, "claude-code", ".claude", "skills", "impeccable")
	if pathExists(claudeSkillsSrc) {
		if err := os.MkdirAll(pluginSkillsDir, 0o777); err != nil {
			return err
		}
		if err := copyDirSync(claudeSkillsSrc, filepath.Join(pluginSkillsDir, "impeccable")); err != nil {
			return err
		}
	}
	if pathExists(claudeAgentsSrc) {
		if err := copyDirSync(claudeAgentsSrc, pluginAgentsDir); err != nil {
			return err
		}
	}

	if err := rewritePluginMarkdownTree(pluginSkillsDir, nil); err != nil {
		return err
	}
	agentRewrite := func(content string, _ bool) string { return rewritePluginAgentMarkdown(content) }
	if err := rewritePluginMarkdownTree(pluginAgentsDir, agentRewrite); err != nil {
		return err
	}
	if err := verifyPluginSkillRewrite(filepath.Join(pluginSkillsDir, "impeccable", "SKILL.md")); err != nil {
		return err
	}
	if pathExists(pluginAgentsDir) {
		entries, err := os.ReadDir(pluginAgentsDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".md") {
				if err := verifyPluginAgentRewrite(filepath.Join(pluginAgentsDir, entry.Name())); err != nil {
					return err
				}
			}
		}
	}

	if err := writeBuildFile(filepath.Join(pluginHooksDir, "hooks.json"), jsonStringify(buildClaudePluginHooksManifest(), "  ")+"\n"); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "📦 Built Claude Code / Grok Build plugin subtree at ./plugin/")
	return err
}

// Builds dist/claude-code and the ./plugin subtree from root/skill and runs the plugin gates of the build
func buildImpeccableClaudePlugin(rootDir string, out, errOut io.Writer) error {
	distDir := filepath.Join(rootDir, "dist")
	fmt.Fprint(out, "\U0001F528 Building cross-provider design skills...\n\n")
	if err := syncEngineVersionFile(rootDir, out); err != nil {
		return err
	}
	skills, err := readImpeccableSource(rootDir)
	if err != nil {
		return err
	}
	patterns := readPatterns(rootDir)
	userInvocableCount := 0
	for _, skill := range skills {
		if skill.UserInvocable {
			userInvocableCount++
		}
	}
	fmt.Fprintf(out, "\U0001F4D6 Read %d skills (%d user-invocable) and %d pattern categories\n\n", len(skills), userInvocableCount, len(patterns.Patterns)+len(patterns.Antipatterns))
	if validateSkillFrontmatter(skills, errOut) > 0 {
		return errors.New("impeccable build: skill frontmatter is invalid")
	}
	rootText, err := readTextFile(filepath.Join(rootDir, ".claude-plugin", "plugin.json"))
	if err != nil {
		return err
	}
	pluginJSON, err := jsonParse(rootText)
	if err != nil {
		return err
	}
	skillsVersion, err := jsonProp(pluginJSON, "version")
	if err != nil {
		return err
	}
	claude, _ := impeccableProviderByName("claude-code")
	if err := claude.transform(skills, distDir, jsonToFrontmatter(skillsVersion), out); err != nil {
		return err
	}
	if err := buildPluginSubtree(rootDir, distDir, out); err != nil {
		return err
	}
	versionErrors := validatePluginVersions(rootDir, out, errOut)
	manifestShapeErrors, err := validatePluginManifestShape(rootDir, out, errOut)
	if err != nil {
		return err
	}
	batchEolErrors, err := validateBatchLineEndings(rootDir, distDir, true, out, errOut)
	if err != nil {
		return err
	}
	if versionErrors > 0 || manifestShapeErrors > 0 || batchEolErrors > 0 {
		return errors.New("impeccable build: plugin validation failed")
	}
	return nil
}
