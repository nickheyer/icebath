package tui

import (
	"context"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the list of languages
const languageContext = "tui.language"

// Keys of the list of languages, registered with the keymap of the app
func registerLanguageKeys(km *keys.Map) {
	if keys.ContextDocs[languageContext] == "" {
		if err := keys.AddContext(languageContext, "languages of the UI"); err != nil {
			panic(err)
		}
	}
	km.Register(keys.Action{ID: "tui.language.use", Context: languageContext, Label: "use", Doc: "show the UI in the language", Default: []string{"enter"}})
}

// Language commands
func (a *app) languageCommands() []slashCommand {
	return []slashCommand{
		{name: "language", aliases: []string{"lang"}, help: "choose the language of the UI", usage: "[id]", run: func(r *slashRun) tea.Cmd {
			value := trimJS(r.arg)
			if value == "" {
				return tea.Batch(r.ran(), a.openLanguagePicker(r))
			}
			return ask(r.rpc, r.sys, tuigateway.I18nLanguages, tuigateway.I18nLanguagesParams{Resolve: value}, func(res *tuigateway.I18nLanguagesResult) tea.Cmd {
				switch {
				case res == nil:
					return nil
				case res.Resolved == "":
					ids := make([]string, len(res.Languages))
					for i, l := range res.Languages {
						ids[i] = l.ID
					}
					r.sys(tr("slashCmd.session.language.unknown", value, strings.Join(ids, ", ")))
					return nil
				}
				return a.setLanguage(r, res.Resolved)
			})
		}},
	}
}

// Saves the display language with the owner of the config and loads its pack at once
func (a *app) setLanguage(r *slashRun, value string) tea.Cmd {
	return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "language", Value: tuigateway.TextValue(value)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
		if res == nil || res.Value == nil {
			return nil
		}
		id, warning, gw, idle := res.Value.Text, res.Warning, a.gw, tr("status.ready")
		return func() tea.Msg {
			syncTUILocale(func(method string, params map[string]any) (any, error) {
				return gw.request(context.Background(), method, params, 0)
			}, id)
			return asyncDoneMsg{run: func() tea.Cmd {
				if a.store.ui.Status == idle {
					a.store.ui.Status = tr("status.ready")
				}
				a.card.view.invalidate()
				if !r.stale() {
					r.sys(tr("slashCmd.session.language.switched", id))
					if warning != "" {
						r.sys(warning)
					}
				}
				return nil
			}}
		}
	})
}

// Opens the list of languages of the profile, the one in effect marked and English shown as covering this UI, whose own texts are English
func (a *app) openLanguagePicker(r *slashRun) tea.Cmd {
	return ask(r.rpc, r.sys, tuigateway.I18nLanguages, tuigateway.I18nLanguagesParams{}, func(res *tuigateway.I18nLanguagesResult) tea.Cmd {
		if res == nil || r.stale() {
			return nil
		}
		languages := res.Languages
		list := cards.NewList(a.cardEnv(), cards.ListSpec{
			Title: "language", Glyph: a.toolkit.Glyphs.Sessions, Key: "tui:language", Context: languageContext, Primary: "tui.language.use",
			Columns: []render.Column{{Title: ""}, {Title: "id"}, {Title: "language", Flex: 1}, {Title: "source"}, {Title: "covers"}},
			Rows: func() [][]string {
				out := make([][]string, len(languages))
				for i, l := range languages {
					mark := ""
					if l.ID == res.Current {
						mark = "●"
					}
					covers := l.Surfaces
					if l.ID == defaultLocale && !slices.Contains(covers, tuiSurface) {
						covers = append(slices.Clone(covers), tuiSurface)
					}
					out[i] = []string{mark, l.ID, l.Endonym, l.Source, strings.Join(covers, " ")}
				}
				return out
			},
			Empty: func() string { return tr("slashCmd.session.language.none") },
			Actions: map[string]func(l *cards.List, i int) tea.Cmd{
				"tui.language.use": func(l *cards.List, i int) tea.Cmd {
					return tea.Batch(wm.RequestDismissOf(l), a.setLanguage(r, languages[i].ID))
				},
			},
		})
		return a.wm.ShowOverlay(list)
	})
}
