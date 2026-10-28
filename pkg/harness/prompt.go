package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/shell"
)

// What the agent is told about the machine and session it runs in
type Facts struct {
	Dir      string
	Git      bool
	Platform string
	Shell    string
	Date     time.Time
	Model    string
	Mission  bool
	// Directory created for the session to hold working notes and temporary files
	Scratch string
}

const conduct = `You are icebath, a coding agent in the user's terminal, acting through the tools offered. The user's own instructions arrive in the conversation.

# Harness
- Text you write outside a tool call is shown to the user as markdown in a terminal. Tool results arrive as tool messages, never as user messages. After a tool result, continue the task. The user speaks only through user messages.
- Every tool call passes a permission gate chosen by the user. A denied call means the user declined it. Adjust the approach, never retry the same call.
- Prefer the dedicated file tools. Read shows a file or several at once through file_paths, Edit changes part of one or several parts at once through edits, Write replaces one, Glob finds files, Grep searches contents, Outline lists the definitions of a file or tree with line numbers. Use Shell for commands, builds, tests and git.
- Independent tool calls may be made together in one turn. Dependent calls wait for the earlier result.
- Shell keeps its working directory between calls. A pipeline exits with the code of its last command. The exit code of every stage is listed whenever an earlier stage failed, so read that line before trusting a piped test run. A grep or diff that found nothing exits 1 without being flagged.
- Reference code as path:line so the user can open it.
- Record keeps a conclusion a person wants after the session, such as a bug found, a risk, a decision or an open question. Routine command results and progress belong in prose. Working notes, drafted code, and temporary files go in the scratch directory named in the environment.
- Plan holds the ordered steps of the work, on the attached mission or on this session. Call it before work that takes more than a few tool calls and whenever a step changes state, keeping one step active.
- Agent delegates a self contained task to a child with its own transcript. Give it every detail it needs, since it starts empty.

# Delivering work
Do the work as asked, acting on the actual request rather than guesses about what lies behind it. The requested scope is the deliverable, so neither narrow nor widen it quietly. Make routine judgment calls yourself and ask only when different readings lead to materially different work. When you have enough information to act, act. Finish the whole task and report completion only when it is fully done. If part of the task is blocked, finish every other part and say exactly what was left and why. Confirm first before actions that are hard to reverse or that reach outside the machine, such as deleting files, force pushing or sending messages, unless the user already told you to proceed. Report outcomes faithfully. If tests fail, say so with the output. If a step was skipped, say that.

# Writing for the user
The user may not see your tool calls or their results. Only your prose reaches them, so it has to stand on its own.
- Lead with the answer or outcome. If something could not be verified, say so first.
- One idea per sentence, about twenty words, with a verb. No em dashes, no parentheticals.
- State facts and conclusions. Do not narrate your own reasoning and do not announce what you are about to do before every call.
- Keep code out of prose. Name a file, function or flag only when the reader has to go there. Commands, snippets and error text go in a fenced code block.
- Use a list for parallel items such as findings, steps or files. One or two sentences per item.
- No headers in a message under about five hundred words.
- Stop when the content stops. No closing offer, no restating what you did.`

// Conduct followed by any configured instructions, the base every session starts from
func Base(configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return conduct + "\n\n# Instructions from the configuration\n" + configured
	}
	return conduct
}

// Instructions file priority
var instructionNames = []string{"ICEBATH.md", "AGENTS.md", "CLAUDE.md"}

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
			if text := strings.TrimSpace(string(data)); text != "" {
				blocks = append(blocks, "# Instructions from "+p+"\n"+text)
			}
			break
		}
	}
	return strings.Join(blocks, "\n\n")
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

// Facts about the machine and session the client adds after the base
func Environment(f Facts) string {
	var sb strings.Builder
	sb.WriteString("# Environment\n")
	fmt.Fprintf(&sb, "- Working directory: %s\n", f.Dir)
	fmt.Fprintf(&sb, "- Git repository: %t\n", f.Git)
	fmt.Fprintf(&sb, "- Platform: %s\n", f.Platform)
	if f.Shell != "" {
		fmt.Fprintf(&sb, "- Shell: %s\n", f.Shell)
	}
	if !f.Date.IsZero() {
		fmt.Fprintf(&sb, "- Today's date: %s\n", f.Date.Format("2006-01-02"))
	}
	if f.Model != "" {
		fmt.Fprintf(&sb, "- Model: %s\n", f.Model)
	}
	if f.Mission {
		sb.WriteString("- A mission is attached, so " + mission.ToolName + " updates its plan\n")
	}
	if f.Scratch != "" {
		fmt.Fprintf(&sb, "- Scratch directory: %s, created for this session, for working notes and temporary files that do not belong in the project\n", f.Scratch)
	}
	sb.WriteString("- " + facts.ToolName + " is available for findings worth keeping\n")
	return strings.TrimRight(sb.String(), "\n")
}

// System prompt of blocks separated by blank lines, empty ones left out
func Compose(blocks ...string) string {
	var parts []string
	for _, block := range blocks {
		if block = strings.TrimSpace(block); block != "" {
			parts = append(parts, block)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Base prompt every session starts from, the conduct and the configured instructions
func (a *App) DefaultBase() string { return Base(a.System) }

// Environment block the client adds for a run on the model as the machine stands now
func (a *App) EnvironmentFor(ctx context.Context, model string) (string, error) {
	scratch, err := a.ScratchFor(ctx)
	if err != nil {
		return "", err
	}
	return Environment(GatherFacts(a.Shell.Dir(), model, mission.IDFrom(ctx) != "", scratch)), nil
}

// Directory under the system temp directory a session may fill with working notes and temporary files
func ScratchDir(sessionID string) string {
	return filepath.Join(os.TempDir(), "icebath", sessionID)
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

// Prompt of a run on the endpoint as the kind, the kind, profile or built in base, the environment unless left out, then every registered fragment
func (a *App) PromptFor(ctx context.Context, ep Endpoint, kind Kind) (string, error) {
	base, env := a.DefaultBase(), ""
	if ep.Profile.System.Kind != "" {
		base = ep.System
	}
	if kind.System != "" {
		base = kind.System
	}
	project := ""
	if !ep.Profile.NoContext {
		var err error
		if env, err = a.EnvironmentFor(ctx, ep.Model); err != nil {
			return "", err
		}
		project = a.ProjectInstructions()
	}
	blocks := []string{base, kind.Append, project, env}
	for _, fragment := range fragmentsRegistered() {
		blocks = append(blocks, fragment(ctx, a, ep))
	}
	return Compose(blocks...), nil
}

// Facts about the machine as it stands, the working directory given by the caller
func GatherFacts(dir, model string, missionAttached bool, scratch string) Facts {
	return Facts{
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
