package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
)

const toolset = "terminal"

type registrant struct {
	host *terminal.Terminal
	// Approvals and command guards every tool shares
	approvals *approvals.Approvals
	// Skills of the session, whose locks the terminal honours
	skills *skills.Registry
	// Settings of the profile in use, which the schemas quote limits from
	config *config.Config
	// Spill folders already pruned of old files
	pruned sync.Map
	// Tools of every plugin, whose registrants keeping verification evidence hear of each command run in the foreground
	tools *tools.Registry
	// Hook points whose transform_terminal_output hooks may replace a command's output before the model reads it
	hooks *hooks.Hooks
}

func Register(registry *tools.Registry, cfg *config.Config, host *terminal.Terminal, checks *approvals.Approvals, catalog *skills.Registry,
	points *hooks.Hooks) {
	registry.Register(&registrant{host: host, config: cfg, approvals: checks, skills: catalog, tools: registry, hooks: points})
}

func (r *registrant) Tools() []tools.Def {
	return []tools.Def{
		{
			Name:           "terminal",
			Toolset:        toolset,
			Title:          "Shell",
			Icon:           tools.IconShell,
			Subject:        "command",
			Risks:          approvals.Kinds(),
			Schema:         r.terminalSchema(),
			Handler:        r.terminal,
			Check:          func(env *tools.Env) bool { return r.host.Check(env.Config) == nil },
			MaxResultChars: 100_000,
			// A command that exits non zero is ordinary output, and one that succeeds is progress
			Effect:   tools.Mutates,
			Fallible: true,
		},
		{
			Name:    "process_manage",
			Toolset: toolset,
			Title:   "Processes",
			Icon:    tools.IconOutput,
			Subject: "session_id",
			Schema:  processSchema,
			Handler: r.process,
			// Polled for a process's output and state, so repeating it is no sign of being stuck
			Effect:   tools.Mutates,
			Fallible: true,
			Polls:    true,
		},
	}
}

// Waits for the session's background processes that notify on completion, so a one-shot session answers their ends before it ends
func (r *registrant) Linger(ctx context.Context, _ *tools.Env, session string, deadline time.Time) (bool, bool) {
	waited, running := r.host.Jobs.Linger(ctx, session, deadline)
	return len(waited) > 0, len(running) > 0
}

// JSON the model reads, its text unescaped
func encode(v any) string {
	var b strings.Builder
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// Where a background process's notices go, nil where nothing can reach the session later
func deliver(env *tools.Env) terminal.Deliver {
	if env.Notify == nil {
		return nil
	}
	return func(text, title string) error { return env.Notify(tools.Notice{Text: text, Title: title}) }
}
