package builtin

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const pluginAgentSourceStep = "after every generation, run `node .claude/skills/impeccable/scripts/embed-prompt.mjs <asset> " +
	"--prompt \"<the prompt used>\"` so the prompt lives inside the image itself. The build thread " +
	"composes what you made."

const pluginVerifyAgentStep = "run `node .claude/skills/impeccable/scripts/embed-prompt.mjs <asset> --prompt \"<p>\"` " +
	"so the prompt lives inside the image itself. Next sentence."

const pluginGoodSkill = "1. Run `<skill-base-dir>/scripts/impeccable context` once per session, where `<skill-base-dir>` is the " +
	"loaded base directory the runtime reports for this skill; keep cwd at the user's project. " +
	"That base directory resolves every `.claude/skills/impeccable/scripts/impeccable <verb>` command in this skill " +
	"and its references, and `.claude/skills/impeccable/scripts` is the fallback only when the runtime " +
	"reports no base directory."

func wantErrMatching(t *testing.T, err error, pattern string) {
	t.Helper()
	if err == nil || !regexp.MustCompile(pattern).MatchString(err.Error()) {
		t.Fatalf("error %v, want one matching %s", err, pattern)
	}
}

func wantContains(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Errorf("missing %q in\n%s", part, text)
		}
	}
}

func wantLacks(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if strings.Contains(text, part) {
			t.Errorf("unexpected %q in\n%s", part, text)
		}
	}
}

func writeAt(t *testing.T, dir, name, contents string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	writeTestFile(t, p, contents)
	return p
}

func TestRewritePluginMarkdownRewritesAScriptInstructionToTheQuotedClaudeSkillDirForm(t *testing.T) {
	input := "Run `node .claude/skills/impeccable/scripts/context.mjs` once per session."
	if got := rewritePluginMarkdown(input, true); got != "Run `node \"${CLAUDE_SKILL_DIR}/scripts/context.mjs\"` once per session." {
		t.Fatal(got)
	}
}

func TestRewritePluginMarkdownRewritesEveryOccurrenceQuotingTheScriptPathButNotTheArguments(t *testing.T) {
	input := strings.Join([]string{
		"node .claude/skills/impeccable/scripts/live.mjs",
		"node .claude/skills/impeccable/scripts/live-poll.mjs --reply EVENT_ID done",
	}, "\n")
	output := rewritePluginMarkdown(input, true)
	wantLacks(t, output, claudeProjectScriptsPath)
	wantContains(t, output, `node "${CLAUDE_SKILL_DIR}/scripts/live.mjs"`, `node "${CLAUDE_SKILL_DIR}/scripts/live-poll.mjs" --reply EVENT_ID done`)
}

func TestRewritePluginMarkdownQuotesTheEngineLauncherPathAndLeavesTheVerbOutsideTheQuotes(t *testing.T) {
	output := rewritePluginMarkdown(
		"Run `<skill-base-dir>/scripts/impeccable context` once, then `<skill-base-dir>/scripts/impeccable.cmd doctor --json`; "+
			"already quoted: `\"<skill-base-dir>/scripts/impeccable\" hooks on`.", true)
	wantContains(t, output, "`\"${CLAUDE_SKILL_DIR}/scripts/impeccable\" context`", "`& \"${CLAUDE_SKILL_DIR}/scripts/impeccable.cmd\" doctor --json`")
	wantLacks(t, output, `""${CLAUDE_SKILL_DIR}`)
}

func TestRewritePluginMarkdownGivesOnlyTheCmdLauncherPowerShellsCallOperator(t *testing.T) {
	input := "Run `<skill-base-dir>/scripts/impeccable context`; without sh, `<skill-base-dir>/scripts/impeccable.cmd context`."
	output := rewritePluginMarkdown(input, true)
	if output != "Run `\"${CLAUDE_SKILL_DIR}/scripts/impeccable\" context`; without sh, `& \"${CLAUDE_SKILL_DIR}/scripts/impeccable.cmd\" context`." {
		t.Fatal(output)
	}
	if regexp.MustCompile(`& "[^"]*/impeccable"`).MatchString(output) {
		t.Fatalf("sh launcher got the call operator: %s", output)
	}
	if again := rewritePluginMarkdown(output, true); again != output {
		t.Fatalf("second rewrite changed the text: %s", again)
	}
	if got := quoteLauncher("<dir>", false); got != `"<dir>/scripts/impeccable"` {
		t.Fatal(got)
	}
	if got := quoteLauncher("<dir>", true); got != `& "<dir>/scripts/impeccable.cmd"` {
		t.Fatal(got)
	}
}

func TestRewritePluginMarkdownQuotesCommandsAlreadyInTheSkillBaseDirFormWithoutDoubleQuoting(t *testing.T) {
	input := "Run `node <skill-base-dir>/scripts/context.mjs` once per session. " +
		"Already quoted: `node \"<skill-base-dir>/scripts/detect.mjs\"`."
	want := "Run `node \"${CLAUDE_SKILL_DIR}/scripts/context.mjs\"` once per session. " +
		"Already quoted: `node \"${CLAUDE_SKILL_DIR}/scripts/detect.mjs\"`."
	if got := rewritePluginMarkdown(input, true); got != want {
		t.Fatal(got)
	}
}

func TestRewritePluginMarkdownRemovesTheEntireAllowedToolsFrontmatterBlock(t *testing.T) {
	input := strings.Join([]string{
		"---",
		"name: impeccable",
		"allowed-tools:",
		"  - Bash(npx impeccable *)",
		"  - Bash(.claude/skills/impeccable/scripts/impeccable *)",
		"license: Apache 2.0",
		"---",
		"",
		"Body text.",
	}, "\n")
	output := rewritePluginMarkdown(input, true)
	if regexp.MustCompile(`(?m)^allowed-tools:`).MatchString(output) {
		t.Fatalf("allowed-tools survived\n%s", output)
	}
	wantLacks(t, output, "scripts/impeccable *", "npx impeccable")
	wantContains(t, output, "license: Apache 2.0", "Body text.")
}

func TestRewritePluginMarkdownDropsTheProjectPathFallbackClauseFromSetupStep1(t *testing.T) {
	input := "1. Run `node <skill-base-dir>/scripts/context.mjs` once per session, where `<skill-base-dir>` is the " +
		"loaded base directory the runtime reports for this skill; keep cwd at the user's project. " +
		"That base directory resolves every `.claude/skills/impeccable/scripts/impeccable <verb>` command in this skill " +
		"and its references, and `.claude/skills/impeccable/scripts` is the fallback only when the runtime " +
		"reports no base directory. Pass a named source file or route as `--target <path>`."
	output := rewritePluginMarkdown(input, true)
	wantContains(t, output, "Every `\"${CLAUDE_SKILL_DIR}/scripts/impeccable\" <verb>` command in this skill and its references resolves against that base directory.")
	wantLacks(t, output, "fallback", claudeProjectScriptsPath)
}

func TestRewritePluginMarkdownLeavesUnrelatedProjectRelativePathsAlone(t *testing.T) {
	input := "State lives in `.impeccable/live/roots.json` and `.claude/settings.json`."
	if got := rewritePluginMarkdown(input, true); got != input {
		t.Fatal(got)
	}
}

func TestRewritePluginAgentMarkdownRewritesAgentInstructionsToTheQuotedPluginRootVariableForm(t *testing.T) {
	output := rewritePluginAgentMarkdown(pluginAgentSourceStep)
	wantContains(t, output, "run `node \"${CLAUDE_PLUGIN_ROOT}/skills/impeccable/scripts/embed-prompt.mjs\" <asset> --prompt \"<the prompt used>\"`")
}

func TestRewritePluginAgentMarkdownAppendsTheSidecarFallbackAsItsOwnSentenceNotMidSentence(t *testing.T) {
	output := rewritePluginAgentMarkdown(pluginAgentSourceStep)
	wantContains(t, output, "so the prompt lives inside the image itself."+agentEmbedFallback+" The build thread")
}

func TestRewritePluginAgentMarkdownNeverEmitsTheSkillBaseDirTokenIntoAnAgentFile(t *testing.T) {
	output := rewritePluginAgentMarkdown(pluginAgentSourceStep)
	wantLacks(t, output, "<skill-base-dir>", claudeProjectScriptsPath)
}

func TestVerifyPluginAgentRewriteAcceptsACorrectlyRewrittenAgentFile(t *testing.T) {
	p := writeAt(t, t.TempDir(), "agent.md", rewritePluginAgentMarkdown(pluginVerifyAgentStep))
	if err := verifyPluginAgentRewrite(p); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPluginAgentRewriteFailsWhenAnUnresolvablePathFormSurvives(t *testing.T) {
	p := writeAt(t, t.TempDir(), "agent.md", "run `node \"<skill-base-dir>/scripts/embed-prompt.mjs\"` please.")
	wantErrMatching(t, verifyPluginAgentRewrite(p), `cannot\s+resolve`)
}

func TestVerifyPluginAgentRewriteFailsWhenTheEmbedInstructionLostItsFallbackSentence(t *testing.T) {
	reworded := strings.Replace(pluginVerifyAgentStep, " so the prompt lives inside the image itself. Next sentence.", " -- no closing period", 1)
	if reworded == pluginVerifyAgentStep {
		t.Fatal("the source step no longer holds the sentence this case rewords")
	}
	p := writeAt(t, t.TempDir(), "agent.md", rewritePluginAgentMarkdown(reworded))
	wantErrMatching(t, verifyPluginAgentRewrite(p), `sidecar`)
}

func TestRewritePluginMarkdownTreeRewritesMdFilesRecursivelyAndLeavesScriptsUntouched(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "SKILL.md", "Run `node .claude/skills/impeccable/scripts/context.mjs`.")
	writeAt(t, root, "reference/live.md", "node .claude/skills/impeccable/scripts/live.mjs")
	writeAt(t, root, "scripts/hook-admin.mjs", "const cmd = 'node \"${CLAUDE_PROJECT_DIR}/.claude/skills/impeccable/scripts/hook.mjs\"';")

	if err := rewritePluginMarkdownTree(root, nil); err != nil {
		t.Fatal(err)
	}

	if got := readTestFile(t, filepath.Join(root, "SKILL.md")); got != "Run `node \"${CLAUDE_SKILL_DIR}/scripts/context.mjs\"`." {
		t.Fatal(got)
	}
	if got := readTestFile(t, filepath.Join(root, "reference", "live.md")); got != `node "<skill-base-dir>/scripts/live.mjs"` {
		t.Fatal(got)
	}
	wantContains(t, readTestFile(t, filepath.Join(root, "scripts", "hook-admin.mjs")), "${CLAUDE_PROJECT_DIR}/.claude/skills/impeccable/scripts/hook.mjs")
}

func TestRewritePluginMarkdownTreeRawReferencesRetainAnExplicitBaseDirectoryPlaceholderIncludingWindowsCommands(t *testing.T) {
	input := "Run `.claude/skills/impeccable/scripts/impeccable context` or `<skill-base-dir>/scripts/impeccable.cmd context`."
	output := rewritePluginMarkdown(input, false)
	if output != "Run `\"<skill-base-dir>/scripts/impeccable\" context` or `& \"<skill-base-dir>/scripts/impeccable.cmd\" context`." {
		t.Fatal(output)
	}
	wantLacks(t, output, "${CLAUDE_SKILL_DIR}")
	if again := rewritePluginMarkdown(output, false); again != output {
		t.Fatalf("second rewrite changed the text: %s", again)
	}
}

func TestRewritePluginMarkdownTreeEntrypointAndExplicitlyResolvedReferenceCommandsWorkFromACachePathWithSpaces(t *testing.T) {
	skillDir := filepath.Join(t.TempDir(), "plugin cache", "skills", "impeccable")
	writeAt(t, skillDir, "scripts/probe.mjs", "console.log(process.argv[2]);")
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLAUDE_SKILL_DIR=") {
			env = append(env, kv)
		}
	}
	for _, isSkillEntrypoint := range []bool{true, false} {
		token := "<skill-base-dir>"
		if isSkillEntrypoint {
			token = "${CLAUDE_SKILL_DIR}"
		}
		command := strings.ReplaceAll(rewritePluginMarkdown("node .claude/skills/impeccable/scripts/probe.mjs resolved", isSkillEntrypoint), token, skillDir)
		cmd := exec.Command("sh", "-c", command)
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) != "resolved" {
			t.Fatalf("%s printed %q: %v", command, out, err)
		}
	}
}

func TestRewritePluginMarkdownTreeAppliesTheAgentRewriteWhenPassedForAnAgentsTree(t *testing.T) {
	agentsDir := filepath.Join(t.TempDir(), "agents")
	writeAt(t, agentsDir, "impeccable-asset-producer.md", "run `node .claude/skills/impeccable/scripts/embed-prompt.mjs <asset>`")

	err := rewritePluginMarkdownTree(agentsDir, func(content string, _ bool) string { return rewritePluginAgentMarkdown(content) })
	if err != nil {
		t.Fatal(err)
	}

	want := "run `node \"${CLAUDE_PLUGIN_ROOT}/skills/impeccable/scripts/embed-prompt.mjs\" <asset>`"
	if got := readTestFile(t, filepath.Join(agentsDir, "impeccable-asset-producer.md")); got != want {
		t.Fatal(got)
	}
}

func TestRewritePluginMarkdownTreeIsANoOpOnAMissingDirectory(t *testing.T) {
	if err := rewritePluginMarkdownTree(filepath.Join(t.TempDir(), "does-not-exist"), nil); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPluginSkillRewriteAcceptsACorrectlyRewrittenSkillMd(t *testing.T) {
	p := writeAt(t, t.TempDir(), "SKILL.md", rewritePluginMarkdown(pluginGoodSkill, true))
	if err := verifyPluginSkillRewrite(p); err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?m)^allowed-tools:`).MatchString(readTestFile(t, p)) {
		t.Fatal("allowed-tools in the rewritten skill")
	}
}

func TestVerifyPluginSkillRewriteFailsTheBuildWhenTheSetupFallbackSentenceNoLongerMatched(t *testing.T) {
	reworded := strings.Replace(pluginGoodSkill, "is the fallback only when", "is used only when", 1)
	if reworded == pluginGoodSkill {
		t.Fatal("the skill text no longer holds the clause this case rewords")
	}
	p := writeAt(t, t.TempDir(), "SKILL.md", rewritePluginMarkdown(reworded, true))
	wantErrMatching(t, verifyPluginSkillRewrite(p), `Setup step 1 fallback sentence`)
}

func TestVerifyPluginSkillRewriteFailsTheBuildWhenAQuotedCmdLauncherLacksTheCallOperator(t *testing.T) {
	root := t.TempDir()
	p := writeAt(t, root, "SKILL.md", rewritePluginMarkdown(pluginGoodSkill, true)+
		"\nOn a Windows shell without `sh`, call `\"${CLAUDE_SKILL_DIR}/scripts/impeccable.cmd\"` instead.\n")
	wantErrMatching(t, verifyPluginSkillRewrite(p), `call operator`)
	fixed := writeAt(t, root, "SKILL.md", rewritePluginMarkdown(pluginGoodSkill+
		"\nOn a Windows shell without `sh`, call `.claude/skills/impeccable/scripts/impeccable.cmd` instead.\n", true))
	if err := verifyPluginSkillRewrite(fixed); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPluginSkillRewriteFailsTheBuildWhenALauncherPreApprovalSurvivesTheRemoval(t *testing.T) {
	p := writeAt(t, t.TempDir(), "SKILL.md", rewritePluginMarkdown(pluginGoodSkill, true)+"\n  - Bash(${CLAUDE_SKILL_DIR}/scripts/impeccable.cmd *)\n")
	wantErrMatching(t, verifyPluginSkillRewrite(p), `pre-approves an engine launcher`)
}

func TestVerifyPluginSkillRewriteFailsTheBuildWhenAllowedToolsFrontmatterSurvivesTheRemoval(t *testing.T) {
	p := writeAt(t, t.TempDir(), "SKILL.md", strings.Join([]string{
		"---",
		"name: impeccable",
		"allowed-tools:",
		"  - Bash(npx impeccable *)",
		"license: Apache 2.0",
		"---",
		"",
		rewritePluginMarkdown(pluginGoodSkill, true),
	}, "\n"))
	wantErrMatching(t, verifyPluginSkillRewrite(p), `allowed-tools`)
}

func TestVerifyPluginSkillRewriteFailsTheBuildWhenALegacyNodePreApprovalSurvives(t *testing.T) {
	p := writeAt(t, t.TempDir(), "SKILL.md", rewritePluginMarkdown(pluginGoodSkill, true)+"\n  - Bash(node ${CLAUDE_SKILL_DIR}/scripts/*)\n")
	wantErrMatching(t, verifyPluginSkillRewrite(p), `pre-approves an engine launcher or node script path`)
}

func TestVerifyPluginSkillRewriteFailsTheBuildWhenTheProjectRelativeScriptsPathSurvivesAtAll(t *testing.T) {
	p := writeAt(t, t.TempDir(), "SKILL.md", rewritePluginMarkdown(pluginGoodSkill, true)+
		"\nState lives next to `.claude/skills/impeccable/scripts` on disk.")
	wantErrMatching(t, verifyPluginSkillRewrite(p), `still contains the project-relative scripts path`)
}

func TestVerifyPluginSkillRewriteFailsTheBuildWhenTheSkillBaseDirTokenSurvivesTheRewrite(t *testing.T) {
	rewritten := rewritePluginMarkdown(pluginGoodSkill, true)
	tampered := strings.Replace(rewritten, "${CLAUDE_SKILL_DIR}", "<skill-base-dir>", 1)
	if tampered == rewritten {
		t.Fatal("the rewritten skill holds no skill directory variable")
	}
	p := writeAt(t, t.TempDir(), "SKILL.md", tampered)
	wantErrMatching(t, verifyPluginSkillRewrite(p), `still contains the <skill-base-dir> token`)
}
