package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/prompt"
	"github.com/nickheyer/icebath/pkg/shell"
)

// Instructions file priority
var instructionNames = []string{"ICEBATH.md", "AGENTS.md", "CLAUDE.md"}

// Base prompt of a session offered the tools, the conduct of a coding agent and then the configured instructions
func Base(configured string, tools []string) string {
	system := prompt.System{
		Identity: []string{"You are icebath, a coding agent in the user's terminal, acting through the tools offered.", "The user's own instructions arrive in the conversation."},
		Harness:  &prompt.Harness{Reader: "the user", Speaker: "the user", Gatekeeper: "the user", Tools: tools},
		Delivery: &prompt.Delivery{Asked: "the user", Confirmer: "the user"},
		Writing:  &prompt.Writing{Reader: "the user"},
	}
	if configured = strings.TrimSpace(configured); configured != "" {
		system.Sections = []prompt.Section{{Title: "Instructions from the configuration", Text: []string{configured}}}
	}
	return system.String()
}

// Instructions block naming its source, empty without text
func instructions(source, text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return prompt.New().Heading(1, "Instructions from "+source).Raw(text).String()
}

// Instructions of the project, one block per file found from the root down to the working directory
func (a *App) ProjectInstructions() string {
	root := a.Root
	if root == "" {
		root = a.Shell.Dir()
	}
	var blocks []string
	for _, dir := range lineage(root, a.Shell.Dir()) {
		for _, name := range instructionNames {
			p := filepath.Join(dir, name)
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if text := instructions(p, string(data)); text != "" {
				blocks = append(blocks, text)
			}
			break
		}
	}
	return prompt.Compose(blocks...)
}

// Directories from root down to dir inclusive, root alone when dir is not under it
func lineage(root, dir string) []string {
	root, dir = filepath.Clean(root), filepath.Clean(dir)
	out := []string{root}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return out
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		out = append(out, cur)
	}
	return out
}

// Environment block of a run offered the tools, the machine as it stands now
func (a *App) EnvironmentFor(ctx context.Context, model string, offered []string) (string, error) {
	scratch, err := a.ScratchFor(ctx)
	if err != nil {
		return "", err
	}
	f := GatherFacts(a.Shell.Dir(), model, mission.IDFrom(ctx) != "", scratch)
	if sess := a.SessionFor(ctx); sess != nil && !f.Mission {
		if plan, ok := mission.CurrentSessionPlan(sess); ok {
			f.Plan = plan.Format()
		}
	}
	if slices.Contains(offered, mission.ToolName) {
		f.PlanTool = mission.ToolName
	}
	if slices.Contains(offered, facts.ToolName) {
		f.RecordTool = facts.ToolName
	}
	return prompt.Environment(f), nil
}

// Directory under the system temp directory a session may fill with working notes and temporary files
func ScratchDir(sessionID string) string {
	return filepath.Join(os.TempDir(), "icebath-scratch", sessionID)
}

// Scratch directory of the calling run's session, created when missing, empty with no session
func (a *App) ScratchFor(ctx context.Context) (string, error) {
	sess := a.SessionFor(ctx)
	if sess == nil {
		return "", nil
	}
	dir := ScratchDir(sess.ID())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("scratch directory: %w", err)
	}
	return dir, nil
}

// Prompt of a run as the kind, its base, the project and environment blocks, then every registered fragment
func (a *App) PromptFor(ctx context.Context, ep Endpoint, kind Kind) (string, error) {
	offered := a.Offered(kind)
	base := Base(a.System, offered)
	if ep.Profile.System.Kind != "" {
		base = ep.System
	}
	if kind.System != "" {
		base = kind.System
	}
	project, env := "", ""
	if !ep.Profile.NoContext {
		var err error
		if env, err = a.EnvironmentFor(ctx, ep.Model, offered); err != nil {
			return "", err
		}
		project = a.ProjectInstructions()
	}
	blocks := []string{base, kind.Append, project, env}
	for _, fragment := range fragmentsRegistered() {
		blocks = append(blocks, fragment(ctx, a, ep))
	}
	return prompt.Compose(blocks...), nil
}

// Built in tools the kind offers and the registry has on, in guide order
func (a *App) Offered(kind Kind) []string {
	var out []string
	for _, name := range prompt.Builtins() {
		if kind.Offers(name) && a.Tools.Enabled(name) {
			out = append(out, name)
		}
	}
	return out
}

// Facts about the machine as it stands, the working directory given by the caller
func GatherFacts(dir, model string, missionAttached bool, scratch string) prompt.Facts {
	return prompt.Facts{
		Dir:      dir,
		Git:      inGitRepo(dir),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Shell:    shell.Dialect,
		Date:     time.Now(),
		Model:    model,
		Mission:  missionAttached,
		Scratch:  scratch,
	}
}

// Reports whether a .git entry exists in the directory or any parent
func inGitRepo(dir string) bool {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
