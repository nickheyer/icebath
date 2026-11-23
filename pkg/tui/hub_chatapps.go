package tui

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the chat apps screen
const chatAppsContext = "tui.chatapps"

// Why installing the background service waits until icebath closes, as the command tree words it
const chatAppsServiceReason = "It stops or replaces the background service this window uses."

// Keys of the chat apps screen beyond the shared hub keys
func registerChatAppsKeys(km *keys.Map) {
	if keys.ContextDocs[chatAppsContext] == "" {
		if err := keys.AddContext(chatAppsContext, "chat apps screen"); err != nil {
			panic(err)
		}
	}
	km.Register(keys.Action{ID: "tui.chatapps.service", Context: chatAppsContext, Label: "install service",
		Doc: "install the background service to start with the computer, once icebath closes", Default: []string{"s"}})
	km.Register(keys.Action{ID: "tui.chatapps.send", Context: chatAppsContext, Label: "send", Doc: "send a message to a chat", Default: []string{"m"}})
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "chatapps", order: 90, title: txt.HubsTitleChatapps, short: txt.HubsShortChatapps, slash: "chat-apps",
		open: func(a *app, _ string) tea.Cmd { return a.openChatApps() }, keys: registerChatAppsKeys})
}

// Kind of a row of the chat apps screen
type chatAppsKind int

const (
	chatAppPlatform chatAppsKind = iota
	chatAppPending
	chatAppUser
	chatAppWebhook
	chatAppPeer
)

// One row of the chat apps screen and the item it shows
type chatAppsRow struct {
	kind chatAppsKind
	at   int
}

// State of the open chat apps screen: what the background service last answered and the rows built from it
type chatAppsHub struct {
	a       *app
	h       *cards.Hub
	list    *cards.List
	data    tuigateway.ChatAppsListResult
	rows    []chatAppsRow
	loading bool
}

// Chat apps screen: messaging platforms with their state, people waiting to pair, webhooks and peers, with every write in place
func (a *app) openChatApps() tea.Cmd {
	c := &chatAppsHub{a: a, loading: true}
	c.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleChatapps), Glyph: a.toolkit.Glyphs.Server, Key: "tui:chatapps", Context: chatAppsContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.HubsColKind)}, {Title: tr(txt.HubsChatappsColName), Max: 28}, {Title: tr(txt.HubsColStatus), Max: 22}, {Title: tr(txt.HubsChatappsColDetail), Flex: 1}},
		Yield:   []int{0},
		Rows:    c.cells,
		Detail:  c.detail,
		Banner:  c.banner,
		Empty: func() string {
			if c.loading {
				return tr(txt.HubsChatappsLoading)
			}
			return tr(txt.HubsChatappsEmpty)
		},
		Hints: c.hints,
		Free:  map[string]bool{"hub.add": true, "hub.refresh": true, "tui.chatapps.service": true, "tui.chatapps.send": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.refresh":          func(*cards.List, int) tea.Cmd { return c.load() },
			"hub.use":              func(l *cards.List, i int) tea.Cmd { return c.open(l, i) },
			"hub.add":              func(*cards.List, int) tea.Cmd { return c.add() },
			"hub.edit":             func(_ *cards.List, i int) tea.Cmd { return c.edit(i) },
			"hub.remove":           func(l *cards.List, i int) tea.Cmd { return c.remove(l, i) },
			"tui.chatapps.service": func(l *cards.List, _ int) tea.Cmd { return c.installService(l) },
			"tui.chatapps.send":    func(_ *cards.List, i int) tea.Cmd { return c.send(c.platformOf(i)) },
		},
		OnFocus: func(*cards.List) tea.Cmd { return c.load() },
	})
	c.h = a.newHub("tui:chatapps", c.list)
	return a.wm.ShowOverlay(c.h)
}

// Reads everything the screen shows again, a failure staying in the hub
func (c *chatAppsHub) load() tea.Cmd {
	return hubCall(c.a, c.h, "", tuigateway.ChatAppsList, tuigateway.ChatAppsListParams{}, func(r *tuigateway.ChatAppsListResult) tea.Cmd {
		c.loading, c.data = false, *r
		c.rows = nil
		for i := range r.Platforms {
			c.rows = append(c.rows, chatAppsRow{chatAppPlatform, i})
		}
		for i := range r.Pending {
			c.rows = append(c.rows, chatAppsRow{chatAppPending, i})
		}
		for i := range r.Approved {
			c.rows = append(c.rows, chatAppsRow{chatAppUser, i})
		}
		for i := range r.Webhooks {
			c.rows = append(c.rows, chatAppsRow{chatAppWebhook, i})
		}
		for i := range r.Peers {
			c.rows = append(c.rows, chatAppsRow{chatAppPeer, i})
		}
		c.list.Refresh()
		return nil
	})
}

// Row at i, false past the rows
func (c *chatAppsHub) at(i int) (chatAppsRow, bool) {
	if i < 0 || i >= len(c.rows) {
		return chatAppsRow{}, false
	}
	return c.rows[i], true
}

// Platform a row belongs to, empty for a row of no platform
func (c *chatAppsHub) platformOf(i int) string {
	r, ok := c.at(i)
	if !ok {
		return ""
	}
	switch r.kind {
	case chatAppPlatform:
		return c.data.Platforms[r.at].Platform
	case chatAppPending:
		return c.data.Pending[r.at].Platform
	case chatAppUser:
		return c.data.Approved[r.at].Platform
	}
	return ""
}

// One sentence above the rows when chat apps are set up but nothing starts the background service with the computer
func (c *chatAppsHub) banner() string {
	if c.loading || len(c.data.Platforms) == 0 || c.data.ServiceInstalled {
		return ""
	}
	return tr(txt.HubsChatappsServiceNeeded)
}

// Words for the state the background service reported for a platform
func chatAppsState(p tuigateway.ChatAppPlatform) string {
	switch p.State {
	case "":
		return tr(txt.HubsChatappsStateUnknown)
	case "connected":
		return tr(txt.HubsChatappsStateConnected)
	case "retrying":
		return tr(txt.HubsChatappsStateRetrying)
	case "failed", "fatal":
		return tr(txt.HubsChatappsStateFailed)
	}
	return p.State
}

// Name a person shows under, their id beside a name
func chatAppsPerson(id, name string) string {
	if name != "" && name != id {
		return name + " (" + id + ")"
	}
	return id
}

// Cells of every row: what it is, its name, its state and one line more
func (c *chatAppsHub) cells() [][]string {
	t := c.a.toolkit
	out := make([][]string, len(c.rows))
	for i, r := range c.rows {
		switch r.kind {
		case chatAppPlatform:
			p := c.data.Platforms[r.at]
			state := chatAppsState(p)
			switch p.State {
			case "connected":
				state = render.Good(state, t)
			case "failed", "fatal", "retrying":
				state = render.Bad(state, t)
			}
			detail := tr(txt.HubsChatappsChats, len(p.Targets))
			if p.Home != "" {
				detail = tr(txt.HubsChatappsHome, p.Home) + " · " + detail
			}
			out[i] = []string{tr(txt.HubsChatappsKindApp), p.Platform, state, detail}
		case chatAppPending:
			p := c.data.Pending[r.at]
			out[i] = []string{tr(txt.HubsChatappsKindPairing), p.Platform + " · " + chatAppsPerson(p.UserID, p.UserName),
				render.Warm(tr(txt.HubsChatappsWaiting, minutesText(p.Minutes)), t), tr(txt.HubsChatappsApproveHint)}
		case chatAppUser:
			u := c.data.Approved[r.at]
			out[i] = []string{tr(txt.HubsChatappsKindAllowed), u.Platform + " · " + chatAppsPerson(u.UserID, u.UserName), tr(txt.HubsChatappsAllowed), ""}
		case chatAppWebhook:
			w := c.data.Webhooks[r.at]
			deliver := w.Deliver
			if w.DeliverOnly {
				deliver += " " + tr(txt.HubsChatappsDirect)
			}
			out[i] = []string{tr(txt.HubsChatappsKindWebhook), w.Name, tr(txt.HubsChatappsDeliversTo, deliver), w.URL}
		case chatAppPeer:
			p := c.data.Peers[r.at]
			state := tr(txt.HubsChatappsKeySet)
			if !p.KeySet {
				state = render.Warm(tr(txt.HubsChatappsNoKey), t)
			}
			out[i] = []string{tr(txt.HubsChatappsKindPeer), p.Name, state, p.URL}
		}
	}
	return out
}

// Minutes as a short age
func minutesText(m int64) string {
	if m < 60 {
		return strconv.FormatInt(m, 10) + "m"
	}
	return strconv.FormatInt(m/60, 10) + "h"
}

// Lines of a row's detail pane
func (c *chatAppsHub) detail(i int) []string {
	r, ok := c.at(i)
	if !ok {
		return nil
	}
	switch r.kind {
	case chatAppPlatform:
		p := c.data.Platforms[r.at]
		out := []string{p.Platform + " · " + chatAppsState(p)}
		if p.Error != "" {
			out = append(out, p.Error)
		}
		if p.Home != "" {
			out = append(out, tr(txt.HubsChatappsHome, p.Home))
		}
		for _, t := range p.Targets {
			out = append(out, "  "+t.Label+"  "+t.Target)
		}
		if len(p.Targets) == 0 {
			out = append(out, tr(txt.HubsChatappsNoChats))
		}
		if slices.Contains(c.data.LockedOut, p.Platform) {
			out = append(out, tr(txt.HubsChatappsLockedOut))
		}
		return out
	case chatAppPending:
		p := c.data.Pending[r.at]
		return []string{tr(txt.HubsChatappsPendingDetail, chatAppsPerson(p.UserID, p.UserName), p.Platform, minutesText(p.Minutes))}
	case chatAppUser:
		u := c.data.Approved[r.at]
		return []string{tr(txt.HubsChatappsUserDetail, chatAppsPerson(u.UserID, u.UserName), u.Platform)}
	case chatAppWebhook:
		w := c.data.Webhooks[r.at]
		out := []string{w.Name + " · " + w.URL}
		if w.Description != "" {
			out = append(out, w.Description)
		}
		events := w.Events
		if events == "" {
			events = tr(txt.HubsChatappsEveryEvent)
		}
		out = append(out, tr(txt.HubsChatappsWebhookEvents, events), tr(txt.HubsChatappsDeliversTo, w.Deliver))
		if w.Prompt != "" {
			out = append(out, tr(txt.HubsChatappsWebhookPrompt, w.Prompt))
		}
		if w.Skills != "" {
			out = append(out, tr(txt.HubsChatappsWebhookSkills, w.Skills))
		}
		return out
	case chatAppPeer:
		p := c.data.Peers[r.at]
		out := []string{p.Name + " · " + p.URL}
		if p.Note != "" {
			out = append(out, p.Note)
		}
		if !p.KeySet {
			out = append(out, tr(txt.HubsChatappsPeerNoKey, p.KeyEnv))
		}
		return out
	}
	return nil
}

// Legend of the row under the cursor
func (c *chatAppsHub) hints(_ *cards.List, i int) []wm.Hint {
	var out []wm.Hint
	if r, ok := c.at(i); ok {
		switch r.kind {
		case chatAppPending:
			out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HubsChatappsApproveLabel)}, wm.Hint{Action: "hub.remove", Label: tr(txt.HubsChatappsDismissLabel)})
		case chatAppUser:
			out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, wm.Hint{Action: "hub.remove", Label: tr(txt.HubsChatappsRevokeLabel)})
		case chatAppWebhook, chatAppPeer:
			out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, wm.Hint{Action: "hub.edit"}, wm.Hint{Action: "hub.remove"})
		default:
			out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)})
		}
	}
	out = append(out, wm.Hint{Action: "hub.add"})
	if len(c.data.Platforms) > 0 {
		out = append(out, wm.Hint{Action: "tui.chatapps.send"})
	}
	if c.banner() != "" {
		out = append(out, wm.Hint{Action: "tui.chatapps.service"})
	}
	return out
}

// Enter on a row: a pairing request approved after a confirmation, every other row's actions screen
func (c *chatAppsHub) open(l *cards.List, i int) tea.Cmd {
	r, ok := c.at(i)
	if !ok {
		return nil
	}
	switch r.kind {
	case chatAppPlatform:
		p := c.data.Platforms[r.at]
		return c.a.hubActions(c.h, p.Platform, func() []string { return c.detail(i) }, func() []hubAction {
			acts := []hubAction{
				{key: "tui.chatapps.send", label: tr(txt.HubsChatappsSend), does: tr(txt.HubsChatappsSendDoes), run: func() tea.Cmd { return c.send(p.Platform) }},
				{label: tr(txt.HubsChatappsSetup), does: tr(txt.HubsChatappsSetupDoes), run: c.setup},
			}
			if p.Platform == "slack" {
				acts = append(acts, hubAction{label: tr(txt.HubsChatappsSlackManifest), does: tr(txt.HubsChatappsSlackManifestDoes), run: c.slackManifest})
			}
			return append(acts, hubAction{label: tr(txt.HubsChatappsApproveCode), does: tr(txt.HubsChatappsApproveCodeDoes), run: func() tea.Cmd { return c.approveCode(p.Platform) }})
		})
	case chatAppPending:
		p := c.data.Pending[r.at]
		code := p.RequestID
		if code == "" {
			return c.approveCode(p.Platform)
		}
		l.Confirm(tr(txt.HubsChatappsApproveConfirm, chatAppsPerson(p.UserID, p.UserName), p.Platform), func() tea.Cmd {
			return c.pairing(tuigateway.ChatAppsPairingParams{Action: "approve", Platform: p.Platform, Code: code})
		})
		return nil
	case chatAppUser:
		u := c.data.Approved[r.at]
		return c.a.hubActions(c.h, chatAppsPerson(u.UserID, u.UserName), func() []string { return c.detail(i) }, func() []hubAction {
			return []hubAction{{key: "hub.remove", label: tr(txt.HubsChatappsRevoke), does: tr(txt.HubsChatappsRevokeDoes, u.Platform),
				confirm: tr(txt.HubsChatappsRevokeConfirm, chatAppsPerson(u.UserID, u.UserName), u.Platform),
				run: func() tea.Cmd {
					return tea.Batch(c.h.PopTo(c.list), c.pairing(tuigateway.ChatAppsPairingParams{Action: "revoke", Platform: u.Platform, UserID: u.UserID}))
				}}}
		})
	case chatAppWebhook:
		w := c.data.Webhooks[r.at]
		return c.a.hubActions(c.h, w.Name, func() []string { return c.detail(i) }, func() []hubAction {
			return []hubAction{
				{label: tr(txt.HubsChatappsWebhookTest), does: tr(txt.HubsChatappsWebhookTestDoes), run: func() tea.Cmd {
					return c.webhook(tuigateway.ChatAppsWebhookParams{Action: "test", Name: w.Name}, tr(txt.HubsChatappsTesting, w.Name))
				}},
				{label: tr(txt.HubsChatappsCopyUrl), does: tr(txt.HubsChatappsCopyUrlDoes), run: func() tea.Cmd { return c.a.hubCopy(c.h, w.URL, tr(txt.HubsChatappsUrlWord)) }},
				{key: "hub.edit", label: tr(txt.HubsChatappsEdit), does: tr(txt.HubsChatappsWebhookEditDoes), run: func() tea.Cmd { return c.webhookForm(&w) }},
				{key: "hub.remove", label: tr(txt.HubsChatappsRemove), does: tr(txt.HubsChatappsWebhookRemoveDoes), confirm: tr(txt.HubsChatappsWebhookRemoveConfirm, w.Name),
					run: func() tea.Cmd {
						return tea.Batch(c.h.PopTo(c.list), c.webhook(tuigateway.ChatAppsWebhookParams{Action: "remove", Name: w.Name}, ""))
					}},
			}
		})
	case chatAppPeer:
		p := c.data.Peers[r.at]
		return c.a.hubActions(c.h, p.Name, func() []string { return c.detail(i) }, func() []hubAction {
			return []hubAction{
				{label: tr(txt.HubsChatappsPeerMessage), does: tr(txt.HubsChatappsPeerMessageDoes), run: func() tea.Cmd { return c.peerMessage(p) }},
				{key: "hub.edit", label: tr(txt.HubsChatappsEdit), does: tr(txt.HubsChatappsPeerEditDoes), run: func() tea.Cmd { return c.peerForm(&p) }},
				{key: "hub.remove", label: tr(txt.HubsChatappsRemove), does: tr(txt.HubsChatappsPeerRemoveDoes), confirm: tr(txt.HubsChatappsPeerRemoveConfirm, p.Name),
					run: func() tea.Cmd {
						return tea.Batch(c.h.PopTo(c.list), c.peer(tuigateway.ChatAppsPeerParams{Action: "remove", Name: p.Name}))
					}},
			}
		})
	}
	return nil
}

// Edit on a row: a webhook or peer form filled with what it holds
func (c *chatAppsHub) edit(i int) tea.Cmd {
	r, ok := c.at(i)
	if !ok {
		return nil
	}
	switch r.kind {
	case chatAppWebhook:
		w := c.data.Webhooks[r.at]
		return c.webhookForm(&w)
	case chatAppPeer:
		p := c.data.Peers[r.at]
		return c.peerForm(&p)
	}
	return nil
}

// Remove on a row after a confirmation: a person's access, a waiting platform's requests, a webhook or a peer
func (c *chatAppsHub) remove(l *cards.List, i int) tea.Cmd {
	r, ok := c.at(i)
	if !ok {
		return nil
	}
	switch r.kind {
	case chatAppPending:
		p := c.data.Pending[r.at]
		l.Confirm(tr(txt.HubsChatappsClearConfirm, p.Platform), func() tea.Cmd {
			return c.pairing(tuigateway.ChatAppsPairingParams{Action: "clear", Platform: p.Platform})
		})
	case chatAppUser:
		u := c.data.Approved[r.at]
		l.Confirm(tr(txt.HubsChatappsRevokeConfirm, chatAppsPerson(u.UserID, u.UserName), u.Platform), func() tea.Cmd {
			return c.pairing(tuigateway.ChatAppsPairingParams{Action: "revoke", Platform: u.Platform, UserID: u.UserID})
		})
	case chatAppWebhook:
		w := c.data.Webhooks[r.at]
		l.Confirm(tr(txt.HubsChatappsWebhookRemoveConfirm, w.Name), func() tea.Cmd {
			return c.webhook(tuigateway.ChatAppsWebhookParams{Action: "remove", Name: w.Name}, "")
		})
	case chatAppPeer:
		p := c.data.Peers[r.at]
		l.Confirm(tr(txt.HubsChatappsPeerRemoveConfirm, p.Name), func() tea.Cmd {
			return c.peer(tuigateway.ChatAppsPeerParams{Action: "remove", Name: p.Name})
		})
	}
	return nil
}

// Ways to add to the screen: set up a chat app, approve a code, add a webhook or a peer, send a message
func (c *chatAppsHub) add() tea.Cmd {
	choices := []hubChoice{
		{id: "setup", title: tr(txt.HubsChatappsSetup), detail: tr(txt.HubsChatappsSetupDoes)},
		{id: "code", title: tr(txt.HubsChatappsApproveCode), detail: tr(txt.HubsChatappsApproveCodeDoes)},
		{id: "webhook", title: tr(txt.HubsChatappsAddWebhook), detail: tr(txt.HubsChatappsAddWebhookDoes)},
		{id: "peer", title: tr(txt.HubsChatappsAddPeer), detail: tr(txt.HubsChatappsAddPeerDoes)},
		{id: "send", title: tr(txt.HubsChatappsSend), detail: tr(txt.HubsChatappsSendDoes)},
		{id: "slack", title: tr(txt.HubsChatappsSlackManifest), detail: tr(txt.HubsChatappsSlackManifestDoes)},
	}
	return c.a.hubPick(c.h, tr(txt.HubsChatappsAddTitle), choices, "", func(ch hubChoice) tea.Cmd {
		switch ch.id {
		case "code":
			return c.approveCode("")
		case "webhook":
			return c.webhookForm(nil)
		case "peer":
			return c.peerForm(nil)
		case "send":
			return c.send("")
		case "slack":
			return c.slackManifest()
		}
		return c.setup()
	})
}

// Chat app setup: pick an app and type its keys, the screen read again once the form closes
func (c *chatAppsHub) setup() tea.Cmd { return c.a.pickChatApp(c.h, c.load) }

// Prints the Slack app manifest with every command as a native slash and copies it
func (c *chatAppsHub) slackManifest() tea.Cmd {
	return c.a.hubRunCLI(c.h, tr(txt.HubsChatappsSlackManifest), []string{"slack", "manifest"}, func(run *cliRun) tea.Cmd {
		if run.err != nil || run.code != 0 || len(run.lines) == 0 {
			return nil
		}
		return c.a.hubCopy(c.h, strings.Join(run.lines, "\n"), tr(txt.HubsChatappsManifestWord))
	})
}

// Queues the install of the background service for after icebath closes and says what it is
func (c *chatAppsHub) installService(l *cards.List) tea.Cmd {
	if c.data.ServiceInstalled {
		return c.h.Done(tr(txt.HubsChatappsServiceInstalled))
	}
	queued := c.a.queueAfterExitCmd([]string{"gateway", "install"}, chatAppsServiceReason)
	l.Confirm(tr(txt.HubsChatappsServiceQuitNow), func() tea.Cmd { return c.a.die(0) })
	return queued
}

// Runs a pairing change and reads the screen again, the command's words in the note
func (c *chatAppsHub) pairing(params tuigateway.ChatAppsPairingParams) tea.Cmd {
	return hubCall(c.a, c.h, tr(txt.HubsChatappsSaving), tuigateway.ChatAppsPairing, params, func(r *tuigateway.ChatAppsResult) tea.Cmd {
		return tea.Batch(c.load(), c.h.Done(strings.Join(r.Lines, " ")))
	})
}

// Runs a webhook change and reads the screen again, the command's words in the note or, for a test, a page
func (c *chatAppsHub) webhook(params tuigateway.ChatAppsWebhookParams, busy string) tea.Cmd {
	if busy == "" {
		busy = tr(txt.HubsChatappsSaving)
	}
	return hubCall(c.a, c.h, busy, tuigateway.ChatAppsWebhook, params, func(r *tuigateway.ChatAppsResult) tea.Cmd {
		if params.Action == "subscribe" || params.Action == "test" {
			return tea.Batch(c.load(), c.page(params.Name, r.Lines))
		}
		return tea.Batch(c.load(), c.h.Done(strings.Join(r.Lines, " ")))
	})
}

// Runs a peer change and reads the screen again
func (c *chatAppsHub) peer(params tuigateway.ChatAppsPeerParams) tea.Cmd {
	return hubCall(c.a, c.h, tr(txt.HubsChatappsSaving), tuigateway.ChatAppsPeer, params, func(r *tuigateway.ChatAppsResult) tea.Cmd {
		return tea.Batch(c.load(), c.h.Done(strings.Join(r.Lines, " ")))
	})
}

// Page of what a command printed, such as a new webhook's address and secret
func (c *chatAppsHub) page(title string, lines []string) tea.Cmd {
	page := cards.NewPage(c.a.hubEnv(), cards.PageSpec{Title: title, Key: "tui:chatapps:out", Lines: func(int) []string { return lines }})
	return tea.Batch(c.h.PopTo(c.list), c.h.Push(page))
}

// Names of the set up platforms as form choices
func (c *chatAppsHub) platformChoices() []cards.Choice {
	out := make([]cards.Choice, 0, len(c.data.Platforms))
	for _, p := range c.data.Platforms {
		out = append(out, cards.Choice{Value: p.Platform, Label: p.Platform})
	}
	return out
}

// Asks for the code a bot sent someone and approves them on the platform
func (c *chatAppsHub) approveCode(platform string) tea.Cmd {
	platforms := c.platformChoices()
	if platform == "" && len(platforms) > 0 {
		platform = platforms[0].Value
	}
	spec := cards.FormSpec{Title: tr(txt.HubsChatappsApproveCode), Submit: tr(txt.HubsChatappsApproveLabel), Intro: []string{tr(txt.HubsChatappsApproveCodeIntro)},
		Fields: []cards.Field{
			{ID: "platform", Label: tr(txt.HubsChatappsFieldPlatform), Kind: cards.FieldChoice, Value: platform, Choices: platforms, Required: true},
			{ID: "code", Label: tr(txt.HubsChatappsFieldCode), Required: true},
		}}
	if len(platforms) == 0 {
		spec.Fields[0] = cards.Field{ID: "platform", Label: tr(txt.HubsChatappsFieldPlatform), Required: true, Placeholder: "telegram"}
	}
	return c.a.hubForm(c.h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		return c.pairing(tuigateway.ChatAppsPairingParams{Action: "approve", Platform: strings.TrimSpace(v["platform"]), Code: strings.TrimSpace(v["code"])})
	})
}

// Form of a webhook route, filled from one to change it, the new route's address and secret shown once added
func (c *chatAppsHub) webhookForm(w *tuigateway.ChatAppWebhook) tea.Cmd {
	if !c.data.WebhooksEnabled {
		return tea.Batch(c.h.Fail(tr(txt.HubsChatappsWebhooksOff)))
	}
	deliver := []cards.Choice{{Value: "log", Label: tr(txt.HubsChatappsDeliverLog), Meaning: tr(txt.HubsChatappsDeliverLogMeaning)}}
	for _, p := range c.data.Platforms {
		if p.Platform != "webhook" && p.Platform != "api_server" {
			deliver = append(deliver, cards.Choice{Value: p.Platform, Label: p.Platform})
		}
	}
	cur := tuigateway.ChatAppWebhook{Deliver: "log"}
	title := tr(txt.HubsChatappsAddWebhook)
	if w != nil {
		cur, title = *w, tr(txt.HubsChatappsEdit)+" · "+w.Name
	}
	only := "false"
	if cur.DeliverOnly {
		only = "true"
	}
	spec := cards.FormSpec{Title: title, Submit: tr(txt.HubsChatappsSave),
		Fields: []cards.Field{
			{ID: "name", Label: tr(txt.HubsChatappsFieldName), Value: cur.Name, Required: true, Help: tr(txt.HubsChatappsWebhookNameHelp)},
			{ID: "description", Label: tr(txt.HubsChatappsFieldDescription), Value: cur.Description},
			{ID: "prompt", Label: tr(txt.HubsChatappsFieldPrompt), Value: cur.Prompt, Help: tr(txt.HubsChatappsWebhookPromptHelp)},
			{ID: "events", Label: tr(txt.HubsChatappsFieldEvents), Value: cur.Events, Help: tr(txt.HubsChatappsWebhookEventsHelp)},
			{ID: "deliver", Label: tr(txt.HubsChatappsFieldDeliver), Kind: cards.FieldChoice, Value: cur.Deliver, Choices: deliver},
			{ID: "chat", Label: tr(txt.HubsChatappsFieldChat), Value: cur.DeliverChat, Help: tr(txt.HubsChatappsWebhookChatHelp), More: true},
			{ID: "skills", Label: tr(txt.HubsChatappsFieldSkills), Value: cur.Skills, Help: tr(txt.HubsChatappsWebhookSkillsHelp), More: true},
			{ID: "only", Label: tr(txt.HubsChatappsFieldDeliverOnly), Kind: cards.FieldBool, Value: only, Help: tr(txt.HubsChatappsWebhookOnlyHelp), More: true},
		}}
	return c.a.hubForm(c.h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		params := tuigateway.ChatAppsWebhookParams{Action: "subscribe", Name: strings.TrimSpace(v["name"]), Description: v["description"], Prompt: v["prompt"],
			Events: v["events"], Deliver: v["deliver"], DeliverChat: strings.TrimSpace(v["chat"]), Skills: v["skills"], DeliverOnly: v["only"] == "true"}
		return c.webhook(params, "")
	})
}

// Form of a peer, filled from one to change it, a blank key keeping the stored one
func (c *chatAppsHub) peerForm(p *tuigateway.ChatAppPeer) tea.Cmd {
	cur := tuigateway.ChatAppPeer{}
	title := tr(txt.HubsChatappsAddPeer)
	if p != nil {
		cur, title = *p, tr(txt.HubsChatappsEdit)+" · "+p.Name
	}
	keyHelp := tr(txt.HubsChatappsPeerKeyHelp)
	if cur.KeySet {
		keyHelp = tr(txt.HubsChatappsPeerKeyKept)
	}
	spec := cards.FormSpec{Title: title, Submit: tr(txt.HubsChatappsSave), Intro: []string{tr(txt.HubsChatappsPeerIntro)},
		Fields: []cards.Field{
			{ID: "name", Label: tr(txt.HubsChatappsFieldName), Value: cur.Name, Required: true, Help: tr(txt.HubsChatappsPeerNameHelp)},
			{ID: "url", Label: tr(txt.HubsChatappsFieldAddress), Value: cur.URL, Required: true, Placeholder: "http://spark.lan:8377"},
			{ID: "key", Label: tr(txt.HubsChatappsFieldKey), Kind: cards.FieldSecret, Help: keyHelp},
			{ID: "note", Label: tr(txt.HubsChatappsFieldNote), Value: cur.Note},
		}}
	return c.a.hubForm(c.h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		return c.peer(tuigateway.ChatAppsPeerParams{Action: "add", Name: v["name"], URL: v["url"], Key: v["key"], Note: v["note"]})
	})
}

// Messages an agent of a peer and shows its reply as it arrives, through the peer command
func (c *chatAppsHub) peerMessage(p tuigateway.ChatAppPeer) tea.Cmd {
	spec := cards.FormSpec{Title: tr(txt.HubsChatappsPeerMessage) + " · " + p.Name, Submit: tr(txt.HubsChatappsSendLabel),
		Fields: []cards.Field{
			{ID: "agent", Label: tr(txt.HubsChatappsFieldAgent), Help: tr(txt.HubsChatappsPeerAgentHelp)},
			{ID: "message", Label: tr(txt.HubsChatappsFieldMessage), Required: true},
		}}
	return c.a.hubForm(c.h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		target := p.Name
		if agent := strings.TrimSpace(v["agent"]); agent != "" {
			target += "/" + agent
		}
		return c.a.hubRunCLI(c.h, tr(txt.HubsChatappsPeerMessage), []string{"peer", "dm", target, v["message"]}, func(*cliRun) tea.Cmd { return nil })
	})
}

// Form of a message to a chat: a chat a platform found or the home chat of a platform, or any target the send command takes
func (c *chatAppsHub) send(platform string) tea.Cmd {
	if len(c.data.Platforms) == 0 {
		return c.h.Fail(tr(txt.HubsChatappsSendNone))
	}
	var targets []cards.Choice
	first := c.data.Platforms[0].Platform
	for _, p := range c.data.Platforms {
		if p.Platform == platform {
			first = platform
		}
		targets = append(targets, cards.Choice{Value: p.Platform, Label: tr(txt.HubsChatappsHomeOf, p.Platform), Meaning: p.Home})
		for _, t := range p.Targets {
			targets = append(targets, cards.Choice{Value: t.Target, Label: t.Label, Meaning: t.Target})
		}
	}
	spec := cards.FormSpec{Title: tr(txt.HubsChatappsSend), Submit: tr(txt.HubsChatappsSendLabel),
		Fields: []cards.Field{
			{ID: "to", Label: tr(txt.HubsChatappsFieldTo), Kind: cards.FieldChoice, Value: first, Choices: targets},
			{ID: "other", Label: tr(txt.HubsChatappsFieldOther), Help: tr(txt.HubsChatappsSendOtherHelp), More: true},
			{ID: "subject", Label: tr(txt.HubsChatappsFieldSubject), More: true},
			{ID: "message", Label: tr(txt.HubsChatappsFieldMessage), Required: true},
		}}
	return c.a.hubForm(c.h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		target := v["to"]
		if other := strings.TrimSpace(v["other"]); other != "" {
			target = other
		}
		params := tuigateway.ChatAppsSendParams{Target: target, Message: v["message"], Subject: strings.TrimSpace(v["subject"])}
		return hubCall(c.a, c.h, tr(txt.HubsChatappsSending, target), tuigateway.ChatAppsSend, params, func(r *tuigateway.ChatAppsResult) tea.Cmd {
			if len(r.Lines) > 0 {
				return c.h.Done(strings.Join(r.Lines, " "))
			}
			return c.h.Done(tr(txt.HubsChatappsSent, target))
		})
	})
}
