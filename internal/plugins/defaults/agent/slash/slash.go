package slash

import (
	"context"
	"errors"
	"fmt"
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
		{Name: "help", Usage: "[filter]", Help: "Show available commands", Run: c.help},
		{Name: "title", Usage: "[name]", Help: "Show the title of this session, or set it", Run: c.title},
		{Name: "retry", Help: "Retry the last message", Run: c.retry},
		{Name: "undo", Usage: "[N]", Help: "Back up N user turns and what followed them", Run: c.undo},
		{Name: "approve", Usage: "[session|always]", Help: "Approve a pending dangerous command, and others like it for this session or from now on", Run: c.approve},
		{Name: "deny", Usage: "[reason]", Help: "Deny a pending dangerous command", Run: c.deny},
		{Name: "yolo", Help: "Toggle YOLO mode (skip all dangerous command approvals)", Run: c.yolo},
		{Name: "model", Usage: "[model|reset] [--provider name] [--reasoning level] [--global|--session]", Help: "Show the model and the ones the provider serves, or switch model for this session", Run: c.model},
	}
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
		return "no command matches " + call.Argument, nil
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
			return "", errors.New("usage: /undo [N]")
		}
	}
	text, err := c.agent.Undo(call.SessionID, count)
	if err != nil {
		return "", err
	}
	return "took back: " + text, nil
}

func (c commands) approve(_ context.Context, call *slash.Call) (string, error) {
	switch call.Argument {
	case "", "session", "always":
		return "", c.agent.Approve(call.SessionID, call.Argument)
	}
	return "", errors.New("usage: /approve [session|always]")
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
		return "yolo on: risky commands run without asking", nil
	}
	return "yolo off: risky commands ask first", nil
}

func (c commands) model(ctx context.Context, call *slash.Call) (string, error) {
	const usage = "usage: /model [model|reset] [--provider name] [--reasoning level] [--global|--session]"
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
