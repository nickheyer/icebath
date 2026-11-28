package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	hooksconfig "github.com/nickheyer/icebath/internal/hooks/config"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1/hooksv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Pages inside Hooks that a search of Settings finds: the page of each hook under its event, of each webhook, of long output, and testing
// an event and checking every hook while there are hooks
func hookPages(kit *tui.Kit, service hooksv1connect.HooksServiceClient, name string) []tui.Page {
	listing, err := service.List(kit.Env.Ctx, &hooksv1.ListRequest{Profile: name})
	if err != nil {
		return nil
	}
	var pages []tui.Page
	for _, hook := range listing.GetHooks() {
		point := shellconfig.PointOf(hook.GetEvent())
		pages = append(pages, tui.Page{Title: hook.GetCommand(), Trail: []string{point.Title}, About: "A hook that " + strings.ToLower(point.About[:1]) +
			point.About[1:], Words: "hook " + hook.GetEvent(), Argument: hookOf + hook.GetEvent() + " " + hook.GetCommand()})
	}
	for _, webhook := range listing.GetWebhooks() {
		pages = append(pages, tui.Page{Title: webhook.GetLabel(), Trail: []string{webhooksGroup}, About: "A webhook that posts to " + webhook.GetUrl(),
			Words: "webhook " + webhook.GetUrl() + " " + eventWords(webhook.GetEvents()), Argument: webhookOf + webhook.GetKey(),
			Keys: []string{shellconfig.Outbound + "." + webhook.GetKey() + "."}})
	}
	if len(listing.GetHooks()) > 0 {
		pages = append(pages, tui.Page{Title: "Test an event", About: "Run every hook of an event once on a sample of it", Argument: argTest},
			tui.Page{Title: "Check every hook", About: "Whether each hook can run, is allowed and answers well", Words: "doctor",
				Argument: argCheck})
	}
	return append(pages, tui.Page{Title: "Long output", About: "Where long text from a hook is saved and what is kept",
		Argument: sectionOf + hooksconfig.SpillKey, Keys: []string{hooksconfig.SpillKey + "."}})
}

// Opens a section of the options of the area over its page: its everyday settings, and its advanced ones in place while few, else behind
// a row of their own
func (p *page) sectionPage(key string) tea.Cmd {
	t := p.kit.Env.Theme
	info := p.options.Info(key)
	if info == nil {
		return p.hub.Fail("The Hooks page has no section " + key + ".")
	}
	var shown []*configv1.Setting
	var advanced bool
	var view *cards.List
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   info.GetTitle(),
		Glyph:   t.Glyphs.Settings,
		Key:     Screen + ":" + key,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{strs.Full(info.GetDescription())} },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			everyday, rare := p.options.Own(key)
			shown = everyday
			_, _, advanced = p.options.OwnAdvanced(key)
			if !advanced {
				shown = append(shown, rare...)
			}
			cells := make([][]string, 0, len(shown)+1)
			for _, setting := range shown {
				cells = append(cells, p.options.Cells(setting))
			}
			if advanced {
				row, _, _ := p.options.OwnAdvanced(key)
				cells = append(cells, row)
			}
			return cells
		},
		Detail: func(i int) []string {
			if i >= len(shown) {
				return []string{"Settings that few people change."}
			}
			return p.options.Detail(shown[i], view.Cut(i, 2))
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return nil
			case i >= len(shown):
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			}
			return p.options.Hints(shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i >= len(shown) {
					_, open, _ := p.options.OwnAdvanced(key)
					return open()
				}
				return p.options.Change(shown[i])
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(shown) {
					return nil
				}
				return p.options.Reset(l, shown[i])
			},
		},
	})
	view.ToggleDetail()
	p.options.Watch(view)
	return p.hub.Push(view)
}
