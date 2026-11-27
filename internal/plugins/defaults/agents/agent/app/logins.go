package app

import (
	"cmp"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/profile"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// One thing the prompt of a login that ran out offers, and what it does
type loginAction struct {
	label, does string
	run         func() tea.Cmd
}

// Prompt shown once the login the session is held to ran out: switch to a login ready now, wait for its reset, retry at once, move the
// session or the provider to its rotation, or add a login
func (c *chat) exhausted(event *agentv1.LoginExhausted) tea.Cmd {
	kit := c.kit
	id := cmp.Or(event.GetSessionId(), c.id)
	name := event.GetLogin().GetName()
	when := ""
	if event.GetResets() != nil {
		when = event.GetResets().AsTime().Local().Format("15:04")
	}
	var hub *cards.Hub
	settle := func(done string) tea.Cmd { return tea.Batch(kit.WM.DismissCard(hub), wm.Notify(done, wm.Success)) }
	setLogin := func(request *agentv1.SetLoginRequest, done string) tea.Cmd {
		hub.Busy("Pinning the login…")
		return tui.Async(func() func() tea.Cmd {
			response, err := c.desk.agents.SetLogin(kit.Env.Ctx, request)
			return func() tea.Cmd {
				switch {
				case err != nil:
					return hub.Fail(err.Error())
				case response.GetWarning() != "":
					return hub.Fail(response.GetWarning())
				}
				return settle(done)
			}
		})
	}
	wait := func(now bool, done string) tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			response, err := c.desk.agents.WaitLogin(kit.Env.Ctx, &agentv1.WaitLoginRequest{SessionId: id, Provider: event.GetProvider(), Now: now})
			return func() tea.Cmd {
				switch {
				case err != nil:
					return hub.Fail(err.Error())
				case response.GetState() == "idle":
					return hub.Fail("The provider gave no reset time; retry now or switch.")
				}
				return settle(done)
			}
		})
	}
	var acts []loginAction
	for _, ready := range event.GetReady() {
		login, label := ready.GetId(), cmp.Or(ready.GetName(), ready.GetId())
		acts = append(acts, loginAction{label: "Switch to " + label, does: "pin that login for this session and send the last message again", run: func() tea.Cmd {
			return setLogin(&agentv1.SetLoginRequest{SessionId: id, Provider: event.GetProvider(), Login: login, Retry: true},
				"This session now uses "+label+"; the last message is sent again.")
		}})
	}
	if when != "" {
		acts = append(acts, loginAction{label: "Wait until " + when, does: "send the last message again by itself once the login resets", run: func() tea.Cmd {
			return wait(false, "The last message is sent again at "+when+".")
		}})
	}
	acts = append(acts, loginAction{label: "Retry now", does: "send the last message again with this login", run: func() tea.Cmd {
		return wait(true, "Retrying the last message now.")
	}})
	if event.GetRotation() {
		acts = append(acts,
			loginAction{label: "Use the rotation for this session", does: "this session moves on to the next login of the rotation", run: func() tea.Cmd {
				return setLogin(&agentv1.SetLoginRequest{SessionId: id, Provider: event.GetProvider(), OnExhausted: providerconfig.Rotate, Retry: true},
					"This session now uses the rotation.")
			}},
			loginAction{label: "Always rotate for " + event.GetProvider(), does: "from now on, move to the next login of the rotation without asking", run: func() tea.Cmd {
				return tui.Async(func() func() tea.Cmd {
					_, err := c.desk.settings.Set(kit.Env.Ctx, &configv1.SetRequest{Profile: c.on(), Value: providerconfig.Rotate,
						Key: providerconfig.Key + "." + event.GetProvider() + "." + providerconfig.OnExhausted})
					return func() tea.Cmd {
						if err != nil {
							return hub.Fail(err.Error())
						}
						return wait(true, "This session now uses the rotation.")
					}
				})
			}})
	}
	acts = append(acts, loginAction{label: "Add a login", does: "sign in to another account of " + event.GetProvider(), run: func() tea.Cmd {
		return tea.Batch(kit.WM.DismissCard(hub), kit.Open(providerapp.Screen, profile.Join(c.on(), providerapp.LoginsOf+event.GetProvider())))
	}})
	banner := []string{"The login " + name + " of " + event.GetProvider() + " has run out."}
	if reason := strings.TrimSpace(event.GetReason()); reason != "" {
		banner = append(banner, reason)
	}
	if when != "" {
		banner = append(banner, "It resets at "+when+".")
	}
	list := cards.NewList(kit.Env, cards.ListSpec{
		Title:   name + " ran out",
		Glyph:   kit.Env.Theme.Glyphs.Ext,
		Key:     "tui:login:exhausted",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: "Action"}, {Title: "What it does", Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(acts))
			for i, act := range acts {
				rows[i] = []string{act.label, render.Dim(act.does, kit.Env.Theme)}
			}
			return rows
		},
		Banner: func() string { return strings.Join(banner, " · ") },
		Hints:  func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.use", Label: "do it"}} },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return acts[i].run() },
		},
	})
	hub = kit.Hub("tui:login:exhausted", list)
	hub.Stick()
	return kit.WM.ShowOverlay(hub)
}
