package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Session database whose message read fails like a malformed disk image
type malformedReadDB struct {
	SessionStoreDB
	t *testing.T
}

var errMalformedImage = errors.New("database disk image is malformed")

func (malformedReadDB) GetCompressionTip(context.Context, string) (string, error) { return "", nil }

func (d malformedReadDB) GetMessagesAsConversation(_ context.Context, _ string, o state.ConversationOptions) ([]map[string]any, error) {
	if !o.RepairAlternation {
		d.t.Error("transcript read without alternation repair")
	}
	return nil, errMalformedImage
}

func TestLoadTranscriptReroutesLoadTranscriptRaisesWhenMessageReadFails(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	store := NewSessionStore(ctx, filepath.Join(t.TempDir(), "gw-failed-read"), DefaultGatewayConfig(ctx), nil)
	t.Cleanup(store.CloseAllDBHandles)
	db := store.db(ctx)
	if db == nil {
		t.Fatal("session store opened no database")
	}
	store.pinnedDB, store.dbPinned = malformedReadDB{SessionStoreDB: db, t: t}, true
	_, err := store.LoadTranscript(ctx, "existing-session")
	var readErr *TranscriptReadError
	if !errors.As(err, &readErr) {
		t.Fatalf("err %v", err)
	}
	if readErr.SessionID != "existing-session" || !errors.Is(readErr.Unwrap(), errMalformedImage) {
		t.Fatalf("session %q cause %v", readErr.SessionID, readErr.Unwrap())
	}
}
