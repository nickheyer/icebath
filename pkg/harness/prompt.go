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
)

// What the agent is told about the machine and session it runs in
type Facts struct {
	Dir      string
	Root     string
	Git      bool
	Platform string
	Shell    string
	Date     time.Time
	Model    string
	Mission  bool
}

const conduct = `You are icebath, a coding agent in the user's terminal, acting through the tools offered. The user's own instructions arrive in the conversation.

# Harness
- Text you write outside a tool call is shown to the user as markdown in a terminal. Tool results arrive as tool messages, never as user messages. After a tool result, continue the task. The user speaks only through user messages.
- Every tool call passes a permission gate chosen by the user. A denied call means the user declined it. Adjust the approach, never retry the same call.
- Prefer the dedicated file tools. Read shows a file, Edit changes part of one, Write replaces one, Glob finds files, Grep searches contents. Use Bash for commands, builds, tests and git.
- Independent tool calls may be made together in one turn. Dependent calls wait for the earlier result.
- Bash keeps its working directory between calls. A pipeline exits with the code of its last command. The exit code of every stage is listed whenever an earlier stage failed, so read that line before trusting a piped test run.
- Reference code as path:line so the user can open it.
- Record keeps a conclusion a person wants after the session, such as a bug found, a risk, a decision or an open question. Routine command results and progress belong in prose.
- Plan is offered only while a mission is attached. Call it before work that takes more than a few tool calls and whenever a step changes state.
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

// Facts about the machine and session the client adds after the base
func Environment(f Facts) string {
	var sb strings.Builder
	sb.WriteString("# Environment\n")
	fmt.Fprintf(&sb, "- Working directory: %s\n", f.Dir)
	if f.Root != "" && f.Root != f.Dir {
		fmt.Fprintf(&sb, "- File tools stay inside: %s\n", f.Root)
	}
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
		sb.WriteString("- A mission is attached, so " + mission.ToolName + " is available\n")
	} else {
		sb.WriteString("- No mission is attached, so " + mission.ToolName + " is not offered\n")
	}
	sb.WriteString("- " + facts.ToolName + " is available for findings worth keeping\n")
	return strings.TrimRight(sb.String(), "\n")
}

// System prompt of a base and an environment block, an empty one left out
func Compose(base, env string) string {
	var parts []string
	for _, part := range []string{strings.TrimSpace(base), env} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Base prompt every session starts from, the conduct and the configured instructions
func (a *App) DefaultBase() string { return Base(a.System) }

// Environment block the client adds for a run on the model as the machine stands now
func (a *App) EnvironmentFor(ctx context.Context, model string) string {
	return Environment(GatherFacts(a.Shell.Dir(), a.Root, model, mission.IDFrom(ctx) != ""))
}

// Prompt of a run on the endpoint, the profile base or the built in one plus the environment unless left out
func (a *App) PromptFor(ctx context.Context, ep Endpoint) string {
	base, env := a.DefaultBase(), ""
	if ep.Profile.System.Kind != "" {
		base = ep.System
	}
	if !ep.Profile.NoContext {
		env = a.EnvironmentFor(ctx, ep.Model)
	}
	return Compose(base, env)
}

// Facts about the machine as it stands, the working directory and root given by the caller
func GatherFacts(dir, root, model string, missionAttached bool) Facts {
	return Facts{
		Dir:      dir,
		Root:     root,
		Git:      inGitRepo(dir),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Shell:    filepath.Base(os.Getenv("SHELL")),
		Date:     time.Now(),
		Model:    model,
		Mission:  missionAttached,
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
