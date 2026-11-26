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

func (a *Agent) Title(id, title string) (string, error) {
	store, err := a.store()
	if err != nil {
		return "", err
	}
	if title != "" {
		if err := store.Rename(id, title); err != nil {
			return "", err
		}
		return "Title: " + title, nil
	}
	session, err := store.Get(id)
	if err != nil {
		return "", err
	}
	if session.GetTitle() == "" {
		return "No title yet.", nil
	}
	return "Title: " + session.GetTitle(), nil
}

func (a *Agent) Retry(id string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busy {
		return errors.New("A reply is running. Stop it first.")
	}
	store, err := a.store()
	if err != nil {
		return err
	}
	messages, err := store.Messages(r.id)
	if err != nil {
		return err
	}
	keep := len(messages)
	for keep > 0 && messages[keep-1].GetRole() != sessionsv1.Role_ROLE_USER {
		keep--
	}
	if keep == 0 {
		return errors.New("Nothing to retry.")
	}
	if keep < len(messages) {
		if err := store.Truncate(r.id, keep); err != nil {
			return err
		}
		r.forget(keep)
		r.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Rewind{Rewind: &agentv1.Rewind{Messages: uint32(keep)}}})
	}
	r.again = true
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
		return "", errors.New("A reply is running. Stop it first.")
	}
	store, err := a.store()
	if err != nil {
		return "", err
	}
	messages, err := store.Messages(r.id)
	if err != nil {
		return "", err
	}
	sent := 0
	for _, message := range messages {
		if message.GetRole() == sessionsv1.Role_ROLE_USER {
			sent++
		}
	}
	switch {
	case sent == 0:
		return "", errors.New("Nothing to undo.")
	case sent == 1 && count > 1:
		return "", errors.New("Only 1 message to undo.")
	case count > sent:
		return "", fmt.Errorf("Only %d messages to undo.", sent)
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
		r.forget(keep)
		r.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Rewind{Rewind: &agentv1.Rewind{Messages: uint32(keep)}}})
		return messages[keep].GetText(), nil
	}
	return "", nil
}

func (a *Agent) Approve(id, scope string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	settings := a.settings(r)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.approval == nil {
		return errors.New("Nothing is waiting for approval.")
	}
	switch scope {
	case "session":
		r.allowed[r.approval.GetReason()] = true
	case "always":
		allowed := append(settings.GetStringSlice(agentconfig.ApprovalsAllowlist), r.approval.GetReason())
		if err := settings.Set(agentconfig.ApprovalsAllowlist, allowed); err != nil {
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
		return errors.New("Nothing is waiting for approval.")
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		reason = " Their reason: " + reason
	}
	r.answer <- errors.New("The user denied this command." + reason)
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

func (a *Agent) Reasoning(id, level string, global bool) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	choices := strings.Join(agentconfig.Efforts[:len(agentconfig.Efforts)-1], ", ") + " or " + agentconfig.Efforts[len(agentconfig.Efforts)-1]
	level = strings.ToLower(level)
	if level != "" && level != "reset" && !slices.Contains(agentconfig.Efforts, level) {
		return "", fmt.Errorf("Reasoning effort must be %s.", choices)
	}
	if level != "" {
		if global {
			var stored any
			if level != "reset" {
				stored = level
			}
			if err := a.settings(r).Set(agentconfig.ReasoningEffort, stored); err != nil {
				return "", err
			}
		}
		r.mu.Lock()
		if r.effort = level; level == "reset" || global {
			r.effort = ""
		}
		r.mu.Unlock()
		a.changed(r)
	}
	current := a.effort(r)
	switch {
	case level == "" && current == "":
		return "Reasoning effort: model default. Options: " + choices + ".", nil
	case level == "":
		return "Reasoning effort: " + current + ". Options: " + choices + ", or reset.", nil
	case current == "":
		return "Reasoning effort reset to the model default.", nil
	case global:
		return "Reasoning effort set to " + current + " for all sessions.", nil
	case level == "reset":
		return "Reasoning effort reset to " + current + ".", nil
	}
	return "Reasoning effort set to " + current + " for this session.", nil
}

func (a *Agent) Model(ctx context.Context, id, model, name, effort string, reset, global bool) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	if name != "" {
		settings := a.settings(r)
		instance, ok := a.providers.Instance(settings, name)
		if !ok {
			return "", fmt.Errorf("Profile %s has no provider named %s.", settings.Profile(), name)
		}
		name = instance.ID
	}
	if effort = strings.ToLower(effort); effort != "" && !slices.Contains(agentconfig.Efforts, effort) {
		return "", fmt.Errorf("Reasoning effort must be one of: %s.", strings.Join(agentconfig.Efforts, ", "))
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
				if err := a.settings(r).Set(key, value); err != nil {
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
		fmt.Fprintf(&out, "No model is set for %s", route.Provider)
	} else {
		fmt.Fprintf(&out, "Using `%s` on %s", route.Model, route.Provider)
	}
	if level := a.effort(r); level != "" {
		fmt.Fprintf(&out, " with %s reasoning", level)
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
	fmt.Fprintf(&out, "- **Session** `%s`\n- **Title** %s\n- **Directory** `%s`\n", session.GetId(), cmp.Or(session.GetTitle(), "none"), session.GetDirectory())
	switch {
	case routeErr != nil:
		fmt.Fprintf(&out, "- **Model** %s\n", routeErr)
	case route.Model == "":
		fmt.Fprintf(&out, "- **Model** none set for %s\n", route.Provider)
	default:
		fmt.Fprintf(&out, "- **Model** `%s` on %s\n", route.Model, route.Provider)
	}
	if effort := a.effort(r); effort != "" {
		fmt.Fprintf(&out, "- **Reasoning effort** %s\n", effort)
	}
	fmt.Fprintf(&out, "- **Messages** %d\n", len(messages))
	switch {
	case yolo:
		out.WriteString("- **Approvals** off for this session\n")
	case a.settings(r).GetString(agentconfig.ApprovalsMode) == agentconfig.ApprovalsOff:
		out.WriteString("- **Approvals** off\n")
	default:
		out.WriteString("- **Approvals** on\n")
	}
	if busy {
		fmt.Fprintf(&out, "- **Replying**, %d queued\n", queued)
	}
	return out.String(), nil
}

// Shows or sets the approval mode of the profile the session runs on
func (a *Agent) Approvals(id, mode string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	settings := a.settings(r)
	switch mode {
	case "":
	case agentconfig.ApprovalsManual, agentconfig.ApprovalsOff:
		if err := settings.Set(agentconfig.ApprovalsMode, mode); err != nil {
			return "", err
		}
	default:
		return "", errors.New("Usage: /approvals [manual|off]")
	}
	if settings.GetString(agentconfig.ApprovalsMode) == agentconfig.ApprovalsOff {
		return "Approvals off: risky commands run without asking.", nil
	}
	return "Approvals on: risky commands ask first.", nil
}

func (a *Agent) Diff(ctx context.Context, id, argument string) (string, error) {
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
	var against, paths []string
	summary, untracked := false, true
	for words := strings.Fields(argument); len(words) > 0; {
		word := words[0]
		switch words = words[1:]; {
		case word == "--":
			paths, words = append(paths, words...), nil
		case word == "--stat":
			summary = true
		case word == "staged" && len(paths) == 0:
			against, untracked = []string{"--cached"}, false
		case word == "all" && len(paths) == 0:
			against = []string{"HEAD"}
		default:
			paths = append(paths, word)
		}
	}
	git := func(arguments ...string) (string, error) {
		command := exec.CommandContext(ctx, "git", append(append([]string{"--no-pager", "-c", "core.quotepath=off"}, arguments...), append([]string{"--"}, paths...)...)...)
		command.Dir = session.GetDirectory()
		output, err := command.CombinedOutput()
		if err != nil {
			return "", errors.New(cmp.Or(strings.TrimSpace(string(output)), err.Error()))
		}
		return strings.TrimRight(string(output), "\n"), nil
	}
	inside := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	inside.Dir = session.GetDirectory()
	if inside.Run() != nil {
		return "", errors.New("Not a git repository.")
	}
	var parts []string
	stat, err := git(append([]string{"diff", "--no-color", "--no-ext-diff", "--stat"}, against...)...)
	if err != nil {
		return "", err
	}
	if stat != "" {
		parts = append(parts, "```\n"+stat+"\n```")
	}
	if untracked {
		added, err := git("ls-files", "--others", "--exclude-standard")
		if err != nil {
			return "", err
		}
		if added != "" {
			parts = append(parts, "Untracked files:\n\n```\n"+added+"\n```")
		}
	}
	if !summary && stat != "" {
		diff, err := git(append([]string{"diff", "--no-color", "--no-ext-diff"}, against...)...)
		if err != nil {
			return "", err
		}
		parts = append(parts, "```diff\n"+tools.Bound(diff, a.settings(r).GetInt(toolsconfig.OutputMaxBytes))+"\n```")
	}
	if len(parts) == 0 {
		return "No changes.", nil
	}
	return strings.Join(parts, "\n\n"), nil
}

// Shows the profile the session runs on, or runs it on another from now on. The provider, model and effort it picked are forgotten, since
// they were those of the profile it ran on
func (a *Agent) Profile(id, name string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	current, busy := r.profile, r.busy
	r.mu.Unlock()
	if name = strings.ToLower(strings.TrimSpace(name)); name == "" {
		return "This session runs on profile " + current + ". Switch it with /profile <name>.", nil
	}
	switch {
	case name == current:
		return "This session already runs on profile " + name + ".", nil
	case !a.config.Exists(name):
		return "", fmt.Errorf("No profile named %s. Make one in /profiles.", name)
	case busy:
		return "", errors.New("Wait for the reply to finish, or stop it, before switching profiles.")
	}
	store, err := a.store()
	if err != nil {
		return "", err
	}
	if err := store.Move(id, name); err != nil {
		return "", err
	}
	r.mu.Lock()
	r.profile, r.provider, r.model, r.effort = name, "", "", ""
	r.mu.Unlock()
	a.changed(r)
	return "This session runs on profile " + name + " from now on.", nil
}
