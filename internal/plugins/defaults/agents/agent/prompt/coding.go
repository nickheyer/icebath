package prompt

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/git"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/tools"
)

// How the agent works inside a code project: the posture brief, the workspace snapshot, and the user's own coding rules
const codingGuidance = "You are a coding agent pairing with the user inside their codebase. Operate like a careful senior engineer.\n" +
	"\n" +
	"Gather context first:\n" +
	"- Read the relevant files with `read_file` and locate code with `search_files` before changing anything. Trace a symbol to its definition and usages rather than guessing its shape.\n" +
	"- Batch independent lookups: when several reads/searches don't depend on each other, issue them together in one turn instead of one at a time.\n" +
	"- Never invent files, symbols, APIs, or imports. If you haven't seen it in the repo, go look. Don't assume a library is available — check the project manifest (pyproject.toml / package.json / Cargo.toml / go.mod) and how neighbouring files import it.\n" +
	"\n" +
	"Make changes through the tools, not the chat:\n" +
	"- Edit with `patch`/`write_file`. Do NOT print code blocks to the user as a substitute for editing — apply the change, then summarise it. Only show code when the user explicitly asks to see it.\n" +
	"- Match the project's existing style and conventions; AGENTS.md / CLAUDE.md / .cursorrules already in context win over your defaults. Touch only what the task needs — no drive-by refactors, renames, or reformatting — and add any imports/dependencies your code requires.\n" +
	"- If an edit fails to apply, re-read the file to get the current exact contents before retrying — don't repeat a stale patch. If the same region fails twice, rewrite the enclosing function or file with `write_file` instead of attempting a third patch.\n" +
	"\n" +
	"Verify, and know when to stop:\n" +
	"- Use `terminal` for git, builds, tests, and inspection. Run the relevant tests/linter/build and confirm they pass before claiming the work is done.\n" +
	"- Terminal state persists across calls: current directory and exported environment variables carry forward. Activate a virtualenv or export setup vars once, then reuse that state instead of re-sourcing it before every test command.\n" +
	"- Fix root causes, not symptoms: when you find a bug, check sibling call paths for the same flaw and fix the class, not just the reported site.\n" +
	"- When fixing linter/type errors on a file, stop after about three attempts on the same file and ask the user rather than looping.\n" +
	todoSentence + "\n" +
	"\n" +
	"Respect the user's repo: don't commit, push, or rewrite history unless asked, and never read, print, or commit secrets — leave `.env` and credential files alone unless the user explicitly asks. The Workspace block below is a snapshot from session start — re-run `git status`/`git branch` before relying on it. Be concise: lead with the change or answer, not a preamble."

const (
	todoSentence   = "- Track multi-step work with `todo_list`. Reference code as `path:line` instead of pasting whole files."
	noTodoSentence = "- Reference code as `path:line` instead of pasting whole files."
)

// The edit format each model family handles best, said only when the patch tool takes a mode
var editFormats = []struct {
	families []string
	line     string
}{
	{[]string{"gpt", "codex"},
		"- Edit format: author new files with `write_file`; for edits to existing code use `patch` with `mode='patch'` (V4A diff) — including single-file edits. It's the edit format you handle most reliably."},
	{[]string{"claude", "sonnet", "opus", "haiku", "gemini", "gemma", "deepseek", "qwen", "kimi", "glm", "grok", "llama", "mistral", "devstral", "minimax"},
		"- Edit format: author new files with `write_file`; for edits to existing code prefer `patch` in `mode='replace'` — match a unique snippet and swap it. Reach for `mode='patch'` (V4A) only when an edit genuinely spans several files at once."},
}

// Surfaces where a code folder turns the coding posture on by itself
var interactive = []string{"cli", "tui", "acp", ""}

var codeExtensions = strings.Fields(`.py .pyi .ipynb .js .jsx .ts .tsx .mjs .cjs .go .rs .java .kt .kts .scala .rb .php .c .h .cc .cpp .hpp
	.cs .swift .m .mm .dart .ex .exs .lua .sh .bash .zsh .sql .vue .svelte .r .jl .hs .clj .erl .pl`)

// Folders the look for code files does not go into
var codeScanSkipped = []string{".git", "node_modules", "venv", ".venv", "__pycache__", "dist", "build", "target", ".next", ".turbo", "vendor"}

const (
	// Entries the look for code files reads before it gives up
	codeScanEntries = 500
	// Manifests named on the Project line
	manifestsShown = 6
)

type coding struct {
	guidance     string
	workspace    string
	instructions string
}

// The coding posture of a session with tools in a code project: on or off by the coding mode, else by whether the working directory is
// in a project, on an interactive surface. The workspace snapshot is taken once per session and directory
func posture(ctx context.Context, env *prompt.Env) coding {
	if len(env.Tools) == 0 || env.Cwd == "" || !codingOn(env.Config, env.Platform, env.Cwd) {
		return coding{}
	}
	settings := env.Config
	cwd := resolve(env.Cwd)
	brief := codingGuidance
	if !env.Offers("todo_list") {
		brief = strings.ReplaceAll(brief, todoSentence, noTodoSentence)
	}
	if env.Takes("patch", "mode") {
		model := strings.ToLower(env.Model)
	found:
		for _, format := range editFormats {
			for _, family := range format.families {
				if model != "" && strings.Contains(model, family) {
					brief += "\n" + format.line
					break found
				}
			}
		}
	}
	out := coding{guidance: brief}
	out.workspace = env.Kept("workspace:"+cwd, func() string { return workspace(ctx, cwd) })
	var rules []string
	for _, rule := range settings.GetStringSlice(agentconfig.CodingInstructions) {
		if rule = strings.TrimSpace(rule); rule != "" {
			rules = append(rules, rule)
		}
	}
	if len(rules) > 0 {
		out.instructions = "Operator instructions (from config):\n" + strings.Join(rules, "\n")
	}
	return out
}

// Whether the coding posture is on for a session in the folder on the surface: as the coding mode says, else when the folder is in a code
// project and the surface is an interactive one
func codingOn(settings *config.Config, surface, dir string) bool {
	cwd := resolve(dir)
	switch mode := codingMode(settings.GetString(agentconfig.CodingContext)); {
	case mode == agentconfig.CodingOff:
		return false
	case mode == agentconfig.CodingOn:
		return true
	case !slices.Contains(interactive, strings.ToLower(strings.TrimSpace(surface))):
		return false
	case tools.MarkerRoot(cwd) != "":
		return true
	}
	root := gitRoot(cwd)
	return root != "" && root != home() && hasCode(root)
}

// Whether a session in the folder on the surface works on code, as the coding posture decides
func Coding(settings *config.Config, surface, dir string) bool {
	return dir != "" && codingOn(settings, surface, dir)
}

// Whether a session in the folder on the surface works in the coding focus posture: the coding mode is focus and the folder is a code
// project, so tools that have nothing to do with code are left out and skills outside coding are listed by name only
func Focused(settings *config.Config, surface, dir string) bool {
	return dir != "" && codingMode(settings.GetString(agentconfig.CodingContext)) == agentconfig.CodingFocus && codingOn(settings, surface, dir)
}

// The coding mode with the other words a config file may say it in
func codingMode(value string) string {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "strict", "lean":
		return agentconfig.CodingFocus
	case "true", "yes", "1", "always":
		return agentconfig.CodingOn
	case "false", "no", "0", "never":
		return agentconfig.CodingOff
	case agentconfig.CodingFocus, agentconfig.CodingOn, agentconfig.CodingOff:
		return value
	}
	return agentconfig.CodingAuto
}

// The user's home directory, resolved, empty when unknown
func home() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return resolve(dir)
}

// Whether a source file is in the folder or one of the folders right inside it, within a bounded number of entries
func hasCode(root string) bool {
	seen := 0
	dirs := []string{root}
	for i := 0; i < len(dirs); i++ {
		entries, err := os.ReadDir(dirs[i])
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if seen++; seen > codeScanEntries {
				return false
			}
			name := entry.Name()
			switch {
			case entry.Type().IsRegular() && slices.Contains(codeExtensions, strings.ToLower(filepath.Ext(name))):
				return true
			case i == 0 && entry.IsDir() && !slices.Contains(codeScanSkipped, name) && !strings.HasPrefix(name, "."):
				dirs = append(dirs, filepath.Join(root, name))
			}
		}
	}
	return false
}

// Opening line of the workspace snapshot
const workspaceHeader = "Workspace (snapshot at session start — re-check with `git` before acting on it):"

// Snapshot of the project the working directory is in: its root, the branch, whether it is a linked worktree, what changed, the last
// commits, the manifests and the commands that check it. Empty outside a project
func workspace(ctx context.Context, cwd string) string {
	root, repo := tools.ProjectRoot(cwd)
	if root == "" {
		return ""
	}
	lines := []string{workspaceHeader, "- Root: " + root}
	if repo != "" {
		status := ""
		if env := git.RepoEnv(ctx, root); env != nil {
			status = gitProbe(ctx, env, "-C", root, "status", "--porcelain=2", "--branch")
		}
		branch, counts := parseStatus(status)
		head := branch["head"]
		switch {
		case head == "(detached)":
			lines = append(lines, "- Branch: (detached HEAD)")
		case head != "":
			line := "- Branch: " + head
			if upstream := branch["upstream"]; upstream != "" {
				line += " → " + upstream
				ahead, behind := cmp.Or(branch["ahead"], "0"), cmp.Or(branch["behind"], "0")
				if ahead != "0" || behind != "0" {
					line += " (ahead " + ahead + ", behind " + behind + ")"
				}
			}
			lines = append(lines, line)
		}
		base := git.Env(ctx, nil)
		gitDir, commonDir := gitProbe(ctx, base, "-C", root, "rev-parse", "--git-dir"), gitProbe(ctx, base, "-C", root, "rev-parse", "--git-common-dir")
		if gitDir != "" && commonDir != "" && resolve(filepath.Join(root, gitDir)) != resolve(filepath.Join(root, commonDir)) {
			on := ""
			if head != "" && head != "(detached)" {
				on = " on branch " + head
			}
			lines = append(lines, "- Worktree: linked, isolated"+on+" (git state shared with primary tree). Changes here do not affect the main "+
				"working tree or other agents; commit and push your changes, and open a PR if appropriate.")
		}
		var dirty []string
		for _, label := range []string{"staged", "modified", "untracked", "conflicts"} {
			if n := counts[label]; n > 0 {
				dirty = append(dirty, strconv.Itoa(n)+" "+label)
			}
		}
		if len(dirty) > 0 {
			lines = append(lines, "- Status: "+strings.Join(dirty, ", "))
		} else {
			lines = append(lines, "- Status: clean")
		}
		if recent := gitProbe(ctx, base, "-C", root, "log", "-3", "--pretty=%h %s"); recent != "" {
			lines = append(lines, "- Recent commits:")
			for _, commit := range strings.Split(recent, "\n") {
				lines = append(lines, "    "+commit)
			}
		}
	}
	project := tools.ProjectAt(root)
	if len(project.Manifests) > 0 {
		managers := ""
		if len(project.Managers) > 0 {
			managers = " (" + strings.Join(project.Managers, "/") + ")"
		}
		lines = append(lines, "- Project: "+strings.Join(project.Manifests[:min(len(project.Manifests), manifestsShown)], ", ")+managers)
	}
	if len(project.Verify) > 0 {
		lines = append(lines, "- Verify: "+strings.Join(project.Verify, "; "))
	}
	if len(project.ContextFiles) > 0 {
		lines = append(lines, "- Context files: "+strings.Join(project.ContextFiles, ", "))
	}
	return strings.Join(lines, "\n")
}

// Branch lines and change counts of git status --porcelain=2 --branch
func parseStatus(porcelain string) (map[string]string, map[string]int) {
	branch := map[string]string{}
	counts := map[string]int{}
	for _, line := range strings.Split(porcelain, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.head") || strings.HasPrefix(line, "# branch.upstream"):
			if parts := strings.SplitN(line, " ", 3); len(parts) == 3 {
				branch[strings.TrimPrefix(parts[1], "branch.")] = parts[2]
			}
		case strings.HasPrefix(line, "# branch.ab"):
			if parts := strings.Fields(line); len(parts) >= 4 {
				branch["ahead"] = strings.TrimLeft(parts[2], "+")
				branch["behind"] = strings.TrimLeft(parts[3], "-")
			}
		case strings.HasPrefix(line, "1 ") || strings.HasPrefix(line, "2 "):
			if parts := strings.SplitN(line, " ", 3); len(parts) >= 2 && len(parts[1]) >= 2 {
				if parts[1][0] != '.' {
					counts["staged"]++
				}
				if parts[1][1] != '.' {
					counts["modified"]++
				}
			}
		case strings.HasPrefix(line, "u "):
			counts["conflicts"]++
		case strings.HasPrefix(line, "? "):
			counts["untracked"]++
		}
	}
	return branch, counts
}
