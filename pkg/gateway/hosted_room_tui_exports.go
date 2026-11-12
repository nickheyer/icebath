package gateway

import (
	"context"
	"database/sql"
	"errors"
)

// Every room method the gateway route serves, in wire order
func GroupMethods() []string { return append([]string(nil), groupMethodOrder...) }

// Live room record of the service store
func (s *HostedRoomService) RoomRecord(roomID string) (map[string]any, error) { return s.room(roomID) }

// Gateway id and epoch of a room this service's gateway holds authority for
func (s *HostedRoomService) OwnedAuthority(roomID string) (string, int64, error) {
	return s.ownedAuthority(roomID)
}

// Authority gateway id the room store records for a room, false when the store has no such room
func StoredRoomAuthority(ctx context.Context, dbPath, roomID string) (string, bool, error) {
	conn, err := hostedReadConnection(ctx, dbPath)
	if err != nil {
		return "", false, err
	}
	defer conn.Close()
	var authority any
	err = conn.QueryRowContext(ctx, "SELECT authority_gateway_id FROM hosted_rooms WHERE room_id=?", roomID).Scan(&authority)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return pyStrOf(authority), true, nil
}
