package app

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Row of an account's page: an act by name, or a field of the account's kind as Settings draws it
type act struct {
	name    string
	setting *configv1.Setting
}

// Acts of an account's page
const (
	actSignIn  = "sign in"
	actKey     = "key"
	actRenew   = "renew"
	actForget  = "forget"
	actTurns   = "turns"
	actUp      = "up"
	actDown    = "down"
	actKeep    = "keep"
	actRename  = "rename"
	actRemove  = "remove"
	actFieldOf = "field"
)

// Whether something holds the account back that forgetting its limit clears
func limited(account *providerv1.Account) bool {
	switch account.GetStatus() {
	case "limited", "dead", "needs_sign_in":
		return true
	}
	return account.GetResets() != nil
}

// Whether the account signs in rather than keeps a key: a sign-in of the provider or another program's
func signsIn(entry *providerv1.Provider, account *providerv1.Account) bool {
	return account.GetAuth() == "oauth" || account.GetProgram() != "" || account.GetAuth() == "" && entry.GetSignsIn() && account.GetKeyEnv() == ""
}

// Rows of an account's page, each only when it applies: signing in or changing its key, renewing its sign-in, forgetting its limit, taking
// turns and its place among two accounts or more, keeping its key, the fields of its kind, then renaming and removing it
func (p *providers) acts(entry *providerv1.Provider, account *providerv1.Account) []act {
	var rows []act
	if signsIn(entry, account) {
		rows = append(rows, act{name: actSignIn})
	} else if entry.GetKeyed() {
		rows = append(rows, act{name: actKey})
	}
	if account.GetRenewable() {
		rows = append(rows, act{name: actRenew})
	}
	if limited(account) {
		rows = append(rows, act{name: actForget})
	}
	all := entry.GetAccounts()
	if len(all) > 1 {
		place := slices.IndexFunc(all, func(other *providerv1.Account) bool { return other.GetName() == account.GetName() })
		rows = append(rows, act{name: actTurns})
		if place > 0 {
			rows = append(rows, act{name: actUp})
		}
		if place >= 0 && place < len(all)-1 {
			rows = append(rows, act{name: actDown})
		}
	}
	if account.GetKeyEnv() != "" {
		rows = append(rows, act{name: actKeep})
	}
	everyday, rare := p.options.Section(account.GetSection())
	for _, setting := range slices.Concat(everyday, rare) {
		rows = append(rows, act{name: actFieldOf, setting: setting})
	}
	return append(rows, act{name: actRename}, act{name: actRemove})
}

// Opens the page of an account over the provider's: its state as a sentence, what stands against it in the warm color, then its rows
func (p *providers) accountPage(provider, name string) tea.Cmd {
	t := p.kit.Env.Theme
	current := func() (*providerv1.Provider, *providerv1.Account) {
		entry := p.find(provider)
		for _, account := range entry.GetAccounts() {
			if account.GetName() == name {
				return entry, account
			}
		}
		return entry, nil
	}
	_, account := current()
	if account == nil {
		return nil
	}
	var rows []act
	list := cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   account.GetTitle(),
		Glyph:   t.Glyphs.Models,
		Key:     "providers:account:" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			entry, account := current()
			if account == nil {
				return nil
			}
			lines := []string{account.GetStanding()}
			if trouble := troubleOf(entry, account); trouble != "" {
				lines = append(lines, render.Warm(trouble, t))
			}
			return lines
		},
		Columns: pageColumns,
		Rows: func() [][]string {
			entry, account := current()
			if account == nil {
				rows = nil
				return nil
			}
			rows = p.acts(entry, account)
			cells := make([][]string, len(rows))
			for i, row := range rows {
				cells[i] = p.actCells(entry, account, row)
			}
			return cells
		},
		Detail: func(i int) []string {
			entry, account := current()
			if account == nil || i < 0 || i >= len(rows) {
				return nil
			}
			if rows[i].setting != nil {
				return p.options.Detail(rows[i].setting, false)
			}
			return []string{actAbout(entry, account, rows[i].name)}
		},
		Empty: func() string { return "This account is no longer there." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			_, account := current()
			if account == nil || i < 0 || i >= len(rows) {
				return nil
			}
			if rows[i].setting != nil {
				return p.options.Hints(rows[i].setting)
			}
			return []wm.Hint{{Action: "hub.use", Label: actVerb(account, rows[i].name)}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				entry, account := current()
				if account == nil {
					return nil
				}
				return p.doAct(l, entry, account, rows[i])
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				entry, account := current()
				switch {
				case account == nil:
					return nil
				case rows[i].setting != nil:
					return p.options.Reset(l, rows[i].setting)
				}
				return p.removeAccount(l, entry, account)
			},
		},
	})
	list.ToggleDetail()
	list.Refresh()
	p.views.Add(list)
	p.options.Watch(list)
	return p.hub.Push(list)
}

// What stands against the account and what to do, one line; empty while nothing does
func troubleOf(entry *providerv1.Provider, account *providerv1.Account) string {
	resets := ""
	if at := account.GetResets(); at != nil {
		resets = " until " + clock(at.AsTime())
	}
	switch account.GetStatus() {
	case "limited":
		return "It reached a limit" + resets + ". Another account is used meanwhile, when there is one."
	case "needs_sign_in":
		return "Its sign-in ran out" + resets + ". Sign in again to use it."
	case "dead":
		return entry.GetDisplayName() + " refused it for good. Connect it again with a new key or sign-in."
	case "unavailable":
		return cmp.Or(account.GetMissing(), "It has no API key or sign-in to use.")
	}
	return ""
}

// Cells of a row of an account's page
func (p *providers) actCells(entry *providerv1.Provider, account *providerv1.Account, row act) []string {
	if row.setting != nil {
		return p.options.Cells(row.setting)
	}
	t := p.kit.Env.Theme
	switch row.name {
	case actSignIn:
		if account.GetCredentialOrigin() == providerv1.Origin_ORIGIN_UNSPECIFIED {
			return []string{"Sign in", "", ""}
		}
		return []string{"Sign in again", "", ""}
	case actKey:
		return []string{"Change the API key", "", ""}
	case actRenew:
		if expires := account.GetExpires(); expires != nil {
			return []string{"Renew the sign-in", "", render.Dim("Lasts until "+clock(expires.AsTime()), t)}
		}
		return []string{"Renew the sign-in", "", ""}
	case actForget:
		return []string{"Forget the limit", "", ""}
	case actTurns:
		if account.GetRotate() {
			return []string{"Takes turns", "", "On"}
		}
		return []string{"Takes turns", "", "Off"}
	case actUp:
		return []string{"Move up", "", ""}
	case actDown:
		return []string{"Move down", "", ""}
	case actKeep:
		return []string{"Keep the key in icebath", "", render.Dim("Now read from "+account.GetKeyEnv(), t)}
	case actRename:
		return []string{"Rename", "", ""}
	}
	return []string{"Remove", "", ""}
}

// What a row of an account's page does, said under the list
func actAbout(entry *providerv1.Provider, account *providerv1.Account, name string) string {
	all := entry.GetAccounts()
	place := slices.IndexFunc(all, func(other *providerv1.Account) bool { return other.GetName() == account.GetName() })
	switch name {
	case actSignIn:
		return "Sign in to " + entry.GetDisplayName() + " for this account."
	case actKey:
		return "Give this account another API key, or the variable that holds one."
	case actRenew:
		return "Get a fresh sign-in from " + entry.GetDisplayName() + " now."
	case actForget:
		return "Try this account again with the next request, before its limit lifts."
	case actTurns:
		return "Take turns with the other accounts of " + entry.GetDisplayName() + " that take turns."
	case actUp:
		return "Try this account before " + all[max(place-1, 0)].GetTitle() + "."
	case actDown:
		return "Try this account after " + all[min(place+1, len(all)-1)].GetTitle() + "."
	case actKeep:
		return "Copy the key from " + account.GetKeyEnv() + " into icebath, which stops reading the variable."
	case actRename:
		return "Give " + account.GetTitle() + " another name."
	}
	return "Take " + account.GetTitle() + " out of icebath, with what icebath keeps for it."
}

// Word on the button of a row of an account's page
func actVerb(account *providerv1.Account, name string) string {
	switch name {
	case actSignIn:
		return "sign in"
	case actKey:
		return "change"
	case actRenew:
		return "renew"
	case actForget:
		return "forget"
	case actTurns:
		if account.GetRotate() {
			return "turn off"
		}
		return "turn on"
	case actUp:
		return "move up"
	case actDown:
		return "move down"
	case actKeep:
		return "keep"
	case actRename:
		return "rename"
	}
	return "remove"
}

// Does what enter does on a row of an account's page
func (p *providers) doAct(l *cards.List, entry *providerv1.Provider, account *providerv1.Account, row act) tea.Cmd {
	if row.setting != nil {
		return p.options.Change(row.setting)
	}
	switch row.name {
	case actSignIn, actKey:
		return p.reconnect(entry, account, row.name == actKey)
	case actRenew:
		p.hub.Busy("Renewing the sign-in of " + account.GetTitle() + "…")
		return tui.Call(p.hub, func() (*providerv1.RenewResponse, error) {
			return p.service.Renew(p.kit.Env.Ctx, &providerv1.RenewRequest{Profile: p.profile, Provider: entry.GetName(), Account: account.GetName()})
		}, func(renewed *providerv1.RenewResponse) tea.Cmd {
			done := "Renewed the sign-in of " + account.GetTitle() + "."
			if expires := renewed.GetAccount().GetExpires(); expires != nil {
				done = "Renewed the sign-in of " + account.GetTitle() + ". It lasts until " + clock(expires.AsTime()) + "."
			}
			return tea.Batch(p.hub.Done(done), p.load(entry.GetName()))
		})
	case actForget:
		return tui.Call(p.hub, func() (*providerv1.ResetResponse, error) {
			return p.service.Reset(p.kit.Env.Ctx, &providerv1.ResetRequest{Profile: p.profile, Provider: entry.GetName(), Account: account.GetName()})
		}, func(*providerv1.ResetResponse) tea.Cmd {
			return tea.Batch(p.hub.Done("Forgot the limit of "+account.GetTitle()+". It is tried again with the next request."), p.load(entry.GetName()))
		})
	case actTurns:
		return tui.Call(p.hub, func() (*providerv1.SetTurnsResponse, error) {
			return p.service.SetTurns(p.kit.Env.Ctx, &providerv1.SetTurnsRequest{Profile: p.profile, Provider: entry.GetName(), Account: account.GetName(),
				On: !account.GetRotate()})
		}, func(turned *providerv1.SetTurnsResponse) tea.Cmd {
			return tea.Batch(p.hub.Done(turned.GetSaid()), p.load(entry.GetName()), p.options.Load(nil))
		})
	case actUp, actDown:
		return p.move(entry, account, map[bool]int{true: -1, false: 1}[row.name == actUp])
	case actKeep:
		l.Confirm(cards.Question{Text: "Keep the key from " + account.GetKeyEnv() + " in icebath? " + account.GetTitle() + " stops reading the variable.",
			Verb: "Keep"}, func() tea.Cmd {
			return tui.Call(p.hub, func() (*providerv1.KeepKeyResponse, error) {
				return p.service.KeepKey(p.kit.Env.Ctx, &providerv1.KeepKeyRequest{Profile: p.profile, Provider: entry.GetName(), Account: account.GetName()})
			}, func(*providerv1.KeepKeyResponse) tea.Cmd {
				return tea.Batch(p.hub.Done(account.GetTitle()+" now keeps its key in icebath."), p.load(entry.GetName()))
			})
		})
		return nil
	case actRename:
		return p.renameAccount(entry, account)
	}
	return p.removeAccount(l, entry, account)
}

// Connects an account again as a job: its key or the variable of one for a key, else its ways to sign in, then that way's step
func (p *providers) reconnect(entry *providerv1.Provider, account *providerv1.Account, key bool) tea.Cmd {
	title := "Sign in again"
	if key {
		title = "Change the API key"
	}
	ways := slices.DeleteFunc(p.ways(entry), func(w way) bool { return (w.key || w.variable) != key })
	if len(ways) == 0 {
		return nil
	}
	j := newJob(p.kit, p.hub, title)
	target := Target{Profile: p.profile, Provider: entry, Account: account, Hub: p.hub, Next: j.Show}
	target.Done = func(kept *providerv1.Account) tea.Cmd {
		return tea.Batch(j.End(), p.hub.Done(kept.GetTitle()+" of "+entry.GetDisplayName()+" is connected."), p.load(entry.GetName()))
	}
	return p.wayStep(j, "How do you want to connect "+account.GetTitle()+"?", ways, target)
}

// Moves the account one place earlier or later in the order the provider's accounts are tried
func (p *providers) move(entry *providerv1.Provider, account *providerv1.Account, step int) tea.Cmd {
	all := entry.GetAccounts()
	to := slices.IndexFunc(all, func(other *providerv1.Account) bool { return other.GetName() == account.GetName() }) + step
	if to < 0 || to >= len(all) {
		return nil
	}
	return tui.Call(p.hub, func() (*providerv1.MoveAccountResponse, error) {
		return p.service.MoveAccount(p.kit.Env.Ctx, &providerv1.MoveAccountRequest{Profile: p.profile, Provider: entry.GetName(), Account: account.GetName(),
			Priority: int32(to)})
	}, func(moved *providerv1.MoveAccountResponse) tea.Cmd {
		return tea.Batch(p.hub.Done(fmt.Sprintf("%s is now tried %s of %d.", account.GetTitle(), ordinal(int(moved.GetPriority())+1), len(all))),
			p.load(entry.GetName()), p.options.Load(nil))
	})
}

// Asks the account's new name: the name shown changes, and the key it is kept under follows it
func (p *providers) renameAccount(entry *providerv1.Provider, account *providerv1.Account) tea.Cmd {
	field := cards.Field{Label: "Name", Value: account.GetTitle(), Select: true, Help: "The name you know this account by, such as Work or Personal."}
	return p.kit.Ask(p.hub, "Rename", field, "Rename", func(title string) tea.Cmd {
		if title == account.GetTitle() {
			return nil
		}
		return tui.Call(p.hub, func() (*providerv1.RenameAccountResponse, error) {
			return p.service.RenameAccount(p.kit.Env.Ctx, &providerv1.RenameAccountRequest{Profile: p.profile, Provider: entry.GetName(),
				Account: account.GetName(), Name: title})
		}, func(renamed *providerv1.RenameAccountResponse) tea.Cmd {
			moved := renamed.GetAccount().GetName()
			return tea.Batch(p.closeView("providers:account:"+account.GetName()), p.hub.Done("Renamed "+account.GetTitle()+" to "+title+"."),
				p.options.Load(nil), p.reload(entry.GetName(), func() tea.Cmd { return p.accountPage(entry.GetName(), moved) }))
		})
	})
}

// Puts the account in hand on its provider's page, which lists the accounts first in the order they are tried
func (p *providers) inHand(provider, account string) tea.Cmd {
	entry := p.find(provider)
	at := slices.IndexFunc(entry.GetAccounts(), func(have *providerv1.Account) bool { return have.GetName() == account })
	for _, view := range slices.Backward(p.views.Lists()) {
		if view.Key() == "providers:"+provider && at >= 0 {
			view.Select(at)
			break
		}
	}
	return nil
}

// What tells an account apart, under the provider's list of accounts: how it signs in and where its credential comes from, who it signs
// in as, what stands against it and what the provider said, its place and the facts a person tells two accounts apart by
func (p *providers) accountFacts(entry *providerv1.Provider, account *providerv1.Account) []string {
	t := p.kit.Env.Theme
	var lines []string
	if standing := account.GetStanding(); strings.TrimSuffix(standing, ".") != account.GetState() {
		lines = append(lines, standing)
	}
	if trouble := troubleOf(entry, account); trouble != "" {
		lines = append(lines, render.Warm(trouble, t))
	}
	if reason := account.GetReason(); reason != "" {
		lines = append(lines, render.Dim(entry.GetDisplayName()+" said: "+reason, t))
	}
	var facts []string
	if n := len(entry.GetAccounts()); n > 1 {
		place := slices.IndexFunc(entry.GetAccounts(), func(other *providerv1.Account) bool { return other.GetName() == account.GetName() })
		facts = append(facts, fmt.Sprintf("Tried %s of %d", ordinal(place+1), n))
	}
	if expires := account.GetExpires(); expires != nil {
		facts = append(facts, "Sign-in lasts until "+clock(expires.AsTime()))
	}
	if fingerprint := account.GetFingerprint(); fingerprint != "" {
		facts = append(facts, "Fingerprint "+fingerprint)
	}
	if n := account.GetRequests(); n > 0 {
		facts = append(facts, strs.Count(int(n), "request"))
	}
	if len(facts) > 0 {
		lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	}
	return lines
}

// Place in an order as a number with its ending: 1st, 2nd, 3rd, 4th
func ordinal(n int) string {
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return strconv.Itoa(n) + "th"
	case n%10 == 1:
		return strconv.Itoa(n) + "st"
	case n%10 == 2:
		return strconv.Itoa(n) + "nd"
	case n%10 == 3:
		return strconv.Itoa(n) + "rd"
	}
	return strconv.Itoa(n) + "th"
}

// Moment as a person reads it: the time alone today, else the day and the time
func clock(at time.Time) string {
	local, now := at.Local(), time.Now()
	if local.Year() == now.Year() && local.YearDay() == now.YearDay() {
		return local.Format("15:04")
	}
	return local.Format("Jan 2") + " at " + local.Format("15:04")
}

// Time left before a moment in words: in about 2 hours, in 5 minutes, soon
func left(at time.Time) string {
	span := time.Until(at)
	switch {
	case span <= time.Minute:
		return "soon"
	case span >= 48*time.Hour:
		return "in about " + strconv.Itoa(int(span.Round(24*time.Hour)/(24*time.Hour))) + " days"
	case span >= 2*time.Hour:
		return "in about " + strconv.Itoa(int(span.Round(time.Hour)/time.Hour)) + " hours"
	case span >= time.Hour:
		return "in about an hour"
	}
	minutes := int((span + time.Minute - 1) / time.Minute)
	if minutes == 1 {
		return "in a minute"
	}
	return "in " + strconv.Itoa(minutes) + " minutes"
}
