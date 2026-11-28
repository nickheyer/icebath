package server

import (
	"cmp"
	"context"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/auxiliary"
	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/skills"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Longest account of a failed title shown
const failureChars = 220

// Names the session from the message the user sent: an instant title from its first line now, then one the title model writes. The first
// message always tries; the second and third try again while the title is only the instant one; later ones only while it has none
func (a *Agent) title(ctx context.Context, r *run, store sessions.Store, aux *auxiliary.Client, route provider.Route, message string) {
	// A message that loads skills is titled as the user typed it, the slash command and its instruction
	if described, ok := skills.Describe(message, " — "); ok {
		message = described
	}
	history, err := store.Messages(ctx, r.sessionID(), sessions.Model)
	if err != nil {
		return
	}
	turns := 0
	for _, m := range history {
		if m.GetRole() == sessionsv1.Role_ROLE_USER && sessions.Titleable(m.GetText()) {
			turns++
		}
	}
	session, err := store.Get(ctx, r.sessionID())
	if err != nil {
		return
	}
	upgraded := session.GetTitle() != "" && sessions.TitleRank(session.GetTitleSource()) > sessions.TitleRank(sessions.TitleDerived)
	if turns > 1 && (upgraded || turns > 3 && session.GetTitle() != "") || !sessions.Titleable(message) {
		return
	}
	task := auxiliary.Task{Name: sessions.TitleTask}
	if !aux.Config.GetBool(task.Key(sessions.TitleEnabled)) {
		return
	}
	if derived := sessions.DeriveTitle(message, ""); derived != "" {
		if stored, err := store.SetTitle(ctx, r.sessionID(), derived, sessions.TitleDerived); err != nil {
			slog.Debug("Instant title failed", "session", r.sessionID(), "error", err)
		} else if stored != "" {
			a.titled(r, stored)
		}
	}
	if !aux.Config.GetBool(task.Key(sessions.TitleUpgrade)) {
		return
	}
	// The title model's call outlives the turn, a stop included, but not icebath, whose close waits for it
	upgrade := func() {
		ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()
		defer context.AfterFunc(a.ctx, cancel)()
		a.upgradeTitle(ctx, r, store, aux, route, message)
	}
	if route.Local && sharesEndpoint(aux, task, route) {
		r.mu.Lock()
		r.life.title = upgrade
		r.mu.Unlock()
		return
	}
	a.aside(upgrade)
}

// Whether the title model would run on the turn's own local server, which then serves the turn first
func sharesEndpoint(aux *auxiliary.Client, task auxiliary.Task, route provider.Route) bool {
	pinned := strings.TrimSpace(aux.Config.GetString(task.Key(auxiliaryconfig.Provider)))
	base := strings.TrimRight(strings.TrimSpace(aux.Config.GetString(task.Key(auxiliaryconfig.BaseURL))), "/")
	sameProvider := pinned == "" || pinned == auxiliaryconfig.Auto || pinned == auxiliaryconfig.Main || pinned == route.Provider
	return sameProvider && (base == "" || base == strings.TrimRight(route.BaseURL, "/"))
}

// Writes the title the model gives, taking a numbered one when another session holds it, and tells the watchers
func (a *Agent) upgradeTitle(ctx context.Context, r *run, store sessions.Store, aux *auxiliary.Client, route provider.Route, message string) {
	session, err := store.Get(ctx, r.sessionID())
	if err != nil || session.GetTitle() != "" && sessions.TitleRank(session.GetTitleSource()) > sessions.TitleRank(sessions.TitleDerived) {
		return
	}
	request := sessions.TitleRequest{Message: message}
	task := auxiliary.Task{Name: sessions.TitleTask}
	if aux.Config.GetBool(task.Key(sessions.TitlePreferFast)) && route.Model != "" {
		if pinned := aux.Config.GetString(task.Key(auxiliaryconfig.Provider)); pinned == "" || pinned == auxiliaryconfig.Auto || pinned == auxiliaryconfig.Main {
			if ids := a.models.Host().Catalog(ctx, aux.Config, route, false).IDs; len(ids) > 0 {
				if fast := sessions.FastModel(ids); fast != "" && fast != route.Model {
					request.Model = fast
				}
			}
		}
	}
	title, source, err := sessions.GenerateTitle(ctx, aux, request)
	if err != nil {
		slog.Warn("Title generation failed", "session", r.sessionID(), "error", err)
		detail := strings.Join(strings.Fields(err.Error()), " ")
		if runes := []rune(detail); len(runes) > failureChars {
			detail = string(runes[:failureChars-1]) + "…"
		}
		r.mu.Lock()
		a.notify(r, "⚠ The helper model could not name this session: "+strings.TrimSuffix(cmp.Or(detail, "no reason given"), ".")+
			". Check its model with /settings session titles.")
		r.mu.Unlock()
	}
	if title == "" {
		title, source = sessions.DeriveTitle(message, ""), sessions.TitleDerived
	}
	if title == "" {
		return
	}
	stored, err := store.SetTitle(ctx, r.sessionID(), title, source)
	if err != nil {
		next, nextErr := store.NextTitle(ctx, session.GetProfile(), title)
		if nextErr != nil || next == title {
			slog.Debug("Failed to set auto-generated title", "session", r.sessionID(), "error", err)
			return
		}
		if stored, err = store.SetTitle(ctx, r.sessionID(), next, source); err != nil {
			slog.Debug("Failed to set auto-generated title", "session", r.sessionID(), "error", err)
			return
		}
	}
	if stored != "" {
		a.titled(r, stored)
	}
}

// Tells the watchers the session's new title
func (a *Agent) titled(r *run, title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Title{Title: title}})
}
