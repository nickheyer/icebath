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

// One thing the question of an account that ran out offers: its words, what it does, the word on its button and the doing
type loginAction struct {
	label, does, verb string
	run               func() tea.Cmd
}

// Question shown once the account the session is held to reached its limit: switch to an account ready now, wait for its reset, retry at
// once, let the session or the provider take turns between accounts, or add an account
func (c *chat) exhausted(event *agentv1.LoginExhausted) tea.Cmd {
	kit := c.kit
	t := kit.Env.Theme
	id := cmp.Or(event.GetSessionId(), c.id)
	// The provider by its name in the profile for the server, by the name a person knows it for the words
	name, at, title := event.GetLogin().GetName(), event.GetProvider(), cmp.Or(event.GetProviderTitle(), event.GetProvider())
	when := ""
	if event.GetResets() != nil {
		when = event.GetResets().AsTime().Local().Format("15:04")
	}
	var hub *cards.Hub
	settle := func(done string) tea.Cmd { return tea.Batch(kit.WM.DismissCard(hub), wm.Notify(done, wm.Success)) }
	// Through the server's one function for the account a session is held to, saying what it says, then what else was done
	setLogin := func(request *agentv1.SetLoginRequest, then string) tea.Cmd {
		hub.Busy("Switching accounts…")
		return tui.Async(func() func() tea.Cmd {
			response, err := c.desk.agents.SetLogin(kit.Env.Ctx, request)
			return func() tea.Cmd {
				switch {
				case err != nil:
					return hub.Fail(tui.Reason(err))
				case response.GetWarning() != "":
					return hub.Fail(response.GetWarning())
				}
				// What else was done is said only when it was: the last message goes again when the server sent it again
				if !response.GetRetried() {
					return settle(response.GetSaid())
				}
				return settle(strings.TrimSpace(response.GetSaid() + " " + then))
			}
		})
	}
	wait := func(now bool, done string) tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			response, err := c.desk.agents.WaitLogin(kit.Env.Ctx, &agentv1.WaitLoginRequest{SessionId: id, Provider: at, Now: now})
			return func() tea.Cmd {
				switch {
				case err != nil:
					return hub.Fail(tui.Reason(err))
				case response.GetState() == "idle":
					return hub.Fail("The provider did not say when the account resets. Retry now, or switch to another account.")
				}
				// The time the server armed the retry for, where it differs from the reset the question named
				if at := response.GetRetryAt(); !now && at != nil {
					return settle("Your last message is sent again at " + at.AsTime().Local().Format("15:04") + ".")
				}
				return settle(done)
			}
		})
	}
	var acts []loginAction
	for _, ready := range event.GetReady() {
		login, other := ready.GetId(), cmp.Or(ready.GetName(), ready.GetId())
		acts = append(acts, loginAction{"Switch to " + other, "Use this account for the session and send your last message again", "switch", func() tea.Cmd {
			return setLogin(&agentv1.SetLoginRequest{SessionId: id, Provider: at, Login: login, Retry: true}, "Your last message is sent again.")
		}})
	}
	if when != "" {
		acts = append(acts, loginAction{"Wait until " + when, "Send your last message again by itself once the account resets", "wait", func() tea.Cmd {
			return wait(false, "Your last message is sent again at "+when+".")
		}})
	}
	acts = append(acts, loginAction{"Retry now", "Send your last message again with this account", "retry", func() tea.Cmd {
		return wait(true, "Your last message is being sent again.")
	}})
	if event.GetRotation() {
		acts = append(acts,
			loginAction{"Take turns for this session", "Move this session on to the next of your accounts that take turns", "take turns", func() tea.Cmd {
				return setLogin(&agentv1.SetLoginRequest{SessionId: id, Provider: at, OnExhausted: providerconfig.Rotate, Retry: true},
					"Your last message is sent again.")
			}},
			loginAction{"Always move on at " + title, "Move on to the next account by itself from now on, in every chat", "move on", func() tea.Cmd {
				on := c.menuProfile()
				return tui.Async(func() func() tea.Cmd {
					_, err := c.desk.settings.Set(kit.Env.Ctx, &configv1.SetRequest{Profile: on, Value: providerconfig.Rotate,
						Key: providerconfig.Key + "." + at + "." + providerconfig.OnExhausted})
					return func() tea.Cmd {
						if err != nil {
							return hub.Fail(tui.Reason(err))
						}
						return wait(true, "When an account runs out is now Move on for "+title+". Your last message is sent again.")
					}
				})
			}})
	}
	acts = append(acts, loginAction{"Add an account", "Sign in to another account at " + title, "add", func() tea.Cmd {
		return tea.Batch(kit.WM.DismissCard(hub), kit.Open(providerapp.Screen, profile.Join(c.menuProfile(), providerapp.LoginsOf+at)))
	}})
	intro := []string{"Your account " + name + " at " + title + " reached its limit."}
	if reason := strings.TrimSpace(event.GetReason()); reason != "" {
		intro = append(intro, reason)
	}
	if when != "" {
		intro = append(intro, "It resets at "+when+".")
	}
	var list *cards.List
	list = cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Account limit reached",
		Glyph:   t.Glyphs.Ext,
		Key:     "tui:login:exhausted",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{strings.Join(intro, " ")} },
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(acts))
			for i, act := range acts {
				rows[i] = []string{act.label, render.Dim(act.does, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			if list.Shows(1) {
				return nil
			}
			return []string{acts[i].does + "."}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: acts[i].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return acts[i].run() },
		},
	})
	list.ToggleDetail()
	hub = kit.Hub("tui:login:exhausted", list)
	hub.Stick()
	return kit.WM.ShowOverlay(hub)
}
