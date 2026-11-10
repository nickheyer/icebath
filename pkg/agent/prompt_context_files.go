package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Profile home of the env, the process home when the env names none
func agentHome(env *tools.Env) string {
	if env != nil && strings.TrimSpace(env.Home) != "" {
		return env.Home
	}
	return home.Process()
}

// Whether path exists, a stat permission or IO failure counting as absent
func existsOrDenied(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Whether path is a regular file, a failed stat counting as no
func isFileOrDenied(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Whether path is a directory, a failed stat counting as no
func isDirOrDenied(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Absolute path with symlinks resolved as far as they exist, like Path.resolve
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	dir, base := filepath.Split(abs)
	dir = filepath.Clean(dir)
	if dir == abs {
		return abs
	}
	return filepath.Join(resolvePath(dir), base)
}

// Path and every ancestor up to the filesystem root
func pathChain(p string) []string {
	chain := []string{p}
	for {
		parent := filepath.Dir(p)
		if parent == p {
			return chain
		}
		chain = append(chain, parent)
		p = parent
	}
}

// Path of target below base, false when target is not inside base
func relativeInside(base, target string) (string, bool) {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// Nearest directory at or above start that contains .git, empty when none
func FindGitRoot(start string) string {
	for _, p := range pathChain(resolvePath(start)) {
		if existsOrDenied(filepath.Join(p, ".git")) {
			return p
		}
	}
	return ""
}

// Nearest .icebath.md or ICEBATH.md from cwd up to the git root, cwd only outside a repository
func FindIcebathMD(cwd string) string {
	stopAt := FindGitRoot(cwd)
	current := resolvePath(cwd)
	dirs := []string{current}
	if stopAt != "" {
		dirs = pathChain(current)
	}
	for _, dir := range dirs {
		found := ""
		for _, name := range []string{".icebath.md", "ICEBATH.md"} {
			if isFileOrDenied(filepath.Join(dir, name)) {
				found = filepath.Join(dir, name)
				break
			}
		}
		if found != "" || dir == stopAt {
			return found
		}
	}
	return ""
}

// Markdown body with an optional leading YAML frontmatter block removed
func StripYAMLFrontmatter(content string) string {
	content = strings.TrimLeft(content, "\ufeff")
	if !strings.HasPrefix(content, "---") {
		return content
	}
	idx := strings.Index(content[3:], "\n---")
	if idx == -1 {
		return content
	}
	if body := strings.TrimLeft(content[3+idx+4:], "\n"); body != "" {
		return body
	}
	return content
}

// Stripped text of path, empty when missing, timed out or unreadable
func readContextFile(env *tools.Env, path string) string {
	if !existsOrDenied(path) {
		return ""
	}
	text, _, err := tools.ReadTextWithTimeout(env, path, nil)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read %s: %v", path, err))
		return ""
	}
	return pystr.Strip(text)
}

// One discovered context file, an empty content meaning empty or unreadable
type ContextFile struct {
	Kind    string
	Label   string
	Path    string
	Content string
}

// Threat scanned content rendered as a labelled section and capped to the budget
func contextSection(ctx context.Context, env *tools.Env, content, label, warnName, path string, contextLength *int) string {
	body := "## " + label + "\n\n" + tools.ScanContextContent(content, label, false)
	return tools.TruncateContent(ctx, env, body, warnName, tools.TruncateOptions{ContextLength: contextLength, ReadPath: path})
}

// Nearest .icebath.md or ICEBATH.md as a candidate labelled relative to cwd
func icebathMDCandidates(env *tools.Env, cwd string) []ContextFile {
	path := FindIcebathMD(cwd)
	if path == "" {
		return nil
	}
	label, ok := relativeInside(cwd, path)
	if !ok {
		label = filepath.Base(path)
	}
	return []ContextFile{{Kind: "icebath_md", Label: label, Path: path, Content: readContextFile(env, path)}}
}

// Directories from the git root down to cwd, cwd alone outside a repository
func agentsMDDirectoryChain(cwd string) []string {
	current := resolvePath(cwd)
	root := FindGitRoot(current)
	if root == "" || root == current {
		return []string{current}
	}
	rel, ok := relativeInside(root, current)
	if !ok {
		return []string{current}
	}
	chain := []string{root}
	dir := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		chain = append(chain, dir)
	}
	return chain
}

// AGENTS.override.md, AGENTS.md or agents.md per chain directory, the first non empty name winning
func agentsMDCandidates(env *tools.Env, cwd string) []ContextFile {
	resolved := resolvePath(cwd)
	found := []ContextFile{}
	for _, dir := range agentsMDDirectoryChain(resolved) {
		for _, name := range []string{"AGENTS.override.md", "AGENTS.md", "agents.md"} {
			candidate := filepath.Join(dir, name)
			if !existsOrDenied(candidate) {
				continue
			}
			content := readContextFile(env, candidate)
			label := name
			if dir != resolved {
				if rel, err := filepath.Rel(resolved, candidate); err == nil {
					label = rel
				}
			}
			found = append(found, ContextFile{Kind: "agents_md", Label: label, Path: candidate, Content: content})
			if content != "" {
				break
			}
		}
	}
	return found
}

// CLAUDE.md or claude.md in cwd only, the first non empty name winning
func claudeMDCandidates(env *tools.Env, cwd string) []ContextFile {
	found := []ContextFile{}
	for _, name := range []string{"CLAUDE.md", "claude.md"} {
		candidate := filepath.Join(cwd, name)
		if !existsOrDenied(candidate) {
			continue
		}
		content := readContextFile(env, candidate)
		found = append(found, ContextFile{Kind: "claude_md", Label: name, Path: candidate, Content: content})
		if content != "" {
			break
		}
	}
	return found
}

// .cursorrules and the sorted .cursor/rules mdc files of cwd that exist
func cursorrulesCandidates(env *tools.Env, cwd string) []ContextFile {
	type labelled struct{ label, path string }
	candidates := []labelled{{".cursorrules", filepath.Join(cwd, ".cursorrules")}}
	rulesDir := filepath.Join(cwd, ".cursor", "rules")
	if isDirOrDenied(rulesDir) {
		matches, _ := filepath.Glob(filepath.Join(rulesDir, "*.mdc"))
		for _, m := range matches {
			candidates = append(candidates, labelled{".cursor/rules/" + filepath.Base(m), m})
		}
	}
	found := []ContextFile{}
	for _, c := range candidates {
		if existsOrDenied(c.path) {
			found = append(found, ContextFile{Kind: "cursorrules", Label: c.label, Path: c.path, Content: readContextFile(env, c.path)})
		}
	}
	return found
}

// Every project context file candidate in load priority order
func DiscoverContextFiles(env *tools.Env, cwd string) []ContextFile {
	found := icebathMDCandidates(env, cwd)
	found = append(found, agentsMDCandidates(env, cwd)...)
	found = append(found, claudeMDCandidates(env, cwd)...)
	return append(found, cursorrulesCandidates(env, cwd)...)
}

// Whether an unconfigured cwd fell back to the install tree, whose contributor files must not load
func projectContextSuppressed(cwd *string, cwdPath string, allowInstallTreeFallback bool) bool {
	return cwd == nil && !allowInstallTreeFallback && tools.IsInstallTree(cwdPath)
}

// First non empty .icebath.md section with frontmatter removed
func loadIcebathMD(ctx context.Context, env *tools.Env, cwd string, contextLength *int) string {
	for _, c := range icebathMDCandidates(env, cwd) {
		if c.Content != "" {
			return contextSection(ctx, env, StripYAMLFrontmatter(c.Content), c.Label, ".icebath.md", c.Path, contextLength)
		}
	}
	return ""
}

// AGENTS.md chain merged root to cwd with identical copies dropped and the merge capped again
func loadAgentsMD(ctx context.Context, env *tools.Env, cwd string, contextLength *int) string {
	sections := []string{}
	seen := map[string]bool{}
	for _, c := range agentsMDCandidates(env, cwd) {
		if c.Content != "" && !seen[c.Content] {
			seen[c.Content] = true
			sections = append(sections, contextSection(ctx, env, c.Content, c.Label, c.Label, c.Path, contextLength))
		}
	}
	if len(sections) <= 1 {
		if len(sections) == 1 {
			return sections[0]
		}
		return ""
	}
	return tools.TruncateContent(ctx, env, strings.Join(sections, "\n\n"), "AGENTS.md (directory chain)",
		tools.TruncateOptions{ContextLength: contextLength, ReadPath: filepath.Join(resolvePath(cwd), "AGENTS.md")})
}

// First non empty CLAUDE.md section of cwd
func loadClaudeMD(ctx context.Context, env *tools.Env, cwd string, contextLength *int) string {
	for _, c := range claudeMDCandidates(env, cwd) {
		if c.Content != "" {
			return contextSection(ctx, env, c.Content, c.Label, "CLAUDE.md", c.Path, contextLength)
		}
	}
	return ""
}

// .cursorrules and .cursor/rules files of cwd concatenated and capped
func loadCursorrules(ctx context.Context, env *tools.Env, cwd string, contextLength *int) string {
	var b strings.Builder
	for _, c := range cursorrulesCandidates(env, cwd) {
		if c.Content != "" {
			b.WriteString("## " + c.Label + "\n\n" + tools.ScanContextContent(c.Content, c.Label, false) + "\n\n")
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return tools.TruncateContent(ctx, env, b.String(), ".cursorrules",
		tools.TruncateOptions{ContextLength: contextLength, ReadPath: filepath.Join(cwd, ".cursorrules")})
}

// SOUL.md of the profile home scanned and capped, false when absent, empty, unreadable or in a safe worker
func LoadSoulMD(ctx context.Context, env *tools.Env, contextLength *int, homeOverride string) (string, bool) {
	if config.SafeWorkerEnabled() {
		return "", false
	}
	if err := config.EnsureIcebathHome(tools.WithEnvHome(ctx, env), agentHome(env)); err != nil {
		slog.Debug(fmt.Sprintf("Could not ensure ICEBATH_HOME before loading SOUL.md: %v", err))
	}
	soulHome := homeOverride
	if soulHome == "" {
		soulHome = agentHome(env)
	}
	soulPath := filepath.Join(soulHome, "SOUL.md")
	if !existsOrDenied(soulPath) {
		return "", false
	}
	text, _, err := tools.ReadTextWithTimeout(env, soulPath, nil)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read SOUL.md from %s: %v", soulPath, err))
		return "", false
	}
	content := pystr.Strip(text)
	if content != "" {
		content = pystr.Strip(tools.StripLegacyBotProtocol(content))
	}
	if content == "" {
		return "", false
	}
	userAuthored := false
	manifest, err := provider.ReadManifest(filepath.Dir(soulPath))
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read distribution manifest next to %s: %v", soulPath, err))
	} else if manifest == nil {
		userAuthored = true
	} else if len(manifest.DistributionOwned) > 0 {
		userAuthored = true
		for _, owned := range manifest.DistributionOwned {
			if owned == "SOUL.md" {
				userAuthored = false
			}
		}
	}
	scanned := tools.ScanContextContent(content, "SOUL.md", userAuthored)
	return tools.TruncateContent(ctx, env, scanned, "SOUL.md", tools.TruncateOptions{ContextLength: contextLength, ReadPath: soulPath}), true
}

// Options of BuildContextFilesPrompt, a nil Cwd meaning the process working directory
type ContextFilesOptions struct {
	Cwd                      *string
	SkipSoul                 bool
	ContextLength            *int
	AllowInstallTreeFallback bool
	HomeOverride             string
}

// Project context block of the first context file kind found plus SOUL.md unless skipped
func BuildContextFilesPrompt(ctx context.Context, env *tools.Env, o ContextFilesOptions) (string, error) {
	if config.SafeWorkerEnabled() {
		return "", nil
	}
	raw := ""
	if o.Cwd != nil {
		raw = *o.Cwd
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		raw = wd
	}
	cwdPath := resolvePath(raw)
	sections := []string{}
	if projectContextSuppressed(o.Cwd, cwdPath, o.AllowInstallTreeFallback) {
		slog.Warn(fmt.Sprintf("skipping project-context discovery: working-directory resolution fell back to the Icebath "+
			"install tree (%s) — set terminal.cwd to your project directory", cwdPath))
	} else {
		section := loadIcebathMD(ctx, env, cwdPath, o.ContextLength)
		if section == "" {
			section = loadAgentsMD(ctx, env, cwdPath, o.ContextLength)
		}
		if section == "" {
			section = loadClaudeMD(ctx, env, cwdPath, o.ContextLength)
		}
		if section == "" {
			section = loadCursorrules(ctx, env, cwdPath, o.ContextLength)
		}
		sections = append(sections, section)
	}
	if !o.SkipSoul {
		soul, _ := LoadSoulMD(ctx, env, o.ContextLength, o.HomeOverride)
		sections = append(sections, soul)
	}
	kept := []string{}
	for _, s := range sections {
		if s != "" {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return "", nil
	}
	return "# Project Context\n\nThe following project context files have been loaded and should be followed:\n\n" +
		strings.Join(kept, "\n"), nil
}
