package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Row of a webhook's page: one of its fields as an option, or something done to it
type entry struct {
	setting *configv1.Setting
	act     *act
}

// Opens the page of a webhook over the page of the area: its state as a sentence, then its address, events, name, the tools it posts for
// while one of its events is about a tool, its time limit and its secret with the variable that can hold it, each changed as Settings
// changes it, then a test and removing it
func (p *page) webhookPage(key string) tea.Cmd {
	t := p.kit.Env.Theme
	var lines []entry
	var view *cards.List
	title := key
	if webhook := p.webhook(key); webhook != nil {
		title = webhook.GetLabel()
	}
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Terminal,
		Key:     Screen + ":webhook:" + key,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			if webhook := p.webhook(key); webhook != nil {
				return p.webhookState(webhook)
			}
			return nil
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			lines = p.webhookLines(key)
			cells := make([][]string, len(lines))
			for i, e := range lines {
				if e.setting != nil {
					cells[i] = p.options.Cells(e.setting)
					continue
				}
				cells[i] = []string{e.act.title, "", ""}
			}
			return cells
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(lines) {
				return nil
			}
			if e := lines[i]; e.setting != nil {
				return p.options.Detail(e.setting, view.Cut(i, 2))
			}
			return []string{lines[i].act.about}
		},
		Empty: func() string { return "That webhook is no longer there." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(lines) {
				return nil
			}
			if e := lines[i]; e.setting != nil {
				return p.options.Hints(e.setting)
			}
			return []wm.Hint{{Action: "hub.use", Label: lines[i].act.verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if e := lines[i]; e.setting != nil {
					return p.options.Change(e.setting)
				}
				return lines[i].act.run(l)
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if e := lines[i]; e.setting != nil {
					return p.options.Reset(l, e.setting)
				}
				return nil
			},
		},
		OnFocus: func(*cards.List) tea.Cmd { return p.load() },
	})
	view.ToggleDetail()
	p.options.Watch(view)
	p.views.Add(view)
	return p.hub.Push(view)
}

// The state of a webhook as a sentence: how many events it posts, and whether its posts are signed and why not when a variable was meant to
// sign them
func (p *page) webhookState(webhook *hooksv1.Webhook) []string {
	t := p.kit.Env.Theme
	said := "Posts " + strs.Count(len(webhook.GetEvents()), "event")
	switch {
	case webhook.GetSigned() && webhook.GetSecretEnv() != "":
		return []string{said + ", each signed with the secret in " + webhook.GetSecretEnv() + "."}
	case webhook.GetSigned():
		return []string{said + ", each signed with your secret."}
	case webhook.GetUnsigned() != "":
		return []string{said + ".", render.Warm("Its posts are not signed, because "+webhook.GetUnsigned()+".", t)}
	}
	return []string{said + ". Its posts are not signed."}
}

// Rows of a webhook's page as it stands now: its fields as options, the tools only while one of its events is about a tool, then the test
// and removing it
func (p *page) webhookLines(key string) []entry {
	webhook := p.webhook(key)
	if webhook == nil {
		return nil
	}
	var out []entry
	everyday, advanced := p.options.Entry(shellconfig.Outbound, key)
	for _, setting := range append(everyday, advanced...) {
		if strings.HasSuffix(setting.GetKey(), "."+shellconfig.Matcher) && !webhook.GetTool() {
			continue
		}
		out = append(out, entry{setting: setting})
	}
	return append(out,
		entry{act: &act{title: "Test", about: "Posts a sample event to its address now, signed as its posts are, and says how the address answered.",
			verb: "test", run: func(*cards.List) tea.Cmd { return p.testWebhook(key) }}},
		entry{act: &act{title: "Remove", about: "Takes it out of icebath. The address is no longer told what the agent does.", verb: "remove",
			run: func(l *cards.List) tea.Cmd { return p.removeWebhook(l, p.webhook(key)) }}})
}

// Posts a sample event to the webhook's address and says how it answered
func (p *page) testWebhook(key string) tea.Cmd {
	webhook := p.webhook(key)
	if webhook == nil {
		return nil
	}
	p.hub.Busy("Posting a sample to " + webhook.GetUrl() + "…")
	return tui.Call(p.hub, func() (*hooksv1.TestWebhookResponse, error) {
		return p.service.TestWebhook(p.kit.Env.Ctx, &hooksv1.TestWebhookRequest{Profile: p.profile, Key: key})
	}, func(response *hooksv1.TestWebhookResponse) tea.Cmd {
		if !response.GetOk() {
			return p.hub.Fail(response.GetSaid())
		}
		return p.hub.Done(response.GetSaid())
	})
}
