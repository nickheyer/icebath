package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Pause after the last edit before the draft's candidates are asked for
const completionDebounce = 60 * time.Millisecond

// Gateway completion of the draft of the composer taking typing, the draft last asked for and whether input is held elsewhere
type composerCompletion struct {
	asked   string
	blocked bool
	// Composer the candidates are for, the session card's or a room card's
	target *cards.Composer
}

// Debounce of one draft ending, the request it settled on and the composer it was typed in
type completionDueMsg struct {
	value  string
	req    completionRequest
	target *cards.Composer
}

// Composer of this session that takes typing now, the session card's or that of a room card it opened, nil while an overlay holds the input, a
// dialog sits in the session card or another card has the focus
func (a *app) typingComposer() *cards.Composer {
	if a.wm.Overlay() != nil {
		return nil
	}
	switch f := a.wm.Focused().(type) {
	case *sessionCard:
		if f == a.card && a.wm.Sheet(a.card) == nil {
			return a.card.composer
		}
	case *roomCard:
		if f.app == a {
			return f.composer
		}
	}
	return nil
}

// Whether text input is held, a prompt of the session open or no composer of the session focused
func (a *app) completionBlocked() bool {
	o := a.store.overlay
	return o.Agents || o.Approval != nil || o.Clarify != nil || o.Confirm != nil || o.Connection != nil || o.Secret != nil ||
		o.Sudo != nil || o.VaultCode != nil || o.VaultSaveLogin != nil || o.VaultUnlock != nil || a.typingComposer() == nil
}

// Asks for the candidates of the draft as it now reads, clearing them while input is held or nothing applies
func (a *app) syncCompletion() tea.Cmd {
	c := &a.completion
	target := a.typingComposer()
	if c.target != nil && c.target != target {
		c.target.Offer("", nil, 0)
		c.asked = ""
	}
	c.target = target
	if a.completionBlocked() {
		if !c.blocked {
			c.blocked, c.asked = true, ""
			a.card.composer.Offer("", nil, 0)
		}
		return nil
	}
	c.blocked = false
	value := target.Value()
	if value == c.asked {
		return nil
	}
	c.asked = value
	req, ok := a.store.completionRequestForInput(value, false)
	if !ok {
		target.Offer(value, nil, 0)
		return nil
	}
	return tea.Tick(completionDebounce, func(time.Time) tea.Msg { return completionDueMsg{value: value, req: req, target: target} })
}

// Whether a completion answer still fits: the composer it was for takes typing and holds the text it was asked for
func (a *app) completionCurrent(m completionDueMsg) bool {
	return a.completion.target == m.target && m.target != nil && m.target.Value() == m.value
}

// Sends the settled request and shows the answer while the draft reads the same, filling what the gateway left out
func (a *app) askCompletion(m completionDueMsg) tea.Cmd {
	if a.completion.asked != m.value || !a.completionCurrent(m) {
		return nil
	}
	show := func(shown []cards.Offer, at int) tea.Cmd {
		if a.completionCurrent(m) {
			m.target.Offer(m.value, shown, at)
		}
		return nil
	}
	if m.req.Path != nil {
		return call(a.gw, tuigateway.CompletePath, *m.req.Path, func(r *tuigateway.CompletionItemsResult, err error) tea.Cmd {
			if err != nil {
				return show(unavailableOffer(err), m.req.ReplaceFrom)
			}
			return show(offersOf(r.Items, false), m.req.ReplaceFrom)
		})
	}
	params := *m.req.Slash
	if sid := a.store.ui.Sid; sid != "" {
		params.SessionID = &sid
	}
	return call(a.gw, tuigateway.CompleteSlash, params, func(r *tuigateway.CompleteSlashResult, err error) tea.Cmd {
		if err != nil {
			return show(unavailableOffer(err), m.req.ReplaceFrom)
		}
		offers := offersOf(r.Items, m.req.SkillsOnly)
		if m.req.SkillsOnly {
			return show(offers, m.req.ReplaceFrom)
		}
		replace := 1
		if r.ReplaceFrom != nil {
			replace = int(*r.ReplaceFrom)
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

// Tab on a word the popup does not open for by itself, a path at the end of the draft: one candidate goes straight in, several put in what they
// share and open the list
func (a *app) tabComplete(target *cards.Composer) tea.Cmd {
	if target == nil {
		return nil
	}
	value := target.Value()
	req, ok := a.store.completionRequestForInput(value, true)
	if !ok || req.Path == nil {
		return nil
	}
	return call(a.gw, tuigateway.CompletePath, *req.Path, func(r *tuigateway.CompletionItemsResult, err error) tea.Cmd {
		if target.Value() != value {
			return nil
		}
		if err != nil {
			target.Offer(value, unavailableOffer(err), req.ReplaceFrom)
			return nil
		}
		offers := offersOf(r.Items, false)
		switch len(offers) {
		case 0:
			return nil
		case 1:
			return target.SetValue(applyCompletion(value, offers[0].Text, req.ReplaceFrom))
		}
		texts := make([]string, len(offers))
		for i, o := range offers {
			texts[i] = o.Text
		}
		var cmd tea.Cmd
		if shared := applyCompletion(value, commonPrefix(texts), req.ReplaceFrom); len(shared) > len(value) && strings.HasPrefix(shared, value) {
			cmd = target.SetValue(shared)
		}
		target.Offer(target.Value(), offers, req.ReplaceFrom)
		return cmd
	})
}

// Longest start every text shares
func commonPrefix(texts []string) string {
	if len(texts) == 0 {
		return ""
	}
	prefix := []rune(texts[0])
	for _, t := range texts[1:] {
		runes := []rune(t)
		n := 0
		for n < len(prefix) && n < len(runes) && prefix[n] == runes[n] {
			n++
		}
		prefix = prefix[:n]
	}
	return string(prefix)
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

// Puts an accepted candidate into the composer taking typing, or sends the draft when the candidate changes nothing or names a local command
func (a *app) applyOffer(text string, replace int) tea.Cmd {
	target := a.completion.target
	if target == nil {
		target = a.card.composer
	}
	value := target.Value()
	if next, ok := completionToApplyOnSubmit(value, &text, replace, func(name string) bool { return a.findSlashCommand(name) != nil }); ok {
		return target.SetValue(next)
	}
	return target.Send()
}
