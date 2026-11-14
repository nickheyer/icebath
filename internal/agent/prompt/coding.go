package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/env"
)

const CodingToolset = "coding"

const (
	codeScanMaxEntries = 500
	maxVerifyCommands  = 8
	maxFactFileBytes   = 256 * 1024
	codingGitTimeout   = 2500 * time.Millisecond
	markerRootDepth    = 7
	projectFactsShown  = 6
)

var interactiveCodingPlatforms = map[string]bool{"cli": true, "tui": true, "acp": true, "desktop": true, "": true}

var projectMarkers = []string{
	"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "package.json", "tsconfig.json", "deno.json",
	"Cargo.toml", "go.mod", "pom.xml", "build.gradle", "build.gradle.kts", "Gemfile", "composer.json", "mix.exs",
	"pubspec.yaml", "CMakeLists.txt", "Makefile", "Dockerfile", "AGENTS.md", "CLAUDE.md", ".cursorrules",
}

var workspaceContextFiles = []string{"AGENTS.md", "CLAUDE.md", ".cursorrules"}

var codeExtensions = map[string]bool{
	".py": true, ".pyi": true, ".ipynb": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".mjs": true, ".cjs": true,
	".go": true, ".rs": true, ".java": true, ".kt": true, ".kts": true, ".scala": true, ".rb": true, ".php": true, ".c": true,
	".h": true, ".cc": true, ".cpp": true, ".hpp": true, ".cs": true, ".swift": true, ".m": true, ".mm": true, ".dart": true,
	".ex": true, ".exs": true, ".lua": true, ".sh": true, ".bash": true, ".zsh": true, ".sql": true, ".vue": true,
	".svelte": true, ".r": true, ".jl": true, ".hs": true, ".clj": true, ".erl": true, ".pl": true,
}

var codeScanSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "venv": true, ".venv": true, "__pycache__": true, "dist": true, "build": true,
	"target": true, ".next": true, ".turbo": true, "vendor": true,
}

type lockfile struct {
	name    string
	manager string
}

var pyLockfiles = []lockfile{{"uv.lock", "uv"}, {"poetry.lock", "poetry"}, {"Pipfile.lock", "pipenv"}}

var jsLockfiles = []lockfile{
	{"pnpm-lock.yaml", "pnpm"}, {"bun.lockb", "bun"}, {"bun.lock", "bun"}, {"yarn.lock", "yarn"}, {"package-lock.json", "npm"},
}

var verifyTargets = []string{"test", "tests", "lint", "typecheck", "check", "build", "fmt", "format"}

type editFormat struct {
	needles []string
	line    string
}

var editFormatGuidance = []editFormat{
	{
		[]string{"gpt", "codex"},
		"- Edit format: author new files with `write_file`; for edits to " +
			"existing code use `patch` with `mode='patch'` (V4A diff) — including " +
			"single-file edits. It's the edit format you handle most reliably.",
	},
	{
		[]string{"claude", "sonnet", "opus", "haiku",
			"gemini", "gemma", "deepseek", "qwen", "kimi", "glm", "grok",
			string([]byte{'h', 'e', 'r', 'm', 'e', 's'}), "llama", "mistral", "devstral", "minimax"},
		"- Edit format: author new files with `write_file`; for edits to " +
			"existing code prefer `patch` in `mode='replace'` — match a unique " +
			"snippet and swap it. Reach for `mode='patch'` (V4A) only when an edit " +
			"genuinely spans several files at once.",
	},
}

const codingAgentGuidance = "You are a coding agent pairing with the user inside their codebase. " +
	"Operate like a careful senior engineer.\n" +
	"\n" +
	"Gather context first:\n" +
	"- Read the relevant files with `read_file` and locate code with " +
	"`search_files` before changing anything. Trace a symbol to its definition " +
	"and usages rather than guessing its shape.\n" +
	"- Batch independent lookups: when several reads/searches don't depend on " +
	"each other, issue them together in one turn instead of one at a time.\n" +
	"- Never invent files, symbols, APIs, or imports. If you haven't seen it in " +
	"the repo, go look. Don't assume a library is available — check the project " +
	"manifest (pyproject.toml / package.json / Cargo.toml / go.mod) and how " +
	"neighbouring files import it.\n" +
	"\n" +
	"Make changes through the tools, not the chat:\n" +
	"- Edit with `patch`/`write_file`. Do NOT print code blocks to the user as " +
	"a substitute for editing — apply the change, then summarise it. Only show " +
	"code when the user explicitly asks to see it.\n" +
	"- Match the project's existing style and conventions; AGENTS.md / " +
	"CLAUDE.md / .cursorrules already in context win over your defaults. Touch " +
	"only what the task needs — no drive-by refactors, renames, or reformatting " +
	"— and add any imports/dependencies your code requires.\n" +
	"- If an edit fails to apply, re-read the file to get the current exact " +
	"contents before retrying — don't repeat a stale patch. If the same region " +
	"fails twice, rewrite the enclosing function or file with `write_file` " +
	"instead of attempting a third patch.\n" +
	"\n" +
	"Verify, and know when to stop:\n" +
	"- Use `terminal` for git, builds, tests, and inspection. Run the relevant " +
	"tests/linter/build and confirm they pass before claiming the work is done.\n" +
	"- Terminal state persists across calls: current directory and exported " +
	"environment variables carry forward. Activate a virtualenv or export setup " +
	"vars once, then reuse that state instead of re-sourcing it before every " +
	"test command.\n" +
	"- Fix root causes, not symptoms: when you find a bug, check sibling call " +
	"paths for the same flaw and fix the class, not just the reported site.\n" +
	"- When fixing linter/type errors on a file, stop after about three " +
	"attempts on the same file and ask the user rather than looping.\n" +
	todoSentence + "\n" +
	"\n" +
	"Respect the user's repo: don't commit, push, or rewrite history unless " +
	"asked, and never read, print, or commit secrets — leave `.env` and " +
	"credential files alone unless the user explicitly asks. The Workspace " +
	"block below is a snapshot from session start — re-run `git status`/" +
	"`git branch` before relying on it. Be concise: lead with the change or " +
	"answer, not a preamble."

const (
	todoSentence = "- Track multi-step work with `todo_list`. Reference code as " +
		"`path:line` instead of pasting whole files."
	noTodoSentence = "- Reference code as `path:line` instead of pasting whole files."
)

var nonCodingSkillCategories = []string{
	"apple", "communication", "cooking", "creative", "email", "finance", "gaming", "gifs", "health", "media",
	"music", "note-taking", "productivity", "shopping", "smart-home", "social-media", "travel", "yuanbao",
}

const testRunner = "py" + "test"

const workspaceBlockHeader = "Workspace (snapshot at session start — re-check with `git` before acting on it):"

type RuntimeMode struct {
	Coding       bool
	Surface      string
	Cwd          string
	ConfigMode   config.CodingContextMode
	Model        string
	Instructions string
}

type ProjectFacts struct {
	Root            string   `json:"root"`
	Manifests       []string `json:"manifests"`
	PackageManagers []string `json:"packageManagers"`
	VerifyCommands  []string `json:"verifyCommands"`
	ContextFiles    []string `json:"contextFiles"`
}

type factsError struct {
	path string
}

func (e *factsError) Error() string {
	return "package.json scripts in " + e.path + " is neither a mapping, a list nor text"
}

type statusError struct {
	line string
}

func (e *statusError) Error() string {
	return "git status line not understood: " + strconv.Quote(e.line)
}

func modelFamily(model string) (editFormat, bool) {
	lowered := strings.ToLower(model)
	if lowered == "" {
		return editFormat{}, false
	}
	for _, f := range editFormatGuidance {
		for _, n := range f.needles {
			if strings.Contains(lowered, n) {
				return f, true
			}
		}
	}
	return editFormat{}, false
}

func (b *Builder) CodingMode(ctx context.Context) config.CodingContextMode {
	cfg, err := b.cfg.LoadReadonly(ctx)
	if err != nil {
		return config.CodingAuto
	}
	return cfg.Agent.CodingContext.Canonical()
}

func (b *Builder) codingInstructions(ctx context.Context) string {
	cfg, err := b.cfg.Load(ctx)
	if err != nil {
		return ""
	}
	raw := cfg.Agent.CodingInstructions
	items := []string{raw.Text}
	if raw.IsList {
		items = raw.Items
	}
	var kept []string
	for _, item := range items {
		if t := trim(item); t != "" {
			kept = append(kept, t)
		}
	}
	return strings.Join(kept, "\n")
}

func (b *Builder) resolveCodingCwd(ctx context.Context, cwd string) (string, error) {
	if cwd != "" {
		return home.ExpandUser(cwd), nil
	}
	resolved, err := env.ResolveAgentCwd(ctx, b.env.Settings())
	if err == nil {
		return resolved, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}
	return os.Getwd()
}

func codingGitRoot(cwd string) (string, bool) {
	return findGitRoot(cwd)
}

func accountHome() (string, bool) {
	h, err := home.UserHome()
	if err != nil {
		return "", false
	}
	return resolvePath(h), true
}

func markerRoot(cwd string) (string, bool) {
	userHome, haveHome := accountHome()
	tempRoot := resolvePath(os.TempDir())
	chain := ancestors(resolvePath(cwd))
	if len(chain) > markerRootDepth {
		chain = chain[:markerRootDepth]
	}
	for _, parent := range chain {
		if (haveHome && parent == userHome) || parent == tempRoot {
			continue
		}
		for _, marker := range projectMarkers {
			if exists(filepath.Join(parent, marker)) {
				return parent, true
			}
		}
	}
	return "", false
}

func hasCodeFiles(root string) bool {
	type frame struct {
		dir    string
		isRoot bool
	}
	seen := 0
	stack := []frame{{root, true}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(f.dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			seen++
			if seen > codeScanMaxEntries {
				return false
			}
			name := entry.Name()
			mode := entry.Type()
			if mode&os.ModeSymlink != 0 {
				info, err := os.Stat(filepath.Join(f.dir, name))
				if err != nil {
					continue
				}
				mode = info.Mode()
			}
			switch {
			case mode.IsRegular():
				if codeExtensions[strings.ToLower(filepath.Ext(name))] {
					return true
				}
			case f.isRoot && mode.IsDir() && !codeScanSkipDirs[name] && !strings.HasPrefix(name, "."):
				stack = append(stack, frame{filepath.Join(f.dir, name), false})
			}
		}
	}
	return false
}

func detectCoding(mode config.CodingContextMode, platform, cwd string) bool {
	switch mode {
	case config.CodingOff:
		return false
	case config.CodingOn:
		return true
	}
	if platform != "" && !interactiveCodingPlatforms[strings.ToLower(trim(platform))] {
		return false
	}
	if _, ok := markerRoot(cwd); ok {
		return true
	}
	gitRoot, ok := codingGitRoot(cwd)
	if !ok {
		return false
	}
	if userHome, haveHome := accountHome(); haveHome && gitRoot == userHome {
		return false
	}
	return hasCodeFiles(gitRoot)
}

func (b *Builder) Mode(ctx context.Context, platform, cwd, model string) (RuntimeMode, error) {
	resolved, err := b.resolveCodingCwd(ctx, cwd)
	if err != nil {
		return RuntimeMode{}, err
	}
	mode := b.CodingMode(ctx)
	return RuntimeMode{
		Coding:       detectCoding(mode, strings.ToLower(trim(platform)), resolved),
		Surface:      platform,
		Cwd:          resolved,
		ConfigMode:   mode,
		Model:        model,
		Instructions: b.codingInstructions(ctx),
	}, nil
}

func (m RuntimeMode) Focus() bool {
	return m.Coding && m.ConfigMode == config.CodingFocus
}

func FocusToolsets(mode config.CodingContextMode, mcpServers []string) []string {
	if mode != config.CodingFocus {
		return nil
	}
	return append([]string{CodingToolset}, mcpServers...)
}

func (m RuntimeMode) Toolsets(mcpServers []string) []string {
	if !m.Focus() {
		return nil
	}
	return FocusToolsets(m.ConfigMode, mcpServers)
}

func (m RuntimeMode) CompactSkillCategories() []string {
	if !m.Focus() {
		return nil
	}
	return slices.Clone(nonCodingSkillCategories)
}

type codingBlocks struct {
	prefix    []string
	workspace []string
	trailing  []string
}

func (b *Builder) codingBlocks(ctx context.Context, m RuntimeMode, names tool.Names, replay *string) (codingBlocks, error) {
	if !m.Coding {
		return codingBlocks{}, nil
	}
	brief := codingAgentGuidance
	if !has(names, "todo_list") {
		brief = strings.ReplaceAll(brief, todoSentence, noTodoSentence)
	}
	if f, ok := modelFamily(m.Model); ok {
		brief += "\n" + f.line
	}
	out := codingBlocks{prefix: []string{brief}}
	workspace := ""
	if replay != nil {
		workspace = *replay
	} else {
		var err error
		workspace, err = b.workspaceBlock(ctx, m.Cwd)
		if err != nil {
			return codingBlocks{}, err
		}
	}
	if workspace != "" {
		out.workspace = []string{workspace}
	}
	if m.Instructions != "" {
		out.trailing = []string{"Operator instructions (from config):\n" + m.Instructions}
	}
	return out, nil
}

type gitBranch struct {
	head     string
	upstream string
	ahead    string
	behind   string
	hasAB    bool
}

type gitCounts struct {
	staged    int
	modified  int
	untracked int
	conflicts int
}

func parseStatus(porcelain string) (gitBranch, gitCounts, error) {
	var branch gitBranch
	var counts gitCounts
	for _, line := range splitLines(porcelain) {
		switch {
		case strings.HasPrefix(line, "# branch.head"), strings.HasPrefix(line, "# branch.upstream"):
			parts := fieldsMax(line, 3)
			if len(parts) < 2 {
				return branch, counts, &statusError{line}
			}
			value := parts[len(parts)-1]
			switch strings.TrimPrefix(parts[1], "branch.") {
			case "head":
				branch.head = value
			case "upstream":
				branch.upstream = value
			}
		case strings.HasPrefix(line, "# branch.ab"):
			parts := strings.FieldsFunc(line, isSpace)
			if len(parts) < 4 {
				return branch, counts, &statusError{line}
			}
			branch.ahead, branch.behind, branch.hasAB = strings.TrimLeft(parts[2], "+"), strings.TrimLeft(parts[3], "-"), true
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "):
			parts := fieldsMax(line, 3)
			if len(parts) < 2 {
				return branch, counts, &statusError{line}
			}
			xy := []rune(parts[1])
			if len(xy) < 2 {
				return branch, counts, &statusError{line}
			}
			if xy[0] != '.' {
				counts.staged++
			}
			if xy[1] != '.' {
				counts.modified++
			}
		case strings.HasPrefix(line, "u "):
			counts.conflicts++
		case strings.HasPrefix(line, "? "):
			counts.untracked++
		}
	}
	return branch, counts, nil
}

func fieldsMax(s string, n int) []string {
	var out []string
	rest := strings.TrimLeftFunc(s, isSpace)
	for rest != "" && len(out) < n-1 {
		i := strings.IndexFunc(rest, isSpace)
		if i < 0 {
			break
		}
		out = append(out, rest[:i])
		rest = strings.TrimLeftFunc(rest[i:], isSpace)
	}
	if rest != "" {
		out = append(out, rest)
	}
	return out
}

func readSmall(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxFactFileBytes {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := strings.TrimPrefix(strings.ToValidUTF8(string(data), string(rune(0xFFFD))), byteOrderMark)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

func packageScripts(path string) (func(name string) bool, error) {
	none := func(string) bool { return false }
	text := readSmall(path)
	if text == "" {
		text = "{}"
	}
	var doc any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return none, nil
	}
	top, ok := doc.(map[string]any)
	if !ok {
		return none, nil
	}
	switch scripts := top["scripts"].(type) {
	case nil:
		return none, nil
	case map[string]any:
		return func(name string) bool { _, ok := scripts[name]; return ok }, nil
	case []any:
		return func(name string) bool { return slices.Contains(scripts, any(name)) }, nil
	case string:
		return func(name string) bool { return strings.Contains(scripts, name) }, nil
	case bool:
		if !scripts {
			return none, nil
		}
	case float64:
		if scripts == 0 {
			return none, nil
		}
	}
	return none, &factsError{path}
}

func detectProjectFacts(root string) (ProjectFacts, error) {
	var verify []string
	if isFile(filepath.Join(root, "scripts", "run_tests.sh")) {
		verify = append(verify, "scripts/run_tests.sh")
	}
	if pkg := filepath.Join(root, "package.json"); isFile(pkg) {
		inScripts, err := packageScripts(pkg)
		if err != nil {
			return ProjectFacts{}, err
		}
		manager := "npm"
		for _, l := range jsLockfiles {
			if isFile(filepath.Join(root, l.name)) {
				manager = l.manager
				break
			}
		}
		for _, name := range verifyTargets {
			if inScripts(name) {
				verify = append(verify, manager+" run "+name)
			}
		}
	}
	if isFile(filepath.Join(root, testRunner+".ini")) || strings.Contains(readSmall(filepath.Join(root, "pyproject.toml")), "[tool."+testRunner) {
		verify = append(verify, testRunner)
	}
	if makefile := readSmall(filepath.Join(root, "Makefile")); makefile != "" {
		for _, name := range verifyTargets {
			if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*:`).MatchString(makefile) {
				verify = append(verify, "make "+name)
			}
		}
	}
	facts := ProjectFacts{Root: root, Manifests: []string{}, PackageManagers: []string{}, VerifyCommands: []string{}, ContextFiles: []string{}}
	for _, m := range projectMarkers {
		if !slices.Contains(workspaceContextFiles, m) && isFile(filepath.Join(root, m)) {
			facts.Manifests = append(facts.Manifests, m)
		}
	}
	for _, l := range append(slices.Clone(pyLockfiles), jsLockfiles...) {
		if isFile(filepath.Join(root, l.name)) && !slices.Contains(facts.PackageManagers, l.manager) {
			facts.PackageManagers = append(facts.PackageManagers, l.manager)
		}
	}
	for _, v := range verify {
		if !slices.Contains(facts.VerifyCommands, v) {
			facts.VerifyCommands = append(facts.VerifyCommands, v)
		}
	}
	if len(facts.VerifyCommands) > maxVerifyCommands {
		facts.VerifyCommands = facts.VerifyCommands[:maxVerifyCommands]
	}
	for _, c := range workspaceContextFiles {
		if isFile(filepath.Join(root, c)) {
			facts.ContextFiles = append(facts.ContextFiles, c)
		}
	}
	return facts, nil
}

func (b *Builder) workspaceRoots(ctx context.Context, cwd string) (gitRoot string, inGit bool, root string, ok bool, err error) {
	resolved, err := b.resolveCodingCwd(ctx, cwd)
	if err != nil {
		return "", false, "", false, err
	}
	if gitRoot, inGit = codingGitRoot(resolved); inGit {
		return gitRoot, true, gitRoot, true, nil
	}
	root, ok = markerRoot(resolved)
	return "", false, root, ok, nil
}

func (b *Builder) Facts(ctx context.Context, cwd string) (*ProjectFacts, error) {
	_, _, root, ok, err := b.workspaceRoots(ctx, cwd)
	if err != nil || !ok {
		return nil, err
	}
	facts, err := detectProjectFacts(root)
	if err != nil {
		return nil, err
	}
	return &facts, nil
}

func (b *Builder) workspaceBlock(ctx context.Context, cwd string) (string, error) {
	_, inGit, root, ok, err := b.workspaceRoots(ctx, cwd)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	lines := []string{workspaceBlockHeader, "- Root: " + root}
	if inGit {
		status := ""
		if statusEnv, ok := b.git.repoEnv(ctx, root); ok {
			status = b.git.probe(ctx, statusEnv, codingGitTimeout, "-C", root, "status", "--porcelain=2", "--branch")
		}
		branch, counts, err := parseStatus(status)
		if err != nil {
			return "", err
		}
		head := branch.head
		switch {
		case head == "(detached)":
			lines = append(lines, "- Branch: (detached HEAD)")
		case head != "":
			ahead, behind := "0", "0"
			if branch.hasAB {
				ahead, behind = branch.ahead, branch.behind
			}
			upstream, ab := "", ""
			if branch.upstream != "" {
				upstream = " → " + branch.upstream
				if ahead != "0" || behind != "0" {
					ab = " (ahead " + ahead + ", behind " + behind + ")"
				}
			}
			lines = append(lines, "- Branch: "+head+upstream+ab)
		}
		gitDir := b.git.probe(ctx, nil, codingGitTimeout, "-C", root, "rev-parse", "--git-dir")
		commonDir := b.git.probe(ctx, nil, codingGitTimeout, "-C", root, "rev-parse", "--git-common-dir")
		if gitDir != "" && commonDir != "" && resolvePath(gitDir) != resolvePath(commonDir) {
			onBranch := ""
			if head != "" && head != "(detached)" {
				onBranch = " on branch " + head
			}
			lines = append(lines, "- Worktree: linked, isolated"+onBranch+" (git state shared with primary tree). Changes here do"+
				" not affect the main working tree or other agents; commit and push your changes, and open a"+
				" PR if appropriate.")
		}
		var dirty []string
		for _, c := range []struct {
			label string
			n     int
		}{{"staged", counts.staged}, {"modified", counts.modified}, {"untracked", counts.untracked}, {"conflicts", counts.conflicts}} {
			if c.n != 0 {
				dirty = append(dirty, strconv.Itoa(c.n)+" "+c.label)
			}
		}
		if len(dirty) == 0 {
			dirty = []string{"clean"}
		}
		lines = append(lines, "- Status: "+strings.Join(dirty, ", "))
		if recent := b.git.probe(ctx, nil, codingGitTimeout, "-C", root, "log", "-3", "--pretty=%h %s"); recent != "" {
			lines = append(lines, "- Recent commits:")
			for _, c := range splitLines(recent) {
				lines = append(lines, "    "+c)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	facts, err := detectProjectFacts(root)
	if err != nil {
		return "", err
	}
	if len(facts.Manifests) > 0 {
		managers := ""
		if len(facts.PackageManagers) > 0 {
			managers = " (" + strings.Join(facts.PackageManagers, "/") + ")"
		}
		shown := facts.Manifests
		if len(shown) > projectFactsShown {
			shown = shown[:projectFactsShown]
		}
		lines = append(lines, "- Project: "+strings.Join(shown, ", ")+managers)
	}
	if len(facts.VerifyCommands) > 0 {
		lines = append(lines, "- Verify: "+strings.Join(facts.VerifyCommands, "; "))
	}
	if len(facts.ContextFiles) > 0 {
		lines = append(lines, "- Context files: "+strings.Join(facts.ContextFiles, ", "))
	}
	return strings.Join(lines, "\n"), nil
}

type WorkspacePin struct {
	Key   string
	Block string
}

func (b *Builder) workspacePinKey(ctx context.Context) string {
	cwd, ok, err := env.ResolveContextCwd(ctx, b.env.Settings(), true)
	if err == nil && ok {
		return cwd
	}
	cwd, err = env.ResolveAgentCwd(ctx, b.env.Settings())
	if err != nil {
		return ""
	}
	return cwd
}

func persistedWorkspaceBlock(prompt, key string) (string, bool) {
	head := "\n\n" + workspaceBlockHeader + "\n- Root: "
	start := strings.Index(prompt, head)
	if start < 0 {
		return "", true
	}
	cwd := resolvePath(key)
	for start >= 0 {
		block, _, _ := strings.Cut(prompt[start+2:], "\n\n")
		lines := strings.SplitN(block, "\n", 3)
		root := resolvePath(strings.TrimPrefix(lines[1], "- Root: "))
		if root == cwd || slices.Contains(ancestors(cwd)[1:], root) {
			return block, true
		}
		next := strings.Index(prompt[start+2:], head)
		if next < 0 {
			break
		}
		start += 2 + next
	}
	return "", false
}

func seedWorkspacePin(stored, key string) *WorkspacePin {
	if stored == "" {
		return nil
	}
	if storedCwd := RuntimeHostValue(stored, "Current working directory"); storedCwd != "" && storedCwd != key {
		return nil
	}
	block, ok := persistedWorkspaceBlock(stored, key)
	if !ok || block == "" {
		return nil
	}
	return &WorkspacePin{Key: key, Block: block}
}

func (b *Builder) codingParts(ctx context.Context, in Inputs) (codingBlocks, *WorkspacePin, error) {
	pin := in.Workspace
	if !anyTool(in.ToolNames) {
		return codingBlocks{}, pin, nil
	}
	cwd, _, err := env.ResolveContextCwd(ctx, b.env.Settings(), true)
	if err != nil {
		return codingBlocks{}, pin, softCodingError(ctx, err)
	}
	key := b.workspacePinKey(ctx)
	if pin == nil {
		pin = seedWorkspacePin(in.StoredPrompt, key)
	}
	var replay *string
	if pin != nil && pin.Key == key {
		replay = &pin.Block
	}
	mode, err := b.Mode(ctx, in.Platform, cwd, in.Model)
	if err != nil {
		return codingBlocks{}, pin, softCodingError(ctx, err)
	}
	blocks, err := b.codingBlocks(ctx, mode, in.ToolNames, replay)
	if err != nil {
		return codingBlocks{}, pin, softCodingError(ctx, err)
	}
	if replay == nil {
		block := ""
		if len(blocks.workspace) > 0 {
			block = blocks.workspace[0]
		}
		pin = &WorkspacePin{Key: key, Block: block}
	}
	return blocks, pin, nil
}

func softCodingError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	var unfilled *tool.UnfilledError
	if errors.As(err, &unfilled) {
		return err
	}
	slog.Debug("coding posture blocks unavailable", "error", err)
	return nil
}
