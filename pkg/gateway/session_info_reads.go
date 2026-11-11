package gateway

import (
	"context"
	"strings"
)

// Line builders of the classic CLI a local session's owner reads run, assigned in cmd/icebath/wiring.go
var SessionReadLines struct {
	Toolsets  func(ctx context.Context, enabled []string) []string
	Platforms func(ctx context.Context) []string
	Cron      func(ctx context.Context, h, arg string) ([]string, error)
}

// Renderers of a TUI session's status and context reads, assigned in cmd/icebath/wiring.go
var TUIReadRender struct {
	Status  func(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession, policy *LocalSessionPolicy, agent any, arg string) (string, error)
	Context func(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession, policy *LocalSessionPolicy, agent any, arg string) (string, error)
}

// Exec reply of the lines built in the owning profile, joined and stripped of outer newlines
func ownerLines(ctx context.Context, a *SessionAuthority, lines func(ctx context.Context) ([]string, error)) (map[string]any, error) {
	built, err := lines(ownerScope(ctx, a, false))
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "exec", "output": strings.Trim(strings.Join(built, "\n"), "\n")}, nil
}

// Toolsets or platforms report of the session built by the owner in its profile
func runInfoRead(ctx context.Context, a *SessionAuthority, live *LiveSession, name string) (map[string]any, error) {
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	return ownerLines(ctx, a, func(ctx context.Context) ([]string, error) {
		if name == "platforms" {
			return SessionReadLines.Platforms(ctx), nil
		}
		var enabled []string
		if policy != nil {
			cfg, err := policy.config(ctx, a, false)
			if err != nil {
				return nil, err
			}
			if enabled, err = policy.currentToolsets(ctx, cfg); err != nil {
				return nil, err
			}
		}
		return SessionReadLines.Toolsets(ctx, enabled), nil
	})
}

// Read form of the split cron argument, overview, list or list-all, empty for a write
func cronRead(tokens []string) string {
	if len(tokens) == 0 {
		return "overview"
	}
	if strings.ToLower(tokens[0]) == "list" {
		for _, token := range tokens[1:] {
			if token != "--all" && token != "-a" {
				return ""
			}
		}
		if len(tokens) > 1 {
			return "list-all"
		}
		return "list"
	}
	return ""
}

// Cron command run by the owner in its profile, the store written here and never by a client
func runCron(ctx context.Context, a *SessionAuthority, arg string) (map[string]any, error) {
	return ownerLines(ctx, a, func(ctx context.Context) ([]string, error) {
		return SessionReadLines.Cron(ctx, a.ProfileID, arg)
	})
}

// TUI session status and context renderers, nil to keep the gateway handler
func tuiRead(policy *LocalSessionPolicy, name string) func(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession, policy *LocalSessionPolicy, agent any, arg string) (string, error) {
	if policy == nil || policy.Source != "tui" {
		return nil
	}
	switch name {
	case "status":
		return TUIReadRender.Status
	case "context":
		return TUIReadRender.Context
	}
	return nil
}

// Exec reply of a TUI read rendered in the owning profile and the session workspace
func runTUIRead(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession, policy *LocalSessionPolicy, render func(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession, policy *LocalSessionPolicy, agent any, arg string) (string, error), arg string) (map[string]any, error) {
	agent, err := a.agent(ref)
	if err != nil {
		return nil, err
	}
	if nilAgent(agent) {
		agent = nil
	}
	scoped, err := sessionWorkspace(ownerScope(ctx, a, false), a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	output, err := render(scoped, a, ref, live, policy, agent, arg)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "exec", "output": output}, nil
}
