package tui

import (
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Interval between two reads of an open room
var roomPollEvery = time.Second

// Thread every message of a room window is sent on
const roomThread = "main"

// Prefix of the composer session id that routes a sent line to its room window
const roomSessionPrefix = "room:"

// Largest file a room message carries
const roomFileLimit = 15_000_000

// One file uploaded for the next message, its manifest item and name
type roomUpload struct {
	item tuigateway.RoomAttachmentItem
	name string
}

// Window of one room, its live log above what waits, its state and a composer
type roomCard struct {
	app      *app
	room     roomInfo
	self     string
	events   []tuigateway.RoomEvent
	cursor   int64
	status   *tuigateway.RoomDriverStatus
	replica  *tuigateway.GroupsReplicaStateResult
	uploads  []roomUpload
	composer *cards.Composer
	scroll   int
	gone     string
	reading  bool
	closed   bool
	seen     map[int64]bool
	// First content row of the composer, for its cursor and clicks
	composerTop int
	lines       int
}

var _ interface {
	wm.Card
	wm.Keyed
	wm.Glypher
	wm.Hinter
	wm.Contexter
	wm.Sized
	wm.Selectable
	wm.Statuser
	wm.Cursorer
} = (*roomCard)(nil)

// Opens the window of a room, focusing it when it is open already
func (a *app) openRoom(r roomInfo, self string) tea.Cmd {
	if card, _ := a.wm.Find(roomKey(r.ID)); card != nil {
		return a.wm.Focus(card)
	}
	c := &roomCard{app: a, room: r, self: self, seen: map[int64]bool{}}
	c.composer = cards.NewComposer(a.uiTheme(), a.keys, "", a.store.sessionCwd)
	c.composer.Literal = true
	c.composer.SessionID = roomSessionPrefix + r.ID
	c.composer.Hint = tr(txt.RoomComposerHint)
	return a.wm.OpenHere(c)
}

// Window manager key of a room window
func roomKey(roomID string) string { return "tui:room:" + roomID }

// Open room window a composer session id names, nil for any other
func (a *app) roomCardOf(session string) *roomCard {
	id, ok := strings.CutPrefix(session, roomSessionPrefix)
	if !ok {
		return nil
	}
	card, _ := a.wm.Find(roomKey(id))
	c, _ := card.(*roomCard)
	return c
}

func (c *roomCard) Title() string {
	if c.room.Replica {
		return tr(txt.RoomTitleShared, c.room.Name, c.room.Home)
	}
	return c.room.Name
}

func (c *roomCard) Key() string         { return roomKey(c.room.ID) }
func (c *roomCard) Glyph() string       { return c.app.toolkit.Glyphs.Agent }
func (c *roomCard) Selectable() bool    { return true }
func (c *roomCard) MinSize() (int, int) { return 40, 10 }
func (c *roomCard) Init() tea.Cmd       { return c.read() }

// Room keys first, then the composer and transcript keys of a session window
func (c *roomCard) Contexts() []string {
	if c.composer.Completing() {
		return []string{roomContext, keys.Complete, keys.Session}
	}
	return []string{roomContext, keys.Session}
}

func (c *roomCard) Hints() []wm.Hint {
	if c.gone != "" {
		if len(c.files()) > 0 {
			return []wm.Hint{{Action: "tui.room.files"}}
		}
		return nil
	}
	out := []wm.Hint{{Action: "composer.send"}, {Action: "tui.room.attach"}}
	if len(c.pendingActions()) > 0 {
		out = append(out, wm.Hint{Action: "tui.room.pending"})
	}
	if len(c.files()) > 0 {
		out = append(out, wm.Hint{Action: "tui.room.files"})
	}
	return append(out, wm.Hint{Action: "tui.room.menu"}, wm.Hint{Action: "tui.room.stop"})
}

// Trailer of the window, what the room's driver is doing or what waits for the home
func (c *roomCard) Status() string {
	if c.gone != "" {
		return c.gone
	}
	parts := []string{}
	if c.status != nil {
		switch {
		case c.status.Blocked:
			parts = append(parts, tr(txt.RoomStatusBlocked))
		case c.status.Working:
			parts = append(parts, tr(txt.RoomStatusWorking))
		}
		if n := len(c.pendingActions()); n > 0 {
			parts = append(parts, tr(txt.RoomStatusWaiting, float64(n)))
		}
	}
	if c.replica != nil {
		if n := val(c.replica.Pending); n > 0 {
			parts = append(parts, tr(txt.RoomStatusQueued, float64(n), c.room.Home))
		}
		if synced := val(c.replica.SyncedAt); synced > 0 {
			parts = append(parts, tr(txt.RoomStatusSynced, whenText(synced)))
		}
	}
	return strings.Join(parts, " · ")
}

// Reads the room's state, then its log after the cursor, then the replica state of a shared room, and schedules the next read
func (c *roomCard) read() tea.Cmd {
	if c.closed || c.reading {
		return nil
	}
	c.reading = true
	params := tuigateway.GroupsStateParams{RoomID: c.room.ID, IncludeDisbanded: ptr(true)}
	return call(c.app.gw, tuigateway.GroupsState, params, func(res *tuigateway.GroupsStateResult, err error) tea.Cmd {
		if err != nil {
			c.reading = false
			c.gone = tr(txt.RoomsErrorLine, roomErrorText(err))
			return c.next()
		}
		c.room = roomOf(res.Room)
		c.status = res.DriverStatus
		if c.room.Disbanded {
			c.gone = tr(txt.RoomDisbanded)
		}
		return c.readLog()
	})
}

// Reads log pages after the cursor until the room has none left
func (c *roomCard) readLog() tea.Cmd {
	params := tuigateway.GroupsLogParams{RoomID: c.room.ID, SinceSeq: ptr(c.cursor), IncludeDisbanded: ptr(true)}
	return call(c.app.gw, tuigateway.GroupsLog, params, func(page *tuigateway.GroupsLogResult, err error) tea.Cmd {
		if err != nil {
			c.reading = false
			c.gone = tr(txt.RoomsErrorLine, roomErrorText(err))
			return c.next()
		}
		c.events = append(c.events, page.Events...)
		c.cursor = page.Cursor
		if page.HasMore && len(page.Events) > 0 {
			return c.readLog()
		}
		if !c.room.Replica {
			c.reading = false
			c.replica = nil
			return c.next()
		}
		return call(c.app.gw, tuigateway.GroupsReplicaState, tuigateway.GroupsReplicaStateParams{RoomID: c.room.ID}, func(res *tuigateway.GroupsReplicaStateResult, err error) tea.Cmd {
			c.reading = false
			if err != nil {
				return tea.Batch(wm.Notify(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error), c.next())
			}
			c.replica = res
			return tea.Batch(c.refusals(), c.next())
		})
	})
}

// Toasts each operation the home refused that this window has not shown yet
func (c *roomCard) refusals() tea.Cmd {
	var cmds []tea.Cmd
	for _, row := range c.replica.Errors {
		if c.seen[row.OutboxID] {
			continue
		}
		c.seen[row.OutboxID] = true
		cmds = append(cmds, wm.Notify(tr(txt.RoomNoticeRefused, row.Op, row.Error), wm.Warn))
	}
	return tea.Batch(cmds...)
}

// Next read of the room while its window is open and the room lives
func (c *roomCard) next() tea.Cmd {
	if c.closed {
		return nil
	}
	return tea.Tick(roomPollEvery, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			if card, _ := c.app.wm.Find(c.Key()); card != c {
				c.closed = true
				return nil
			}
			return c.read()
		}}
	})
}

// Pending actions the room's driver reports
func (c *roomCard) pendingActions() []tuigateway.RoomPendingAction {
	if c.status == nil {
		return nil
	}
	return c.status.PendingActions
}

// Member label of a member id, the id when the roster does not seat it
func (c *roomCard) memberLabel(id string) string {
	for _, m := range c.room.Members {
		if m.ID == id {
			return m.label()
		}
	}
	return id
}

// Who wrote a user message, you for this gateway's user
func (c *roomCard) userLabel(actor tuigateway.RoomActor) string {
	id := actor.ID
	if (id == "local-user" && !c.room.Replica) || (c.self != "" && id == "peer:"+c.self) {
		return tr(txt.RoomYou)
	}
	if name := val(actor.DisplayName); name != "" {
		return tr(txt.RoomPeerUser, name)
	}
	if id == "local-user" {
		return tr(txt.RoomPeerUser, c.room.Home)
	}
	return id
}

// Lines of one log event, a message with its author and files, a turn or room change as a dim note
func (c *roomCard) eventLines(event tuigateway.RoomEvent) []string {
	t := c.app.toolkit
	actor := event.Actor
	turn, _ := tuigateway.RoomPayload[tuigateway.RoomTurnPayload](event)
	member := func() string {
		if turn.MemberID != "" {
			return c.memberLabel(turn.MemberID)
		}
		if actor.ID != "" {
			return c.memberLabel(actor.ID)
		}
		return ""
	}
	switch event.Kind {
	case "message.user", "message.member":
		message, _ := tuigateway.RoomPayload[tuigateway.RoomMessagePayload](event)
		who := c.userLabel(actor)
		if event.Kind == "message.member" {
			who = c.memberLabel(actor.ID)
			if name := val(actor.DisplayName); name != "" {
				who = name
			}
		}
		out := []string{render.Heading(who, t) + "  " + render.Dim(whenText(event.CreatedAt), t)}
		for _, line := range strings.Split(message.Text, "\n") {
			out = append(out, "  "+line)
		}
		for _, item := range message.Attachments {
			out = append(out, "  "+render.Warm(tr(txt.RoomFile, item.Name, render.Bytes(item.Size)), t))
		}
		return out
	case "turn.started":
		return []string{render.Dim(tr(txt.RoomEventTurnStarted, member()), t)}
	case "turn.failed":
		return []string{render.Bad(tr(txt.RoomEventTurnFailed, member(), turn.Error), t)}
	case "turn.cancelled":
		return []string{render.Dim(tr(txt.RoomEventTurnCancelled, member()), t)}
	case "turn.deferred":
		if turn.Reason != "" {
			return []string{render.Dim(tr(txt.RoomEventTurnDeferredReason, member(), turn.Reason), t)}
		}
		return []string{render.Dim(tr(txt.RoomEventTurnDeferred, member()), t)}
	case "member.unavailable":
		return []string{render.Bad(tr(txt.RoomEventUnavailable, member()), t)}
	case "room.created":
		return []string{render.Dim(tr(txt.RoomEventCreated), t)}
	case "room.renamed":
		renamed, _ := tuigateway.RoomPayload[tuigateway.RoomRenamedPayload](event)
		return []string{render.Dim(tr(txt.RoomEventRenamed, renamed.Name), t)}
	case "room.stop_requested":
		return []string{render.Dim(tr(txt.RoomEventStopped), t)}
	case "room.disbanded":
		return []string{render.Dim(tr(txt.RoomEventDisbanded), t)}
	case "authority.claimed", "authority.lost":
		claim, _ := tuigateway.RoomPayload[tuigateway.RoomAuthorityPayload](event)
		return []string{render.Dim(tr(txt.RoomEventAuthority, claim.AuthorityGatewayID), t)}
	}
	return nil
}

func (c *roomCard) View(w, h int) string {
	c.composer.Fit(w)
	var chrome []string
	if len(c.uploads) > 0 {
		names := make([]string, len(c.uploads))
		for i, u := range c.uploads {
			names[i] = u.name
		}
		chrome = append(chrome, render.Fit(" "+render.Warm(tr(txt.RoomAttached, strings.Join(names, ", ")), c.app.toolkit), w, c.app.toolkit))
	}
	if n := len(c.pendingActions()); n > 0 {
		chrome = append(chrome, render.Fit(" "+render.Warm(tr(txt.RoomWaitingLine, float64(n), c.app.keys.Display("tui.room.pending")), c.app.toolkit), w, c.app.toolkit))
	}
	composerRows := min(c.composer.Height(), max(h-len(chrome)-3, 2))
	bodyRows := max(h-len(chrome)-composerRows, 1)
	var lines []string
	for _, event := range c.events {
		for _, line := range c.eventLines(event) {
			lines = append(lines, strings.Split(ansi.Wrap(line, max(w-2, 8), ""), "\n")...)
		}
	}
	if len(lines) == 0 {
		lines = []string{render.Dim(tr(txt.RoomEmpty), c.app.toolkit)}
	}
	c.lines = len(lines)
	c.scroll = min(max(c.scroll, 0), max(len(lines)-bodyRows, 0))
	end := len(lines) - c.scroll
	start := max(end-bodyRows, 0)
	shown := append([]string{}, lines[start:end]...)
	for len(shown) < bodyRows {
		shown = append([]string{""}, shown...)
	}
	for i := range shown {
		shown[i] = " " + shown[i]
	}
	c.composerTop = bodyRows + len(chrome)
	parts := append(shown, chrome...)
	parts = append(parts, c.composer.View(w, composerRows))
	return strings.Join(parts, "\n")
}

// Cursor of the composer, moved below the log
func (c *roomCard) Cursor() *tea.Cursor {
	cur := c.composer.Cursor()
	if cur == nil {
		return nil
	}
	cur.Y += c.composerTop
	return cur
}

func (c *roomCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	switch m := m.(type) {
	case wm.FocusMsg:
		return c, c.composer.SetFocus(m.Focused)
	case wm.ActionMsg:
		return c, c.action(m.ID)
	case tea.KeyPressMsg, tea.PasteMsg:
		return c, c.composer.Update(m)
	case tea.MouseWheelMsg:
		switch m.Button {
		case tea.MouseWheelUp:
			c.scroll += 3
		case tea.MouseWheelDown:
			c.scroll -= 3
		}
	case tea.MouseClickMsg:
		if m.Button == tea.MouseLeft && m.Y >= c.composerTop {
			if cmd, ok := c.composer.Click(m.X, m.Y-c.composerTop); ok {
				return c, cmd
			}
		}
	case keys.ChangedMsg:
		c.composer.Rebind()
	}
	return c, nil
}

// Carries out a keymap action, the room ones here, scrolling of the log and the prompt ones in the composer
func (c *roomCard) action(id string) tea.Cmd {
	page := 10
	switch id {
	case "tui.room.attach":
		return c.askAttach()
	case "tui.room.files":
		return c.pickFile()
	case "tui.room.pending":
		return c.pickPending()
	case "tui.room.menu":
		return c.menu()
	case "tui.room.stop":
		if c.gone != "" {
			return nil
		}
		return c.app.stopRoom(c.room)
	case "session.scroll.up":
		c.scroll += page
	case "session.scroll.down":
		c.scroll -= page
	case "session.scroll.top":
		c.scroll = c.lines
	case "session.scroll.end":
		c.scroll = 0
	case "tui.scroll.line.up":
		c.scroll++
	case "tui.scroll.line.down":
		c.scroll--
	default:
		if strings.HasPrefix(id, "composer.") || strings.HasPrefix(id, "complete.") {
			return c.composer.Action(id)
		}
	}
	return nil
}

// Sends a typed line with the files uploaded for it, a slash command run by the app as the session runs it
func (c *roomCard) send(text string) tea.Cmd {
	if strings.HasPrefix(text, "/") {
		return c.app.slash(text, nil, false)
	}
	if c.gone != "" {
		return wm.Notify(c.gone, wm.Warn)
	}
	payload := tuigateway.RoomMessagePayload{Text: text, ThreadID: roomThread}
	for _, u := range c.uploads {
		payload.Attachments = append(payload.Attachments, u.item)
	}
	uploads := c.uploads
	c.uploads = nil
	c.scroll = 0
	params := tuigateway.GroupsSendParams{RoomID: c.room.ID, EventID: ptr("msg-" + uuid.NewString()), Payload: payload}
	return call(c.app.gw, tuigateway.GroupsSend, params, func(res *tuigateway.GroupsSendResult, err error) tea.Cmd {
		if err != nil {
			c.uploads = uploads
			return wm.Notify(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error)
		}
		if val(res.Queued) {
			return tea.Batch(wm.Notify(tr(txt.RoomsNoticeQueued, c.room.Home), wm.Info), c.read())
		}
		return c.read()
	})
}

// Asks the path of a file to attach to the next message
func (c *roomCard) askAttach() tea.Cmd {
	return c.app.wm.ShowOverlay(wm.NewPrompt(c.app.toolkit, tr(txt.RoomAttachPrompt), "", func(text string) tea.Cmd {
		path := strings.TrimSpace(text)
		if path == "" {
			return nil
		}
		return c.attach(path)
	}))
}

// Uploads one file to the room for the next message, its kind told by its type
func (c *roomCard) attach(path string) tea.Cmd {
	if strings.HasPrefix(path, "~/") {
		if dir, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(dir, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.app.store.sessionCwd(), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return wm.Notify(tr(txt.RoomsErrorLine, err.Error()), wm.Error)
	}
	if len(data) == 0 || len(data) > roomFileLimit {
		return wm.Notify(tr(txt.RoomAttachTooLarge, render.Bytes(roomFileLimit)), wm.Warn)
	}
	media := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if media == "" {
		media = http.DetectContentType(data)
	}
	media, _, _ = strings.Cut(media, ";")
	kind := "file"
	switch {
	case strings.HasPrefix(media, "image/"):
		kind = "image"
	case media == "application/pdf":
		kind = "pdf"
	}
	name := filepath.Base(path)
	params := tuigateway.GroupsAttachmentUploadParams{RoomID: c.room.ID, UploadID: "up-" + uuid.NewString(), Kind: kind, Name: name, Mime: media,
		DataBase64: base64.StdEncoding.EncodeToString(data)}
	return call(c.app.gw, tuigateway.GroupsAttachmentUpload, params, func(res *tuigateway.RoomAttachment, err error) tea.Cmd {
		if err != nil {
			return wm.Notify(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error)
		}
		item := tuigateway.RoomAttachmentItem{AttachmentID: res.AttachmentID, Kind: res.Kind, Mime: res.Mime, Name: res.Name, Size: res.Size}
		c.uploads = append(c.uploads, roomUpload{item: item, name: name})
		return wm.Notify(tr(txt.RoomNoticeAttached, name, render.Bytes(int64(len(data)))), wm.Success)
	})
}

// One file of the log, the event it came with and its manifest item
type roomFile struct {
	eventID string
	item    tuigateway.RoomAttachmentItem
}

// Files the log's messages carry, newest first
func (c *roomCard) files() []roomFile {
	var out []roomFile
	for i := len(c.events) - 1; i >= 0; i-- {
		event := c.events[i]
		message, _ := tuigateway.RoomPayload[tuigateway.RoomMessagePayload](event)
		for _, item := range message.Attachments {
			out = append(out, roomFile{eventID: event.EventID, item: item})
		}
	}
	return out
}

// Lists the log's files, picking one saves it to the project folder
func (c *roomCard) pickFile() tea.Cmd {
	files := c.files()
	if len(files) == 0 {
		return wm.Notify(tr(txt.RoomFilesNone), wm.Info)
	}
	items := make([]wm.Item, 0, len(files))
	for _, f := range files {
		items = append(items, wm.Item{ID: "room:file:" + f.item.AttachmentID, Title: f.item.Name, Detail: render.Bytes(f.item.Size),
			Run: func() tea.Cmd { return c.saveFile(f, 0) }})
	}
	picker := wm.NewPalette(c.app.toolkit, items)
	picker.Placeholder = tr(txt.RoomFilesPick)
	return c.app.wm.ShowOverlay(picker)
}

// Downloads a file of the log into the project folder, asking again while a shared room's home sends it
func (c *roomCard) saveFile(f roomFile, tries int) tea.Cmd {
	params := tuigateway.GroupsAttachmentDownloadParams{RoomID: c.room.ID, EventID: f.eventID, AttachmentID: f.item.AttachmentID}
	return call(c.app.gw, tuigateway.GroupsAttachmentDownload, params, func(res *tuigateway.RoomAttachmentBytes, err error) tea.Cmd {
		if err != nil {
			if shapeOf(err).data["reason"] == "attachment_fetching" && tries < 120 {
				var notice tea.Cmd
				if tries == 0 {
					notice = wm.Notify(tr(txt.RoomFilesFetching, c.room.Home), wm.Info)
				}
				return tea.Batch(notice, tea.Tick(roomPollEvery, func(time.Time) tea.Msg {
					return asyncDoneMsg{run: func() tea.Cmd { return c.saveFile(f, tries+1) }}
				}))
			}
			return wm.Notify(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error)
		}
		data, err := base64.StdEncoding.DecodeString(res.DataBase64)
		if err != nil {
			return wm.Notify(tr(txt.RoomsErrorLine, err.Error()), wm.Error)
		}
		dir := c.app.store.sessionCwd()
		target := filepath.Join(dir, filepath.Base(res.Name))
		for n := 1; ; n++ {
			if _, err := os.Stat(target); os.IsNotExist(err) {
				break
			}
			ext := filepath.Ext(res.Name)
			target = filepath.Join(dir, fmt.Sprintf("%s-%d%s", strings.TrimSuffix(filepath.Base(res.Name), ext), n, ext))
		}
		tmp := target + ".part"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return wm.Notify(tr(txt.RoomsErrorLine, err.Error()), wm.Error)
		}
		if err := os.Rename(tmp, target); err != nil {
			return wm.Notify(tr(txt.RoomsErrorLine, err.Error()), wm.Error)
		}
		return wm.Notify(tr(txt.RoomFilesSaved, target), wm.Success)
	})
}

// Lists what waits in the room, an approval answered once or denied, a stuck turn retried or discarded
func (c *roomCard) pickPending() tea.Cmd {
	actions := c.pendingActions()
	if len(actions) == 0 {
		return wm.Notify(tr(txt.RoomPendingNone), wm.Info)
	}
	items := []wm.Item{}
	for i, action := range actions {
		member := c.memberLabel(action.MemberID)
		key := fmt.Sprintf("room:pending:%d:", i)
		if action.Kind == "approval" {
			what := ""
			if action.Approval != nil {
				what = action.Approval.Description
				if action.Approval.Command != "" {
					what = action.Approval.Command
				}
			}
			for _, verdict := range []struct {
				choice tuigateway.ApprovalChoice
				title  txt.Key
			}{{"once", txt.RoomPendingApprovalOnce}, {"deny", txt.RoomPendingApprovalDeny}} {
				choice := verdict.choice
				params := tuigateway.GroupsApproveParams{RoomID: c.room.ID, MemberID: action.MemberID, TaskID: action.TaskID,
					ExecutionGeneration: action.ExecutionGeneration, Choice: choice, RequestID: action.RequestID}
				items = append(items, wm.Item{ID: key + string(choice), Title: tr(verdict.title, member), Detail: what,
					Run: func() tea.Cmd {
						return resolveRoom(c, tuigateway.GroupsApprove, params, func(r *tuigateway.GroupsApproveResult) bool { return val(r.Queued) },
							tr(txt.RoomNoticeAnswered, member))
					}})
			}
			continue
		}
		detail := action.TaskID
		if action.Reason != "" {
			detail = tr(txt.RoomPendingWaitingReason, action.Reason)
		}
		retry := tuigateway.GroupsRetryParams{RoomID: c.room.ID, TaskID: action.TaskID, MemberID: ptr(action.MemberID), ExecutionGeneration: ptr(action.ExecutionGeneration)}
		discard := tuigateway.GroupsDiscardParams{RoomID: c.room.ID, MemberID: action.MemberID, TaskID: action.TaskID, ExecutionGeneration: action.ExecutionGeneration}
		items = append(items,
			wm.Item{ID: key + "retry", Title: tr(txt.RoomPendingRetry, member), Detail: detail,
				Run: func() tea.Cmd {
					return resolveRoom(c, tuigateway.GroupsRetry, retry, func(r *tuigateway.GroupsRetryResult) bool { return val(r.Queued) },
						tr(txt.RoomNoticeRetried, member))
				}},
			wm.Item{ID: key + "discard", Title: tr(txt.RoomPendingDiscard, member), Detail: detail,
				Run: func() tea.Cmd {
					return resolveRoom(c, tuigateway.GroupsDiscard, discard, func(r *tuigateway.GroupsDiscardResult) bool { return val(r.Queued) },
						tr(txt.RoomNoticeDiscarded, member))
				}})
	}
	picker := wm.NewPalette(c.app.toolkit, items)
	picker.Placeholder = tr(txt.RoomPendingPick)
	return c.app.wm.ShowOverlay(picker)
}

// Answers one pending action and reads the room again, a replica's answer noted as queued for the home
func resolveRoom[P, R any](c *roomCard, m tuigateway.Method[P, R], params P, queued func(*R) bool, notice string) tea.Cmd {
	return call(c.app.gw, m, params, func(res *R, err error) tea.Cmd {
		if err != nil {
			return wm.Notify(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error)
		}
		if queued(res) {
			notice = tr(txt.RoomsNoticeQueued, c.room.Home)
		}
		return tea.Batch(wm.Notify(notice, wm.Success), c.read())
	})
}

// Room actions of the window, rename, stop, peers, disband and for a shared room continue here
func (c *roomCard) menu() tea.Cmd {
	if c.room.Disbanded {
		return wm.Notify(c.gone, wm.Info)
	}
	back := func(notice string, level wm.Level) tea.Cmd {
		if notice == "" {
			return c.read()
		}
		return tea.Batch(wm.Notify(notice, level), c.read())
	}
	items := []wm.Item{
		{ID: "room:rename", Title: tr(txt.RoomMenuRename), Run: func() tea.Cmd { return c.app.renameRoom(c.room, back) }},
		{ID: "room:stop", Title: tr(txt.RoomMenuStop), Run: func() tea.Cmd { return c.app.stopRoom(c.room) }},
	}
	if !c.room.Replica {
		items = append(items, wm.Item{ID: "room:peers", Title: tr(txt.RoomMenuPeers), Run: func() tea.Cmd { return c.app.roomPeers(c.room, back) }})
	} else {
		items = append(items, wm.Item{ID: "room:promote", Title: tr(txt.RoomMenuPromote, c.room.Home), Run: func() tea.Cmd {
			return c.confirm(tr(txt.RoomsPromoteConfirm, c.room.Name, c.room.Home), func() tea.Cmd {
				return call(c.app.gw, tuigateway.GroupsPromote, tuigateway.GroupsPromoteParams{RoomID: c.room.ID, Confirm: ptr(true)}, func(_ *tuigateway.GroupsPromoteResult, err error) tea.Cmd {
					if err != nil {
						return wm.Notify(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error)
					}
					c.room.Replica, c.replica = false, nil
					return back(tr(txt.RoomsNoticePromoted, c.room.Name), wm.Success)
				})
			})
		}})
	}
	items = append(items, wm.Item{ID: "room:disband", Title: tr(txt.RoomMenuDisband), Run: func() tea.Cmd {
		return c.confirm(tr(txt.RoomsDisbandConfirm, c.room.Name), func() tea.Cmd { return c.app.disbandRoom(c.room, back) })
	}})
	picker := wm.NewPalette(c.app.toolkit, items)
	picker.Placeholder = c.room.Name
	return c.app.wm.ShowOverlay(picker)
}

// Asks a yes or no question before running yes
func (c *roomCard) confirm(question string, yes func() tea.Cmd) tea.Cmd {
	items := []wm.Item{
		{ID: "room:confirm:yes", Title: tr(txt.RoomConfirmYes), Run: yes},
		{ID: "room:confirm:no", Title: tr(txt.RoomConfirmNo), Run: func() tea.Cmd { return nil }},
	}
	picker := wm.NewPalette(c.app.toolkit, items)
	picker.Placeholder = question
	return c.app.wm.ShowOverlay(picker)
}
