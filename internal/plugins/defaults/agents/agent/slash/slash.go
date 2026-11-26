package slash

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/slash"
)

type Agent interface {
	Title(sessionID, title string) (string, error)
	Retry(sessionID string) error
	Undo(sessionID string, count int) (string, error)
	Approve(sessionID, scope string) error
	Deny(sessionID, reason string) error
	Yolo(sessionID string) (bool, error)
	Model(ctx context.Context, sessionID, model, provider, effort string, reset, global bool) (string, error)
	Reasoning(sessionID, level string, global bool) (string, error)
	Status(sessionID string) (string, error)
	Approvals(sessionID, mode string) (string, error)
	Profile(sessionID, name string) (string, error)
	Diff(ctx context.Context, sessionID, argument string) (string, error)
}

type commands struct {
	registry *slash.Registry
	agent    Agent
}

func Register(registry *slash.Registry, agent Agent) {
	registry.Register(commands{registry: registry, agent: agent})
}

func (c commands) Commands() []slash.Command {
	return []slash.Command{
		{Name: "help", Usage: "[filter]", Help: "List commands", Run: c.help},
		{Name: "title", Usage: "[title]", Help: "Show or set the session title", Run: c.title},
		{Name: "retry", Help: "Regenerate the last reply", Run: c.retry},
		{Name: "undo", Usage: "[n]", Help: "Remove the last message, or last n, and the replies, to edit and resend", Run: c.undo},
		{Name: "approve", Usage: "[session|always]", Help: "Allow the pending command once, for this session, or always", Run: c.approve},
		{Name: "deny", Usage: "[reason]", Help: "Deny the pending command, optionally saying why", Run: c.deny},
		{Name: "yolo", Help: "Toggle approvals off for this session", Run: c.yolo},
		{Name: "status", Help: "Show session info", Run: c.status},
		{Name: "reasoning", Usage: "[level|reset] [--global]", Help: "Show or set the reasoning effort", Run: c.reasoning},
		{Name: "approvals", Usage: "[manual|off]", Help: "Show or set the approval mode", Run: c.approvals},
		{Name: "diff", Usage: "[staged|all] [--stat] [path...]", Help: "Show uncommitted git changes", Run: c.diff},
		{Name: "version", Aliases: []string{"v"}, Help: "Show the icebath version", Run: c.version},
		{Name: "profile", Usage: "[name]", Help: "Show the profile this session runs on, or switch it to another", Run: c.profile},
		{Name: "model", Usage: "[model|reset] [--provider name] [--reasoning level] [--global|--session]", Help: "Show or switch the model", Run: c.model},
	}
}

func (c commands) profile(_ context.Context, call *slash.Call) (string, error) {
	return c.agent.Profile(call.SessionID, call.Argument)
}

func (c commands) help(_ context.Context, call *slash.Call) (string, error) {
	var out strings.Builder
	for _, command := range c.registry.Commands() {
		line := strings.TrimSpace("/" + command.Name + " " + command.Usage)
		if call.Argument != "" && !strings.Contains(line+" "+command.Help, call.Argument) {
			continue
		}
		fmt.Fprintf(&out, "- `%s` %s\n", line, command.Help)
	}
	if out.Len() == 0 {
		return "No commands match " + call.Argument, nil
	}
	return out.String(), nil
}

func (c commands) title(_ context.Context, call *slash.Call) (string, error) {
	return c.agent.Title(call.SessionID, call.Argument)
}

func (c commands) retry(_ context.Context, call *slash.Call) (string, error) {
	return "", c.agent.Retry(call.SessionID)
}

func (c commands) undo(_ context.Context, call *slash.Call) (string, error) {
	count := 1
	if call.Argument != "" {
		var err error
		if count, err = strconv.Atoi(call.Argument); err != nil || count < 1 {
			return "", errors.New("Usage: /undo [n]")
		}
	}
	if _, err := c.agent.Undo(call.SessionID, count); err != nil {
		return "", err
	}
	if count == 1 {
		return "Removed the last message.", nil
	}
	return fmt.Sprintf("Removed the last %d messages.", count), nil
}

func (c commands) approve(_ context.Context, call *slash.Call) (string, error) {
	switch call.Argument {
	case "", "session", "always":
		return "", c.agent.Approve(call.SessionID, call.Argument)
	}
	return "", errors.New("Usage: /approve [session|always]")
}

func (c commands) deny(_ context.Context, call *slash.Call) (string, error) {
	return "", c.agent.Deny(call.SessionID, call.Argument)
}

func (c commands) yolo(_ context.Context, call *slash.Call) (string, error) {
	on, err := c.agent.Yolo(call.SessionID)
	switch {
	case err != nil:
		return "", err
	case on:
		return "Approvals off for this session.", nil
	}
	return "Approvals on.", nil
}

func (c commands) status(_ context.Context, call *slash.Call) (string, error) {
	return c.agent.Status(call.SessionID)
}

func (c commands) reasoning(_ context.Context, call *slash.Call) (string, error) {
	words := strings.Fields(call.Argument)
	global := slices.Contains(words, "--global")
	if words = slices.DeleteFunc(words, func(word string) bool { return word == "--global" }); len(words) > 1 {
		return "", errors.New("Usage: /reasoning [level|reset] [--global]")
	}
	return c.agent.Reasoning(call.SessionID, strings.Join(words, ""), global)
}

func (c commands) approvals(_ context.Context, call *slash.Call) (string, error) {
	return c.agent.Approvals(call.SessionID, call.Argument)
}

func (c commands) diff(ctx context.Context, call *slash.Call) (string, error) {
	return c.agent.Diff(ctx, call.SessionID, call.Argument)
}

func (c commands) version(context.Context, *slash.Call) (string, error) {
	version := "(devel)"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	return "icebath " + version + " " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH, nil
}

func (c commands) model(ctx context.Context, call *slash.Call) (string, error) {
	const usage = "Usage: /model [model|reset] [--provider name] [--reasoning level] [--global|--session]"
	var model, provider, effort string
	var reset, global bool
	words := strings.Fields(call.Argument)
	for i := 0; i < len(words); i++ {
		switch word := words[i]; {
		case word == "--global":
			global = true
		case word == "--session":
			global = false
		case word == "--provider" || word == "--reasoning":
			if i++; i == len(words) {
				return "", errors.New(usage)
			}
			if word == "--provider" {
				provider = words[i]
			} else {
				effort = words[i]
			}
		case strings.HasPrefix(word, "--") || model != "" || reset:
			return "", errors.New(usage)
		case word == "reset":
			reset = true
		default:
			model = word
		}
	}
	return c.agent.Model(ctx, call.SessionID, model, provider, effort, reset, global)
}
