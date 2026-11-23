package tui

import (
	"sort"
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

// Keymap context of the sessions screen
const sessionsContext = "tui.sessions"

// Keys of the sessions screen beyond the shared hub keys
func registerSessionsKeys(km *keys.Map) {
	if keys.ContextDocs[sessionsContext] == "" {
		if err := keys.AddContext(sessionsContext, "sessions screen"); err != nil {
			panic(err)
		}
	}
	for _, a := range []keys.Action{
		{ID: "tui.sessions.beside", Context: sessionsContext, Label: "beside", Doc: "open the session in a new card beside this one", Default: []string{"b"}},
		{ID: "tui.sessions.space", Context: sessionsContext, Label: "new space", Doc: "open the session in a new space", Default: []string{"c"}},
		{ID: "tui.sessions.pin", Context: sessionsContext, Label: "pin", Doc: "pin the session to the top of the list, or unpin it", Default: []string{"p"}},
		{ID: "tui.sessions.archive", Context: sessionsContext, Label: "archive", Doc: "archive the session out of the list, or bring it back", Default: []string{"A"}},
		{ID: "tui.sessions.archived", Context: sessionsContext, Label: "archived", Doc: "show or hide the archived sessions", Default: []string{"tab"}},
		{ID: "tui.sessions.export", Context: sessionsContext, Label: "export", Doc: "save the session as a Markdown, JSON or HTML file", Default: []string{"s"}},
		{ID: "tui.sessions.prune", Context: sessionsContext, Label: "prune", Doc: "delete old stored sessions in one go, after a confirmation", Default: []string{"P"}},
	} {
		km.Register(a)
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "sessions", order: 10, title: txt.HubsTitleSessions, short: txt.HubsShortSessions, slash: "sessions",
		usage: "[session id|words to search]", run: (*app).sessionsSlash,
		open: func(a *app, query string) tea.Cmd { return a.openSessions(query) }, keys: registerSessionsKeys})
}

// /sessions: the sessions screen narrowed to the words, or the session with the id resumed here
func (a *app) sessionsSlash(r *slashRun) tea.Cmd {
	arg := trimJS(r.arg)
	switch {
	case arg != "" && !strings.ContainsAny(arg, " \t") && (strings.HasPrefix(arg, "local-") || len(arg) >= 16):
		if a.life.guardBusySessionSwitch(tr(txt.SlashCmdSessionSessionsBusyGuardAction)) {
			return nil
		}
		return tea.Batch(r.ran(), a.resumeByID(arg, nil))
	}
	return tea.Batch(r.ran(), a.openSessions(arg))
}

// One session of the sessions screen
type sessionRow struct {
	id      string
	title   string
	preview string
	source  string
	model   string
	when    float64
	count   int64
	live    bool
	running bool
	pinned  bool
	// Archived, listed only while the archived ones are shown
	archived bool
}

// Name a session shows under, its title or its short id
func (r sessionRow) name() string {
	if r.title != "" {
		return r.title
	}
	return sessionLabel(r.id)
}

// Sessions screen: every live and stored session with its last message, opened here, beside or in a new space, renamed, pinned, archived,
// exported, deleted and pruned, read again whenever the sessions of the background service change
func (a *app) openSessions(query string) tea.Cmd {
	var (
		rows     []sessionRow
		h        *cards.Hub
		list     *cards.List
		archived bool
		// Live rows and stored rows as they last loaded, each kept through a failed reading of the other
		live, stored []tuigateway.CanonicalSessionListRow
	)
	// Cards of this window showing a session, by session id
	shown := func() map[string]*app {
		out := map[string]*app{}
		for _, s := range a.shell.sessions {
			if sid := s.store.ui.Sid; sid != "" {
				out[sid] = s
			}
		}
		return out
	}
	place := func() {
		anchor := ""
		if i := list.Index(); i >= 0 && i < len(rows) {
			anchor = rows[i].id
		}
		seen := map[string]bool{}
		var next []sessionRow
		add := func(src []tuigateway.CanonicalSessionListRow, isLive bool) {
			for _, s := range src {
				id := firstText(s.ID, s.SessionID)
				if id == "" || seen[id] || (!isLive && s.MessageCount == 0 && shown()[id] == nil) {
					continue
				}
				seen[id] = true
				when := val(s.LastActive)
				if when == 0 {
					when = val(s.StartedAt)
				}
				next = append(next, sessionRow{id: id, title: s.Title, preview: strings.Join(strings.Fields(val(s.Preview)), " "), source: val(s.Source),
					model: val(s.Model), when: when, count: s.MessageCount, live: isLive, running: s.Running, pinned: s.Pinned, archived: s.Archived})
			}
		}
		add(live, true)
		add(stored, false)
		sort.SliceStable(next, func(i, j int) bool {
			x, y := next[i], next[j]
			switch {
			case x.archived != y.archived:
				return !x.archived
			case x.pinned != y.pinned:
				return x.pinned
			case x.live != y.live:
				return x.live
			}
			return x.when > y.when
		})
		rows = next
		list.Refresh()
		for i, r := range rows {
			if r.id == anchor || (anchor == "" && r.id == a.store.ui.Sid) {
				list.Select(i)
			}
		}
	}
	load := func(storedToo bool) tea.Cmd {
		scope := tuigateway.CanonicalListParamsScope("live")
		return call(a.gw, tuigateway.CanonicalSessionList, tuigateway.CanonicalListParams{Scope: &scope, Limit: ptr(int64(200))}, func(r *tuigateway.CanonicalListResult, err error) tea.Cmd {
			if err != nil {
				return h.Fail(rpcErrorMessage(err))
			}
			live = r.Sessions
			if !storedToo {
				place()
				return nil
			}
			scope := tuigateway.CanonicalListParamsScope("stored")
			params := tuigateway.CanonicalListParams{Scope: &scope, Limit: ptr(int64(200)), IncludeArchived: archived}
			return call(a.gw, tuigateway.CanonicalSessionList, params, func(r *tuigateway.CanonicalListResult, err error) tea.Cmd {
				if err != nil {
					place()
					return h.Fail(tr(txt.PickersSessionErrorsCouldNotLoadResumable))
				}
				stored = r.Sessions
				place()
				return nil
			})
		})
	}
	at := func(i int) (sessionRow, bool) {
		if i < 0 || i >= len(rows) {
			return sessionRow{}, false
		}
		return rows[i], true
	}
	// Changes the session's own record through the background service, the list read again after
	mutate := func(r sessionRow, operation string, payload map[string]any, done string) tea.Cmd {
		h.Busy(tr(txt.HubsSessionsSaving, r.name()))
		return a.mutateStoredSession(r.id, operation, payload, func(err error) tea.Cmd {
			if err != nil {
				return h.Fail(rpcErrorMessage(err))
			}
			return tea.Batch(h.Done(done), load(true))
		})
	}
	openHere := func(r sessionRow) tea.Cmd {
		dismiss := wm.RequestDismissOf(h)
		switch other := shown()[r.id]; {
		case other == a:
			return dismiss
		case other != nil:
			return tea.Batch(dismiss, a.shell.openSession(r.id, false))
		case r.live:
			if a.store.ui.Busy {
				a.leftRunning(a.store.ui.Sid)
			}
			return tea.Batch(dismiss, a.life.activateLiveSession(r.id))
		case a.life.guardBusySessionSwitch(tr(txt.SessionLifecycleSwitchSessions)):
			return dismiss
		}
		return tea.Batch(dismiss, a.resumeByID(r.id, nil))
	}
	rename := func(_ *cards.List, i int) tea.Cmd {
		r, ok := at(i)
		if !ok {
			return nil
		}
		return a.hubAsk(h, tr(txt.HubsSessionsRenameTitle, r.name()), tr(txt.HubsSessionsRenameLabel), r.title, false, func(title string) tea.Cmd {
			if title == "" || title == r.title {
				return nil
			}
			return mutate(r, "rename", map[string]any{"title": title}, tr(txt.HubsSessionsRenamed, title))
		})
	}
	pin := func(_ *cards.List, i int) tea.Cmd {
		r, ok := at(i)
		if !ok {
			return nil
		}
		done := tr(txt.HubsSessionsPinned, r.name())
		if r.pinned {
			done = tr(txt.HubsSessionsUnpinned, r.name())
		}
		return mutate(r, "sidebar", map[string]any{"pinned": !r.pinned}, done)
	}
	archive := func(_ *cards.List, i int) tea.Cmd {
		r, ok := at(i)
		if !ok {
			return nil
		}
		if shown()[r.id] != nil && !r.archived {
			return h.Fail(tr(txt.HubsSessionsArchiveOpen, r.name()))
		}
		done := tr(txt.HubsSessionsArchived, r.name())
		if r.archived {
			done = tr(txt.HubsSessionsUnarchived, r.name())
		}
		return mutate(r, "archive", map[string]any{"archived": !r.archived}, done)
	}
	remove := func(l *cards.List, i int) tea.Cmd {
		r, ok := at(i)
		if !ok {
			return nil
		}
		if shown()[r.id] != nil {
			return h.Fail(tr(txt.HubsSessionsOpenInCard, r.name()))
		}
		if r.live {
			l.Confirm(tr(txt.HubsSessionsCloseConfirm, r.name()), func() tea.Cmd {
				h.Busy(tr(txt.PickersSessionRowClosing))
				return a.closeBackgroundSession(r.id, func(err error) tea.Cmd {
					if err != nil {
						return h.Fail(rpcErrorMessage(err))
					}
					return tea.Batch(h.Done(tr(txt.HubsSessionsClosed, r.name())), load(true))
				})
			})
			return nil
		}
		l.Confirm(tr(txt.PickersSessionDeleteConfirm, r.name()), func() tea.Cmd {
			h.Busy(tr(txt.HubsSessionsDeleting, r.name()))
			return a.deleteStoredSession(r.id, func(err error) tea.Cmd {
				if err != nil {
					return h.Fail(rpcErrorMessage(err))
				}
				return tea.Batch(h.Done(tr(txt.HubsSessionsDeleted, r.name())), load(true))
			})
		})
		return nil
	}
	export := func(_ *cards.List, i int) tea.Cmd {
		r, ok := at(i)
		if !ok {
			return nil
		}
		spec := cards.FormSpec{Title: tr(txt.HubsSessionsExportTitle, r.name()), Submit: tr(txt.HubsSessionsExportSubmit), Fields: []cards.Field{
			{ID: "format", Label: tr(txt.HubsSessionsExportFormat), Kind: cards.FieldChoice, Value: "md", Choices: []cards.Choice{
				{Value: "md", Label: "Markdown", Meaning: tr(txt.HubsSessionsExportMd)}, {Value: "json", Label: "JSON", Meaning: tr(txt.HubsSessionsExportJSON)},
				{Value: "html", Label: "HTML", Meaning: tr(txt.HubsSessionsExportHTML)}}},
			{ID: "file", Label: tr(txt.HubsSessionsExportFile), Kind: cards.FieldText, Placeholder: tr(txt.HubsSessionsExportFileHint)},
			{ID: "redact", Label: tr(txt.HubsSessionsExportRedact), Kind: cards.FieldBool, Value: "true", Help: tr(txt.HubsSessionsExportRedactHelp)},
		}}
		return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
			format := v["format"]
			params := tuigateway.SessionExportParams{SessionID: r.id, Format: &format, Redact: v["redact"] == "true"}
			return hubCall(a, h, tr(txt.HubsSessionsExporting, r.name()), tuigateway.CanonicalSessionExport, params, func(res *tuigateway.SessionExportResult) tea.Cmd {
				path, err := writeSessionExport(res, strings.TrimSpace(v["file"]), a.store.sessionCwd())
				if err != nil {
					return h.Fail(err.Error())
				}
				return h.Done(tr(txt.HubsSessionsExported, path))
			})
		})
	}
	prune := func(*cards.List, int) tea.Cmd {
		spec := cards.FormSpec{Title: tr(txt.HubsSessionsPruneTitle), Submit: tr(txt.HubsSessionsPruneSubmit),
			Intro: []string{tr(txt.HubsSessionsPruneIntro)},
			Fields: []cards.Field{
				{ID: "days", Label: tr(txt.HubsSessionsPruneDays), Kind: cards.FieldNumber, Value: "30", Required: true, Help: tr(txt.HubsSessionsPruneDaysHelp)},
				{ID: "archived", Label: tr(txt.HubsSessionsPruneArchived), Kind: cards.FieldBool, Value: "false", Help: tr(txt.HubsSessionsPruneArchivedHelp)},
				{ID: "pinned", Label: tr(txt.HubsSessionsPrunePinned), Kind: cards.FieldBool, Value: "false"},
			}}
		return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
			days, err := strconv.ParseFloat(strings.TrimSpace(v["days"]), 64)
			if err != nil || days < 0 {
				return h.Fail(tr(txt.KitFormNumber))
			}
			filters := map[string]any{"older_than_days": days, "include_pinned": v["pinned"] == "true"}
			if v["archived"] == "true" {
				filters["archived"] = true
			}
			ask := tr(txt.HubsSessionsPruneConfirm, strconv.FormatFloat(days, 'f', -1, 64))
			list.Confirm(ask, func() tea.Cmd {
				return hubCall(a, h, tr(txt.HubsSessionsPruning), tuigateway.CanonicalSessionPrune, tuigateway.SessionPruneParams{Filters: filters}, func(res *tuigateway.SessionPruneResult) tea.Cmd {
					return tea.Batch(h.Done(tr(txt.HubsSessionsPruned, res.Deleted)), load(true))
				})
			})
			return nil
		})
	}
	open := func(_ *cards.List, i int) tea.Cmd {
		r, ok := at(i)
		if !ok {
			return nil
		}
		return openHere(r)
	}
	t := a.toolkit
	list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleSessions), Glyph: t.Glyphs.Sessions, Key: "tui:sessions", Context: sessionsContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsColSession), Max: 36}, {Title: tr(txt.HubsColStartedWith), Flex: 1},
			{Title: tr(txt.HubsColMsgs), Right: true}, {Title: tr(txt.HubsColWhen), Right: true}},
		Yield: []int{3},
		Rows: func() [][]string {
			cards := shown()
			out := make([][]string, len(rows))
			for i, r := range rows {
				mark := " "
				switch other := cards[r.id]; {
				case other == a:
					mark = "●"
				case other != nil:
					mark = "◐"
				case r.running:
					mark = "▶"
				case r.live:
					mark = "◌"
				}
				if r.pinned {
					mark += "⚑"
				}
				name := r.name()
				count := ""
				if r.count > 0 {
					count = strconv.FormatInt(r.count, 10)
				}
				cells := []string{mark, name, r.preview, count, whenText(r.when)}
				if r.archived {
					for c := range cells {
						cells[c] = render.Dim(cells[c], t)
					}
				}
				out[i] = cells
			}
			return out
		},
		Detail: func(i int) []string {
			r, ok := at(i)
			if !ok {
				return nil
			}
			return sessionDetail(r, shown()[r.id] != nil)
		},
		Status: func() string {
			if archived {
				return tr(txt.HubsSessionsArchivedShown)
			}
			return tr(txt.HubsSessionsLegend)
		},
		Empty: func() string { return tr(txt.HubsSessionsEmpty, a.keys.Display("hub.add")) },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			out := []wm.Hint{}
			if r, ok := at(i); ok {
				out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, wm.Hint{Action: "tui.sessions.beside"})
				if r.live {
					out = append(out, wm.Hint{Action: "hub.remove", Label: tr(txt.HintsLabelClose)})
				} else {
					out = append(out, wm.Hint{Action: "hub.remove", Label: tr(txt.HintsLabelDelete)})
				}
				out = append(out, wm.Hint{Action: "hub.rename"}, wm.Hint{Action: "tui.sessions.pin"})
			}
			return append(out, wm.Hint{Action: "hub.add", Label: tr(txt.HintsLabelNew)}, wm.Hint{Action: "tui.sessions.archived"})
		},
		Help: func(i int) []string {
			r, ok := at(i)
			if !ok {
				return []string{tr(txt.HubsShortSessions)}
			}
			return append([]string{tr(txt.HubsShortSessions), ""}, sessionDetail(r, shown()[r.id] != nil)...)
		},
		Free: map[string]bool{"hub.add": true, "tui.sessions.archived": true, "tui.sessions.prune": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use": open,
			"hub.add": func(*cards.List, int) tea.Cmd { return tea.Batch(wm.RequestDismissOf(h), a.shell.openBeside()) },
			"tui.sessions.beside": func(_ *cards.List, i int) tea.Cmd {
				if r, ok := at(i); ok {
					return tea.Batch(wm.RequestDismissOf(h), a.shell.openSession(r.id, false))
				}
				return nil
			},
			"tui.sessions.space": func(_ *cards.List, i int) tea.Cmd {
				if r, ok := at(i); ok {
					return tea.Batch(wm.RequestDismissOf(h), a.shell.openSession(r.id, true))
				}
				return nil
			},
			"hub.rename":           rename,
			"hub.edit":             rename,
			"tui.sessions.pin":     pin,
			"tui.sessions.archive": archive,
			"tui.sessions.archived": func(*cards.List, int) tea.Cmd {
				archived = !archived
				return load(true)
			},
			"tui.sessions.export": export,
			"tui.sessions.prune":  prune,
			"hub.remove":          remove,
		},
	})
	h = a.newHub("tui:sessions", list)
	if query != "" {
		list.Filter(query)
	}
	a.switcherReload = func() tea.Cmd {
		if !h.Shown() {
			a.switcherReload, a.switcherPoll = nil, nil
			return nil
		}
		return load(true)
	}
	a.switcherPoll = func() tea.Cmd {
		if !h.Shown() {
			a.switcherReload, a.switcherPoll = nil, nil
			return nil
		}
		return load(false)
	}
	return tea.Batch(a.wm.ShowOverlay(h), load(true))
}

// Lines of a session's detail pane: its name, last message, where it runs and how big it is
func sessionDetail(r sessionRow, open bool) []string {
	out := []string{r.name()}
	if r.preview != "" {
		out = append(out, r.preview)
	}
	state := tr(txt.HubsSessionsStateStored)
	switch {
	case open:
		state = tr(txt.HubsSessionsStateOpen)
	case r.running:
		state = tr(txt.HubsSessionsStateWorking)
	case r.live:
		state = tr(txt.HubsSessionsStateLive)
	}
	if r.archived {
		state += " · " + tr(txt.HubsSessionsStateArchived)
	}
	if r.pinned {
		state += " · " + tr(txt.HubsSessionsStatePinned)
	}
	out = append(out, "", tr(txt.HubsSessionsDetailState, state))
	if r.model != "" {
		out = append(out, tr(txt.HubsSessionsDetailModel, r.model))
	}
	if r.source != "" {
		out = append(out, tr(txt.HubsSessionsDetailSource, r.source))
	}
	if r.count > 0 {
		out = append(out, tr(txt.HubsSessionsDetailMessages, r.count))
	}
	return append(out, tr(txt.HubsSessionsDetailID, r.id))
}

// Closes a session the background service runs that no card of this window shows: attached for a moment and let go with the close
// stamp, the service ending it once no one else views it
func (a *app) closeBackgroundSession(sid string, done func(err error) tea.Cmd) tea.Cmd {
	return call(a.gw, tuigateway.CanonicalSessionResume, tuigateway.CanonicalResumeParams{SessionID: sid, OmitMessages: true}, func(snap *tuigateway.CanonicalSnapshot, err error) tea.Cmd {
		if err != nil {
			return done(err)
		}
		params := tuigateway.SessionDetachParams{SessionID: firstText(snap.SessionID, sid), SubscriptionID: snap.SubscriptionID, EndReason: ptr("tui_close")}
		return call(a.gw, tuigateway.CanonicalSessionDetach, params, func(_ *tuigateway.SessionDetachResult, err error) tea.Cmd { return done(err) })
	})
}
