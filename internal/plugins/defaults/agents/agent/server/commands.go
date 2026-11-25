package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

var efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func (a *Agent) Title(id, title string) (string, error) {
	store, err := a.store()
	if err != nil {
		return "", err
	}
	if title != "" {
		if err := store.Rename(id, title); err != nil {
			return "", err
		}
		return "title set to " + title, nil
	}
	session, err := store.Get(id)
	if err != nil {
		return "", err
	}
	return "title: " + session.GetTitle(), nil
}

func (a *Agent) Retry(id string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	text, err := a.rewind(r, 1)
	if err != nil {
		return err
	}
	r.queue = append(r.queue, text)
	return a.start(r)
}

func (a *Agent) Undo(id string, count int) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return a.rewind(r, count)
}

func (a *Agent) rewind(r *run, count int) (string, error) {
	if r.busy {
		return "", errors.New("a turn is running: stop it first")
	}
	store, err := a.store()
	if err != nil {
		return "", err
	}
	messages, err := store.Messages(r.id)
	if err != nil {
		return "", err
	}
	for keep := len(messages) - 1; keep >= 0; keep-- {
		if messages[keep].GetRole() != sessionsv1.Role_ROLE_USER {
			continue
		}
		if count--; count > 0 {
			continue
		}
		if err := store.Truncate(r.id, keep); err != nil {
			return "", err
		}
		r.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Rewind{Rewind: &agentv1.Rewind{Messages: uint32(keep)}}})
		return messages[keep].GetText(), nil
	}
	return "", errors.New("nothing to take back")
}

func (a *Agent) Approve(id, scope string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.approval == nil {
		return errors.New("nothing waits for approval")
	}
	switch scope {
	case "session":
		r.allowed[r.approval.GetReason()] = true
	case "always":
		allowed := append(a.config.GetStringSlice(agentconfig.ApprovalsAllowlist), r.approval.GetReason())
		if err := a.config.Set(agentconfig.ApprovalsAllowlist, allowed); err != nil {
			return err
		}
	}
	r.answer <- nil
	r.approval = nil
	return nil
}

func (a *Agent) Deny(id, reason string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.approval == nil {
		return errors.New("nothing waits for approval")
	}
	r.answer <- errors.New(strings.TrimSpace("denied by the user " + reason))
	r.approval = nil
	return nil
}

func (a *Agent) Yolo(id string) (bool, error) {
	r, err := a.run(id)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	r.yolo = !r.yolo
	yolo := r.yolo
	r.mu.Unlock()
	a.changed(r)
	return yolo, nil
}

func (a *Agent) Model(ctx context.Context, id, model, name, effort string, reset, global bool) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	if name != "" {
		profile, ok := a.providers.Profile(name)
		if !ok {
			return "", fmt.Errorf("unknown provider %q", name)
		}
		name = profile.Name
	}
	if effort = strings.ToLower(effort); effort != "" && !slices.Contains(efforts, effort) {
		return "", fmt.Errorf("reasoning is one of %s", strings.Join(efforts, ", "))
	}
	changed := reset || model != "" || name != "" || effort != ""
	if global {
		stored := map[string]any{}
		if reset {
			stored[agentconfig.Provider], stored[agentconfig.Model], stored[agentconfig.ReasoningEffort] = nil, nil, nil
		}
		if name != "" {
			stored[agentconfig.Provider] = name
			if model == "" {
				stored[agentconfig.Model] = nil
			}
		}
		if model != "" {
			stored[agentconfig.Model] = model
		}
		if effort != "" {
			stored[agentconfig.ReasoningEffort] = effort
		}
		for _, key := range []string{agentconfig.Provider, agentconfig.Model, agentconfig.ReasoningEffort} {
			if value, ok := stored[key]; ok {
				if err := a.config.Set(key, value); err != nil {
					return "", err
				}
			}
		}
	}
	r.mu.Lock()
	if reset {
		r.provider, r.model, r.effort = "", "", ""
	}
	if name != "" {
		r.provider, r.model = name, ""
	}
	if model != "" {
		r.model = model
	}
	if effort != "" {
		r.effort = effort
	}
	r.mu.Unlock()
	a.changed(r)
	_, route, err := a.route(r)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	if route.Model == "" {
		fmt.Fprintf(&out, "no model set on %s", route.Provider)
	} else {
		fmt.Fprintf(&out, "model: `%s` on %s", route.Model, route.Provider)
	}
	if level := a.effort(r); level != "" {
		fmt.Fprintf(&out, ", reasoning %s", level)
	}
	if changed {
		return out.String(), nil
	}
	if ids, ok := a.models.Models(ctx, route); ok && len(ids) > 0 {
		out.WriteString("\n\nModels:\n\n")
		for _, id := range ids {
			fmt.Fprintf(&out, "- `%s`\n", id)
		}
	}
	return out.String(), nil
}

func (a *Agent) Status(id string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	store, err := a.store()
	if err != nil {
		return "", err
	}
	session, err := store.Get(id)
	if err != nil {
		return "", err
	}
	messages, err := store.Messages(id)
	if err != nil {
		return "", err
	}
	_, route, routeErr := a.route(r)
	r.mu.Lock()
	yolo, busy, queued := r.yolo, r.busy, len(r.queue)
	r.mu.Unlock()
	var out strings.Builder
	fmt.Fprintf(&out, "- session: `%s`\n- title: %s\n- directory: `%s`\n", session.GetId(), cmp.Or(session.GetTitle(), "untitled"), session.GetDirectory())
	switch {
	case routeErr != nil:
		fmt.Fprintf(&out, "- model: %s\n", routeErr)
	case route.Model == "":
		fmt.Fprintf(&out, "- model: none set on %s\n", route.Provider)
	default:
		fmt.Fprintf(&out, "- model: `%s` on %s (%s)\n", route.Model, route.Provider, route.Mode)
	}
	if effort := a.effort(r); effort != "" {
		fmt.Fprintf(&out, "- reasoning: %s\n", effort)
	}
	fmt.Fprintf(&out, "- messages: %d\n- approvals: %s", len(messages), a.config.GetString(agentconfig.ApprovalsMode))
	if yolo {
		out.WriteString(", yolo on for this session")
	}
	if busy {
		fmt.Fprintf(&out, "\n- working, %d queued", queued)
	}
	return out.String(), nil
}

func (a *Agent) Approvals(mode string) (string, error) {
	switch mode {
	case "":
	case agentconfig.ApprovalsManual, agentconfig.ApprovalsOff:
		if err := a.config.Set(agentconfig.ApprovalsMode, mode); err != nil {
			return "", err
		}
	default:
		return "", errors.New("usage: /approvals [manual|off]")
	}
	return "approvals: " + a.config.GetString(agentconfig.ApprovalsMode), nil
}

func (a *Agent) Diff(ctx context.Context, id string) (string, error) {
	store, err := a.store()
	if err != nil {
		return "", err
	}
	session, err := store.Get(id)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, arguments := range [][]string{{"status", "--short"}, {"diff"}} {
		command := exec.CommandContext(ctx, "git", arguments...)
		command.Dir = session.GetDirectory()
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %s", arguments[0], cmp.Or(strings.TrimSpace(string(output)), err.Error()))
		}
		out.Write(output)
	}
	if strings.TrimSpace(out.String()) == "" {
		return "no changes", nil
	}
	return "```diff\n" + tools.Bound(out.String(), a.config.GetInt(toolsconfig.OutputMaxBytes)) + "\n```", nil
}
