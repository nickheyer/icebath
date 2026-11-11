package builtin

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dlclark/regexp2/v2"
)

const (
	claudeProjectScriptsPath = ".claude/skills/impeccable/scripts"
	pluginScriptsPath        = "${CLAUDE_SKILL_DIR}/scripts"
	projectAllowedToolsLine  = "  - Bash(" + claudeProjectScriptsPath + "/impeccable *)\n"
	pluginAgentScriptsPath   = "${CLAUDE_PLUGIN_ROOT}/skills/impeccable/scripts"
	skillBaseDirToken        = "<skill-base-dir>"

	setupFallbackText = "That base directory resolves every `.claude/skills/impeccable/scripts/impeccable <verb>` command in this skill and its references, " +
		"and `.claude/skills/impeccable/scripts` is the fallback only when the runtime reports no base directory."
	setupPluginText = "Every `\"${CLAUDE_SKILL_DIR}/scripts/impeccable\" <verb>` command in this skill and its references resolves against that base directory. " +
		"In reference files, replace the skill-base-dir placeholder with this directory before running commands; it is not a shell variable."

	agentEmbedFallback = " When that script path is unreachable in your environment, write the same prompt to " +
		"`<asset>.prompt.txt` beside the asset and note it in your manifest so the parent can embed it."

	quotedCmdLauncher = `"${CLAUDE_SKILL_DIR}/scripts/impeccable.cmd"`
)

// Members of the ECMAScript whitespace class written literally for use inside a character class
const jsSpaceClass = "\t\n\v\f\r \u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

var (
	pluginNodeScriptRe = regexp.MustCompile(`node (\$\{CLAUDE_SKILL_DIR\}|<skill-base-dir>)/scripts/([^` + jsSpaceClass + "`\"]+)")
	pluginLauncherRe   = regexp2.MustCompile(`(?<!["A-Za-z0-9_/])(\$\{CLAUDE_SKILL_DIR\}|<skill-base-dir>)/scripts/impeccable(\.cmd)?(?=[`+jsSpaceClass+"`])", regexp2.None)
	agentNodeScriptRe  = regexp.MustCompile(`node \$\{CLAUDE_PLUGIN_ROOT\}/skills/impeccable/scripts/([^` + jsSpaceClass + "`\"]+)")
	agentEmbedRe       = regexp.MustCompile("`node \"\\$\\{CLAUDE_PLUGIN_ROOT\\}/skills/impeccable/scripts/embed-prompt\\.mjs\"[^`]*`[^.]*\\.")
	bashPreApprovalRe  = regexp.MustCompile(`Bash\((?:node |[^)]*scripts/impeccable)`)
)

// Content with the allowed-tools frontmatter key dropped, unchanged when the key is absent
func stripAllowedToolsFrontmatter(content string) string {
	fm, body := parseImpeccableFrontmatter(content)
	kept := make([]fmPair, 0, len(fm))
	found := false
	for _, p := range fm {
		if p.Key == "allowed-tools" {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		return content
	}
	return generateYamlFrontmatter(kept) + "\n" + body
}

// Plugin copy of one skill markdown file with quoted skill directory launcher and script paths
func rewritePluginMarkdown(content string, isSkillEntrypoint bool) string {
	baseDir := skillBaseDirToken
	if isSkillEntrypoint {
		baseDir = "${CLAUDE_SKILL_DIR}"
	}
	out := stripAllowedToolsFrontmatter(content)
	out = strings.ReplaceAll(out, projectAllowedToolsLine, "")
	out = strings.ReplaceAll(out, setupFallbackText, setupPluginText)
	scripts := baseDir + "/scripts"
	if isSkillEntrypoint {
		scripts = pluginScriptsPath
	}
	out = strings.ReplaceAll(out, claudeProjectScriptsPath, scripts)
	out = strings.ReplaceAll(out, skillBaseDirToken, baseDir)
	out = pluginNodeScriptRe.ReplaceAllString(out, `node "${1}/scripts/${2}"`)
	quoted, err := pluginLauncherRe.ReplaceFunc(out, func(m regexp2.Match) string {
		groups := m.Groups()
		return quoteLauncher(groups[1].String(), groups[2].String() != "")
	}, -1, -1)
	if err != nil {
		panic(err)
	}
	return quoted
}

// Quoted launcher path as a command, the cmd launcher behind the PowerShell call operator
func quoteLauncher(dir string, windowsCmd bool) string {
	if windowsCmd {
		return `& "` + dir + `/scripts/impeccable.cmd"`
	}
	return `"` + dir + `/scripts/impeccable"`
}

// Plugin copy of one agent file using the plugin root variable and the sidecar sentence
func rewritePluginAgentMarkdown(content string) string {
	out := strings.ReplaceAll(content, claudeProjectScriptsPath, pluginAgentScriptsPath)
	out = agentNodeScriptRe.ReplaceAllString(out, `node "$${CLAUDE_PLUGIN_ROOT}/skills/impeccable/scripts/${1}"`)
	return agentEmbedRe.ReplaceAllStringFunc(out, func(m string) string { return m + agentEmbedFallback })
}

// Error when an agent file keeps an unresolvable scripts path or lost its sidecar sentence
func verifyPluginAgentRewrite(agentPath string) error {
	data, err := os.ReadFile(agentPath)
	if err != nil {
		return err
	}
	content := decodeUTF8Lossy(data)
	if strings.Contains(content, claudeProjectScriptsPath) || strings.Contains(content, skillBaseDirToken) {
		return fmt.Errorf("Plugin rewrite drift: %s references a scripts path a spawned agent cannot "+
			"resolve (the project-relative form or the <skill-base-dir> token). Agent files must "+
			"carry the ${CLAUDE_PLUGIN_ROOT} form; see rewritePluginAgentMarkdown (issue #523).", agentPath)
	}
	if strings.Contains(content, "embed-prompt.mjs") && !strings.Contains(content, agentEmbedFallback) {
		return fmt.Errorf("Plugin rewrite drift: %s carries an embed instruction without the sidecar "+
			"fallback sentence. The source sentence no longer matches the anchor in "+
			"scripts/lib/plugin-paths.js (issue #523); update the fallback anchor to the new wording.", agentPath)
	}
	return nil
}

// Whether a quoted cmd launcher appears without the call operator before it
func hasBareQuotedCmdLauncher(content string) bool {
	for at := 0; ; {
		i := strings.Index(content[at:], quotedCmdLauncher)
		if i < 0 {
			return false
		}
		i += at
		if !strings.HasSuffix(content[:i], "& ") {
			return true
		}
		at = i + 1
	}
}

// Error when the plugin SKILL.md no longer matches what the skill rewrite must produce
func verifyPluginSkillRewrite(skillMdPath string) error {
	data, err := os.ReadFile(skillMdPath)
	if err != nil {
		return err
	}
	content := decodeUTF8Lossy(data)
	if !strings.Contains(content, setupPluginText) {
		return fmt.Errorf("Plugin rewrite drift: %s is missing the ${CLAUDE_SKILL_DIR} resolution sentence. "+
			"SKILL.src.md's Setup step 1 fallback sentence no longer matches the replacement in "+
			"scripts/lib/plugin-paths.js (issue #523); update SETUP_FALLBACK_TEXT to the new wording.", skillMdPath)
	}
	if hasBareQuotedCmdLauncher(content) {
		return fmt.Errorf("Plugin rewrite drift: %s quotes the impeccable.cmd launcher without PowerShell's "+
			"call operator. A quoted path followed by a verb fails to parse in PowerShell (issue #744); "+
			"every quoted .cmd spelling must go through quoteLauncher in scripts/lib/plugin-paths.js.", skillMdPath)
	}
	if strings.Contains(content, skillBaseDirToken) {
		return fmt.Errorf("Plugin rewrite drift: %s still contains the <skill-base-dir> token. "+
			"Plugin skill markdown must use ${CLAUDE_SKILL_DIR} (issue #744); check replaceAll in "+
			"scripts/lib/plugin-paths.js.", skillMdPath)
	}
	if fm, _ := parseImpeccableFrontmatter(content); fmHas(fm, "allowed-tools") {
		return fmt.Errorf("Plugin rewrite drift: %s still declares allowed-tools in frontmatter. "+
			"The plugin copy must drop the entire block so non-interactive sessions can activate "+
			"the skill (issue #736); update the removal in scripts/lib/plugin-paths.js.", skillMdPath)
	}
	if bashPreApprovalRe.MatchString(content) {
		return fmt.Errorf("Plugin rewrite drift: %s still pre-approves an engine launcher or node script path. "+
			"A stray Bash(...) entry survived the allowed-tools removal in "+
			"scripts/lib/plugin-paths.js (issue #523); the plugin ships no launcher pre-approval.", skillMdPath)
	}
	if strings.Contains(content, claudeProjectScriptsPath) {
		return fmt.Errorf("Plugin rewrite drift: %s still contains the project-relative scripts path "+
			"(%s). A wording or path shape in SKILL.src.md slipped past "+
			"the replacements in scripts/lib/plugin-paths.js (issue #523); the plugin copy must not "+
			"reference the project's scripts directory.", skillMdPath, claudeProjectScriptsPath)
	}
	return nil
}

// Rewrites every markdown file under a directory in place, a nil rewrite meaning the skill rewrite
func rewritePluginMarkdownTree(dir string, rewrite func(content string, isSkillEntrypoint bool) string) error {
	if rewrite == nil {
		rewrite = rewritePluginMarkdown
	}
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		entryPath := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			if err := rewritePluginMarkdownTree(entryPath, rewrite); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(entryPath)
		if err != nil {
			return err
		}
		original := decodeUTF8Lossy(data)
		rewritten := rewrite(original, entry.Name() == "SKILL.md")
		if rewritten != original {
			if err := os.WriteFile(entryPath, []byte(rewritten), 0o666); err != nil {
				return err
			}
		}
	}
	return nil
}
