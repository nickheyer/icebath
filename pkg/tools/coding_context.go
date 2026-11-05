package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CodingToolset is the toolset the coding posture collapses to under focus
const CodingToolset = "coding"

var interactiveCodingPlatforms = map[string]bool{"cli": true, "tui": true, "acp": true, "desktop": true, "": true}

var projectMarkers = []string{
	"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "package.json", "tsconfig.json", "deno.json",
	"Cargo.toml", "go.mod", "pom.xml", "build.gradle", "build.gradle.kts", "Gemfile", "composer.json", "mix.exs",
	"pubspec.yaml", "CMakeLists.txt", "Makefile", "Dockerfile", "AGENTS.md", "CLAUDE.md", ".cursorrules",
}

var contextFileNames = []string{"AGENTS.md", "CLAUDE.md", ".cursorrules"}

var codeExtensions = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`.py .pyi .ipynb .js .jsx .ts .tsx .mjs .cjs .go .rs .java .kt .kts .scala .rb .php .c .h .cc .cpp .hpp
		.cs .swift .m .mm .dart .ex .exs .lua .sh .bash .zsh .sql .vue .svelte .r .jl .hs .clj .erl .pl`) {
		codeExtensions[e] = true
	}
}

var codeScanSkipDirs = map[string]bool{".git": true, "node_modules": true, "venv": true, ".venv": true, "__pycache__": true, "dist": true, "build": true, "target": true, ".next": true, ".turbo": true, "vendor": true}

const codeScanMaxEntries = 500

var pyLockfiles = [][2]string{{"uv.lock", "uv"}, {"poetry.lock", "poetry"}, {"Pipfile.lock", "pipenv"}}
var jsLockfiles = [][2]string{{"pnpm-lock.yaml", "pnpm"}, {"bun.lockb", "bun"}, {"bun.lock", "bun"}, {"yarn.lock", "yarn"}, {"package-lock.json", "npm"}}
var verifyTargets = []string{"test", "tests", "lint", "typecheck", "check", "build", "fmt", "format"}

const (
	maxVerifyCommands = 8
	maxFactFileBytes  = 256 * 1024
	gitProbeTimeout   = 2500 * time.Millisecond
)

type editFormatGuidance struct {
	needles []string
	line    string
}

var editFormatGuidances = []struct {
	family string
	editFormatGuidance
}{
	{"patch", editFormatGuidance{[]string{"gpt", "codex"},
		"- Edit format: author new files with `write_file`; for edits to existing code use `patch` with `mode='patch'` (V4A diff) — including single-file edits. It's the edit format you handle most reliably."}},
	{"replace", editFormatGuidance{[]string{"claude", "sonnet", "opus", "haiku", "gemini", "gemma", "deepseek", "qwen", "kimi", "glm", "grok", "hermes", "llama", "mistral", "devstral", "minimax"},
		"- Edit format: author new files with `write_file`; for edits to existing code prefer `patch` in `mode='replace'` — match a unique snippet and swap it. Reach for `mode='patch'` (V4A) only when an edit genuinely spans several files at once."}},
}

// CodingAgentGuidance is the coding posture brief prepended to the system prompt
const CodingAgentGuidance = "You are a coding agent pairing with the user inside their codebase. Operate like a careful senior engineer.\n" +
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
	"- Track multi-step work with `todo_list`. Reference code as `path:line` instead of pasting whole files.\n" +
	"\n" +
	"Respect the user's repo: don't commit, push, or rewrite history unless asked, and never read, print, or commit secrets — leave `.env` and credential files alone unless the user explicitly asks. The Workspace block below is a snapshot from session start — re-run `git status`/`git branch` before relying on it. Be concise: lead with the change or answer, not a preamble."

const todoSentence = "- Track multi-step work with `todo_list`. Reference code as `path:line` instead of pasting whole files."
const noTodoSentence = "- Reference code as `path:line` instead of pasting whole files."

var nonCodingSkillCategories = []string{"apple", "communication", "cooking", "creative", "email", "finance", "gaming", "gifs", "health", "media", "music", "note-taking", "productivity", "shopping", "smart-home", "social-media", "travel", "yuanbao"}

var codingModeAliases = map[string]string{"focus": "focus", "strict": "focus", "lean": "focus", "on": "on", "true": "on", "yes": "on", "1": "on", "always": "on", "off": "off", "false": "off", "no": "off", "0": "off", "never": "off"}

// ContextProfile is a named operating posture
type ContextProfile struct {
	Name                   string
	Toolset                string
	Guidance               string
	ModelHint              string
	CompactSkillCategories []string
}

// GeneralProfile is the posture outside code workspaces
var GeneralProfile = ContextProfile{Name: "general"}

// CodingProfile is the posture inside a code workspace on an interactive surface
var CodingProfile = ContextProfile{Name: "coding", Toolset: CodingToolset, Guidance: CodingAgentGuidance, ModelHint: "coding", CompactSkillCategories: nonCodingSkillCategories}

// Edit format family key for a model id or empty
func modelFamily(model string) string {
	lowered := strings.ToLower(model)
	if lowered == "" {
		return ""
	}
	for _, g := range editFormatGuidances {
		for _, n := range g.needles {
			if strings.Contains(lowered, n) {
				return g.family
			}
		}
	}
	return ""
}

func editFormatLine(family string) string {
	for _, g := range editFormatGuidances {
		if g.family == family {
			return g.line
		}
	}
	return ""
}

// Normalized agent.coding_context mode
func codingMode(env *Env) string {
	raw := ""
	if env != nil {
		raw = env.Config.String("agent.coding_context", "auto")
	}
	if m, ok := codingModeAliases[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return m
	}
	return "auto"
}

func resolveCodingCwd(env *Env, cwd string) string {
	if cwd != "" {
		return expandUser(cwd)
	}
	if p, err := ResolveAgentCwd(env); err == nil && p != "" {
		return p
	}
	wd, _ := os.Getwd()
	return wd
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func resolveAbs(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func ancestors(p string) []string {
	var out []string
	cur := p
	for {
		out = append(out, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			return out
		}
		cur = parent
	}
}

func gitRoot(cwd string) string {
	for _, p := range ancestors(resolveAbs(cwd)) {
		if pathExists(filepath.Join(p, ".git")) {
			return p
		}
	}
	return ""
}

func userHomeResolved() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return resolveAbs(h)
}

// Nearest project root within six levels, skipping the home and shared temp roots
func markerRoot(cwd string) string {
	home := userHomeResolved()
	temp := resolveAbs(os.TempDir())
	anc := ancestors(resolveAbs(cwd))
	if len(anc) > 7 {
		anc = anc[:7]
	}
	for _, parent := range anc {
		if parent == home || parent == temp {
			continue
		}
		for _, m := range projectMarkers {
			if pathExists(filepath.Join(parent, m)) {
				return parent
			}
		}
	}
	return ""
}

// Bounded check for source files in the root and its immediate subdirectories
func hasCodeFiles(root string) bool {
	seen := 0
	type item struct {
		dir    string
		isRoot bool
	}
	stack := []item{{root, true}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(it.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			seen++
			if seen > codeScanMaxEntries {
				return false
			}
			if e.Type().IsRegular() {
				if codeExtensions[strings.ToLower(filepath.Ext(e.Name()))] {
					return true
				}
			} else if it.isRoot && e.IsDir() && !codeScanSkipDirs[e.Name()] && !strings.HasPrefix(e.Name(), ".") {
				stack = append(stack, item{filepath.Join(it.dir, e.Name()), false})
			}
		}
	}
	return false
}

func detectProfile(mode, platform, cwd string) ContextProfile {
	switch mode {
	case "off":
		return GeneralProfile
	case "on":
		return CodingProfile
	}
	if platform != "" && !interactiveCodingPlatforms[strings.ToLower(strings.TrimSpace(platform))] {
		return GeneralProfile
	}
	if markerRoot(cwd) != "" {
		return CodingProfile
	}
	if root := gitRoot(cwd); root != "" && root != userHomeResolved() && hasCodeFiles(root) {
		return CodingProfile
	}
	return GeneralProfile
}

// Names of MCP servers the user enabled, kept in the coding posture
func enabledMCPServers(env *Env) []string {
	var portable map[string]bool
	if PortableMCPServerNames != nil {
		portable = PortableMCPServerNames(env)
	}
	cfg := env.Config
	if raw, err := readRawConfig(env); err == nil {
		cfg = raw
	}
	names := EnabledMCPServerNames(cfg, portable)
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sortStrings(out)
	return out
}

// PortableMCPServerNames is installed by pkg/plugins to add portable plugin MCP servers
var PortableMCPServerNames func(env *Env) map[string]bool

func postureToolsets(env *Env) []string {
	return append([]string{CodingToolset}, enabledMCPServers(env)...)
}

// FocusToolsets returns the coding posture toolsets under focus mode, nil otherwise
func FocusToolsets(env *Env) []string {
	if codingMode(env) == "focus" {
		return postureToolsets(env)
	}
	return nil
}

// RuntimeMode is the resolved operating posture for a session
type RuntimeMode struct {
	Profile      ContextProfile
	Surface      string
	Cwd          string
	ConfigMode   string
	Model        string
	Instructions string
}

// Kind names the posture profile
func (m RuntimeMode) Kind() string { return m.Profile.Name }

// IsCoding reports whether the coding posture is active
func (m RuntimeMode) IsCoding() bool { return m.Profile.Name == CodingProfile.Name }

// ToolsetSelection returns the focus toolset list or nil to keep the platform default
func (m RuntimeMode) ToolsetSelection(env *Env) []string {
	if m.ConfigMode != "focus" || m.Profile.Toolset == "" {
		return nil
	}
	return postureToolsets(env)
}

// SystemPromptParts returns the prefix, workspace, and trailing posture blocks in order
func (m RuntimeMode) SystemPromptParts(validToolNames map[string]bool, workspaceBlock *string) ([]string, []string, []string) {
	if !m.IsCoding() {
		return nil, nil, nil
	}
	var prefix []string
	if m.Profile.Guidance != "" {
		brief := m.Profile.Guidance
		if validToolNames != nil && !validToolNames["todo_list"] {
			brief = strings.ReplaceAll(brief, todoSentence, noTodoSentence)
		}
		if family := modelFamily(m.Model); family != "" {
			brief = brief + "\n" + editFormatLine(family)
		}
		prefix = append(prefix, brief)
	}
	workspace := ""
	if workspaceBlock == nil {
		workspace = BuildCodingWorkspaceBlock(m.Cwd)
	} else {
		workspace = *workspaceBlock
	}
	var workspaceParts []string
	if workspace != "" {
		workspaceParts = []string{workspace}
	}
	var trailing []string
	if m.Instructions != "" {
		trailing = []string{"Operator instructions (from config):\n" + m.Instructions}
	}
	return prefix, workspaceParts, trailing
}

// SystemBlocks returns the posture blocks as one flat list
func (m RuntimeMode) SystemBlocks() []string {
	prefix, workspace, trailing := m.SystemPromptParts(nil, nil)
	out := append([]string{}, prefix...)
	out = append(out, workspace...)
	return append(out, trailing...)
}

// CompactSkillCategories lists skill categories demoted to names only under focus
func (m RuntimeMode) CompactSkillCategories() map[string]bool {
	out := map[string]bool{}
	if !m.IsCoding() || m.ConfigMode != "focus" {
		return out
	}
	for _, c := range m.Profile.CompactSkillCategories {
		out[c] = true
	}
	return out
}

// ResolveRuntimeMode resolves the operating posture once for a session
func ResolveRuntimeMode(env *Env, platform, cwd, model string) RuntimeMode {
	resolvedCwd := resolveCodingCwd(env, cwd)
	mode := codingMode(env)
	var items []string
	if env != nil {
		switch raw := env.Config.Get("agent.coding_instructions").(type) {
		case []any:
			for _, it := range raw {
				items = append(items, strings.TrimSpace(asStr(it)))
			}
		case nil:
		default:
			items = append(items, strings.TrimSpace(asStr(raw)))
		}
	}
	var kept []string
	for _, it := range items {
		if it != "" {
			kept = append(kept, it)
		}
	}
	return RuntimeMode{
		Profile:      detectProfile(mode, strings.ToLower(strings.TrimSpace(platform)), resolvedCwd),
		Surface:      platform,
		Cwd:          resolvedCwd,
		ConfigMode:   mode,
		Model:        model,
		Instructions: strings.Join(kept, "\n"),
	}
}

// IsCodingContext reports whether the coding posture applies right now
func IsCodingContext(env *Env, platform, cwd string) bool {
	return ResolveRuntimeMode(env, platform, cwd, "").IsCoding()
}

// InFocusWorkspace reports whether the cwd selects the coding toolsets under focus
func InFocusWorkspace(env *Env, platform, cwd string) bool {
	return codingMode(env) == "focus" && IsCodingContext(env, platform, cwd)
}

// CodingSelection returns the focus toolset selection or nil
func CodingSelection(env *Env, platform, cwd string) []string {
	if codingMode(env) != "focus" {
		return nil
	}
	return ResolveRuntimeMode(env, platform, cwd, "").ToolsetSelection(env)
}

// CodingSystemPromptParts returns the coding prefix, workspace snapshot, and trailing guidance
func CodingSystemPromptParts(env *Env, platform, cwd, model string, validToolNames map[string]bool, workspaceBlock *string) ([]string, []string, []string) {
	return ResolveRuntimeMode(env, platform, cwd, model).SystemPromptParts(validToolNames, workspaceBlock)
}

// CodingCompactSkillCategories lists the categories the active posture demotes
func CodingCompactSkillCategories(env *Env, platform, cwd string) map[string]bool {
	return ResolveRuntimeMode(env, platform, cwd, "").CompactSkillCategories()
}

func gitIn(cwd string, args ...string) string {
	return boundedGitProbe(append([]string{"git", "-C", cwd}, args...), gitProbeTimeout, nil)
}

// Parses git status porcelain v2 branch lines and change counts
func parseGitStatus(porcelain string) (map[string]string, map[string]int) {
	branch := map[string]string{}
	counts := map[string]int{"staged": 0, "modified": 0, "untracked": 0, "conflicts": 0}
	for _, line := range strings.Split(porcelain, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.head") || strings.HasPrefix(line, "# branch.upstream"):
			parts := strings.SplitN(line, " ", 3)
			if len(parts) == 3 {
				branch[strings.TrimPrefix(parts[1], "branch.")] = parts[2]
			}
		case strings.HasPrefix(line, "# branch.ab"):
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				branch["ahead"] = strings.TrimLeft(parts[2], "+")
				branch["behind"] = strings.TrimLeft(parts[3], "-")
			}
		case strings.HasPrefix(line, "1 ") || strings.HasPrefix(line, "2 "):
			parts := strings.SplitN(line, " ", 3)
			if len(parts) >= 2 && len(parts[1]) >= 2 {
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

// Reads a small text file, empty on error or when over the fact size cap
func readSmall(path string) string {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxFactFileBytes {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToValidUTF8(string(b), string(rune(0xFFFD))), string(rune(0xFEFF)))
}

// ProjectFacts are the detected manifests, package managers, verify commands, and context files
type ProjectFacts struct {
	Manifests       []string
	PackageManagers []string
	VerifyCommands  []string
	ContextFiles    []string
}

// DetectProjectFacts detects manifests, package managers, verify commands, and context files
func DetectProjectFacts(root string) ProjectFacts {
	var verify []string
	if isFile(filepath.Join(root, "scripts", "run_tests.sh")) {
		verify = append(verify, "scripts/run_tests.sh")
	}
	if isFile(filepath.Join(root, "package.json")) {
		scripts := map[string]any{}
		var doc map[string]any
		if err := json.Unmarshal([]byte(readSmall(filepath.Join(root, "package.json"))), &doc); err == nil {
			if s, ok := doc["scripts"].(map[string]any); ok {
				scripts = s
			}
		}
		jsPM := "npm"
		for _, lf := range jsLockfiles {
			if isFile(filepath.Join(root, lf[0])) {
				jsPM = lf[1]
				break
			}
		}
		for _, name := range verifyTargets {
			if _, ok := scripts[name]; ok {
				verify = append(verify, jsPM+" run "+name)
			}
		}
	}
	if isFile(filepath.Join(root, "pytest.ini")) || strings.Contains(readSmall(filepath.Join(root, "pyproject.toml")), "[tool.pytest") {
		verify = append(verify, "pytest")
	}
	makefile := readSmall(filepath.Join(root, "Makefile"))
	if makefile != "" {
		for _, name := range verifyTargets {
			if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*:`).MatchString(makefile) {
				verify = append(verify, "make "+name)
			}
		}
	}
	facts := ProjectFacts{}
	for _, m := range projectMarkers {
		if !inList(contextFileNames, m) && isFile(filepath.Join(root, m)) {
			facts.Manifests = append(facts.Manifests, m)
		}
	}
	seenPM := map[string]bool{}
	for _, lf := range append(append([][2]string{}, pyLockfiles...), jsLockfiles...) {
		if isFile(filepath.Join(root, lf[0])) && !seenPM[lf[1]] {
			seenPM[lf[1]] = true
			facts.PackageManagers = append(facts.PackageManagers, lf[1])
		}
	}
	seenV := map[string]bool{}
	for _, v := range verify {
		if !seenV[v] {
			seenV[v] = true
			facts.VerifyCommands = append(facts.VerifyCommands, v)
		}
	}
	if len(facts.VerifyCommands) > maxVerifyCommands {
		facts.VerifyCommands = facts.VerifyCommands[:maxVerifyCommands]
	}
	for _, c := range contextFileNames {
		if isFile(filepath.Join(root, c)) {
			facts.ContextFiles = append(facts.ContextFiles, c)
		}
	}
	return facts
}

func workspaceRoots(cwd string) (string, string) {
	git := gitRoot(cwd)
	if git != "" {
		return git, git
	}
	return "", markerRoot(cwd)
}

// Structured project facts for a directory, nil outside a workspace
func projectFactsFor(cwd string) map[string]any {
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil
		}
		cwd = wd
	}
	_, root := workspaceRoots(expandUser(cwd))
	if root == "" {
		return nil
	}
	f := DetectProjectFacts(root)
	return map[string]any{
		"root":            root,
		"manifests":       orEmpty(f.Manifests),
		"packageManagers": orEmpty(f.PackageManagers),
		"verifyCommands":  orEmpty(f.VerifyCommands),
		"contextFiles":    orEmpty(f.ContextFiles),
	}
}

// ProjectFactsFor exposes the structured project facts of a directory
func ProjectFactsFor(cwd string) map[string]any { return projectFactsFor(cwd) }

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// WorkspaceBlockHeader opens the workspace snapshot in the system prompt
const WorkspaceBlockHeader = "Workspace (snapshot at session start — re-check with `git` before acting on it):"

// BuildCodingWorkspaceBlock renders the workspace snapshot, empty outside a workspace
func BuildCodingWorkspaceBlock(cwd string) string {
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		cwd = wd
	}
	git, root := workspaceRoots(expandUser(cwd))
	if root == "" {
		return ""
	}
	lines := []string{WorkspaceBlockHeader, "- Root: " + root}
	if git != "" {
		statusEnv := noninteractiveRepoGitEnv(root, nil)
		status := ""
		if statusEnv != nil {
			status = boundedGitProbe([]string{"git", "-C", root, "status", "--porcelain=2", "--branch"}, gitProbeTimeout, statusEnv)
		}
		branch, counts := parseGitStatus(status)
		head := branch["head"]
		if head == "(detached)" {
			lines = append(lines, "- Branch: (detached HEAD)")
		} else if head != "" {
			ahead, behind := branch["ahead"], branch["behind"]
			if ahead == "" {
				ahead = "0"
			}
			if behind == "" {
				behind = "0"
			}
			upstream := ""
			if branch["upstream"] != "" {
				upstream = " → " + branch["upstream"]
			}
			ab := ""
			if upstream != "" && (ahead != "0" || behind != "0") {
				ab = " (ahead " + ahead + ", behind " + behind + ")"
			}
			lines = append(lines, "- Branch: "+head+upstream+ab)
		}
		gitDir, commonDir := gitIn(root, "rev-parse", "--git-dir"), gitIn(root, "rev-parse", "--git-common-dir")
		if gitDir != "" && commonDir != "" && resolveAbs(filepath.Join(root, gitDir)) != resolveAbs(filepath.Join(root, commonDir)) {
			onBranch := ""
			if head != "" && head != "(detached)" {
				onBranch = " on branch " + head
			}
			lines = append(lines, "- Worktree: linked, isolated"+onBranch+" (git state shared with primary tree). Changes here do not affect the main working tree or other agents; commit and push your changes, and open a PR if appropriate.")
		}
		var dirty []string
		for _, label := range []string{"staged", "modified", "untracked", "conflicts"} {
			if n := counts[label]; n > 0 {
				dirty = append(dirty, strings.TrimSpace(strings.Join([]string{itoa(n), label}, " ")))
			}
		}
		if len(dirty) > 0 {
			lines = append(lines, "- Status: "+strings.Join(dirty, ", "))
		} else {
			lines = append(lines, "- Status: clean")
		}
		if recent := gitIn(root, "log", "-3", "--pretty=%h %s"); recent != "" {
			lines = append(lines, "- Recent commits:")
			for _, c := range strings.Split(recent, "\n") {
				lines = append(lines, "    "+c)
			}
		}
	}
	f := DetectProjectFacts(root)
	if len(f.Manifests) > 0 {
		managers := ""
		if len(f.PackageManagers) > 0 {
			managers = " (" + strings.Join(f.PackageManagers, "/") + ")"
		}
		shown := f.Manifests
		if len(shown) > 6 {
			shown = shown[:6]
		}
		lines = append(lines, "- Project: "+strings.Join(shown, ", ")+managers)
	}
	if len(f.VerifyCommands) > 0 {
		lines = append(lines, "- Verify: "+strings.Join(f.VerifyCommands, "; "))
	}
	if len(f.ContextFiles) > 0 {
		lines = append(lines, "- Context files: "+strings.Join(f.ContextFiles, ", "))
	}
	return strings.Join(lines, "\n")
}
