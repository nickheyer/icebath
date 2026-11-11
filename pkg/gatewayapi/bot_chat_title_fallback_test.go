package gatewayapi

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Store double holding one titled row whose local receipt carries a reset lineage
type titleFallbackDB struct{ row *pyjson.Object }

func newTitleFallbackDB(overrides ...any) *titleFallbackDB {
	row := pyjson.ObjectOf("id", "local-root", "title", "Bot Chat", "end_reason", "session_reset", "hidden", int64(0),
		"archived", int64(0), "source", "local")
	for i := 0; i+1 < len(overrides); i += 2 {
		row.Set(overrides[i].(string), overrides[i+1])
	}
	return &titleFallbackDB{row: row}
}

func (d *titleFallbackDB) GetSessionByTitle(_ context.Context, title string) (*pyjson.Object, error) {
	if title == d.row.GetOr("title", nil) {
		return d.row.Copy(), nil
	}
	return nil, nil
}

func (d *titleFallbackDB) LocalReceipt(_ context.Context, sessionID string) (*pyjson.Object, error) {
	return pyjson.ObjectOf("lineage", []any{sessionID, "fresh"}), nil
}

// Ids of the rows the fallback answers
func fallbackIDs(t *testing.T, db *titleFallbackDB, includeHidden bool, source string) []any {
	t.Helper()
	rows, err := resetLocalTitleOwner(context.Background(), db, "Bot Chat", includeHidden, source)
	if err != nil {
		t.Fatal(err)
	}
	ids := []any{}
	for _, row := range rows {
		ids = append(ids, row.GetOr("id", nil))
	}
	return ids
}

func TestTheResetBotChatIsFound(t *testing.T) {
	if ids := fallbackIDs(t, newTitleFallbackDB(), true, ""); len(ids) != 1 || ids[0] != "local-root" {
		t.Fatalf("ids = %v", ids)
	}
	if ids := fallbackIDs(t, newTitleFallbackDB(), true, "local"); len(ids) == 0 {
		t.Fatal("matching source filter excluded the row")
	}
}

func TestADeliberatelyArchivedBotChatIsNotReturned(t *testing.T) {
	if ids := fallbackIDs(t, newTitleFallbackDB("archived", int64(1)), true, ""); len(ids) != 0 {
		t.Fatalf("ids = %v", ids)
	}
}

func TestASourceFilterTheRowDoesNotMatchExcludesIt(t *testing.T) {
	if ids := fallbackIDs(t, newTitleFallbackDB(), true, "telegram"); len(ids) != 0 {
		t.Fatalf("ids = %v", ids)
	}
}

func TestHiddenNeedsIncludeHidden(t *testing.T) {
	if ids := fallbackIDs(t, newTitleFallbackDB("hidden", int64(1)), false, ""); len(ids) != 0 {
		t.Fatalf("ids = %v", ids)
	}
}
