package app

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const initQualityBar = `Quality bar for the file you write (this is what separates a useful AGENTS.md
from noise):
- CONCISE: target under 100 lines. Agents load this file every session — every
  line costs context. No essays, no marketing prose, no filler.
- Commands must be EXACT invocations you verified from the repo (package.json
  scripts, Makefile targets, pyproject/tox/CI config, existing docs). Write
  ` + "`npm run test:unit`" + ` or ` + "`scripts/run_tests.sh tests/foo`" + `, never "run the
  tests". NEVER invent a command you didn't see evidence for.
- No generic advice. "Write tests for new code" and "follow best practices"
  are banned — if a line would be true of any repo, cut it.
- Conventions must be OBSERVED, not assumed: naming patterns, module layout,
  error-handling style, commit-message format — only what the code actually
  shows.
- Include pitfalls that would genuinely trip up a newcomer or an agent
  (required env vars, generated files not to hand-edit, slow test suites,
  ports already in use), if you found any. Skip the section if you found none.
- Markdown structure: a short title + one-paragraph overview, then focused
  sections (e.g. "Dev environment", "Build & test", "Conventions",
  "Pitfalls"). Flat and scannable — no deep nesting.`

// The /init prompt, an existing AGENTS.md switching it to merge discipline and extra carrying the user's notes
func buildInitPrompt(cwd string, existingFile *string, extra string) string {
	extra = pystr.Strip(extra)
	update := existingFile != nil
	verb, write := "generate an", "."
	if update {
		verb, write = "UPDATE the existing", " — but this is an UPDATE, so follow the merge discipline below."
	}
	parts := []string{
		"[/init] The user wants you to " + verb + " AGENTS.md project-instructions file for the project at: " + cwd + "\n",
		"AGENTS.md is the instruction file coding agents (Hermes included) " +
			"load as project context every session. It should teach an agent how " +
			"to work in THIS repo: what the project is, how to set up, the exact " +
			"build/test/lint commands, the conventions the code actually follows, " +
			"and the pitfalls that waste time.\n",
		"Do this:\n" +
			"1. Inspect the project with your read-only tools (`read_file`, " +
			"`search_files`) — start with manifests and toolchain files " +
			"(package.json, pyproject.toml, Cargo.toml, go.mod, Makefile, " +
			"CI workflow configs, lockfiles), then the directory layout, existing " +
			"README/docs, and test/lint configuration. Learn the real commands, " +
			"don't guess them.\n" +
			"2. Write the file to " + strings.TrimRight(cwd, "/") + "/AGENTS.md with `write_file`" + write + "\n" +
			"3. Confirm to the user the exact path you wrote and summarize in one " +
			"or two lines what the file covers.\n",
	}
	if update {
		parts = append(parts, "MERGE DISCIPLINE — an AGENTS.md already exists (its current "+
			"content is below). Do NOT overwrite or regenerate it from "+
			"scratch. Preserve the user's existing content — their wording, "+
			"their sections, their rules — and merge in only what is missing "+
			"or verifiably stale (e.g. a command that no longer exists in the "+
			"repo). When existing content conflicts with what you observed, "+
			"prefer minimal surgical edits over rewrites, and keep the "+
			"user's intent. The result must still meet the quality bar.\n\n"+
			"CURRENT AGENTS.md CONTENT:\n"+
			"<<<EXISTING_AGENTS_MD\n"+
			*existingFile+"\n"+
			"EXISTING_AGENTS_MD\n")
	}
	parts = append(parts, initQualityBar)
	if extra != "" {
		parts = append(parts, "\nUSER NOTES — honor these while authoring (they override the "+
			"defaults above where they conflict):\n"+extra)
	}
	return strings.Join(parts, "\n")
}

// The session's active directory, its recorded terminal cwd, else the agent cwd, else the process cwd
func resolveSessionCwd(env *tools.Env, sessionKey *string) string {
	key := ""
	if sessionKey != nil {
		key = *sessionKey
	} else {
		key = tools.GetSessionEnv(env, "ICEBATH_SESSION_KEY", "")
	}
	if recorded, ok := tools.GetSessionCwd(env, key); ok && recorded != "" {
		if info, err := os.Stat(recorded); err == nil && info.IsDir() {
			return recorded
		}
	}
	if cwd, err := tools.ResolveAgentCwd(env); err == nil {
		return cwd
	}
	cwd, _ := os.Getwd()
	return cwd
}

// The /init prompt for an explicit existing cwd or the session's active directory with its AGENTS.md
func buildInitPromptForCwd(env *tools.Env, cwd, extra string, sessionKey *string) string {
	target := cwd
	if info, err := os.Stat(cwd); cwd == "" || err != nil || !info.IsDir() {
		target = resolveSessionCwd(env, sessionKey)
	}
	resolved, err := filepath.Abs(target)
	if err != nil {
		resolved = target
	}
	var existing *string
	agentsPath := filepath.Join(resolved, "AGENTS.md")
	if info, err := os.Stat(agentsPath); err == nil && info.Mode().IsRegular() {
		if raw, err := os.ReadFile(agentsPath); err == nil {
			text := strings.ToValidUTF8(strings.TrimPrefix(string(raw), "\ufeff"), "\ufffd")
			existing = &text
		}
	}
	return buildInitPrompt(resolved, existing, extra)
}

// Handles /init, generating or updating AGENTS.md through the live agent's read only scan
func (c *CLI) handleInitCommand(cmd string) {
	key := ""
	msg := buildInitPromptForCwd(c.env, "", commandArg(cmd, false), &key)
	ack := "init.generating"
	if strings.Contains(msg, "UPDATE the existing AGENTS.md") {
		ack = "init.updating"
	}
	c.queuePromptTurn(msg, "/init", c.commandsT(ack, nil))
}
