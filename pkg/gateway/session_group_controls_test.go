package gateway

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Room service double answering only what a groups.list page reads, rooms outside m* refused
type pagingRoomService struct{ dbPath string }

func (s *pagingRoomService) StorePath() string             { return s.dbPath }
func (s *pagingRoomService) RuntimeStatus() map[string]any { return map[string]any{"running": true} }
func (s *pagingRoomService) CheckAdmission(context.Context, SessionRef, *pyjson.Object) error {
	return nil
}
func (s *pagingRoomService) AuthorizeRoom(_ context.Context, _, roomID any, _ bool, _ state.SQLiteConn) (bool, error) {
	if id, _ := roomID.(string); !strings.HasPrefix(id, "m") {
		return false, runtimeRefusal("permission_denied")
	}
	return true, nil
}

func TestGroupsListPagesTheActorVisibleRooms(t *testing.T) {
	ctx, db := newTestStateDB(t)
	for index, name := range []string{"f1", "f2", "m1", "f3", "m2", "m3"} {
		now := 1000.0 - float64(index)
		if _, err := CreateRoom(ctx, db.Path(), CreateRoomInput{RoomID: name, Name: name, Members: []any{},
			AuthorityGatewayID: "gw", Now: &now}); err != nil {
			t.Fatal(err)
		}
	}
	a := &SessionAuthority{DB: db, HostedRoomService: &pagingRoomService{dbPath: db.Path()}}
	actor := Principal{Subject: "me"}
	var pages [][]string
	params := map[string]any{"limit": int64(2)}
	for {
		result, err := groupControl(ctx, a, actor, "groups.list", params, false)
		if err != nil {
			t.Fatal(err)
		}
		page := result.(*pyjson.Object)
		var ids []string
		for _, room := range page.GetOr("rooms", nil).([]any) {
			ids = append(ids, room.(map[string]any)["room_id"].(string))
		}
		pages = append(pages, ids)
		next := page.GetOr("next_offset", nil)
		if next == nil {
			break
		}
		params = map[string]any{"limit": int64(2), "offset": next}
	}
	if !slices.EqualFunc(pages, [][]string{{"m1", "m2"}, {"m3"}}, slices.Equal[[]string]) {
		t.Fatalf("pages %v", pages)
	}
}
