package prompt

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
)

const (
	// Characters of a context file kept when the model's window is not known, and the least kept when it is
	contextFileChars = 20000
	// Most characters kept whatever the window
	contextFileCeiling = 500000
	// Share of the model's window one context file may take, at four characters a token
	contextFileShare  = 0.06
	charsPerToken     = 4
	truncateHeadShare = 0.7
	truncateTailShare = 0.2
	// Headings of a cut middle named in its marker, the rest summed up as "..."
	omittedHeadingsShown = 15
)

// Text of the file as UTF-8 with its line ends made \n, and false when it is missing, unreadable, or slower to read than the context file
// wait; a slow file is told to the user so a stuck mount cannot stall the prompt
func read(ctx context.Context, env *prompt.Env, path string) (string, bool) {
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	type outcome struct {
		text string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		data, err := os.ReadFile(path)
		if err == nil && !utf8.Valid(data) {
			err = fmt.Errorf("%s is not valid UTF-8", path)
		}
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
		done <- outcome{strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(string(data)), err}
	}()
	wait := time.Duration(env.Config.GetInt(agentconfig.ContextFileReadTimeout)) * time.Second
	if wait <= 0 {
		wait = 5 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.text, result.err == nil
	case <-timer.C:
		env.Notify(fmt.Sprintf("Context file %s took longer than %s to read, so it was left out.", path, wait))
	case <-ctx.Done():
	}
	return "", false
}

// Characters a context file keeps: the context file limit when it is set, else a share of the model's window within the floor and ceiling
func limit(env *prompt.Env) int {
	if set := env.Config.GetInt(agentconfig.ContextFileMaxChars); set > 0 {
		return set
	}
	if env.ContextLength <= 0 {
		return contextFileChars
	}
	return max(contextFileChars, min(int(float64(env.ContextLength*charsPerToken)*contextFileShare), contextFileCeiling))
}

// The content within the limit: its head and tail around a marker naming the headings of the middle left out and the file to read for
// all of it. A cut is told to the user
func truncate(env *prompt.Env, content, name, path string) string {
	most := limit(env)
	runes := []rune(content)
	if len(runes) <= most {
		return content
	}
	env.Notify(fmt.Sprintf("Context file %s was cut: %d characters is over the limit of %d. Trim the file, raise the context file limit "+
		"in /settings (Agent, Prompt), or use a model with a larger context.", name, len(runes), most))
	head, tail := int(float64(most)*truncateHeadShare), int(float64(most)*truncateTailShare)
	sections := ""
	if omitted := omittedHeadings(runes, head, len(runes)-tail); len(omitted) > 0 {
		sections = " Omitted sections: " + strings.Join(omitted, "; ") + "."
	}
	return string(runes[:head]) + fmt.Sprintf("\n\n[...truncated %s: kept %d+%d of %d chars. The middle is omitted.%s If you need the full "+
		"instructions, read the complete file with the read_file tool: %s]\n\n", name, head, tail, len(runes), sections, path) + string(runes[len(runes)-tail:])
}

var heading = regexp.MustCompile(`^#{1,6} \S`)

// Markdown headings that start between the two character offsets, outside fenced code
func omittedHeadings(runes []rune, start, end int) []string {
	var headings []string
	offset, fenced := 0, false
	for line := range strings.Lines(string(runes)) {
		stripped := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(stripped, "```") || strings.HasPrefix(stripped, "~~~"):
			fenced = !fenced
		case !fenced && start <= offset && offset < end && heading.MatchString(stripped):
			headings = append(headings, strings.TrimSpace(strings.TrimLeft(stripped, "#")))
		}
		offset += utf8.RuneCountInString(line)
	}
	if len(headings) > omittedHeadingsShown {
		return append(headings[:omittedHeadingsShown:omittedHeadingsShown], "...")
	}
	return headings
}

// The content, or in its place a notice that it was not loaded when it looks like an injection. A blocked file is told to the user
func scan(env *prompt.Env, content, name string) string {
	content = strings.TrimPrefix(content, "\ufeff")
	found := prompt.Threats(content, prompt.Context)
	if len(found) == 0 {
		return content
	}
	joined := strings.Join(found, ", ")
	env.Notify("Context file " + name + " was not loaded: it matched injection pattern(s) " + joined + ".")
	return fmt.Sprintf("[BLOCKED: %s contained potential prompt injection (%s). Content not loaded.]", name, joined)
}

// A context file under its label, scanned and cut to the limit
func labelled(env *prompt.Env, content, label, name, path string) string {
	return truncate(env, "## "+label+"\n\n"+scan(env, content, label), name, path)
}

// Project instructions of the working directory: the first kind found of .icebath.md, AGENTS.md, CLAUDE.md and the cursor rules
func contextFiles(ctx context.Context, env *prompt.Env) string {
	if env.IgnoreRules {
		return ""
	}
	return ContextFiles(ctx, env)
}

// Project instructions of the env's folder whatever it says of rules, as a subagent's prompt carries those of the workspace it works in
func ContextFiles(ctx context.Context, env *prompt.Env) string {
	if env.Cwd == "" {
		return ""
	}
	cwd := resolve(env.Cwd)
	for _, load := range []func(context.Context, *prompt.Env, string) string{icebathMD, agentsMD, claudeMD, cursorRules} {
		if section := load(ctx, env, cwd); section != "" {
			return "# Project Context\n\nThe following project context files have been loaded and should be followed:\n\n" + section
		}
	}
	return ""
}

// Absolute path with its links resolved as far as they exist
func resolve(path string) string {
	path, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(resolve(parent), filepath.Base(path))
}

// The directory and each one above it up to the root
func ancestors(path string) []string {
	chain := []string{path}
	for parent := filepath.Dir(path); parent != path; parent = filepath.Dir(path) {
		chain = append(chain, parent)
		path = parent
	}
	return chain
}

// Nearest directory at or above start that holds .git, empty outside a repository
func gitRoot(start string) string {
	return tools.Worktree(resolve(start))
}

// The nearest .icebath.md or ICEBATH.md from the directory up to its repository's root, the directory alone outside one, its front
// matter left out
func icebathMD(ctx context.Context, env *prompt.Env, cwd string) string {
	root := gitRoot(cwd)
	dirs := []string{cwd}
	if root != "" {
		dirs = ancestors(cwd)
	}
	for _, dir := range dirs {
		for _, name := range []string{".icebath.md", "ICEBATH.md"} {
			path := filepath.Join(dir, name)
			if !fsx.IsFile(path) {
				continue
			}
			label, err := filepath.Rel(cwd, path)
			if err != nil || !filepath.IsLocal(label) {
				label = name
			}
			text, _ := read(ctx, env, path)
			if text = strings.TrimSpace(text); text == "" {
				return ""
			}
			return labelled(env, frontmatterless(text), label, ".icebath.md", path)
		}
		if dir == root {
			break
		}
	}
	return ""
}

// Markdown without a leading YAML front matter block, unless that block is all there is
func frontmatterless(content string) string {
	content = strings.TrimLeft(content, "\ufeff")
	if !strings.HasPrefix(content, "---") {
		return content
	}
	end := strings.Index(content[3:], "\n---")
	if end < 0 {
		return content
	}
	if body := strings.TrimLeft(content[3+end+4:], "\n"); body != "" {
		return body
	}
	return content
}

// AGENTS.override.md, AGENTS.md or agents.md of each directory from the repository's root down to the working directory, the first one
// with text winning in each; the same text twice is kept once, and all of them together are cut to the limit again
func agentsMD(ctx context.Context, env *prompt.Env, cwd string) string {
	chain := []string{cwd}
	if root := gitRoot(cwd); root != "" && root != cwd {
		if rel, err := filepath.Rel(root, cwd); err == nil && filepath.IsLocal(rel) {
			chain = []string{root}
			for _, part := range strings.Split(rel, string(filepath.Separator)) {
				chain = append(chain, filepath.Join(chain[len(chain)-1], part))
			}
		}
	}
	var sections []string
	seen := map[string]bool{}
	for _, dir := range chain {
		for _, name := range []string{"AGENTS.override.md", "AGENTS.md", "agents.md"} {
			path := filepath.Join(dir, name)
			if !fsx.Exists(path) {
				continue
			}
			text, _ := read(ctx, env, path)
			if text = strings.TrimSpace(text); text == "" {
				continue
			}
			label := name
			if dir != cwd {
				if rel, err := filepath.Rel(cwd, path); err == nil {
					label = rel
				}
			}
			if !seen[text] {
				seen[text] = true
				sections = append(sections, labelled(env, text, label, label, path))
			}
			break
		}
	}
	switch len(sections) {
	case 0:
		return ""
	case 1:
		return sections[0]
	}
	return truncate(env, strings.Join(sections, "\n\n"), "AGENTS.md (directory chain)", filepath.Join(cwd, "AGENTS.md"))
}

// CLAUDE.md or claude.md of the working directory, the first one with text
func claudeMD(ctx context.Context, env *prompt.Env, cwd string) string {
	for _, name := range []string{"CLAUDE.md", "claude.md"} {
		path := filepath.Join(cwd, name)
		if !fsx.Exists(path) {
			continue
		}
		if text, _ := read(ctx, env, path); strings.TrimSpace(text) != "" {
			return labelled(env, strings.TrimSpace(text), name, "CLAUDE.md", path)
		}
	}
	return ""
}

// .cursorrules and the .mdc files of .cursor/rules of the working directory, one after the other, cut to the limit together
func cursorRules(ctx context.Context, env *prompt.Env, cwd string) string {
	type rule struct{ label, path string }
	rules := []rule{{".cursorrules", filepath.Join(cwd, ".cursorrules")}}
	matches, _ := filepath.Glob(filepath.Join(cwd, ".cursor", "rules", "*.mdc"))
	for _, match := range matches {
		rules = append(rules, rule{".cursor/rules/" + filepath.Base(match), match})
	}
	var out strings.Builder
	for _, rule := range rules {
		if !fsx.Exists(rule.path) {
			continue
		}
		if text, _ := read(ctx, env, rule.path); strings.TrimSpace(text) != "" {
			out.WriteString("## " + rule.label + "\n\n" + scan(env, strings.TrimSpace(text), rule.label) + "\n\n")
		}
	}
	if out.Len() == 0 {
		return ""
	}
	return truncate(env, out.String(), ".cursorrules", filepath.Join(cwd, ".cursorrules"))
}
