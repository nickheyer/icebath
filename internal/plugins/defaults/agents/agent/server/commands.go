package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/approvals"
	approvalsconfig "github.com/nickheyer/icebath/internal/approvals/config"
	"github.com/nickheyer/icebath/internal/checkpoints"
	"github.com/nickheyer/icebath/internal/contexts"
	"github.com/nickheyer/icebath/internal/models"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/skills"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Shows the session's title, or sets one the user chose
func (a *Agent) Title(id, title string) (string, error) {
	store, err := a.store()
	if err != nil {
		return "", err
	}
	if title = strings.TrimSpace(title); title != "" {
		cleaned, err := sessions.SanitizeTitle(title)
		if err != nil {
			return "", err
		}
		if cleaned == "" {
			return "", errors.New("Title is empty after cleanup. Please use printable characters.")
		}
		stored, err := store.SetTitle(a.ctx, id, cleaned, sessions.TitleUser)
		if err != nil {
			return "", err
		}
		if r, err := a.run(id); err == nil {
			a.titled(r, stored)
		}
		return "session title set: " + stored, nil
	}
	session, err := store.Get(a.ctx, id)
	if err != nil {
		return "", err
	}
	if session.GetTitle() == "" {
		return "no title set", nil
	}
	return "title: " + session.GetTitle(), nil
}

// Sends the last message the user sent again, the replies to it taken back
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
	sent, err := a.sent(r)
	if err != nil {
		return err
	}
	if len(sent) == 0 {
		return errors.New("Nothing to retry.")
	}
	last := sent[len(sent)-1]
	if err := a.takeBack(r, last); err != nil {
		return err
	}
	a.admit(r, last.GetText())
	r.queue = append(r.queue, resent(last))
	return a.start(r)
}

// A message the user sent as it is sent again: the text the model was sent, without the note of skill changes the turn adds again if it is
// still news, shown as the user typed it
func resent(message *sessionsv1.Message) queued {
	text := skills.Unnoted(cmp.Or(message.GetSent(), message.GetText()))
	if text == message.GetText() {
		return queued{text: text}
	}
	return queued{text: text, display: message.GetText()}
}

// Files an undo hint names before it counts the rest
const undoHintFiles = 8

// Takes back the last messages the user sent and the replies to them: the line naming the files the undone turns changed, which /rollback
// restores, empty when they changed none or snapshots don't serve the session
func (a *Agent) Undo(id string, count int) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	since, err := a.rewind(r, count)
	r.mu.Unlock()
	if err != nil {
		return "", err
	}
	return a.undoHint(r, since), nil
}

// Takes back the count's last messages the user sent and everything after the earliest of them, r.mu held: when the undone span began,
// the time of that message or of the newest message kept when that is later
func (a *Agent) rewind(r *run, count int) (time.Time, error) {
	if r.busy {
		return time.Time{}, errors.New("A reply is running. Stop it first.")
	}
	sent, err := a.sent(r)
	if err != nil {
		return time.Time{}, err
	}
	switch {
	case len(sent) == 0:
		return time.Time{}, errors.New("Nothing to undo.")
	case len(sent) == 1 && count > 1:
		return time.Time{}, errors.New("Only 1 message to undo.")
	case count > len(sent):
		return time.Time{}, fmt.Errorf("Only %d messages to undo.", len(sent))
	}
	target := sent[len(sent)-count]
	since := a.spanStart(r, target)
	if err := a.takeBack(r, target); err != nil {
		return time.Time{}, err
	}
	return since, nil
}

// When a span of the session undone from the message began: the message's time, or the newest time of the messages before it when that
// is later, summaries left out; zero when none is known
func (a *Agent) spanStart(r *run, target *sessionsv1.Message) time.Time {
	var since time.Time
	if target.GetTime() != nil {
		since = target.GetTime().AsTime()
	}
	store, err := a.store()
	if err != nil {
		return since
	}
	messages, err := store.Messages(a.ctx, r.sessionID(), sessions.Model)
	if err != nil {
		return since
	}
	for _, m := range messages {
		if m.GetId() >= target.GetId() || m.GetSummary() || m.GetTime() == nil {
			continue
		}
		since = laterOf(since, m.GetTime().AsTime())
	}
	return since
}

// Line naming the files of the session's project that changed since the span began, from the snapshot it began from, pointing at
// /rollback; empty when none did or snapshots don't serve the session
func (a *Agent) undoHint(r *run, since time.Time) string {
	if since.IsZero() {
		return ""
	}
	store, err := a.store()
	if err != nil {
		return ""
	}
	session, err := store.Get(a.ctx, r.sessionID())
	if err != nil {
		return ""
	}
	scope := checkpoints.For(a.settings(r), session.GetDirectory(), a.terminal)
	if scope.Refusal != "" {
		return ""
	}
	files := scope.Changed(scope.Directory, since)
	if len(files) == 0 {
		return ""
	}
	named := strings.Join(files[:min(len(files), undoHintFiles)], ", ")
	if len(files) > undoHintFiles {
		named += fmt.Sprintf(" +%d more", len(files)-undoHintFiles)
	}
	return "↶ files changed in the undone turns: " + named + ". run /rollback to see checkpoints and restore them"
}

// Messages the user sent in the session that the model sees, oldest first, r.mu held
func (a *Agent) sent(r *run) ([]*sessionsv1.Message, error) {
	store, err := a.store()
	if err != nil {
		return nil, err
	}
	messages, err := store.Messages(a.ctx, r.sessionID(), sessions.Model)
	if err != nil {
		return nil, err
	}
	var sent []*sessionsv1.Message
	for _, message := range messages {
		if message.GetRole() == sessionsv1.Role_ROLE_USER && message.GetDisplay() != "auto_continue" && message.GetDisplay() != "hidden" {
			sent = append(sent, message)
		}
	}
	return sent, nil
}

// Takes back the user's message and everything after it, telling the watchers how many of the shown messages are kept, r.mu held. A
// message a compaction merged its summary into leaves the summary behind for the model
func (a *Agent) takeBack(r *run, target *sessionsv1.Message) error {
	store, err := a.store()
	if err != nil {
		return err
	}
	if _, err := store.Rewind(a.ctx, r.sessionID(), target.GetId()); err != nil {
		return err
	}
	if target.GetSummary() && target.GetRole() == sessionsv1.Role_ROLE_USER {
		if handoff := contexts.HandoffOnly(contexts.Text(target)); handoff != "" {
			kept := &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: handoff, Summary: true, ModelOnly: true, Display: "hidden"}
			if err := store.Append(a.ctx, r.sessionID(), sessions.Guard{}, kept); err != nil {
				return err
			}
		}
	}
	shown, err := store.Messages(a.ctx, r.sessionID(), sessions.Display)
	if err != nil {
		return err
	}
	keep := len(shown)
	r.forget(keep)
	r.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Rewind{Rewind: &agentv1.Rewind{Messages: uint32(keep)}}})
	return nil
}

// Answers the session's approval prompts yes, the oldest, the one named or all of them, giving how many were answered
func (a *Agent) Approve(id string, answer approvals.Answer) (int, error) {
	if _, err := a.run(id); err != nil {
		return 0, err
	}
	count := a.approvals.Resolve(id, answer)
	if count == 0 {
		return 0, errors.New("No pending command to approve.")
	}
	return count, nil
}

// Answers the session's approval prompts no, with the reason the user gave cut to a line the agent reads
func (a *Agent) Deny(id string, answer approvals.Answer) (int, error) {
	if _, err := a.run(id); err != nil {
		return 0, err
	}
	answer.Choice = approvals.Deny
	if reason := []rune(strings.TrimSpace(answer.Reason)); len(reason) > 280 {
		answer.Reason = strings.TrimSpace(string(reason[:280]))
	} else {
		answer.Reason = string(reason)
	}
	count := a.approvals.Resolve(id, answer)
	if count == 0 {
		return 0, errors.New("No pending command to deny.")
	}
	return count, nil
}

// Turns asking before risky commands off for the session, or on again, keeping it on the session's record; says which it did
func (a *Agent) Yolo(id string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	on := !a.approvals.Yolo(id)
	a.setYolo(r, on)
	return yoloText(on), nil
}

func (a *Agent) setYolo(r *run, on bool) {
	a.approvals.SetYolo(r.sessionID(), on)
	if store, err := a.store(); err == nil {
		if err := store.Update(a.ctx, r.sessionID(), sessions.Change{Yolo: &on}); err != nil {
			slog.Warn("failed to persist session yolo", "enabled", on, "session", r.sessionID(), "error", err)
		}
	}
	a.changed(r)
}

// Asks before a slash command that deletes or resets something: whether to go ahead, and what to add to its reply
func (a *Agent) Confirm(ctx context.Context, id, command, detail string) (bool, string, error) {
	r, err := a.run(id)
	if err != nil {
		return false, "", err
	}
	store, err := a.store()
	if err != nil {
		return false, "", err
	}
	session, err := store.Get(ctx, id)
	if err != nil {
		return false, "", err
	}
	ok, note := a.approvals.Confirm(ctx, approvals.Context{Session: id, Config: a.settings(r), Source: session.GetSource()}, command, detail)
	return ok, note, nil
}

func (a *Agent) Reasoning(id, level string, global bool) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	choices := strings.Join(provider.Efforts[:len(provider.Efforts)-1], ", ") + " or " + provider.Efforts[len(provider.Efforts)-1]
	level = strings.ToLower(level)
	if level != "" && level != "reset" && !slices.Contains(provider.Efforts, level) {
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
	current := a.ownEffort(r)
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
		instance, ok := a.provider(settings, name)
		if !ok {
			return "", fmt.Errorf("Profile %s has no provider named %s.", settings.Profile(), name)
		}
		name = instance.ID
	}
	if effort = strings.ToLower(effort); effort != "" && !slices.Contains(provider.Efforts, effort) {
		return "", fmt.Errorf("Reasoning effort must be one of: %s.", strings.Join(provider.Efforts, ", "))
	}
	note := ""
	// A shortcut's name, or with no provider named one of their models, switches to the shortcut, which runs where it says
	shortcut := false
	if model != "" {
		if found, ok := lookupAlias(a.settings(r), model); ok {
			model, shortcut = found.name, true
		} else if name == "" {
			current, _, _ := a.route(r)
			if found, ok := a.switchAlias(a.settings(r), current, model); ok {
				model, shortcut = found.name, true
			}
		}
	}
	if model != "" && !shortcut {
		verdict := a.checkModel(ctx, r, name, model)
		if !verdict.Accepted {
			return "", errors.New(verdict.Message)
		}
		note = verdict.Message
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
	if shortcut && name == "" {
		r.provider = ""
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
	if level := a.ownEffort(r); level != "" {
		fmt.Fprintf(&out, " with %s reasoning", level)
	}
	if changed {
		out.WriteString(a.modelInfo(a.settings(r), route))
		if note != "" {
			out.WriteString("\n\n" + note)
		}
		return out.String(), nil
	}
	if ids := a.models.Host().Catalog(ctx, a.settings(r), route, false).IDs; len(ids) > 0 {
		out.WriteString("\n\nModels:\n\n")
		for _, id := range ids {
			fmt.Fprintf(&out, "- `%s`\n", id)
		}
	}
	return out.String(), nil
}

// What the provider the switch lands on, the one named or the session's own, makes of the model it names; taken when the switch can't be
// routed yet, which the switch itself then says
func (a *Agent) checkModel(ctx context.Context, r *run, name, model string) models.Verdict {
	settings := a.settings(r)
	instance, ok := a.provider(settings, name)
	if name == "" {
		var err error
		instance, _, err = a.route(r)
		ok = err == nil
	}
	if !ok {
		return models.Verdict{Accepted: true}
	}
	route, err := a.credentials.Route(instance, model)
	if err != nil {
		return models.Verdict{Accepted: true}
	}
	return a.models.Host().Validate(ctx, settings, route)
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
	session, err := store.Get(a.ctx, id)
	if err != nil {
		return "", err
	}
	messages, err := store.Messages(a.ctx, id, sessions.Model)
	if err != nil {
		return "", err
	}
	_, route, routeErr := a.route(r)
	r.mu.Lock()
	busy, waiting := r.busy, len(r.queue)
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
	if effort := a.ownEffort(r); effort != "" {
		fmt.Fprintf(&out, "- **Reasoning effort** %s\n", effort)
	}
	r.mu.Lock()
	if f := r.failover; f != nil && f.active.Model != "" {
		fmt.Fprintf(&out, "- **Fallback** `%s` on %s, while %s via %s is unavailable\n", f.active.Model, f.active.Provider, f.primary.Model, f.primary.Provider)
	}
	r.mu.Unlock()
	fmt.Fprintf(&out, "- **Messages** %d\n", len(messages))
	out.WriteString(a.usageLines(r, session))
	if a.contexts != nil {
		out.WriteString(a.contextLines(r))
	}
	switch {
	case a.approvals.Launched() || a.approvals.Yolo(id):
		out.WriteString("- **Approvals** off for this session (yolo)\n")
	default:
		fmt.Fprintf(&out, "- **Approvals** %s\n", approvals.Mode(a.settings(r)))
	}
	if waiting := len(a.approvals.Pending(id)); waiting > 0 {
		fmt.Fprintf(&out, "- **Waiting for approval** %d\n", waiting)
	}
	if busy {
		fmt.Fprintf(&out, "- **Replying**, %d queued\n", waiting)
	}
	return out.String(), nil
}

// Shows the approval mode of the profile the session runs on and whether the session skips it, sets the mode for good, or with --session
// asks or skips for this session alone, the switch /yolo flips
func (a *Agent) Approvals(id, argument string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	mode, session := "", false
	for _, word := range strings.Fields(strings.ToLower(argument)) {
		switch word {
		case "--session":
			session = true
		case approvalsconfig.Manual, approvalsconfig.Smart, approvalsconfig.Off:
			mode = word
		default:
			return "", errors.New("Usage: /approvals [manual|smart|off] [--session]")
		}
	}
	settings := a.settings(r)
	current := approvals.Mode(settings)
	switch {
	case mode == "" && (a.approvals.Launched() || a.approvals.Yolo(id)):
		return fmt.Sprintf("Approvals: %s, but this session runs everything without asking (/yolo).", current), nil
	case mode == "":
		return fmt.Sprintf("Approval mode: %s (persistent profile setting).", current), nil
	case session:
		on := mode == approvalsconfig.Off
		a.setYolo(r, on)
		return yoloText(on), nil
	}
	if err := settings.Set(approvalsconfig.Mode, mode); err != nil {
		return "", fmt.Errorf("Failed to save approval mode: %w", err)
	}
	if effective := approvals.Mode(settings); effective != mode {
		return fmt.Sprintf("Approval mode remains %s; the requested value did not become effective.", effective), nil
	}
	return fmt.Sprintf("Approval mode: %s (persistent profile setting).", mode), nil
}

// What /yolo says it did
func yoloText(on bool) string {
	if on {
		return "⚡ YOLO mode **ON** for this session — all commands auto-approved. Use with caution."
	}
	return "⚠️ YOLO mode **OFF** for this session — dangerous commands will require approval."
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
		dir := a.config.ProfileDir(current)
		return fmt.Sprintf("Profile: %s\nHome: %s\nSwitch this session with /profile <name>.", profile.Label(current, profile.ReadMeta(dir).DisplayName),
			profile.Tilde(dir)), nil
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
	if err := store.Move(a.ctx, id, name); err != nil {
		return "", err
	}
	r.mu.Lock()
	r.profile, r.provider, r.model, r.effort = name, "", "", ""
	r.mu.Unlock()
	a.changed(r)
	return "This session runs on profile " + name + " from now on.", nil
}
