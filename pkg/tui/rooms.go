package tui

import (
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap contexts of the rooms list and of one open room
const (
	roomsContext = "tui.rooms"
	roomContext  = "tui.room"
)

// Fewest and most members a room seats
const (
	roomMinMembers = 2
	roomMaxMembers = 6
)

// Keys of the rooms list and the room window, registered with the keymap of the app
func registerRoomKeys(km *keys.Map) {
	for name, doc := range map[string]string{roomsContext: "hosted rooms", roomContext: "hosted room"} {
		if keys.ContextDocs[name] == "" {
			if err := keys.AddContext(name, doc); err != nil {
				panic(err)
			}
		}
	}
	for _, a := range []keys.Action{
		{ID: "tui.rooms.open", Context: roomsContext, Label: "open", Doc: "open the room and its live log", Default: []string{"enter"}},
		{ID: "tui.rooms.new", Context: roomsContext, Label: "new", Doc: "create a room and seat its members", Default: []string{"a"}},
		{ID: "tui.rooms.rename", Context: roomsContext, Label: "rename", Doc: "rename the room", Default: []string{"r"}},
		{ID: "tui.rooms.stop", Context: roomsContext, Label: "stop", Doc: "stop the work running in the room", Default: []string{"s"}},
		{ID: "tui.rooms.disband", Context: roomsContext, Label: "disband", Doc: "disband the room for good after a confirmation", Default: []string{"x"}},
		{ID: "tui.rooms.peers", Context: roomsContext, Label: "peers", Doc: "share the room with peers or stop sharing it", Default: []string{"p"}},
		{ID: "tui.rooms.promote", Context: roomsContext, Label: "continue here", Doc: "continue here a room whose icebath is gone", Default: []string{"c"}},
		{ID: "tui.rooms.history", Context: roomsContext, Label: "disbanded", Doc: "show or hide disbanded rooms", Default: []string{"h"}},
		{ID: "tui.room.attach", Context: roomContext, Label: "attach", Doc: "attach a file to the next message"},
		{ID: "tui.room.files", Context: roomContext, Label: "files", Doc: "save a file of the room log to the project folder"},
		{ID: "tui.room.pending", Context: roomContext, Label: "waiting", Doc: "approve, retry or discard what waits in the room"},
		{ID: "tui.room.menu", Context: roomContext, Label: "room", Doc: "rename, stop, share or disband the room"},
		{ID: "tui.room.stop", Context: roomContext, Label: "stop", Doc: "stop the work running in the room"},
		{ID: "leader.tui.room.attach", Context: keys.Leader, Group: "room", Label: "attach", Doc: "attach a file to the next message of the room", Default: []string{"u"}},
		{ID: "leader.tui.room.files", Context: keys.Leader, Group: "room", Label: "files", Doc: "save a file of the room log to the project folder", Default: []string{"F"}},
		{ID: "leader.tui.room.pending", Context: keys.Leader, Group: "room", Label: "waiting", Doc: "approve, retry or discard what waits in the room", Default: []string{"P"}},
		{ID: "leader.tui.room.menu", Context: keys.Leader, Group: "room", Label: "room", Doc: "rename, stop, share or disband the room", Default: []string{"M"}},
		{ID: "leader.tui.room.stop", Context: keys.Leader, Group: "room", Label: "stop", Doc: "stop the work running in the room", Default: []string{"X"}},
	} {
		km.Register(a)
	}
}

// One seat of a room roster
type roomMember struct {
	ID, Profile, Handle, Name string
}

// Label of a member, its display name, else its handle
func (m roomMember) label() string {
	if m.Name != "" {
		return m.Name
	}
	return m.Handle
}

// One room of a groups listing
type roomInfo struct {
	ID, Name, Home string
	Replica        bool
	Disbanded      bool
	Members        []roomMember
	Latest         int64
	Updated        float64
}

// Room of a listing or state row
func roomOf(row tuigateway.Room) roomInfo {
	r := roomInfo{ID: row.RoomID, Name: row.Name, Home: val(row.Home), Replica: val(row.Replica), Disbanded: row.DisbandedAt != nil,
		Latest: val(row.LatestSeq), Updated: row.UpdatedAt}
	for _, m := range row.Members {
		r.Members = append(r.Members, roomMember{ID: m.MemberID, Profile: m.Profile, Handle: m.Handle, Name: m.DisplayName})
	}
	return r
}

// Member labels of a room joined with commas
func (r roomInfo) memberLine() string {
	names := make([]string, len(r.Members))
	for i, m := range r.Members {
		names[i] = m.label()
	}
	return strings.Join(names, ", ")
}

// Text of a failed room request, the gateway's detail when it gave one
func roomErrorText(err error) string {
	data := shapeOf(err).data
	if detail, _ := data["detail"].(string); detail != "" {
		return detail
	}
	switch data["reason"] {
	case "runtime_coordination_required":
		return tr(txt.RoomsErrorNoDriver)
	case "permission_denied":
		return tr(txt.RoomsErrorDenied)
	}
	return rpcErrorMessage(err)
}

// Rooms list, every room of this gateway and every room peers share with it
type roomsPanel struct {
	*cards.List
	app      *app
	rooms    []roomInfo
	seatable []string
	gateways []string
	self     string
	history  bool
	loading  bool
	busy     bool
	watching bool
}

// Opens the rooms list and reads the rooms into it
func (a *app) openRooms() tea.Cmd {
	p := &roomsPanel{app: a, loading: true}
	p.List = cards.NewList(a.cardEnv(), cards.ListSpec{
		Title: tr(txt.RoomsTitle), Glyph: a.toolkit.Glyphs.Sessions, Key: "tui:rooms", Context: roomsContext, Primary: "tui.rooms.open",
		Columns: []render.Column{{Title: tr(txt.HubsColRoom), Flex: 2, Max: 32}, {Title: tr(txt.HubsColMembers), Flex: 3, Max: 48}, {Title: tr(txt.HubsColLog), Max: 6}, {Title: tr(txt.HubsColWhere), Max: 24}},
		Rows:    p.rows,
		Detail:  p.detail,
		Free:    map[string]bool{"tui.rooms.new": true, "tui.rooms.history": true},
		Hints:   p.hints,
		Empty:   p.empty,
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"tui.rooms.open":    p.open,
			"tui.rooms.new":     p.create,
			"tui.rooms.rename":  p.rename,
			"tui.rooms.stop":    p.stop,
			"tui.rooms.disband": p.disband,
			"tui.rooms.peers":   p.peers,
			"tui.rooms.promote": p.promote,
			"tui.rooms.history": p.toggleHistory,
		},
	})
	return tea.Batch(a.wm.ShowOverlay(p), p.load())
}

// Interval between two reads of the rooms while the list is up
var roomsPollEvery = 2 * time.Second

// Reads the rooms again each interval while the list is the overlay, so rooms peers share appear
func (p *roomsPanel) watch() tea.Cmd {
	if p.watching {
		return nil
	}
	p.watching = true
	return tea.Tick(roomsPollEvery, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			p.watching = false
			if p.app.wm.Overlay() != p {
				return nil
			}
			if p.busy {
				return p.watch()
			}
			return tea.Batch(p.loadRooms(), p.watch())
		}}
	})
}

// Keeps the panel as the card the window manager holds while the list underneath takes the message
func (p *roomsPanel) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	_, cmd := p.List.Update(msg)
	return p, cmd
}

// Trailer of the panel, the load in flight
func (p *roomsPanel) Status() string {
	if p.loading {
		return tr(txt.RoomsLoading)
	}
	if p.history {
		return tr(txt.RoomsShowingDisbanded)
	}
	return ""
}

// Where a room lives, here, shared from its home or disbanded
func (r roomInfo) where() string {
	switch {
	case r.Disbanded:
		return tr(txt.RoomsWhereDisbanded)
	case r.Replica:
		return tr(txt.RoomsWhereFrom, r.Home)
	}
	return tr(txt.RoomsWhereHere)
}

// Rows of the rooms, disbanded ones dimmed
func (p *roomsPanel) rows() [][]string {
	out := make([][]string, len(p.rooms))
	for i, r := range p.rooms {
		cells := []string{r.Name, r.memberLine(), strconv.FormatInt(r.Latest, 10), r.where()}
		if r.Disbanded {
			for c := range cells {
				cells[c] = render.Dim(cells[c], p.app.toolkit)
			}
		}
		out[i] = cells
	}
	return out
}

// Detail of the room under the cursor, its id, seats and home
func (p *roomsPanel) detail(i int) []string {
	if i < 0 || i >= len(p.rooms) {
		return nil
	}
	r := p.rooms[i]
	out := []string{tr(txt.RoomsDetailId, r.ID)}
	for _, m := range r.Members {
		out = append(out, tr(txt.RoomsDetailMember, m.label(), m.Profile))
	}
	if r.Replica {
		out = append(out, tr(txt.RoomsDetailReplica, r.Home))
	}
	if r.Updated > 0 {
		out = append(out, tr(txt.RoomsDetailUpdated, whenText(r.Updated)))
	}
	return out
}

// Line of an empty panel, the load in flight or how to create the first room
func (p *roomsPanel) empty() string {
	if p.loading {
		return tr(txt.RoomsLoading)
	}
	return tr(txt.RoomsEmpty)
}

// Legend of the room under the cursor, continue here offered only for a shared room
func (p *roomsPanel) hints(_ *cards.List, i int) []wm.Hint {
	out := []wm.Hint{{Action: "tui.rooms.new"}, {Action: "tui.rooms.history"}}
	r, ok := p.at(i)
	if !ok {
		return out
	}
	out = append([]wm.Hint{{Action: "tui.rooms.open"}}, out...)
	if r.Disbanded {
		return out
	}
	out = append(out, wm.Hint{Action: "tui.rooms.rename"}, wm.Hint{Action: "tui.rooms.stop"}, wm.Hint{Action: "tui.rooms.peers"}, wm.Hint{Action: "tui.rooms.disband"})
	if r.Replica {
		out = append(out, wm.Hint{Action: "tui.rooms.promote"})
	}
	return out
}

// Room under the cursor
func (p *roomsPanel) at(i int) (roomInfo, bool) {
	if i < 0 || i >= len(p.rooms) {
		return roomInfo{}, false
	}
	return p.rooms[i], true
}

// Room under the cursor, false while a request runs or no row is selected
func (p *roomsPanel) selected(i int) (roomInfo, bool) {
	if p.busy {
		return roomInfo{}, false
	}
	return p.at(i)
}

// Toast of a failed request
func (p *roomsPanel) fail(err error) tea.Cmd {
	return wm.Notify(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error)
}

// Reads the gateway's room capabilities, then the rooms, a failure leaving the rows as they were
func (p *roomsPanel) load() tea.Cmd {
	return call(p.app.gw, tuigateway.GroupsCapabilities, tuigateway.GroupsCapabilitiesParams{}, func(caps *tuigateway.GroupsCapabilitiesResult, err error) tea.Cmd {
		if err != nil {
			p.loading = false
			p.List.Refresh()
			return p.fail(err)
		}
		p.seatable = caps.SeatableProfiles
		p.self = caps.AuthorityGatewayID
		return call(p.app.gw, tuigateway.CanonicalGroupsPeers, tuigateway.GroupsPeersParams{}, func(res *tuigateway.GroupsPeersResult, err error) tea.Cmd {
			if err != nil {
				p.loading = false
				p.List.Refresh()
				return p.fail(err)
			}
			p.gateways = p.gateways[:0]
			for _, peer := range res.Peers {
				if peer.Name != "" && peer.Key {
					p.gateways = append(p.gateways, peer.Name)
				}
			}
			return p.loadRooms()
		})
	})
}

// Reads the rooms into the list
func (p *roomsPanel) loadRooms() tea.Cmd {
	return call(p.app.gw, tuigateway.GroupsList, tuigateway.GroupsListParams{IncludeDisbanded: ptr(p.history)}, func(res *tuigateway.GroupsListResult, err error) tea.Cmd {
		p.loading = false
		if err != nil {
			p.List.Refresh()
			return p.fail(err)
		}
		p.rooms = p.rooms[:0]
		for _, row := range res.Rooms {
			p.rooms = append(p.rooms, roomOf(row))
		}
		p.List.Refresh()
		return p.watch()
	})
}

// Shows the panel again unless it is up or another card replaced the card it left
func (p *roomsPanel) reshow(from wm.Card) tea.Cmd {
	if overlay := p.app.wm.Overlay(); overlay == p || (from != nil && overlay != from) {
		return nil
	}
	return p.app.wm.ShowOverlay(p)
}

// Back on the panel with a notice and the rooms read again
func (p *roomsPanel) back(notice string, level wm.Level) tea.Cmd {
	cmds := []tea.Cmd{p.reshow(nil), p.load()}
	if notice != "" {
		cmds = append(cmds, wm.Notify(notice, level))
	}
	return tea.Batch(cmds...)
}

// Request of the panel marked busy until it answers, a failure shown as a toast
func panelCall[P, R any](p *roomsPanel, m tuigateway.Method[P, R], params P, then func(*R) tea.Cmd) tea.Cmd {
	p.busy = true
	return call(p.app.gw, m, params, func(res *R, err error) tea.Cmd {
		p.busy = false
		if err != nil {
			return p.fail(err)
		}
		return then(res)
	})
}

// Opens the room under the cursor in its own window
func (p *roomsPanel) open(_ *cards.List, i int) tea.Cmd {
	r, ok := p.selected(i)
	if !ok {
		return nil
	}
	return tea.Batch(p.app.wm.Dismiss(), p.app.openRoom(r, p.self))
}

// Toggles disbanded rooms in the list
func (p *roomsPanel) toggleHistory(_ *cards.List, _ int) tea.Cmd {
	p.history = !p.history
	p.loading = true
	return p.load()
}

// Asks the new room's name, then its members
func (p *roomsPanel) create(_ *cards.List, _ int) tea.Cmd {
	if p.busy {
		return nil
	}
	if len(p.seatable)+len(p.gateways) < roomMinMembers {
		return wm.Notify(tr(txt.RoomsNewTooFewProfiles, float64(roomMinMembers)), wm.Warn)
	}
	return p.app.wm.ShowOverlay(wm.NewPrompt(p.app.toolkit, tr(txt.RoomsNewName), "", func(text string) tea.Cmd {
		name := strings.TrimSpace(text)
		if name == "" {
			return p.back("", wm.Info)
		}
		return p.pickMembers(name, nil)
	}))
}

// One member chosen for a new room, a local profile or a profile on a peer gateway
type roomSeat struct {
	label  string
	member tuigateway.RoomMemberInput
}

// Seat of a local profile
func localSeat(profile string) roomSeat {
	return roomSeat{label: profile, member: tuigateway.RoomMemberInput{MemberID: profile, Profile: profile, Handle: profile}}
}

// Seat of a profile a peer gateway serves, its default profile when profile is empty
func peerSeat(peer, profile string) roomSeat {
	label, handle, seated, target := peer, peer, "default", &tuigateway.RoomTarget{Kind: "peer", Peer: peer}
	if profile != "" {
		label, handle, seated = peer+"/"+profile, peer+"-"+profile, profile
		target.Profile = profile
	}
	return roomSeat{label: label, member: tuigateway.RoomMemberInput{MemberID: handle, Profile: seated, Handle: handle, Target: target}}
}

// Labels of the chosen seats
func seatLabels(chosen []roomSeat) []string {
	out := make([]string, len(chosen))
	for i, s := range chosen {
		out[i] = s.label
	}
	return out
}

// Asks for one more member until the roster is full or the user creates the room
func (p *roomsPanel) pickMembers(name string, chosen []roomSeat) tea.Cmd {
	taken := seatLabels(chosen)
	more := func(seat roomSeat) tea.Cmd { return p.pickMembers(name, append(append([]roomSeat{}, chosen...), seat)) }
	items := []wm.Item{}
	if len(chosen) >= roomMinMembers {
		items = append(items, wm.Item{ID: "rooms:create", Title: tr(txt.RoomsNewCreateItem, name), Detail: strings.Join(taken, ", "),
			Run: func() tea.Cmd { return p.createRoom(name, chosen) }})
	}
	if len(chosen) < roomMaxMembers {
		for _, profile := range p.seatable {
			if !slices.Contains(taken, profile) {
				items = append(items, wm.Item{ID: "rooms:seat:" + profile, Title: profile, Detail: tr(txt.RoomsNewSeat),
					Run: func() tea.Cmd { return more(localSeat(profile)) }})
			}
		}
		for _, peer := range p.gateways {
			if !slices.Contains(taken, peer) {
				items = append(items, wm.Item{ID: "rooms:peer:" + peer, Title: peer, Detail: tr(txt.RoomsNewSeatPeer, peer),
					Run: func() tea.Cmd { return more(peerSeat(peer, "")) }})
			}
		}
		if len(p.gateways) > 0 {
			items = append(items, wm.Item{ID: "rooms:peer:profile", Title: tr(txt.RoomsNewPeerProfile), Detail: tr(txt.RoomsNewPeerProfileDetail),
				Run: func() tea.Cmd {
					return p.app.wm.ShowOverlay(wm.NewPrompt(p.app.toolkit, tr(txt.RoomsNewPeerProfilePrompt), "", func(text string) tea.Cmd {
						peer, profile, _ := strings.Cut(strings.TrimSpace(text), "/")
						if !slices.Contains(p.gateways, peer) || profile == "" {
							return tea.Batch(wm.Notify(tr(txt.RoomsNewPeerProfileUnknown, text), wm.Warn), p.pickMembers(name, chosen))
						}
						return more(peerSeat(peer, profile))
					}))
				}})
		}
	}
	picker := wm.NewPalette(p.app.toolkit, items)
	if len(chosen) == 0 {
		picker.Placeholder = tr(txt.RoomsNewPickFirst, name)
	} else {
		picker.Placeholder = tr(txt.RoomsNewPickMore, strings.Join(taken, ", "))
	}
	return p.app.wm.ShowOverlay(picker)
}

// Creates the room with the chosen members seated, then opens it
func (p *roomsPanel) createRoom(name string, chosen []roomSeat) tea.Cmd {
	members := make([]tuigateway.RoomMemberInput, len(chosen))
	for i, seat := range chosen {
		members[i] = seat.member
	}
	roomID := "room-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	params := tuigateway.GroupsCreateParams{RoomID: roomID, Name: name, Members: members}
	return panelCall(p, tuigateway.GroupsCreate, params, func(res *tuigateway.GroupsCreateResult) tea.Cmd {
		created := roomOf(res.Room)
		return tea.Batch(p.app.wm.Dismiss(), p.load(), wm.Notify(tr(txt.RoomsNoticeCreated, created.Name), wm.Success), p.app.openRoom(created, p.self))
	})
}

// Asks the new name of the room under the cursor
func (p *roomsPanel) rename(_ *cards.List, i int) tea.Cmd {
	r, ok := p.selected(i)
	if !ok || r.Disbanded {
		return nil
	}
	return p.app.renameRoom(r, func(notice string, level wm.Level) tea.Cmd { return p.back(notice, level) })
}

// Stops the work running in the room under the cursor
func (p *roomsPanel) stop(_ *cards.List, i int) tea.Cmd {
	r, ok := p.selected(i)
	if !ok || r.Disbanded {
		return nil
	}
	return p.app.stopRoom(r)
}

// Asks before disbanding the room under the cursor
func (p *roomsPanel) disband(l *cards.List, i int) tea.Cmd {
	r, ok := p.selected(i)
	if !ok || r.Disbanded {
		return nil
	}
	l.Confirm(tr(txt.RoomsDisbandConfirm, r.Name), func() tea.Cmd {
		return p.app.disbandRoom(r, func(notice string, level wm.Level) tea.Cmd { return p.back(notice, level) })
	})
	return nil
}

// Lists the peers with this room's share state, picking one shares or stops sharing
func (p *roomsPanel) peers(_ *cards.List, i int) tea.Cmd {
	r, ok := p.selected(i)
	if !ok || r.Disbanded {
		return nil
	}
	return p.app.roomPeers(r, func(notice string, level wm.Level) tea.Cmd { return p.back(notice, level) })
}

// Asks before continuing a shared room on this gateway
func (p *roomsPanel) promote(l *cards.List, i int) tea.Cmd {
	r, ok := p.selected(i)
	if !ok || !r.Replica || r.Disbanded {
		return nil
	}
	l.Confirm(tr(txt.RoomsPromoteConfirm, r.Name, r.Home), func() tea.Cmd {
		return panelCall(p, tuigateway.GroupsPromote, tuigateway.GroupsPromoteParams{RoomID: r.ID, Confirm: ptr(true)}, func(*tuigateway.GroupsPromoteResult) tea.Cmd {
			return p.back(tr(txt.RoomsNoticePromoted, r.Name), wm.Success)
		})
	})
	return nil
}

// Asks a new name for a room and renames it, the answer handed to back
func (a *app) renameRoom(r roomInfo, back func(string, wm.Level) tea.Cmd) tea.Cmd {
	return a.wm.ShowOverlay(wm.NewPrompt(a.toolkit, tr(txt.RoomsRenamePrompt, r.Name), r.Name, func(text string) tea.Cmd {
		name := strings.TrimSpace(text)
		if name == "" || name == r.Name {
			return back("", wm.Info)
		}
		params := tuigateway.GroupsRenameParams{RoomID: r.ID, EventID: "rename-" + uuid.NewString(), Name: name}
		return call(a.gw, tuigateway.GroupsRename, params, func(res *tuigateway.GroupsRenameResult, err error) tea.Cmd {
			if err != nil {
				return back(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error)
			}
			if val(res.Queued) {
				return back(tr(txt.RoomsNoticeQueued, r.Home), wm.Info)
			}
			return back(tr(txt.RoomsNoticeRenamed, name), wm.Success)
		})
	}))
}

// Stops the work running in a room
func (a *app) stopRoom(r roomInfo) tea.Cmd {
	params := tuigateway.GroupsStopParams{RoomID: r.ID, CancelID: ptr("stop-" + uuid.NewString())}
	return call(a.gw, tuigateway.GroupsStop, params, func(res *tuigateway.GroupsStopResult, err error) tea.Cmd {
		if err != nil {
			return wm.Notify(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error)
		}
		if val(res.Queued) {
			return wm.Notify(tr(txt.RoomsNoticeQueued, r.Home), wm.Info)
		}
		return wm.Notify(tr(txt.RoomsNoticeStopped, r.Name, float64(res.Cancelled)), wm.Success)
	})
}

// Disbands a room, the answer handed to back
func (a *app) disbandRoom(r roomInfo, back func(string, wm.Level) tea.Cmd) tea.Cmd {
	params := tuigateway.GroupsDisbandParams{RoomID: r.ID, CancelID: ptr("disband-" + uuid.NewString())}
	return call(a.gw, tuigateway.GroupsDisband, params, func(res *tuigateway.GroupsDisbandResult, err error) tea.Cmd {
		if err != nil {
			return back(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error)
		}
		if val(res.Queued) {
			return back(tr(txt.RoomsNoticeQueued, r.Home), wm.Info)
		}
		return back(tr(txt.RoomsNoticeDisbanded, r.Name), wm.Success)
	})
}

// Peer gateways with the room's share state, picking one shares the room with it or stops sharing
func (a *app) roomPeers(r roomInfo, back func(string, wm.Level) tea.Cmd) tea.Cmd {
	if r.Replica {
		return wm.Notify(tr(txt.RoomsPeersOnlyHome, r.Home), wm.Info)
	}
	return call(a.gw, tuigateway.CanonicalGroupsPeers, tuigateway.GroupsPeersParams{RoomID: ptr(r.ID)}, func(res *tuigateway.GroupsPeersResult, err error) tea.Cmd {
		if err != nil {
			return wm.Notify(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error)
		}
		shares := map[string]tuigateway.RoomShare{}
		for _, share := range res.Shares {
			shares[strings.SplitN(share.Peer, "/", 2)[0]] = share
		}
		if len(res.Peers) == 0 {
			return wm.Notify(tr(txt.RoomsPeersNone), wm.Info)
		}
		items := []wm.Item{}
		for _, peer := range res.Peers {
			name := peer.Name
			share, shared := shares[name]
			detail := tr(txt.RoomsPeersNotShared, peer.URL)
			switch {
			case !peer.Key:
				detail = tr(txt.RoomsPeersNoKey, name)
			case shared && share.State == "removing":
				detail = tr(txt.RoomsPeersRemoving)
			case shared && share.Error != nil:
				detail = tr(txt.RoomsPeersFailing, *share.Error)
			case shared && share.SyncedAt != nil:
				detail = tr(txt.RoomsPeersSynced, whenText(*share.SyncedAt))
			case shared:
				detail = tr(txt.RoomsPeersWaiting)
			}
			method, notice := tuigateway.CanonicalGroupsShare, txt.RoomsNoticeShared
			if shared && share.State != "removing" {
				method, notice = tuigateway.CanonicalGroupsUnshare, txt.RoomsNoticeUnshared
			}
			items = append(items, wm.Item{ID: "rooms:peer:" + name, Title: name, Detail: detail, Run: func() tea.Cmd {
				return call(a.gw, method, tuigateway.GroupsShareParams{RoomID: r.ID, Peer: name}, func(_ *tuigateway.GroupsShareResult, err error) tea.Cmd {
					if err != nil {
						return back(tr(txt.RoomsErrorLine, roomErrorText(err)), wm.Error)
					}
					return back(tr(notice, r.Name, name), wm.Success)
				})
			}})
		}
		picker := wm.NewPalette(a.toolkit, items)
		picker.Placeholder = tr(txt.RoomsPeersPick, r.Name)
		return a.wm.ShowOverlay(picker)
	})
}

// Command that opens the rooms list
func (a *app) roomsCommands() []slashCommand {
	return []slashCommand{{name: "rooms", help: tr(txt.SlashCmdCoreHelpRooms), helpFn: helpOf(txt.SlashCmdCoreHelpRooms), run: func(*slashRun) tea.Cmd { return a.openRooms() }}}
}
