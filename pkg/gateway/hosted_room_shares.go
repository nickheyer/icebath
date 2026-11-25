package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Pause between two syncs of every peer a hosted room is shared with
var roomShareInterval = time.Second

const (
	// Raw bytes of attachment data one sync request carries to a peer
	roomShareChunkBytes = 3_000_000
	// Wait for one sync exchange with a peer
	roomShareTimeout = 60 * time.Second
	// Log events one sync pushes per room
	roomSharePageLimit = 100
	// Back to back exchanges one sync round may run while a peer has more to move
	roomShareMaxRounds = 64
)

// Creates the share table of the room home
func shareInitializeSchema(ctx context.Context, conn state.SQLiteConn) error {
	_, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS hosted_room_shares (
            room_id TEXT NOT NULL, peer TEXT NOT NULL,
            pushed_seq INTEGER NOT NULL DEFAULT 0 CHECK (pushed_seq >= 0),
            removing INTEGER NOT NULL DEFAULT 0, last_error TEXT, synced_at REAL,
            created_at REAL NOT NULL, PRIMARY KEY (room_id, peer))`)
	return err
}

// Runs body in one transaction over the room store with the share table in place
func shareTransaction(ctx context.Context, dbPath string, immediate bool, body func(conn state.SQLiteConn) error) error {
	return hostedTransaction(ctx, dbPath, immediate, func(conn state.SQLiteConn) error {
		if err := shareInitializeSchema(ctx, conn); err != nil {
			return err
		}
		return body(conn)
	})
}

// Refusal whose detail the client shows
func roomRefusal(reason, detail string) error {
	return state.NewRuntimeStoreError(reason, nil, &detail)
}

// Registered peer, its key and the base URL a share target addresses
type sharePeer struct {
	target, name, base, key string
}

// Peer of a share target from the icebath peer registry, refused when unknown or keyless
func resolveSharePeer(ctx context.Context, target string) (sharePeer, error) {
	name, profile, err := peerParseTarget(target)
	if err != nil {
		return sharePeer{}, roomRefusal("invalid_params", err.Error())
	}
	peers, err := loadPeers(ctx, home.Dir(ctx))
	if err != nil {
		return sharePeer{}, err
	}
	entry, ok := peers[strings.ToLower(name)].(map[string]any)
	if !ok {
		return sharePeer{}, roomRefusal("invalid_params", fmt.Sprintf("no peer named '%s', add it with icebath peer add", name))
	}
	key := peerSecret(ctx, strings.ToLower(name))
	if key == "" {
		return sharePeer{}, roomRefusal("invalid_params", fmt.Sprintf("peer '%s' has no key, set it with icebath peer add %s --url <url> --key <key>", name, name))
	}
	canonical := strings.ToLower(name)
	if profile != "" {
		canonical += "/" + profile
	}
	return sharePeer{target: canonical, name: strings.ToLower(name), base: peerBaseURL(entry, profile), key: key}, nil
}

// Registered peers with their URL, key presence and note, sorted by name
func RoomPeerRegistry(ctx context.Context) ([]any, error) {
	peers, err := loadPeers(ctx, home.Dir(ctx))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(peers))
	for name := range peers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]any, 0, len(names))
	for _, name := range names {
		entry, _ := peers[name].(map[string]any)
		row := map[string]any{"name": name, "url": anyutil.String(entry["url"]), "key": peerSecret(ctx, name) != ""}
		if note, ok := entry["note"].(string); ok && note != "" {
			row["note"] = note
		}
		out = append(out, row)
	}
	return out, nil
}

// Share rows of one room, or of every room for an empty id
func RoomShares(ctx context.Context, dbPath, roomID string) ([]any, error) {
	var rows []*jsonutil.Object
	err := shareTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) (err error) {
		rows, err = state.QueryObjects(ctx, conn, `SELECT room_id, peer, pushed_seq, removing, last_error, synced_at
            FROM hosted_room_shares WHERE room_id=? OR ?='' ORDER BY peer`, roomID, roomID)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		share := map[string]any{"room_id": hrStr(row, "room_id"), "peer": hrStr(row, "peer"), "pushed_seq": hrInt(row, "pushed_seq"),
			"state": "active", "error": nil, "synced_at": nil}
		if hrInt(row, "removing") != 0 {
			share["state"] = "removing"
		}
		if text := hrStr(row, "last_error"); row.Value("last_error") != nil && text != "" {
			share["error"] = text
		}
		if row.Value("synced_at") != nil {
			share["synced_at"] = hrFloat(row, "synced_at")
		}
		out = append(out, share)
	}
	return out, nil
}

// Shares a room this gateway hosts with a registered peer, idempotent per peer
func (c *CanonicalHostedRoomService) ShareRoom(roomID, target string) (map[string]any, error) {
	if _, _, err := c.ownedAuthority(roomID); err != nil {
		return nil, err
	}
	peer, err := resolveSharePeer(c.ctx, target)
	if err != nil {
		return nil, err
	}
	now := hostedClock(nil)
	err = shareTransaction(c.ctx, c.DBPath, true, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(c.ctx, `INSERT INTO hosted_room_shares (room_id, peer, created_at) VALUES (?, ?, ?)
            ON CONFLICT(room_id, peer) DO UPDATE SET removing=0, last_error=NULL`, roomID, peer.target, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	c.wakeShares()
	return map[string]any{"room_id": roomID, "peer": peer.target, "state": "active"}, nil
}

// Stops sharing a room with a peer, the peer told to drop its replica on the next sync
func (c *CanonicalHostedRoomService) UnshareRoom(roomID, target string) (map[string]any, error) {
	var changed int64
	err := shareTransaction(c.ctx, c.DBPath, true, func(conn state.SQLiteConn) error {
		result, err := conn.ExecContext(c.ctx, "UPDATE hosted_room_shares SET removing=1 WHERE room_id=? AND peer=?", roomID, strings.ToLower(target))
		if err == nil {
			changed, err = result.RowsAffected()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		return nil, roomRefusal("invalid_params", fmt.Sprintf("room is not shared with '%s'", target))
	}
	c.wakeShares()
	return map[string]any{"room_id": roomID, "peer": strings.ToLower(target), "state": "removing"}, nil
}

// Sync state of the room home, peers in flight, acknowledgements owed and files on their way
type roomSharePump struct {
	mu       sync.Mutex
	running  bool
	wake     chan struct{}
	inflight map[string]bool
	acks     map[string][]any
	files    map[string][]*shareFile
}

// One committed attachment a peer asked for, sent in chunks
type shareFile struct {
	meta   map[string]any
	data   []byte
	offset int
}

// Pump of this service, created on first use
func (c *CanonicalHostedRoomService) pump() *roomSharePump {
	c.flagsMu.Lock()
	defer c.flagsMu.Unlock()
	if c.shares == nil {
		c.shares = &roomSharePump{wake: make(chan struct{}, 1), inflight: map[string]bool{}, acks: map[string][]any{}, files: map[string][]*shareFile{}}
	}
	return c.shares
}

// Starts the room worker and the sync of shared rooms with peers
func (c *CanonicalHostedRoomService) Start() error {
	if err := c.HostedRoomService.Start(); err != nil {
		return err
	}
	p := c.pump()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		p.running = true
		go c.runShares(p)
	}
	return nil
}

// Wakes the share loop now instead of at its next tick
func (c *CanonicalHostedRoomService) wakeShares() {
	select {
	case c.pump().wake <- struct{}{}:
	default:
	}
}

// Syncs every peer with shared rooms each interval until the room worker stops
func (c *CanonicalHostedRoomService) runShares(p *roomSharePump) {
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()
	ticker := time.NewTicker(roomShareInterval)
	defer ticker.Stop()
	for {
		status := c.RuntimeStatus()
		if anyutil.IsTrue(status["stopping"]) || !anyutil.IsTrue(status["running"]) {
			return
		}
		targets, err := c.shareTargets()
		if err != nil {
			pkgLog().Warn("hosted room shares: reading the share table failed", "exc_info", err)
		}
		for _, target := range targets {
			p.mu.Lock()
			busy := p.inflight[target]
			p.inflight[target] = true
			p.mu.Unlock()
			if busy {
				continue
			}
			go func(target string) {
				defer func() {
					p.mu.Lock()
					delete(p.inflight, target)
					p.mu.Unlock()
				}()
				c.syncPeer(p, target)
			}(target)
		}
		select {
		case <-ticker.C:
		case <-p.wake:
		}
	}
}

// Distinct peers some room is shared with or being unshared from
func (c *CanonicalHostedRoomService) shareTargets() ([]string, error) {
	var rows []*jsonutil.Object
	err := shareTransaction(c.ctx, c.DBPath, false, func(conn state.SQLiteConn) (err error) {
		rows, err = state.QueryObjects(c.ctx, conn, "SELECT DISTINCT peer FROM hosted_room_shares ORDER BY peer")
		return err
	})
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, hrStr(row, "peer"))
	}
	return out, err
}

// Runs exchanges with one peer until nothing is left to move, the failure kept on its shares
func (c *CanonicalHostedRoomService) syncPeer(p *roomSharePump, target string) {
	for round := 0; round < roomShareMaxRounds; round++ {
		more, err := c.syncPeerOnce(p, target)
		if err != nil {
			text := err.Error()
			var he *peerHTTPError
			if errors.As(err, &he) {
				text = fmt.Sprintf("HTTP %d %s", he.code, peerHTTPErrorDetail(he))
			}
			if markErr := shareTransaction(c.ctx, c.DBPath, true, func(conn state.SQLiteConn) error {
				_, err := conn.ExecContext(c.ctx, "UPDATE hosted_room_shares SET last_error=? WHERE peer=?", text, target)
				return err
			}); markErr != nil {
				pkgLog().Warn("hosted room shares: recording a sync failure failed", "exc_info", markErr)
			}
			return
		}
		if !more {
			return
		}
	}
}

// Label this gateway gives itself toward its peers, the host name and a non default profile
func roomShareHomeLabel(ctx context.Context) string {
	label, err := os.Hostname()
	if err != nil || label == "" {
		label = "gateway"
	}
	if name := profiles.CurrentProfileName(ctx); name != "" && name != "default" {
		label += "/" + name
	}
	return label
}

// One exchange with a peer, pushing log pages, status, removals, acknowledgements and files, applying what it queued
func (c *CanonicalHostedRoomService) syncPeerOnce(p *roomSharePump, target string) (bool, error) {
	peer, err := resolveSharePeer(c.ctx, target)
	if err != nil {
		return false, err
	}
	var shares []*jsonutil.Object
	if err := shareTransaction(c.ctx, c.DBPath, false, func(conn state.SQLiteConn) (err error) {
		shares, err = state.QueryObjects(c.ctx, conn, "SELECT room_id, pushed_seq, removing FROM hosted_room_shares WHERE peer=? ORDER BY room_id", target)
		return err
	}); err != nil {
		return false, err
	}
	local, err := LocalAuthorityGatewayID(c.ctx)
	if err != nil {
		return false, err
	}
	rooms, removed, more := []any{}, []any{}, false
	latest := map[string]int64{}
	disbanded := map[string]bool{}
	for _, share := range shares {
		roomID := hrStr(share, "room_id")
		if hrInt(share, "removing") != 0 {
			removed = append(removed, roomID)
			continue
		}
		room, err := RoomState(c.ctx, c.DBPath, roomID, true)
		if err != nil {
			removed = append(removed, roomID)
			continue
		}
		since := hrInt(share, "pushed_seq")
		if seq, _ := room["latest_seq"].(int64); since > seq {
			since = 0
		}
		page, err := ReadEvents(c.ctx, c.DBPath, roomID, since, int64(roomSharePageLimit), true)
		if err != nil {
			return false, err
		}
		latest[roomID], _ = page["latest_seq"].(int64)
		more = more || anyutil.IsTrue(page["has_more"])
		entry := map[string]any{"room_id": roomID, "name": room["name"], "members": room["members"], "page": page,
			"disbanded_at": room["disbanded_at"]}
		if room["disbanded_at"] == nil {
			if status, err := c.Status(&roomID); err == nil {
				entry["status"] = status
			}
		} else {
			disbanded[roomID] = true
		}
		rooms = append(rooms, entry)
	}
	p.mu.Lock()
	acks := p.acks[target]
	p.acks[target] = nil
	files, sentFrom, sentBytes, budget := []any{}, []*shareFile{}, []int{}, roomShareChunkBytes
	for _, f := range p.files[target] {
		if budget <= 0 {
			break
		}
		end := min(len(f.data), f.offset+budget)
		chunk := map[string]any{"offset": f.offset, "data_base64": base64.StdEncoding.EncodeToString(f.data[f.offset:end])}
		for k, v := range f.meta {
			chunk[k] = v
		}
		files = append(files, chunk)
		sentFrom, sentBytes = append(sentFrom, f), append(sentBytes, end-f.offset)
		budget -= end - f.offset
	}
	p.mu.Unlock()
	body := jsonutil.ObjectOf("home_gateway_id", local, "home_label", roomShareHomeLabel(c.ctx), "rooms", rooms, "removed", removed,
		"acks", acks, "files", files)
	answer, err := peerRequest(c.ctx, peer.base+"/v1/rooms/sync", peer.key, peerRequestOptions{method: "POST", body: body, timeout: roomShareTimeout})
	if err != nil {
		p.mu.Lock()
		p.acks[target] = append(acks, p.acks[target]...)
		p.mu.Unlock()
		return false, err
	}
	reply, _ := jsonutil.Plain(answer).(map[string]any)
	p.mu.Lock()
	for i, f := range sentFrom {
		f.offset += sentBytes[i]
	}
	kept := []*shareFile{}
	for _, f := range p.files[target] {
		if f.offset < len(f.data) {
			kept = append(kept, f)
		}
	}
	p.files[target] = kept
	more = more || len(kept) > 0
	p.mu.Unlock()
	now := hostedClock(nil)
	results, _ := reply["rooms"].([]any)
	for _, raw := range results {
		result, _ := raw.(map[string]any)
		roomID, _ := result["room_id"].(string)
		if err := c.settleShare(target, roomID, result, latest[roomID], disbanded[roomID], now); err != nil {
			return false, err
		}
	}
	dropped, _ := reply["removed"].([]any)
	for _, raw := range dropped {
		roomID, _ := raw.(string)
		if err := shareTransaction(c.ctx, c.DBPath, true, func(conn state.SQLiteConn) error {
			_, err := conn.ExecContext(c.ctx, "DELETE FROM hosted_room_shares WHERE room_id=? AND peer=? AND removing=1", roomID, target)
			return err
		}); err != nil {
			return false, err
		}
	}
	gatewayID, _ := reply["gateway_id"].(string)
	ops, _ := reply["outbox"].([]any)
	newAcks := []any{}
	for _, raw := range ops {
		op, _ := raw.(map[string]any)
		ack := map[string]any{"outbox_id": op["outbox_id"], "error": nil}
		if err := c.applyPeerOp(p, target, gatewayID, peer.name, op); err != nil {
			ack["error"] = shareErrorText(err)
		}
		newAcks = append(newAcks, ack)
	}
	if len(newAcks) > 0 {
		p.mu.Lock()
		p.acks[target] = append(p.acks[target], newAcks...)
		p.mu.Unlock()
		more = true
	}
	return more, nil
}

// Text of a failed peer operation as the peer's user reads it
func shareErrorText(err error) string {
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		if refusal.Detail != nil && *refusal.Detail != "" {
			return *refusal.Detail
		}
		if refusal.Cause != nil {
			return refusal.Cause.Error()
		}
		return refusal.Reason
	}
	return err.Error()
}

// Records one room's answer, the peer's stored cursor, its newer authority or its refusal
func (c *CanonicalHostedRoomService) settleShare(target, roomID string, result map[string]any, latest int64, disbanded bool, now float64) error {
	if authority, ok := result["authority"].(map[string]any); ok {
		if _, err := DemoteRoom(c.ctx, c.DBPath, roomID, authority["gateway_id"], authority["epoch"], nil); err != nil {
			pkgLog().Warn(fmt.Sprintf("hosted room %s: the peer %s holds a newer authority and the demotion failed", roomID, target), "exc_info", err)
		}
		return shareTransaction(c.ctx, c.DBPath, true, func(conn state.SQLiteConn) error {
			_, err := conn.ExecContext(c.ctx, "DELETE FROM hosted_room_shares WHERE room_id=?", roomID)
			return err
		})
	}
	return shareTransaction(c.ctx, c.DBPath, true, func(conn state.SQLiteConn) error {
		if text, failed := result["error"].(string); failed {
			if stored, known := result["stored_seq"].(int64); known {
				_, err := conn.ExecContext(c.ctx, "UPDATE hosted_room_shares SET pushed_seq=?, last_error=?, synced_at=? WHERE room_id=? AND peer=?", stored, text, now, roomID, target)
				return err
			}
			_, err := conn.ExecContext(c.ctx, "UPDATE hosted_room_shares SET last_error=?, synced_at=? WHERE room_id=? AND peer=?", text, now, roomID, target)
			return err
		}
		stored, _ := result["stored_seq"].(int64)
		if disbanded && stored >= latest {
			_, err := conn.ExecContext(c.ctx, "DELETE FROM hosted_room_shares WHERE room_id=? AND peer=?", roomID, target)
			return err
		}
		_, err := conn.ExecContext(c.ctx, "UPDATE hosted_room_shares SET pushed_seq=?, last_error=NULL, synced_at=? WHERE room_id=? AND peer=? AND removing=0",
			stored, now, roomID, target)
		return err
	})
}

// Runs one operation a peer's user queued on its replica, a fetch queuing the file for the peer
func (c *CanonicalHostedRoomService) applyPeerOp(p *roomSharePump, target, peerGateway, peerName string, op map[string]any) error {
	roomID, _ := op["room_id"].(string)
	kind, _ := op["op"].(string)
	params, _ := op["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	if !c.sharedWith(roomID, target) {
		return roomRefusal("permission_denied", "this room is no longer shared with you")
	}
	if kind != "fetch" {
		return c.applyRoomOp(roomID, kind, params, map[string]any{"kind": "user", "id": "peer:" + peerGateway, "display_name": peerName})
	}
	saved, err := ReadDownload(c.ctx, c.HostedRoomService, roomID, map[string]any{"room_id": roomID, "event_id": params["event_id"], "attachment_id": params["attachment_id"]})
	if err != nil {
		return err
	}
	data, err := base64.StdEncoding.DecodeString(anyutil.String(saved["data_base64"]))
	if err != nil {
		return err
	}
	meta := map[string]any{"room_id": roomID, "event_id": params["event_id"]}
	for _, key := range []string{"attachment_id", "kind", "name", "mime", "size", "sha256"} {
		meta[key] = saved[key]
	}
	p.mu.Lock()
	p.files[target] = append(p.files[target], &shareFile{meta: meta, data: data})
	p.mu.Unlock()
	return nil
}

// Runs one queued room operation on a room this gateway hosts, a message sent as the actor
func (c *CanonicalHostedRoomService) applyRoomOp(roomID, kind string, params, actor map[string]any) error {
	gatewayID, _, err := c.ownedAuthority(roomID)
	if err != nil {
		return err
	}
	verb := map[string]any{"room_id": roomID}
	for k, v := range params {
		verb[k] = v
	}
	switch kind {
	case "send":
		return c.applySend(roomID, params, actor)
	case "rename":
		_, err := RenameRoom(c.ctx, c.DBPath, roomID, params["event_id"], params["name"], nil)
		return err
	case "stop", "continue", "approve", "retry", "discard":
		_, err := groupExecutionControl(c.ctx, c, "groups."+kind, verb)
		return err
	case "disband":
		driver := func(string) (*CanonicalHostedRoomService, error) { return c, nil }
		_, err := groupDisband(c.ctx, true, driver, c.DBPath, gatewayID, verb)
		return err
	}
	return roomRefusal("invalid_params", "unknown room operation "+kind)
}

// Whether a room is shared with the peer and not being unshared
func (c *CanonicalHostedRoomService) sharedWith(roomID, target string) bool {
	var row *jsonutil.Object
	err := shareTransaction(c.ctx, c.DBPath, false, func(conn state.SQLiteConn) (err error) {
		row, err = state.QueryRowObject(c.ctx, conn, "SELECT 1 AS hit FROM hosted_room_shares WHERE room_id=? AND peer=? AND removing=0", roomID, target)
		return err
	})
	return err == nil && row != nil
}

// Stores the files a queued message carries under this gateway's ids, then appends the message as the actor
func (c *CanonicalHostedRoomService) applySend(roomID string, params, actor map[string]any) error {
	payload, _ := params["payload"].(map[string]any)
	if payload == nil {
		return roomRefusal("invalid_params", "message payload is missing")
	}
	renamed := map[string]string{}
	uploads, _ := params["uploads"].([]any)
	for _, raw := range uploads {
		upload, _ := raw.(map[string]any)
		stored, err := StoreUpload(c.ctx, c.HostedRoomService, roomID, map[string]any{"upload_id": "peer-" + anyutil.String(upload["upload_id"]),
			"kind": upload["kind"], "name": upload["name"], "mime": upload["mime"], "data_base64": upload["data_base64"]})
		if err != nil {
			return err
		}
		renamed[anyutil.String(upload["attachment_id"])] = anyutil.String(stored["attachment_id"])
	}
	if items, ok := payload["attachments"].([]any); ok {
		mapped := make([]any, 0, len(items))
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			own, known := renamed[anyutil.String(item["attachment_id"])]
			if !known {
				return roomRefusal("invalid_params", "an attachment of the message did not arrive")
			}
			copied := map[string]any{}
			for k, v := range item {
				copied[k] = v
			}
			copied["attachment_id"] = own
			mapped = append(mapped, copied)
		}
		payload["attachments"] = mapped
	}
	eventID, err := UserEventID(params["event_id"])
	if err != nil {
		return err
	}
	_, err = c.SendAs(roomID, eventID, payload, actor)
	return err
}
