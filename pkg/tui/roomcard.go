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

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
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
	item map[string]any
	name string
}

// Window of one room, its live log above what waits, its state and a composer
type roomCard struct {
	app      *app
	room     roomInfo
	self     string
	events   []map[string]any
	cursor   int64
	status   map[string]any
	replica  map[string]any
	uploads  []roomUpload
	composer *cards.Composer
	scroll   int
	gone     string
	reading  bool
	closed   bool
	seen     map[float64]bool
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
	c := &roomCard{app: a, room: r, self: self, seen: map[float64]bool{}}
	c.composer = cards.NewComposer(a.uiTheme(), a.keys, "", a.store.sessionCwd)
	c.composer.Literal = true
	c.composer.SessionID = roomSessionPrefix + r.ID
	c.composer.Hint = tr("room.composerHint")
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
		return tr("room.titleShared", c.room.Name, c.room.Home)
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
		case c.status["blocked"] == true:
			parts = append(parts, tr("room.status.blocked"))
		case c.status["working"] == true:
			parts = append(parts, tr("room.status.working"))
		}
		if n := len(c.pendingActions()); n > 0 {
			parts = append(parts, tr("room.status.waiting", float64(n)))
		}
	}
	if c.replica != nil {
		if n := jsNumberOf(c.replica["pending"]); n > 0 {
			parts = append(parts, tr("room.status.queued", n, c.room.Home))
		}
		if synced := jsNumberOf(c.replica["synced_at"]); synced > 0 {
			parts = append(parts, tr("room.status.synced", whenText(synced)))
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
	return gatewayCall(c.app.gw, "groups.state", map[string]any{"room_id": c.room.ID, "include_disbanded": true}, func(v any, err error) tea.Cmd {
		if err != nil {
			c.reading = false
			c.gone = tr("rooms.errorLine", roomErrorText(err))
			return c.next()
		}
		res := asRPCResult(v)
		room, _ := res["room"].(map[string]any)
		c.room = roomOf(room)
		c.status, _ = res["driver_status"].(map[string]any)
		if c.room.Disbanded {
			c.gone = tr("room.disbanded")
		}
		return c.readLog()
	})
}

// Reads log pages after the cursor until the room has none left
func (c *roomCard) readLog() tea.Cmd {
	return gatewayCall(c.app.gw, "groups.log", map[string]any{"room_id": c.room.ID, "since_seq": c.cursor, "include_disbanded": true}, func(v any, err error) tea.Cmd {
		if err != nil {
			c.reading = false
			c.gone = tr("rooms.errorLine", roomErrorText(err))
			return c.next()
		}
		page := asRPCResult(v)
		rows, _ := page["events"].([]any)
		for _, raw := range rows {
			if event, ok := raw.(map[string]any); ok {
				c.events = append(c.events, event)
			}
		}
		c.cursor = int64(jsNumberOf(page["cursor"]))
		if page["has_more"] == true && len(rows) > 0 {
			return c.readLog()
		}
		if !c.room.Replica {
			c.reading = false
			c.replica = nil
			return c.next()
		}
		return gatewayCall(c.app.gw, "groups.replica_state", map[string]any{"room_id": c.room.ID}, func(v any, err error) tea.Cmd {
			c.reading = false
			if err != nil {
				return tea.Batch(wm.Notify(tr("rooms.errorLine", roomErrorText(err)), wm.Error), c.next())
			}
			c.replica = asRPCResult(v)
			return tea.Batch(c.refusals(), c.next())
		})
	})
}

// Toasts each operation the home refused that this window has not shown yet
func (c *roomCard) refusals() tea.Cmd {
	failed, _ := c.replica["errors"].([]any)
	var cmds []tea.Cmd
	for _, raw := range failed {
		row, _ := raw.(map[string]any)
		id := jsNumberOf(row["outbox_id"])
		if c.seen[id] {
			continue
		}
		c.seen[id] = true
		cmds = append(cmds, wm.Notify(tr("room.notice.refused", fmt.Sprint(row["op"]), fmt.Sprint(row["error"])), wm.Warn))
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
func (c *roomCard) pendingActions() []map[string]any {
	rows, _ := c.status["pending_actions"].([]any)
	out := make([]map[string]any, 0, len(rows))
	for _, raw := range rows {
		if row, ok := raw.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
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
func (c *roomCard) userLabel(actor map[string]any) string {
	id, _ := actor["id"].(string)
	if (id == "local-user" && !c.room.Replica) || (c.self != "" && id == "peer:"+c.self) {
		return tr("room.you")
	}
	if name, _ := actor["display_name"].(string); name != "" {
		return tr("room.peerUser", name)
	}
	if id == "local-user" {
		return tr("room.peerUser", c.room.Home)
	}
	return id
}

// Lines of one log event, a message with its author and files, a turn or room change as a dim note
func (c *roomCard) eventLines(event map[string]any) []string {
	t := c.app.toolkit
	kind, _ := event["kind"].(string)
	actor, _ := event["actor"].(map[string]any)
	payload, _ := event["payload"].(map[string]any)
	text, _ := payload["text"].(string)
	member := func() string {
		if id, _ := payload["member_id"].(string); id != "" {
			return c.memberLabel(id)
		}
		if id, _ := actor["id"].(string); id != "" {
			return c.memberLabel(id)
		}
		return ""
	}
	switch kind {
	case "message.user", "message.member":
		who := c.userLabel(actor)
		if kind == "message.member" {
			who = c.memberLabel(fmt.Sprint(actor["id"]))
			if name, _ := actor["display_name"].(string); name != "" {
				who = name
			}
		}
		out := []string{render.Heading(who, t) + "  " + render.Dim(whenText(jsNumberOf(event["created_at"])), t)}
		for _, line := range strings.Split(text, "\n") {
			out = append(out, "  "+line)
		}
		files, _ := payload["attachments"].([]any)
		for _, raw := range files {
			item, _ := raw.(map[string]any)
			out = append(out, "  "+render.Warm(tr("room.file", fmt.Sprint(item["name"]), render.Bytes(int64(jsNumberOf(item["size"])))), t))
		}
		return out
	case "turn.started":
		return []string{render.Dim(tr("room.event.turnStarted", member()), t)}
	case "turn.failed":
		return []string{render.Bad(tr("room.event.turnFailed", member(), fmt.Sprint(payload["error"])), t)}
	case "turn.cancelled":
		return []string{render.Dim(tr("room.event.turnCancelled", member()), t)}
	case "turn.deferred":
		return []string{render.Dim(tr("room.event.turnDeferred", member()), t)}
	case "member.unavailable":
		return []string{render.Bad(tr("room.event.unavailable", member()), t)}
	case "room.created":
		return []string{render.Dim(tr("room.event.created"), t)}
	case "room.renamed":
		return []string{render.Dim(tr("room.event.renamed", fmt.Sprint(payload["name"])), t)}
	case "room.stop_requested":
		return []string{render.Dim(tr("room.event.stopped"), t)}
	case "room.disbanded":
		return []string{render.Dim(tr("room.event.disbanded"), t)}
	case "authority.claimed", "authority.lost":
		return []string{render.Dim(tr("room.event.authority", fmt.Sprint(payload["authority_gateway_id"])), t)}
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
		chrome = append(chrome, render.Fit(" "+render.Warm(tr("room.attached", strings.Join(names, ", ")), c.app.toolkit), w, c.app.toolkit))
	}
	if n := len(c.pendingActions()); n > 0 {
		chrome = append(chrome, render.Fit(" "+render.Warm(tr("room.waitingLine", float64(n), c.app.keys.Display("tui.room.pending")), c.app.toolkit), w, c.app.toolkit))
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
		lines = []string{render.Dim(tr("room.empty"), c.app.toolkit)}
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
	payload := map[string]any{"text": text, "thread_id": roomThread}
	if len(c.uploads) > 0 {
		items := make([]any, len(c.uploads))
		for i, u := range c.uploads {
			items[i] = u.item
		}
		payload["attachments"] = items
	}
	uploads := c.uploads
	c.uploads = nil
	c.scroll = 0
	return gatewayCall(c.app.gw, "groups.send", map[string]any{"room_id": c.room.ID, "event_id": "msg-" + uuid.NewString(), "payload": payload}, func(v any, err error) tea.Cmd {
		if err != nil {
			c.uploads = uploads
			return wm.Notify(tr("rooms.errorLine", roomErrorText(err)), wm.Error)
		}
		if asRPCResult(v)["queued"] == true {
			return tea.Batch(wm.Notify(tr("rooms.notice.queued", c.room.Home), wm.Info), c.read())
		}
		return c.read()
	})
}

// Asks the path of a file to attach to the next message
func (c *roomCard) askAttach() tea.Cmd {
	return c.app.wm.ShowOverlay(wm.NewPrompt(c.app.toolkit, tr("room.attach.prompt"), "", func(text string) tea.Cmd {
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
		return wm.Notify(tr("rooms.errorLine", err.Error()), wm.Error)
	}
	if len(data) == 0 || len(data) > roomFileLimit {
		return wm.Notify(tr("room.attach.tooLarge", render.Bytes(roomFileLimit)), wm.Warn)
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
	params := map[string]any{"room_id": c.room.ID, "upload_id": "up-" + uuid.NewString(), "kind": kind, "name": name, "mime": media,
		"data_base64": base64.StdEncoding.EncodeToString(data)}
	return gatewayCall(c.app.gw, "groups.attachment.upload", params, func(v any, err error) tea.Cmd {
		if err != nil {
			return wm.Notify(tr("rooms.errorLine", roomErrorText(err)), wm.Error)
		}
		res := asRPCResult(v)
		item := map[string]any{"attachment_id": res["attachment_id"], "kind": res["kind"], "mime": res["mime"], "name": res["name"], "size": res["size"]}
		c.uploads = append(c.uploads, roomUpload{item: item, name: name})
		return wm.Notify(tr("room.notice.attached", name, render.Bytes(int64(len(data)))), wm.Success)
	})
}

// One file of the log, the event it came with and its manifest item
type roomFile struct {
	eventID string
	item    map[string]any
}

// Files the log's messages carry, newest first
func (c *roomCard) files() []roomFile {
	var out []roomFile
	for i := len(c.events) - 1; i >= 0; i-- {
		event := c.events[i]
		payload, _ := event["payload"].(map[string]any)
		items, _ := payload["attachments"].([]any)
		for _, raw := range items {
			if item, ok := raw.(map[string]any); ok {
				out = append(out, roomFile{eventID: fmt.Sprint(event["event_id"]), item: item})
			}
		}
	}
	return out
}

// Lists the log's files, picking one saves it to the project folder
func (c *roomCard) pickFile() tea.Cmd {
	files := c.files()
	if len(files) == 0 {
		return wm.Notify(tr("room.files.none"), wm.Info)
	}
	items := make([]wm.Item, 0, len(files))
	for _, f := range files {
		name := fmt.Sprint(f.item["name"])
		items = append(items, wm.Item{ID: "room:file:" + fmt.Sprint(f.item["attachment_id"]), Title: name, Detail: render.Bytes(int64(jsNumberOf(f.item["size"]))),
			Run: func() tea.Cmd { return c.saveFile(f, 0) }})
	}
	picker := wm.NewPalette(c.app.toolkit, items)
	picker.Placeholder = tr("room.files.pick")
	return c.app.wm.ShowOverlay(picker)
}

// Downloads a file of the log into the project folder, asking again while a shared room's home sends it
func (c *roomCard) saveFile(f roomFile, tries int) tea.Cmd {
	params := map[string]any{"room_id": c.room.ID, "event_id": f.eventID, "attachment_id": f.item["attachment_id"]}
	return gatewayCall(c.app.gw, "groups.attachment.download", params, func(v any, err error) tea.Cmd {
		if err != nil {
			if shapeOf(err).data["reason"] == "attachment_fetching" && tries < 120 {
				var notice tea.Cmd
				if tries == 0 {
					notice = wm.Notify(tr("room.files.fetching", c.room.Home), wm.Info)
				}
				return tea.Batch(notice, tea.Tick(roomPollEvery, func(time.Time) tea.Msg {
					return asyncDoneMsg{run: func() tea.Cmd { return c.saveFile(f, tries+1) }}
				}))
			}
			return wm.Notify(tr("rooms.errorLine", roomErrorText(err)), wm.Error)
		}
		res := asRPCResult(v)
		data, err := base64.StdEncoding.DecodeString(fmt.Sprint(res["data_base64"]))
		if err != nil {
			return wm.Notify(tr("rooms.errorLine", err.Error()), wm.Error)
		}
		dir := c.app.store.sessionCwd()
		target := filepath.Join(dir, filepath.Base(fmt.Sprint(res["name"])))
		for n := 1; ; n++ {
			if _, err := os.Stat(target); os.IsNotExist(err) {
				break
			}
			ext := filepath.Ext(fmt.Sprint(res["name"]))
			target = filepath.Join(dir, fmt.Sprintf("%s-%d%s", strings.TrimSuffix(filepath.Base(fmt.Sprint(res["name"])), ext), n, ext))
		}
		tmp := target + ".part"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return wm.Notify(tr("rooms.errorLine", err.Error()), wm.Error)
		}
		if err := os.Rename(tmp, target); err != nil {
			return wm.Notify(tr("rooms.errorLine", err.Error()), wm.Error)
		}
		return wm.Notify(tr("room.files.saved", target), wm.Success)
	})
}

// Lists what waits in the room, an approval answered once or denied, a stuck turn retried or discarded
func (c *roomCard) pickPending() tea.Cmd {
	actions := c.pendingActions()
	if len(actions) == 0 {
		return wm.Notify(tr("room.pending.none"), wm.Info)
	}
	items := []wm.Item{}
	for i, action := range actions {
		member := c.memberLabel(fmt.Sprint(action["member_id"]))
		attempt := map[string]any{"room_id": c.room.ID, "member_id": action["member_id"], "task_id": action["task_id"],
			"execution_generation": action["execution_generation"]}
		key := fmt.Sprintf("room:pending:%d:", i)
		if action["kind"] == "approval" {
			approval, _ := action["approval"].(map[string]any)
			what := fmt.Sprint(approval["description"])
			if command, _ := approval["command"].(string); command != "" {
				what = command
			}
			for _, choice := range []string{"once", "deny"} {
				params := withKeys(attempt, "choice", choice, "request_id", action["request_id"])
				items = append(items, wm.Item{ID: key + choice, Title: tr("room.pending.approval."+choice, member), Detail: what,
					Run: func() tea.Cmd { return c.resolve("groups.approve", params, tr("room.notice.answered", member)) }})
			}
			continue
		}
		items = append(items,
			wm.Item{ID: key + "retry", Title: tr("room.pending.retry", member), Detail: fmt.Sprint(action["task_id"]),
				Run: func() tea.Cmd { return c.resolve("groups.retry", attempt, tr("room.notice.retried", member)) }},
			wm.Item{ID: key + "discard", Title: tr("room.pending.discard", member), Detail: fmt.Sprint(action["task_id"]),
				Run: func() tea.Cmd { return c.resolve("groups.discard", attempt, tr("room.notice.discarded", member)) }})
	}
	picker := wm.NewPalette(c.app.toolkit, items)
	picker.Placeholder = tr("room.pending.pick")
	return c.app.wm.ShowOverlay(picker)
}

// Answers one pending action and reads the room again
func (c *roomCard) resolve(method string, params map[string]any, notice string) tea.Cmd {
	return gatewayCall(c.app.gw, method, params, func(v any, err error) tea.Cmd {
		if err != nil {
			return wm.Notify(tr("rooms.errorLine", roomErrorText(err)), wm.Error)
		}
		if asRPCResult(v)["queued"] == true {
			notice = tr("rooms.notice.queued", c.room.Home)
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
		{ID: "room:rename", Title: tr("room.menu.rename"), Run: func() tea.Cmd { return c.app.renameRoom(c.room, back) }},
		{ID: "room:stop", Title: tr("room.menu.stop"), Run: func() tea.Cmd { return c.app.stopRoom(c.room) }},
	}
	if !c.room.Replica {
		items = append(items, wm.Item{ID: "room:peers", Title: tr("room.menu.peers"), Run: func() tea.Cmd { return c.app.roomPeers(c.room, back) }})
	} else {
		items = append(items, wm.Item{ID: "room:promote", Title: tr("room.menu.promote", c.room.Home), Run: func() tea.Cmd {
			return c.confirm(tr("rooms.promote.confirm", c.room.Name, c.room.Home), func() tea.Cmd {
				return gatewayCall(c.app.gw, "groups.promote", map[string]any{"room_id": c.room.ID, "confirm": true}, func(_ any, err error) tea.Cmd {
					if err != nil {
						return wm.Notify(tr("rooms.errorLine", roomErrorText(err)), wm.Error)
					}
					c.room.Replica, c.replica = false, nil
					return back(tr("rooms.notice.promoted", c.room.Name), wm.Success)
				})
			})
		}})
	}
	items = append(items, wm.Item{ID: "room:disband", Title: tr("room.menu.disband"), Run: func() tea.Cmd {
		return c.confirm(tr("rooms.disband.confirm", c.room.Name), func() tea.Cmd { return c.app.disbandRoom(c.room, back) })
	}})
	picker := wm.NewPalette(c.app.toolkit, items)
	picker.Placeholder = c.room.Name
	return c.app.wm.ShowOverlay(picker)
}

// Asks a yes or no question before running yes
func (c *roomCard) confirm(question string, yes func() tea.Cmd) tea.Cmd {
	items := []wm.Item{
		{ID: "room:confirm:yes", Title: tr("room.confirm.yes"), Run: yes},
		{ID: "room:confirm:no", Title: tr("room.confirm.no"), Run: func() tea.Cmd { return nil }},
	}
	picker := wm.NewPalette(c.app.toolkit, items)
	picker.Placeholder = question
	return c.app.wm.ShowOverlay(picker)
}
