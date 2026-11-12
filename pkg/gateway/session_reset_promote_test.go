package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Real session database recording the reset writes and returning a scripted peer recovery row
type resetRecordingDB struct {
	*state.SessionDB
	recovered *pyjson.Object
	promoted  [][2]string
	ended     [][2]string
	reopened  []string
}

func (d *resetRecordingDB) PromoteToSessionReset(ctx context.Context, sessionID, reason string) bool {
	d.promoted = append(d.promoted, [2]string{sessionID, reason})
	return true
}

func (d *resetRecordingDB) EndSession(sessionID, endReason string) error {
	d.ended = append(d.ended, [2]string{sessionID, endReason})
	return nil
}

func (d *resetRecordingDB) ReopenSession(ctx context.Context, sessionID string) error {
	d.reopened = append(d.reopened, sessionID)
	return nil
}

func (d *resetRecordingDB) FindLatestGatewaySessionForPeer(ctx context.Context, q state.GatewayPeerQuery) (*pyjson.Object, error) {
	return d.recovered, nil
}

var _ sessionResetPromoter = (*resetRecordingDB)(nil)

func TestAutoResetDoesNotRecoverSessionBeingEnded(t *testing.T) {
	ctx, real := newTestStateDB(t)
	db := &resetRecordingDB{SessionDB: real}
	store := loadedTestStore(ctx, t, db)
	source := &SessionSource{Platform: PlatformTelegram, ChatID: "12345", ChatType: "dm", UserID: strPtr("12345")}
	key, err := store.GenerateSessionKey(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := NewSessionEntry(key, "old-session", pyDatetime{T: now.Add(-2 * time.Hour)}, pyDatetime{T: now.Add(-time.Hour)})
	old.Platform = PlatformTelegram
	old.Suspended = true
	store.entries.set(key, old)
	db.recovered = pyjson.ObjectOf("id", old.SessionID, "session_key", key,
		"started_at", float64(old.CreatedAt.T.UnixNano())/1e9)

	entry, err := store.GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if entry.SessionID == old.SessionID || !entry.WasAutoReset {
		t.Fatalf("entry %q auto reset %v", entry.SessionID, entry.WasAutoReset)
	}
	if len(db.reopened) != 0 {
		t.Fatalf("reopened %v", db.reopened)
	}
	if len(db.promoted) != 1 || db.promoted[0] != [2]string{old.SessionID, "suspended"} {
		t.Fatalf("promoted %v", db.promoted)
	}
	if len(db.ended) != 0 {
		t.Fatalf("ended %v", db.ended)
	}
}
