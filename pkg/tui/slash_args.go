package tui

import (
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// How long values read from the gateway answer completion before they are read again
const argValuesTTL = 15 * time.Second

// Kinds of argument value, each read from one gateway method
const (
	argModels    = "models"
	argProviders = "providers"
	argSessions  = "sessions"
	argProfiles  = "profiles"
	argPaths     = "paths"
)

// Values of one kind as last read, with when
type argCacheEntry struct {
	at     time.Time
	offers []cards.Offer
}

// Argument being typed in a slash draft, the command, the finished words and the partial word
type slashArg struct {
	name    string
	done    []string
	partial string
}

// Command, finished argument words and partial word of a draft with a space after the command
func parseSlashArg(input string) (slashArg, bool) {
	if !strings.HasPrefix(input, "/") || !strings.Contains(input, " ") {
		return slashArg{}, false
	}
	words := strings.Fields(input[1:])
	if len(words) == 0 {
		return slashArg{}, false
	}
	arg := slashArg{name: strings.ToLower(words[0]), done: words[1:]}
	if !strings.HasSuffix(input, " ") && len(arg.done) > 0 {
		arg.partial, arg.done = arg.done[len(arg.done)-1], arg.done[:len(arg.done)-1]
	}
	return arg, true
}

// Finished words that are not flags or flag values
func positional(done []string, valued map[string]bool) []string {
	var out []string
	for i := 0; i < len(done); i++ {
		switch {
		case valued[done[i]]:
			i++
		case strings.HasPrefix(done[i], "-"):
		default:
			out = append(out, done[i])
		}
	}
	return out
}

// Kind of value the argument takes, and whether an empty partial lists nothing because the bare command opens a picker
func argKind(a slashArg) (string, bool, bool) {
	last := ""
	if n := len(a.done); n > 0 {
		last = a.done[n-1]
	}
	if strings.HasPrefix(a.partial, "-") {
		return "", false, false
	}
	switch a.name {
	case "model":
		if last == "--provider" {
			return argProviders, false, true
		}
		if last == "--reasoning" {
			return "", false, false
		}
		return argModels, true, len(positional(a.done, map[string]bool{"--provider": true, "--reasoning": true})) == 0
	case "sessions":
		return argSessions, true, len(a.done) == 0
	case "export":
		if last == "-o" {
			return argPaths, false, true
		}
		return argProfiles, false, len(positional(a.done, map[string]bool{"-o": true})) == 0
	case "import":
		return argPaths, false, len(positional(a.done, nil)) == 0
	case "image":
		return argPaths, false, len(a.done) == 0
	case "save":
		return argPaths, false, len(a.done) == 1
	case "diff":
		return argPaths, false, true
	}
	return "", false, false
}

// Offers whose text or display starts with the partial, case folded
func matchOffers(offers []cards.Offer, partial string) []cards.Offer {
	want := strings.ToLower(partial)
	var out []cards.Offer
	for _, o := range offers {
		if strings.HasPrefix(strings.ToLower(o.Text), want) || strings.HasPrefix(strings.ToLower(o.Display), want) {
			out = append(out, o)
		}
	}
	return out
}

// Values of the local command's own list for its first argument, or for a later one when the command lists those
func localArgOffers(c *slashCommand, a slashArg) []cards.Offer {
	if c == nil {
		return nil
	}
	var values []string
	switch {
	case len(a.done) == 0 && c.suggest != nil:
		values = c.suggest(a.partial)
	case len(a.done) > 0 && c.suggestAfter != nil:
		values = c.suggestAfter(a.done)
	}
	var out []cards.Offer
	for _, v := range values {
		out = append(out, cards.Offer{Text: v, Display: v, Meta: c.helpText()})
	}
	return matchOffers(out, a.partial)
}

// Offers of one value kind from the gateway, cached while fresh, or the failure when the read fails
func (a *app) argValues(kind string, then func([]cards.Offer, error) tea.Cmd) tea.Cmd {
	// Keyed by session too, the values depending on it; the session list is read every time, a session opened or ended a moment ago counting
	cacheKey := kind + "\x00" + a.store.ui.Sid
	if e, ok := a.argCache[cacheKey]; ok && kind != argSessions && a.store.now().Sub(e.at) < argValuesTTL {
		return then(e.offers, nil)
	}
	keep := func(offers []cards.Offer) tea.Cmd {
		if a.argCache == nil {
			a.argCache = map[string]argCacheEntry{}
		}
		a.argCache[cacheKey] = argCacheEntry{at: a.store.now(), offers: offers}
		return then(offers, nil)
	}
	switch kind {
	case argModels, argProviders:
		options := tuigateway.ModelOptionsParams{SessionID: a.store.ui.Sid}
		return call(a.gw, tuigateway.ModelOptions, options, func(r *tuigateway.ModelOptionsResult, err error) tea.Cmd {
			if err != nil {
				return then(nil, err)
			}
			return keep(modelArgOffers(r, kind))
		})
	case argSessions:
		return requestSessions(a.gw, true, func(sessions []listedSession, err error) tea.Cmd {
			if err != nil {
				return then(nil, err)
			}
			return keep(sessionArgOffers(sessions))
		})
	case argProfiles:
		return call(a.gw, tuigateway.ProfilesList, tuigateway.ProfilesListParams{}, func(r *tuigateway.ProfilesListResult, err error) tea.Cmd {
			if err != nil {
				return then(nil, err)
			}
			return keep(profileArgOffers(r))
		})
	}
	return then(nil, nil)
}

// Models of the providers holding credentials, or every provider slug, as the gateway's model picker lists them
func modelArgOffers(r *tuigateway.ModelOptionsResult, kind string) []cards.Offer {
	var out []cards.Offer
	for _, p := range r.Providers {
		slug, name := p.Slug, p.Name
		if name == "" {
			name = slug
		}
		if kind == argProviders {
			if slug != "" {
				out = append(out, cards.Offer{Text: slug, Display: slug, Meta: name})
			}
			continue
		}
		if p.Unauthenticated() {
			continue
		}
		for _, m := range p.Models {
			out = append(out, cards.Offer{Text: m, Display: m, Meta: name})
		}
	}
	return out
}

// Session ids with their titles, new first, as session.list answers them
func sessionArgOffers(sessions []listedSession) []cards.Offer {
	out := []cards.Offer{{Text: "new", Display: "new", Meta: tr(txt.SessionLifecycleNewLiveSessionStarted)}}
	for _, s := range sessions {
		if s.ID != "" {
			out = append(out, cards.Offer{Text: s.ID, Display: s.ID, Meta: s.Title})
		}
	}
	return out
}

// Profile names as profiles.list answers them
func profileArgOffers(r *tuigateway.ProfilesListResult) []cards.Offer {
	var out []cards.Offer
	for _, p := range r.Profiles {
		if p.Name == "" {
			continue
		}
		out = append(out, cards.Offer{Text: p.Name, Display: p.Name, Meta: val(p.Model)})
	}
	return out
}

// Terminal UI commands and the session catalog's commands, quick and plugin ones among them, after the gateway's offers
func (a *app) mergeNameOffers(input string, offers []cards.Offer) []cards.Offer {
	if strings.Contains(input, " ") {
		return offers
	}
	known := map[string]bool{}
	for _, o := range offers {
		known["/"+strings.TrimPrefix(strings.TrimSpace(o.Text), "/")] = true
	}
	add := func(o cards.Offer) {
		if !known[o.Text] {
			known[o.Text] = true
			offers = append(offers, o)
		}
	}
	for _, c := range a.localCommands {
		if strings.HasPrefix("/"+c.name, strings.ToLower(input)) {
			add(cards.Offer{Text: "/" + c.name, Display: "/" + c.name, Meta: c.helpText()})
		}
	}
	if a.catalog != nil {
		for _, pair := range a.catalog.Pairs {
			if name := strings.Fields(pair[0]); len(name) > 0 && strings.HasPrefix(strings.ToLower(name[0]), strings.ToLower(input)) {
				add(cards.Offer{Text: name[0], Display: name[0], Meta: pair[1]})
			}
		}
	}
	for _, pair := range a.commandAliases() {
		if strings.HasPrefix(pair[0], strings.ToLower(input)) {
			add(cards.Offer{Text: pair[0], Display: pair[0], Meta: aliasOf(pair[1])})
		}
	}
	// Names that start with what was typed lead the fuzzy matches
	typed := strings.ToLower(strings.TrimPrefix(input, "/"))
	starts := func(o cards.Offer) bool {
		return strings.HasPrefix(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(o.Text), "/")), typed)
	}
	slices.SortStableFunc(offers, func(x, y cards.Offer) int {
		switch sx, sy := starts(x), starts(y); {
		case sx && !sy:
			return -1
		case sy && !sx:
			return 1
		}
		return 0
	})
	return offers
}

// Argument offers the gateway left out, from the command's local list, a gateway value source or the path completer
func (a *app) argOffers(input string, then func([]cards.Offer, int) tea.Cmd) tea.Cmd {
	arg, ok := parseSlashArg(input)
	if !ok {
		return then(nil, 0)
	}
	replace := unitLen(input) - unitLen(arg.partial)
	if local := localArgOffers(a.findSlashCommand(arg.name), arg); len(local) > 0 {
		return then(local, replace)
	}
	kind, picker, applies := argKind(arg)
	if !applies || (picker && arg.partial == "") {
		return then(nil, replace)
	}
	if kind == argPaths {
		return call(a.gw, tuigateway.CompletePath, *a.store.pathCompletionParams(arg.partial), func(r *tuigateway.CompletionItemsResult, err error) tea.Cmd {
			if err != nil {
				return then(unavailableOffer(err), replace)
			}
			return then(offersOf(r.Items, false), replace)
		})
	}
	return a.argValues(kind, func(values []cards.Offer, err error) tea.Cmd {
		if err != nil {
			return then(unavailableOffer(err), replace)
		}
		return then(matchOffers(values, arg.partial), replace)
	})
}

// Row saying completion is unavailable, with the gateway's reason
func unavailableOffer(err error) []cards.Offer {
	meta := rpcErrorMessage(err)
	if meta == "" {
		meta = tr(txt.LibTextCompletionUnavailableMeta)
	}
	return []cards.Offer{{Display: tr(txt.LibTextCompletionUnavailable), Meta: meta}}
}

// Offers of completion rows, only the skills when asked
func offersOf(items []tuigateway.CompletionItem, skillsOnly bool) []cards.Offer {
	var out []cards.Offer
	for _, item := range items {
		if skillsOnly && (item.Kind == nil || *item.Kind != "skill") {
			continue
		}
		out = append(out, cards.Offer{Text: item.Text, Display: item.Display, Meta: item.Meta})
	}
	return out
}
