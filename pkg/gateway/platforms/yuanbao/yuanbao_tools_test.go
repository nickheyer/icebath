package yuanbao

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Decoded JSON result of a registered Yuanbao tool called with the arguments
func callTool(t *testing.T, name string, args map[string]any) *pyjson.Object {
	t.Helper()
	def := tools.Default.Get(name)
	if def == nil || def.Toolset != yuanbaoToolset || def.Check == nil {
		t.Fatalf("tool %s is not registered for the yuanbao toolset", name)
	}
	decoded, err := pyjson.Loads(def.Handler(context.Background(), &tools.Call{Name: name, Args: args}))
	if err != nil {
		t.Fatal(err)
	}
	result, isObject := decoded.(*pyjson.Object)
	if !isObject {
		t.Fatalf("tool %s returned %v", name, decoded)
	}
	return result
}

// Compact JSON of a decoded tool result
func compact(t *testing.T, value any) string {
	t.Helper()
	encoded, err := pyjson.Dumps(value, pyjson.EnsureASCII(false))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// Connected adapter whose server knows one group with a user, an assistant and a namesake
func connectGroupAdapter(t *testing.T) (*YuanbaoAdapter, *fakeServer) {
	t.Helper()
	adapter, server := connectAdapter(t, nil)
	group, info, members := &pbuf{}, &pbuf{}, &pbuf{}
	group.s(1, "Team")
	group.s(2, "owner-1")
	group.s(3, "Olivia")
	group.v(4, big.NewInt(3))
	info.m(3, group.b)
	for _, row := range [][2]string{{"u1", "Alice"}, {"u2", "Alina"}, {"u3", "Bob"}} {
		member := &pbuf{}
		member.s(1, row[0])
		member.s(2, row[1])
		members.m(3, member.b)
	}
	server.mu.Lock()
	server.replies["query_group_info"], server.replies["get_group_member_list"] = info.b, members.b
	server.mu.Unlock()
	return adapter, server
}

func TestToolsNeedAConnectedAdapterAndGateOnTheSession(t *testing.T) {
	isolatedHome(t)
	want := `{"success": false, "error": "Yuanbao adapter is not connected"}`
	for name, args := range map[string]map[string]any{"yb_query_group_info": {"group_code": "grp-1"},
		"yb_query_group_members": {"group_code": "grp-1"}, "yb_send_sticker": {"chat_id": "direct:alice"}, "yb_send_dm": {"message": "hi"}} {
		if got := compact(t, callTool(t, name, args)); got != want {
			t.Fatalf("%s without an adapter %s", name, got)
		}
	}
	t.Setenv("ICEBATH_SESSION_CHAT_ID", "")
	t.Setenv("ICEBATH_SESSION_PLATFORM", "")
	for name, wantError := range map[string]string{"yb_query_group_info": "group_code is required", "yb_query_group_members": "group_code is required",
		"yb_send_sticker": "chat_id is required (no active yuanbao session detected)", "yb_send_dm": "message or media_files is required"} {
		if got := compact(t, callTool(t, name, map[string]any{})); got != `{"success": false, "error": "`+wantError+`"}` {
			t.Fatalf("%s without arguments gave %s", name, got)
		}
	}
	env := &tools.Env{Home: os.Getenv("ICEBATH_HOME")}
	if checkYuanbao(env) {
		t.Fatal("the tools must hide outside a yuanbao session with no adapter")
	}
	t.Setenv("ICEBATH_SESSION_PLATFORM", "yuanbao")
	if !checkYuanbao(env) {
		t.Fatal("a yuanbao session must show the tools")
	}
	t.Setenv("ICEBATH_SESSION_PLATFORM", "telegram")
	connectAdapter(t, nil)
	if !checkYuanbao(&tools.Env{Home: os.Getenv("ICEBATH_HOME")}) {
		t.Fatal("a connected adapter must show the tools")
	}
}

func TestGroupInfoAndMemberTools(t *testing.T) {
	connectGroupAdapter(t)
	info := callTool(t, "yb_query_group_info", map[string]any{"group_code": "grp-1"})
	if got := compact(t, info); got != `{"success": true, "group_code": "grp-1", "group_name": "Team", "member_count": 3, `+
		`"owner": {"user_id": "owner-1", "nickname": "Olivia"}, "note": "The group is called \"派 (Pai)\" in the app."}` {
		t.Fatalf("group info %s", got)
	}
	all := callTool(t, "yb_query_group_members", map[string]any{"group_code": "grp-1", "action": "list_all", "mention": true})
	if got := compact(t, all); got != `{"success": true, "msg": "Found 3 member(s).", "members": [{"user_id": "u1", "nickname": "Alice", "role": "unknown"}, `+
		`{"user_id": "u2", "nickname": "Alina", "role": "unknown"}, {"user_id": "u3", "nickname": "Bob", "role": "unknown"}], "mention_hint": "`+
		strings.ReplaceAll(mentionHint, `"`, `\"`)+`"}` {
		t.Fatalf("all members %s", got)
	}
	found := callTool(t, "yb_query_group_members", map[string]any{"group_code": "grp-1", "action": "find", "name": " ALI "})
	if members := found.GetOr("members", nil).([]any); found.GetOr("msg", nil) != `Found 2 member(s) matching " ALI ".` || len(members) != 2 {
		t.Fatalf("found members %s", compact(t, found))
	}
	missed := callTool(t, "yb_query_group_members", map[string]any{"group_code": "grp-1", "action": "find", "name": "zed"})
	if members := missed.GetOr("members", nil).([]any); missed.GetOr("success", nil) != false || len(members) != 3 ||
		missed.GetOr("msg", nil) != `No match for "zed". All members listed below.` {
		t.Fatalf("missed members %s", compact(t, missed))
	}
	if got := compact(t, callTool(t, "yb_query_group_members", map[string]any{"group_code": "grp-1", "action": "list_bots"})); got !=
		`{"success": false, "error": "No bots found in this group."}` {
		t.Fatalf("bots %s", got)
	}
	if got := compact(t, callTool(t, "yb_query_group_info", map[string]any{"group_code": int64(7)})); got !=
		`{"success": false, "error": "'int' object has no attribute 'encode'"}` {
		t.Fatalf("group code of the wrong type %s", got)
	}
}

func TestStickerTools(t *testing.T) {
	_, server := connectAdapter(t, nil)
	listed := callTool(t, "yb_search_sticker", map[string]any{"limit": int64(3)})
	rows := listed.GetOr("results", nil).([]any)
	first := rows[0].(*pyjson.Object)
	if listed.GetOr("success", nil) != true || listed.GetOr("query", nil) != "" || !pyjson.Equal(listed.GetOr("count", nil), int64(3)) || len(rows) != 3 ||
		!reflect.DeepEqual(first.Keys, []string{"sticker_id", "name", "description", "package_id"}) {
		t.Fatalf("listing %s", compact(t, listed))
	}
	for limit, want := range map[any]int{int64(999): 50, int64(-4): 1, "many": 10, nil: 10, "2": 2} {
		got := callTool(t, "yb_search_sticker", map[string]any{"query": "", "limit": limit})
		if !pyjson.Equal(got.GetOr("count", nil), int64(want)) {
			t.Fatalf("limit %v returned %v results", limit, got.GetOr("count", nil))
		}
	}
	name, stickerID := first.GetOr("name", "").(string), first.GetOr("sticker_id", "").(string)
	t.Setenv("ICEBATH_SESSION_CHAT_ID", "group:grp-1")
	byID := callTool(t, "yb_send_sticker", map[string]any{"sticker": " " + stickerID + " ", "reply_to": "ref-9"})
	if byID.GetOr("success", nil) != true || byID.GetOr("chat_id", nil) != "group:grp-1" ||
		compact(t, byID.GetOr("sticker", nil)) != compact(t, pyjson.ObjectOf("sticker_id", stickerID, "name", name)) {
		t.Fatalf("sticker by id %s", compact(t, byID))
	}
	frame := server.sent("send_group_message")[0]
	fields, err := parseDict(frame.Vals["data"].([]byte))
	if err != nil {
		t.Fatal(err)
	}
	if elems := bodyElems(t, frame, 6); len(elems) != 1 || elems[0].Vals["msg_type"] != "TIMFaceElem" || fields.GetString(7) != "ref-9" {
		t.Fatalf("sticker frame %v", elems)
	}
	byName := callTool(t, "yb_send_sticker", map[string]any{"sticker": name, "chat_id": " direct:alice "})
	if byName.GetOr("success", nil) != true || byName.GetOr("chat_id", nil) != "direct:alice" || len(server.sent("send_c2c_message")) != 1 {
		t.Fatalf("sticker by name %s", compact(t, byName))
	}
	if random := callTool(t, "yb_send_sticker", map[string]any{}); random.GetOr("success", nil) != true {
		t.Fatalf("random sticker %s", compact(t, random))
	}
}

func TestSendDMToolResolvesTheRecipientAndSendsMedia(t *testing.T) {
	backend := startCOSBackend(t, pyjson.ObjectOf("bucketName", "bucket-1", "region", "ap-test", "location", "media/doc.txt",
		"encryptTmpSecretId", "sid", "encryptTmpSecretKey", "skey", "encryptToken", "session", "resourceUrl", "https://cdn.example.com/res/2"))
	adapter, server := connectGroupAdapter(t)
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	adapter.accessPolicy = makePolicy(t, "open", nil, "pairing", nil)
	t.Setenv("ICEBATH_SESSION_CHAT_ID", "group:grp-1")
	ambiguous := callTool(t, "yb_send_dm", map[string]any{"name": "ali", "message": "hi"})
	if got := compact(t, ambiguous); got != `{"success": false, "error": "Multiple members match \"ali\". Please specify which one.", `+
		`"candidates": [{"user_id": "u1", "nickname": "Alice"}, {"user_id": "u2", "nickname": "Alina"}]}` {
		t.Fatalf("ambiguous recipient %s", got)
	}
	if got := compact(t, callTool(t, "yb_send_dm", map[string]any{"name": "zed", "message": "hi"})); got !=
		`{"success": false, "error": "No member matching \"zed\" found in group grp-1."}` {
		t.Fatalf("unknown recipient %s", got)
	}
	document := filepath.Join(os.Getenv("ICEBATH_HOME"), "cache", "documents", "note.txt")
	if err := os.MkdirAll(filepath.Dir(document), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(document, []byte("attached"), 0o644); err != nil {
		t.Fatal(err)
	}
	sent := callTool(t, "yb_send_dm", map[string]any{"name": "bob", "message": "hello Bob", "media_files": []any{pyjson.ObjectOf("path", document)}})
	if got := compact(t, sent); got != `{"success": true, "user_id": "u3", "nickname": "Bob", "message_id": "", "note": "DM sent to \"Bob\" successfully."}` {
		t.Fatalf("sent DM %s", got)
	}
	frames := server.sent("send_c2c_message")
	if len(frames) != 2 || firstText(t, frames[0], 5) != "hello Bob" || bodyElems(t, frames[1], 5)[0].Vals["msg_type"] != "TIMFileElem" ||
		backend.uploaded("/media/doc.txt") != "attached" {
		t.Fatalf("DM frames %d", len(frames))
	}
	for _, frame := range frames {
		fields, err := parseDict(frame.Vals["data"].([]byte))
		if err != nil {
			t.Fatal(err)
		}
		if fields.GetString(2) != "u3" || fields.GetString(6) != "grp-1" {
			t.Fatalf("DM went to %q from group %q", fields.GetString(2), fields.GetString(6))
		}
	}
	if got := compact(t, callTool(t, "yb_send_dm", map[string]any{"user_id": int64(5), "message": "hi"})); got !=
		`{"success": false, "error": "'int' object has no attribute 'strip'"}` {
		t.Fatalf("user id of the wrong type %s", got)
	}
	adapter.accessPolicy = makePolicy(t, "allowlist", []string{"someone-else"}, "pairing", nil)
	if got := compact(t, callTool(t, "yb_send_dm", map[string]any{"user_id": " u9 ", "name": " Zoe ", "message": "hi"})); got !=
		`{"success": false, "error": "DM access denied for this user"}` {
		t.Fatalf("denied DM %s", got)
	}
}
