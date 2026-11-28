package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/skills"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
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
			return "", errors.New("Nothing of that title is left once characters that can't be shown are taken out. Use letters, digits or punctuation.")
		}
		stored, err := store.SetTitle(a.ctx, id, cleaned, sessions.TitleUser)
		if err != nil {
			return "", err
		}
		if r, err := a.run(id); err == nil {
			a.titled(r, stored)
		}
		return "This session is now titled “" + stored + "”.", nil
	}
	session, err := store.Get(a.ctx, id)
	if err != nil {
		return "", err
	}
	if session.GetTitle() == "" {
		return "This session has no title yet. Give it one with /title followed by the title.", nil
	}
	return "This session is titled “" + session.GetTitle() + "”.", nil
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
		return errors.New(replyRunning)
	}
	sent, err := a.sent(r)
	if err != nil {
		return err
	}
	if len(sent) == 0 {
		return errNothingToRetry
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

// Refusal of /retry in a session with no message of the person's
var errNothingToRetry = errors.New("You haven't sent a message in this session yet, so there is nothing to send again.")

// Refusal of a command that needs the session idle
const replyRunning = "A reply is still running. Stop it first, then try again."

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
		return time.Time{}, errors.New(replyRunning)
	}
	sent, err := a.sent(r)
	if err != nil {
		return time.Time{}, err
	}
	switch {
	case len(sent) == 0:
		return time.Time{}, errors.New("You haven't sent a message in this session yet, so there is nothing to take back.")
	case count > len(sent):
		return time.Time{}, fmt.Errorf("This session holds only %s of yours to take back.", strs.Count(len(sent), "message"))
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
	return "Your files keep the changes those replies made: " + strs.AndMore(files, undoHintFiles+1) + ". To undo them, type /rollback and restore a checkpoint."
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
		return 0, errors.New("No command is waiting for your approval.")
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
		return 0, errors.New("No command is waiting for your answer.")
	}
	return count, nil
}

// Flips Skip approvals in this chat for the session, through the one function of that switch, with the note it gives
func (a *Agent) Yolo(id string) (string, error) {
	if _, err := a.run(id); err != nil {
		return "", err
	}
	return a.approvals.Skip(id, !a.approvals.Skips(id))
}

// Tells the watchers of a session this process holds how it stands once whether it skips approvals changed, whichever way that was
func (a *Agent) skipped(id string) {
	a.mu.Lock()
	r := a.runs[id]
	a.mu.Unlock()
	if r != nil {
		a.changed(r)
	}
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

// What /reasoning alone says outside this app: the effort of the session and what can be chosen
func (a *Agent) EffortNow(id string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	return "Reasoning effort is " + effortWords(a.ownEffort(r)) + " in this chat. You can choose " + efforts() + ", or reset for the effort of new chats.", nil
}

// What /busy alone says outside this app: what enter does while the agent works in the session
func (a *Agent) BusyNow(id string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	return "Enter while busy is " + agentconfig.BusyTitle(a.settings(r).GetString(agentconfig.BusyInputMode)) + " in this chat.", nil
}

// What /model alone says outside this app: the model in effect for the session and the models its provider offers
func (a *Agent) ModelNow(ctx context.Context, id string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	_, route, err := a.route(r)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	switch level := a.ownEffort(r); {
	case route.Model == "":
		fmt.Fprintf(&out, "No model is set for %s in this chat.", a.providerTitle(a.settings(r), route.Provider))
	case level != "":
		fmt.Fprintf(&out, "This chat uses `%s` · %s, with %s reasoning effort.", route.Model, a.providerTitle(a.settings(r), route.Provider),
			strings.ToLower(effortWords(level)))
	default:
		fmt.Fprintf(&out, "This chat uses `%s` · %s.", route.Model, a.providerTitle(a.settings(r), route.Provider))
	}
	out.WriteString(a.modelInfo(a.settings(r), route))
	if ids := a.models.Host().Catalog(ctx, a.settings(r), route, false).IDs; len(ids) > 0 {
		out.WriteString("\n\n**Models you can pick**\n\n")
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
	fmt.Fprintf(&out, "- **Session** `%s`\n- **Title** %s\n- **Folder** `%s`\n", session.GetId(), cmp.Or(session.GetTitle(), "None"), session.GetDirectory())
	switch {
	case routeErr != nil:
		fmt.Fprintf(&out, "- **Model** %s\n", routeErr)
	case route.Model == "":
		fmt.Fprintf(&out, "- **Model** Not set for %s\n", a.providerTitle(a.settings(r), route.Provider))
	default:
		fmt.Fprintf(&out, "- **Model** `%s` · %s\n", route.Model, a.providerTitle(a.settings(r), route.Provider))
	}
	if effort := a.ownEffort(r); effort != "" {
		fmt.Fprintf(&out, "- **Reasoning effort** %s\n", effortWords(effort))
	}
	r.mu.Lock()
	var active, primary provider.Route
	if f := r.failover; f != nil {
		active, primary = f.active, f.primary
	}
	r.mu.Unlock()
	if active.Model != "" {
		settings := a.settings(r)
		fmt.Fprintf(&out, "- **Fallback** `%s` · %s, while `%s` · %s can't be used\n", active.Model, a.providerTitle(settings, active.Provider),
			primary.Model, a.providerTitle(settings, primary.Provider))
	}
	fmt.Fprintf(&out, "- **Messages** %s\n", strs.Number(float64(len(messages))))
	out.WriteString(a.usageLines(r, session))
	if a.contexts != nil {
		out.WriteString(a.contextLines(r))
	}
	settings := a.settings(r)
	fmt.Fprintf(&out, "- **When to ask** %s\n", approvals.ModeTitle(settings, approvals.Mode(settings)))
	if a.approvals.Skips(id) {
		fmt.Fprintf(&out, "- **%s** On\n", approvalsconfig.SkipTitle)
	}
	if waiting := len(a.approvals.Pending(id)); waiting > 0 {
		fmt.Fprintf(&out, "- **Waiting for approval** %s\n", strs.Count(waiting, "command"))
	}
	if busy {
		fmt.Fprintf(&out, "- **Replying** Yes, %s queued\n", strs.Count(waiting, "message"))
	}
	return out.String(), nil
}

// /approvals as the server runs it, for chat apps and icebath chat: alone it says When to ask and how the session stands against it; a mode
// is the shortcut to When to ask, and with --session, or --session alone, to Skip approvals in this chat. Each goes through the one function
// of its control and answers with its note
func (a *Agent) Approvals(id, argument string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	mode, chat := "", false
	for _, word := range strings.Fields(strings.ToLower(argument)) {
		switch word {
		case "--session":
			chat = true
		case approvalsconfig.Manual, approvalsconfig.Smart, approvalsconfig.Off:
			mode = word
		default:
			return "", errors.New("Usage: /approvals [manual|smart|off] [--session]")
		}
	}
	switch {
	case chat && mode == "":
		return a.approvals.Skip(id, !a.approvals.Skips(id))
	case chat:
		return a.approvals.Skip(id, mode == approvalsconfig.Off)
	case mode == "":
		return a.approvals.Standing(a.settings(r), id), nil
	}
	note, err := a.approvals.SetMode(a.config.In(r.profileName()), mode)
	if err != nil {
		return "", err
	}
	// What the session was started with wins in it over the profile's mode
	if own := approvals.Mode(a.settings(r)); own != mode && a.approvals.Winning(a.settings(r)) == "" {
		note += " This session started with When to ask set to " + approvals.ModeTitle(a.settings(r), own) + ", which wins in it."
	}
	return note, nil
}

// Shows the profile the session runs on, or runs it on another from now on. The provider, model and effort it picked and the accounts it
// was held to are forgotten, since they were those of the profile it ran on
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
		return fmt.Sprintf("This session runs on the profile %s, kept in %s. Type /profile and a name to switch it to another.",
			profile.Label(current, profile.ReadMeta(dir).DisplayName), profile.Tilde(dir)), nil
	}
	switch {
	case name == current:
		return "This session already runs on the profile " + name + ".", nil
	case !a.config.Exists(name):
		return "", a.noProfile(id, name, current)
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
	// What it held of its own named the providers of the profile it ran on, and the accounts of theirs it was held to
	dropped := map[string]any{agentconfig.Provider: nil, agentconfig.Model: nil, agentconfig.ReasoningEffort: nil}
	for key := range r.own() {
		if strings.HasPrefix(key, providerconfig.Key+".") {
			dropped[key] = nil
		}
	}
	if err := a.keepOwn(a.ctx, r, dropped); err != nil {
		return "", err
	}
	return "This session runs on the profile " + name + " from now on.", nil
}

// Refusal of /profile with a name no profile has, listing those the session can move to; with none other, outside this app it names the
// command that makes one
func (a *Agent) noProfile(id, name, current string) error {
	names, err := profile.Names(a.config)
	if err != nil {
		return err
	}
	others := slices.DeleteFunc(names, func(other string) bool { return other == current })
	if len(others) > 0 {
		return fmt.Errorf("There is no profile named %s. You can choose %s.", name, strs.Or(others))
	}
	refusal := fmt.Sprintf("There is no profile named %s, and %s is the only one.", name, current)
	if store, err := a.store(); err == nil {
		if session, err := store.Get(a.ctx, id); err == nil && platformOf(session) != "tui" {
			refusal += " icebath profile create " + name + " makes it."
		}
	}
	return errors.New(refusal)
}
