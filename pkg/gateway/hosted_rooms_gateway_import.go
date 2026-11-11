package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/state"
)

const (
	hostedImportSourceName = "shared-state.db"
	HostedHandoffTable     = "hosted_room_gateway_handoffs"
	hostedRoomOwnerPrefix  = "gateway.hosted.owner.v1:"
)

// Authority a standalone room is fenced to once the gateway store holds it
func HandoffGatewayID(ctx context.Context) (string, error) {
	local, err := LocalAuthorityGatewayID(ctx)
	if err != nil {
		return "", err
	}
	return local + ":gateway-store", nil
}

// Standalone store this gateway store imports from, empty for a named profile or a missing source
func HostedImportSourceFor(targetDB string) string {
	source := filepath.Join(filepath.Dir(targetDB), hostedImportSourceName)
	if filepath.Base(targetDB) == "state.db" && hostedIsFile(source) {
		return source
	}
	return ""
}

// Moves every source room still on the local authority to the handoff authority at the next epoch
func hostedFenceSource(ctx context.Context, source, localID, handoffID string) (int, error) {
	fenced, offset := 0, 0
	for {
		page, err := ListRooms(ctx, source, false, int64(MaxRoomListLimit), int64(offset))
		if err != nil {
			return fenced, err
		}
		for _, room := range page {
			if room["authority_gateway_id"] == localID {
				if _, err := DemoteRoom(ctx, source, room["room_id"], handoffID, room["authority_epoch"].(int64)+1, nil); err != nil {
					return fenced, err
				}
				fenced++
			}
		}
		if len(page) < MaxRoomListLimit {
			return fenced, nil
		}
		offset += len(page)
	}
}

// Creates the source side handoff record table
func hostedEnsureHandoffTable(ctx context.Context, conn state.SQLiteConn) error {
	_, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+HostedHandoffTable+" (room_id TEXT PRIMARY KEY, handed_at REAL NOT NULL)")
	return err
}

// Fenced live source rooms not yet recorded as handed over
func hostedFencedIDs(ctx context.Context, source, handoffID string) (map[string]bool, error) {
	out := map[string]bool{}
	err := hostedTransaction(ctx, source, true, func(conn state.SQLiteConn) error {
		if err := hostedEnsureHandoffTable(ctx, conn); err != nil {
			return err
		}
		ids, err := hostedRoomIDs(ctx, conn, "SELECT room_id FROM hosted_rooms WHERE authority_gateway_id=? AND disbanded_at IS NULL"+
			" AND room_id NOT IN (SELECT room_id FROM "+HostedHandoffTable+")", handoffID)
		for _, id := range ids {
			out[id] = true
		}
		return err
	})
	return out, err
}

// Records each handed room in the source so it is never offered again
func hostedRecordHandoff(ctx context.Context, source string, roomIDs map[string]bool, now float64) error {
	ids := make([]string, 0, len(roomIDs))
	for id := range roomIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return hostedTransaction(ctx, source, true, func(conn state.SQLiteConn) error {
		if err := hostedEnsureHandoffTable(ctx, conn); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO "+HostedHandoffTable+" (room_id, handed_at) VALUES (?, ?)", id, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// Every room the source handed to the gateway store, recorded or still only fenced
func HandedRoomIDs(ctx context.Context, source string) (map[string]bool, error) {
	handoffID, err := HandoffGatewayID(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := hostedReadConnection(ctx, source)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ids, err := hostedRoomIDs(ctx, conn, "SELECT room_id FROM hosted_rooms WHERE authority_gateway_id=?", handoffID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// Claims a copied room for this install at the next epoch and records its host owner
func hostedImportClaim(ctx context.Context, conn state.SQLiteConn, roomID, localID, handoffID, owner string, now float64) error {
	row, err := state.QueryRowObject(ctx, conn, "SELECT authority_epoch, next_seq FROM hosted_rooms WHERE room_id=?", roomID)
	if err != nil {
		return err
	}
	if row == nil {
		return errors.New("copied room could not be reloaded")
	}
	epoch := hrInt(row, "authority_epoch") + 1
	claim, err := replicaControlEvent("claimed", epoch, map[string]any{"previous_gateway_id": handoffID, "authority_gateway_id": localID, "authority_epoch": epoch})
	if err != nil {
		return err
	}
	if err := replicaAppendControlEvent(ctx, conn, roomID, hrInt(row, "next_seq"), epoch, claim, now); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE hosted_rooms SET authority_gateway_id=?, authority_epoch=?, next_seq=next_seq+1,
                 revision=revision+1, updated_at=? WHERE room_id=?`, localID, epoch, now, roomID); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "INSERT OR IGNORE INTO state_meta(key, value) VALUES (?, ?)", hostedRoomOwnerPrefix+roomID, owner)
	return err
}

// Whether an import failure is one the next start retries, an OS, SQLite, room or runtime store error
func hostedImportRetryable(err error) bool {
	var pathErr *fs.PathError
	var roomErr *HostedRoomError
	var runtimeErr *state.RuntimeStoreError
	_, isSQLite := state.SQLiteClassOf(err)
	return isSQLite || errors.As(err, &pathErr) || errors.As(err, &roomErr) || errors.As(err, &runtimeErr)
}

// Moves the standalone backend's live rooms into targetDB once, returning the rooms copied and never failing on a retryable error
func ImportStandaloneRooms(ctx context.Context, targetDB string, runtimeEpoch *int64) (int, error) {
	source := HostedImportSourceFor(targetDB)
	if source == "" {
		return 0, nil
	}
	copied, err := func() (int, error) {
		localID, err := LocalAuthorityGatewayID(ctx)
		if err != nil {
			return 0, err
		}
		handoffID, err := HandoffGatewayID(ctx)
		if err != nil {
			return 0, err
		}
		if _, err := hostedFenceSource(ctx, source, localID, handoffID); err != nil {
			return 0, err
		}
		fenced, err := hostedFencedIDs(ctx, source, handoffID)
		if err != nil || len(fenced) == 0 {
			return 0, err
		}
		owner, now := HostOwnerSubject(), hostedClock(nil)
		copied := 0
		if err := hostedTransaction(ctx, targetDB, true, func(conn state.SQLiteConn) error {
			if runtimeEpoch != nil {
				if err := state.RuntimeEpoch(ctx, conn, *runtimeEpoch); err != nil {
					return err
				}
			}
			known, err := hostedRoomIDs(ctx, conn, "SELECT room_id FROM hosted_rooms UNION SELECT room_id FROM hosted_room_retired_ids")
			if err != nil {
				return err
			}
			missing := map[string]bool{}
			for id := range fenced {
				missing[id] = true
			}
			for _, id := range known {
				delete(missing, id)
			}
			if len(missing) > 0 {
				if copied, err = hostedCopyRows(ctx, conn, source, missing); err != nil {
					return err
				}
			}
			ids := make([]string, 0, len(missing))
			for id := range missing {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				if err := hostedImportClaim(ctx, conn, id, localID, handoffID, owner, now); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return 0, err
		}
		return copied, hostedRecordHandoff(ctx, source, fenced, now)
	}()
	if err != nil {
		if !hostedImportRetryable(err) {
			return 0, err
		}
		slog.Warn(fmt.Sprintf("hosted rooms: could not import the standalone Group Chats from %s (%v); will retry on the next start", source, err))
		return 0, nil
	}
	if copied > 0 {
		slog.Info(fmt.Sprintf("hosted rooms: imported %d standalone Group Chat(s) from %s", copied, source))
	}
	return copied, nil
}
