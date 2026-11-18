package tui

import (
	"maps"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Pause after the last edit before the draft's candidates are asked for
const completionDebounce = 60 * time.Millisecond

// Gateway completion of the draft, the draft last asked for and whether input is held by an overlay
type composerCompletion struct {
	asked   string
	blocked bool
}

// Debounce of one draft ending, the request it settled on
type completionDueMsg struct {
	value string
	req   completionRequest
}

// Whether text input is held, an overlay or prompt open or the prompt window not focused
func (a *app) completionBlocked() bool {
	o := a.store.overlay
	return o.Agents || o.Approval != nil || o.Clarify != nil || o.Confirm != nil || o.Connection != nil || o.Secret != nil || o.Sessions ||
		o.Sudo != nil || o.VaultCode != nil || o.VaultSaveLogin != nil || o.VaultUnlock != nil || a.wm.Overlay() != nil || a.wm.Focused() != wm.Card(a.card)
}

// Asks for the candidates of the draft as it now reads, clearing them while input is held or nothing applies
func (a *app) syncCompletion() tea.Cmd {
	c := &a.completion
	if a.completionBlocked() {
		if !c.blocked {
			c.blocked, c.asked = true, ""
			a.card.composer.Offer(nil, 0)
		}
		return nil
	}
	c.blocked = false
	value := a.card.composer.Value()
	if value == c.asked {
		return nil
	}
	c.asked = value
	req, ok := a.store.completionRequestForInput(value)
	if !ok {
		a.card.composer.Offer(nil, 0)
		return nil
	}
	return tea.Tick(completionDebounce, func(time.Time) tea.Msg { return completionDueMsg{value: value, req: req} })
}

// Sends the settled request and shows the answer while the draft reads the same, filling what the gateway left out
func (a *app) askCompletion(m completionDueMsg) tea.Cmd {
	if a.completion.asked != m.value {
		return nil
	}
	slash := m.req.Method == "complete.slash"
	params := maps.Clone(m.req.Params)
	if sid := a.store.ui.Sid; slash && sid != "" {
		params["session_id"] = sid
	}
	return gatewayCall(a.gw, m.req.Method, params, func(raw any, err error) tea.Cmd {
		if a.completion.asked != m.value {
			return nil
		}
		if err != nil {
			a.card.composer.Offer(unavailableOffer(err), m.req.ReplaceFrom)
			return nil
		}
		r := asRPCResult(raw)
		items, _ := r["items"].([]any)
		var offers []cards.Offer
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if item == nil || (slash && m.req.SkillsOnly && item["kind"] != "skill") {
				continue
			}
			offers = append(offers, cards.Offer{Text: stringOr(item["text"], ""), Display: stringOr(item["display"], ""), Meta: stringOr(item["meta"], "")})
		}
		if !slash || m.req.SkillsOnly {
			a.card.composer.Offer(offers, m.req.ReplaceFrom)
			return nil
		}
		replace := 1
		if n, isNum := r["replace_from"].(float64); isNum {
			replace = int(n)
		}
		show := func(shown []cards.Offer, at int) tea.Cmd {
			if a.completion.asked == m.value {
				a.card.composer.Offer(shown, at)
			}
			return nil
		}
		if !strings.Contains(m.value, " ") {
			return show(a.mergeNameOffers(m.value, a.mergeWidgetOffers(m.value, offers)), replace)
		}
		if len(offers) > 0 {
			return show(offers, replace)
		}
		return a.argOffers(m.value, show)
	})
}

// Widgets ranked against a command name being typed, added after the gateway's candidates they are not already among
func (a *app) mergeWidgetOffers(input string, offers []cards.Offer) []cards.Offer {
	if strings.Contains(input, " ") {
		return offers
	}
	ranked := rankSlashItems(a.widgets.list(), input, func(w Widget) slashScoreItem { return slashScoreItem{Description: w.Help, ID: w.ID} })
	for _, w := range ranked {
		text := "/" + w.ID
		known := false
		for _, o := range offers {
			known = known || o.Text == text
		}
		if !known {
			offers = append(offers, cards.Offer{Text: text, Display: text, Meta: w.Help})
		}
	}
	return offers
}

// Puts an accepted candidate in, or sends the draft when the candidate changes nothing or names a local command
func (a *app) applyOffer(text string, replace int) tea.Cmd {
	value := a.card.composer.Value()
	if next, ok := completionToApplyOnSubmit(value, &text, replace, func(name string) bool { return a.findSlashCommand(name) != nil }); ok {
		return a.card.composer.SetValue(next)
	}
	return a.card.composer.Action("composer.send")
}
