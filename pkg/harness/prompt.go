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
	"github.com/nickheyer/icebath/pkg/plugin"
	"github.com/nickheyer/icebath/pkg/prompt"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/shell"
)

// Base prompt of a session offered the tools, the conduct of a coding agent and then the configured instructions, an
// output style standing in for the conduct sections unless it keeps them
func Base(configured string, tools []string, style *plugin.Style) string {
	system := prompt.System{
		Identity: []string{"You are icebath, a coding agent in the user's terminal, acting through the tools offered.", "The user's own instructions arrive in the conversation."},
		Harness:  &prompt.Harness{Reader: "the user", Speaker: "the user", Gatekeeper: "the user", Tools: tools},
		Delivery: &prompt.Delivery{Asked: "the user", Confirmer: "the user"},
		Writing:  &prompt.Writing{Reader: "the user"},
	}
	if style != nil && !style.KeepCoding {
		system.Delivery, system.Writing = nil, nil
	}
	if style != nil {
		system.Sections = append(system.Sections, prompt.Section{Title: "Output style " + style.Name, Text: []string{style.Body}})
	}
	if configured = strings.TrimSpace(configured); configured != "" {
		system.Sections = append(system.Sections, prompt.Section{Title: "Instructions from the configuration", Text: []string{configured}})
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

// Instructions that always apply, one block per file, managed and user ones then the project walk, imports resolved
func (a *App) ProjectInstructions() string {
	var blocks []string
	for _, in := range plugin.Always(a.Instructions()) {
		if text := instructions(in.Path, in.Text); text != "" {
			blocks = append(blocks, text)
		}
	}
	return prompt.Compose(blocks...)
}

// Instructions for a session, hooks hearing of each file the first time the session takes it in
func (a *App) instructionsFor(ctx context.Context, sess *session.Session) string {
	var blocks []string
	for _, in := range plugin.Always(a.Instructions()) {
		text := instructions(in.Path, in.Text)
		if text == "" {
			continue
		}
		blocks = append(blocks, text)
		if !a.markLoaded(sess, "instructions:"+in.Path) {
			a.instructionsLoaded(ctx, sess, in, "")
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

// Prompt of a run as the kind, its base or style, the kind text, instructions, memory, preloaded skills, the skill
// listing and the environment, then every registered fragment
func (a *App) PromptFor(ctx context.Context, ep Endpoint, kind Kind) (string, error) {
	offered := a.Offered(kind)
	style, err := a.styleFor()
	if err != nil {
		return "", err
	}
	base := Base(a.System, offered, style)
	if ep.Profile.System.Kind != "" {
		base = ep.System
	}
	if kind.System != "" {
		base = kind.System
	}
	sess := a.SessionFor(ctx)
	project, env := "", ""
	if !ep.Profile.NoContext {
		if env, err = a.EnvironmentFor(ctx, ep.Model, offered); err != nil {
			return "", err
		}
		if !kind.NoInstructions {
			project = a.instructionsFor(ctx, sess)
		}
	}
	memory, err := a.memoryBlock(kind)
	if err != nil {
		return "", err
	}
	preloaded := a.preloadedSkills(ctx, sess, kind)
	blocks := []string{base, kind.Append, project, memory, preloaded, a.skillListing(ep, offered), env}
	for _, fragment := range fragmentsRegistered() {
		blocks = append(blocks, fragment(ctx, a, ep))
	}
	return prompt.Compose(blocks...), nil
}

// Block listing the skills the model may load, empty without the skill tool or any skill
func (a *App) skillListing(ep Endpoint, offered []string) string {
	lib := a.Library()
	if lib == nil || !slices.Contains(offered, SkillTool) {
		return ""
	}
	listing := lib.Listing(ep.Catalog.Context, a.Config.Plugins.ListingShare, a.skillUsage())
	if listing == "" {
		return ""
	}
	return prompt.New().Heading(1, "Skills").Para("Skills are instructions loaded on demand with the " + SkillTool + " tool, each named with what it covers. When the task at hand is one a skill covers, load it before working your own way. A skill the person invoked arrives in the conversation on its own.").Raw(listing).String()
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
