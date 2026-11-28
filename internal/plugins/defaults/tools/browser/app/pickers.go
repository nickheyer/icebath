package app

import (
	"cmp"
	"strconv"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	browserv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1/browserv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Id of the choice that types a value the list lacks
const typeOwn = "\x00"

// Text a value held as YAML holds, empty for none
func held(ask tui.Ask, key string) string {
	var text string
	yaml.Unmarshal([]byte(ask.Values[key]), &text)
	return text
}

// Hands back the value picked: the text as YAML, null for what is in effect when nothing is picked
func done(ask tui.Ask, key, value string) tea.Cmd {
	if value == "" {
		return ask.Done(map[string]string{key: "null"})
	}
	return ask.Done(map[string]string{key: strconv.Quote(value)})
}

// Picker of the browser program: the bundled Chromium, each Chromium browser installed on this computer, or a program typed
func pickProgram(kit *tui.Kit, hub *cards.Hub, service browserv1connect.BrowserServiceClient, ask tui.Ask) tea.Cmd {
	key := cmp.Or(ask.Key, browserconfig.ExecutablePath)
	hub.Busy("Looking for the browsers on this computer…")
	return tui.Call(hub, func() (*browserv1.InstalledResponse, error) {
		return service.Installed(kit.Env.Ctx, &browserv1.InstalledRequest{})
	}, func(response *browserv1.InstalledResponse) tea.Cmd {
		hub.Quiet()
		choices := []tui.Choice{{ID: "", Title: "Bundled Chromium", Detail: "The Chromium that icebath downloads"}}
		if len(ask.First) > 0 {
			choices[0].Title = ask.First[0].Title
		}
		for _, program := range response.GetPrograms() {
			choices = append(choices, tui.Choice{ID: program.GetPath(), Title: program.GetTitle(), Detail: program.GetPath()})
		}
		choices = append(choices, tui.Choice{ID: typeOwn, Title: "Type its path", Detail: "A browser program this list lacks"})
		return kit.PickFrom(hub, cmp.Or(ask.Title, "Program"), ask.Intro, "choose", choices, held(ask, key), func(choice tui.Choice) tea.Cmd {
			if choice.ID != typeOwn {
				return done(ask, key, choice.ID)
			}
			field := cards.Field{Label: "Program", Value: held(ask, key), Path: cards.PathProgram,
				Help: "The whole path of a Chromium browser program, such as /usr/bin/google-chrome."}
			return kit.Ask(hub, "Program", field, "Save", func(text string) tea.Cmd { return done(ask, key, text) })
		})
	})
}

// Picker of the profile of your own browser the agent copies: the one it used last, or one of its profiles by the name it shows
func pickProfile(kit *tui.Kit, hub *cards.Hub, service browserv1connect.BrowserServiceClient, ask tui.Ask) tea.Cmd {
	key := cmp.Or(ask.Key, browserconfig.RealProfilePin)
	hub.Busy("Reading the profiles of your browser…")
	return tui.Call(hub, func() (*browserv1.InstalledResponse, error) {
		return service.Installed(kit.Env.Ctx, &browserv1.InstalledRequest{})
	}, func(response *browserv1.InstalledResponse) tea.Cmd {
		hub.Quiet()
		first := "Last one used"
		if len(ask.First) > 0 {
			first = ask.First[0].Title
		}
		choices := []tui.Choice{{ID: "", Title: first, Detail: "The profile your browser used last"}}
		for _, p := range response.GetProfiles() {
			detail := p.GetFolder()
			if p.GetLast() {
				detail += " · used last"
			}
			choices = append(choices, tui.Choice{ID: p.GetFolder(), Title: p.GetName(), Detail: detail})
		}
		intro := ask.Intro
		switch browser := response.GetBrowser(); {
		case browser == "":
			intro = append(intro, "Your default browser is none that icebath can copy a profile of.")
		case len(response.GetProfiles()) == 0:
			intro = append(intro, browser+" has no profiles on this computer yet.")
		default:
			intro = append(intro, "Profiles of "+browser+", your default browser.")
		}
		return kit.PickFrom(hub, cmp.Or(ask.Title, "Profile to copy"), intro, "choose", choices, held(ask, key), func(choice tui.Choice) tea.Cmd {
			return done(ask, key, choice.ID)
		})
	})
}
