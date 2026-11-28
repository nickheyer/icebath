package app

import (
	"cmp"
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	generatev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1/generatev1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Titles of the models of each service as last read, by the kind of model, the service and the model, so a row says a model by name
type titles struct {
	mu   sync.Mutex
	seen map[string]string
}

func newTitles() *titles {
	return &titles{seen: map[string]string{}}
}

// Keeps the titles of the models a service listed
func (t *titles) keep(kind, service string, response *generatev1.ModelsResponse) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range response.GetModels() {
		if m.GetTitle() != "" {
			t.seen[kind+"\x00"+service+"\x00"+m.GetId()] = m.GetTitle()
		}
	}
}

// Title of a model, empty while none was read
func (t *titles) of(kind, service, model string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen[kind+"\x00"+service+"\x00"+model]
}

// Mark of the choice that types a model in place of picking one
const typeOwn = "\x00"

// Picker of the model of a service of the kind, the service named after the kind in the setting's Picks: the models the service lists with
// what each is good at, its default marked as such and picked as no model at all, and a row that types a model the list lacks
func modelPicker(service generatev1connect.GenerateServiceClient, k kind, names *titles) tui.Picker {
	return tui.Picker{
		Open: func(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd {
			return pickModel(kit, hub, service, k, names, ask)
		},
		Shown: func(ask tui.Ask) string {
			return names.of(k.models, ask.Of, held(ask))
		},
	}
}

// Model the asked setting holds, empty for its default
func held(ask tui.Ask) string {
	var model string
	if yaml.Unmarshal([]byte(ask.Values[ask.Key]), &model) != nil {
		return ""
	}
	return strings.TrimSpace(model)
}

// Lists the models of the service the ask names to pick one, read as the list opens
func pickModel(kit *tui.Kit, hub *cards.Hub, service generatev1connect.GenerateServiceClient, k kind, names *titles, ask tui.Ask) tea.Cmd {
	hub.Busy("Reading the models…")
	return tui.Async(func() func() tea.Cmd {
		response, err := service.Models(kit.Env.Ctx, &generatev1.ModelsRequest{Profile: ask.Profile, Kind: k.proto, Service: ask.Of})
		return func() tea.Cmd {
			hub.Quiet()
			if err == nil {
				names.keep(k.models, ask.Of, response)
			}
			keep := func(model string) tea.Cmd {
				value := "null"
				if model != "" && model != response.GetDefault() {
					value = strconv.Quote(model)
				}
				return ask.Done(map[string]string{ask.Key: value})
			}
			current := cmp.Or(held(ask), response.GetDefault())
			var choices []tui.Choice
			for _, m := range response.GetModels() {
				var facts []string
				if m.GetId() == response.GetDefault() {
					facts = append(facts, "The default")
				}
				choices = append(choices, tui.Choice{ID: m.GetId(), Title: cmp.Or(m.GetTitle(), m.GetId()),
					Detail: strings.Join(filled(append(facts, m.GetStrengths(), m.GetSpeed(), m.GetPrice())...), " · ")})
			}
			why := "One this list does not have"
			switch {
			case err != nil:
				why = "The list could not be read. " + strs.Full(tui.Reason(err))
			case len(choices) == 0:
				why = "The service lists no models now"
			}
			choices = append(choices, tui.Choice{ID: typeOwn, Title: "Type a model name", Detail: why})
			return kit.PickFrom(hub, ask.Title, ask.Intro, "choose", choices, current, func(choice tui.Choice) tea.Cmd {
				if choice.ID != typeOwn {
					return keep(choice.ID)
				}
				field := cards.Field{Label: "Model", Value: held(ask), Select: true, Help: "The name of the model as the service knows it."}
				return kit.Ask(hub, ask.Title, field, "Save", keep)
			})
		}
	})
}

// Texts that are not empty
func filled(texts ...string) []string {
	var out []string
	for _, text := range texts {
		if text = strings.TrimSpace(text); text != "" {
			out = append(out, text)
		}
	}
	return out
}
