package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Separate stdout and stderr buffers for the lifecycle prints, the capsys out and err split
func captureStreams(t *testing.T) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errOut bytes.Buffer
	t.Cleanup(SwapOutput(&out, &errOut))
	return &out, &errOut
}

// Session store at path opened read write, closed when the test ends
func openSessionDB(t *testing.T, path string) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// COUNT of gateway_heartbeats rows for one profile through a read connection
func heartbeatCount(t *testing.T, db *state.SessionDB, profile string) int {
	t.Helper()
	var n int
	err := db.ReadCtx(context.Background(), func(conn state.SQLiteConn) error {
		return conn.QueryRowContext(context.Background(), "SELECT COUNT(*) AS n FROM gateway_heartbeats WHERE profile = ?", profile).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Routing keys of the store at scope, sorted
func routingKeys(t *testing.T, db *state.SessionDB, scope string) []string {
	t.Helper()
	entries, err := db.LoadGatewayRoutingEntries(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Session key and profile name of one sessions row
func sessionIdentity(t *testing.T, db *state.SessionDB, id string) (string, string, bool) {
	t.Helper()
	var key, profile string
	found := false
	err := db.ReadCtx(context.Background(), func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(context.Background(), "SELECT session_key, profile_name FROM sessions WHERE id = ?", id)
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			found = true
			return rows.Scan(&key, &profile)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return key, profile, found
}

// Replaces the delete seams the Python identity cases patch, cleanup a no op and the multiplexer liveness fixed
func patchDeleteSeams(t *testing.T, live bool) {
	t.Helper()
	oldClean, oldLive := cleanupGatewayService, liveDefaultMultiplexer
	t.Cleanup(func() { cleanupGatewayService, liveDefaultMultiplexer = oldClean, oldLive })
	cleanupGatewayService = func(context.Context, string, string) bool { return false }
	liveDefaultMultiplexer = func() bool { return live }
}

func TestDeleteProfileDeletePurgesProfileKeyedIdentity(t *testing.T) {
	tmp := manageEnv(t)
	captureStreams(t)
	ctx := context.Background()
	for _, name := range []string{"gone", "keepme"} {
		if _, err := CreateProfile(ctx, name, CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	scope := filepath.Join(tmp, ".icebath", "sessions")
	dbPath := filepath.Join(tmp, ".icebath", "state.db")
	db := openSessionDB(t, dbPath)
	for _, row := range [][2]string{{"gone", "chatA"}, {"keepme", "chatB"}} {
		key := "agent:" + row[0] + ":feishu:dm:" + row[1]
		entry := `{"session_key": "` + key + `", "origin": {"platform": "feishu", "chat_id": "` + row[1] + `", "profile": "` + row[0] + `"}}`
		if err := db.SaveGatewayRoutingEntry(ctx, key, entry, scope); err != nil {
			t.Fatal(err)
		}
	}
	now := float64(time.Now().UnixNano()) / 1e9
	for i, profile := range []string{"gone", "keepme"} {
		id := map[string]string{"gone": "be-gone", "keepme": "be-keep"}[profile]
		if err := db.RegisterBackendHeartbeat(ctx, state.BackendHeartbeat{BackendID: id, PID: i + 1, StartedAt: now, Profile: profile, Host: "h"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	patchDeleteSeams(t, false)
	if _, err := DeleteProfile(ctx, "gone", true); err != nil {
		t.Fatal(err)
	}
	check := openSessionDB(t, dbPath)
	defer check.Close()
	if got := routingKeys(t, check, scope); !reflect.DeepEqual(got, []string{"agent:keepme:feishu:dm:chatB"}) {
		t.Fatalf("routing %v", got)
	}
	if n := heartbeatCount(t, check, "gone"); n != 0 {
		t.Fatalf("gone heartbeats %d", n)
	}
	if n := heartbeatCount(t, check, "keepme"); n != 1 {
		t.Fatalf("keepme heartbeats %d", n)
	}
}

func TestDeleteProfileDeleteReportsPendingSettlementForALiveMultiplexer(t *testing.T) {
	tmp := manageEnv(t)
	_, errOut := captureStreams(t)
	ctx := context.Background()
	if _, err := CreateProfile(ctx, "gone", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(tmp, ".icebath", "sessions")
	dbPath := filepath.Join(tmp, ".icebath", "state.db")
	db := openSessionDB(t, dbPath)
	if err := db.SaveGatewayRoutingEntry(ctx, "agent:gone:feishu:dm:chatA", `{"session_key": "agent:gone:feishu:dm:chatA"}`, scope); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	patchDeleteSeams(t, true)
	_, err := DeleteProfile(ctx, "gone", true)
	var pending *ProfileIdentitySettlementPending
	if !errors.As(err, &pending) || !strings.Contains(err.Error(), "identity settlement is still pending") {
		t.Fatalf("%v", err)
	}
	if pending.Profile != "gone" || pending.RetryCommand != "icebath profile purge-identity gone" {
		t.Fatalf("%+v", pending)
	}
	if pathExists(pending.Path) {
		t.Fatal("profile dir survives")
	}
	var rt *RuntimeError
	if !errors.As(err, &rt) {
		t.Fatal("settlement pending is not a RuntimeError")
	}
	if !strings.Contains(errOut.String(), "icebath profile purge-identity gone") {
		t.Fatalf("stderr %q", errOut.String())
	}
	check := openSessionDB(t, dbPath)
	defer check.Close()
	if got := routingKeys(t, check, scope); !reflect.DeepEqual(got, []string{"agent:gone:feishu:dm:chatA"}) {
		t.Fatalf("routing %v", got)
	}
}

func TestRenameProfileRenamesDirectory(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	if _, err := CreateProfile(ctx, "oldname", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(tmp, ".icebath", "profiles", "oldname")
	if !isDir(oldDir) {
		t.Fatal("old dir missing")
	}
	skipAliasCollision(t)
	newDir, err := RenameProfile(ctx, "oldname", "newname")
	if err != nil {
		t.Fatal(err)
	}
	if isDir(oldDir) || !isDir(newDir) || newDir != filepath.Join(tmp, ".icebath", "profiles", "newname") {
		t.Fatalf("old %v new %q", isDir(oldDir), newDir)
	}
}

func TestRenameProfileRenamesRootHonchoHostWithoutChangingAiPeer(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	if _, err := CreateProfile(ctx, "ssi_health", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	honcho := filepath.Join(tmp, ".icebath", "honcho.json")
	writeFile(t, honcho, `{"hosts": {"icebath.ssi_health": {"recallMode": "hybrid", "writeFrequency": "async", "sessionStrategy": "per-session", "saveMessages": true, "peerName": "user-peer", "aiPeer": "ssi_health", "workspace": "icebath", "enabled": true}}}`)
	skipAliasCollision(t)
	if _, err := RenameProfile(ctx, "ssi_health", "heimdall"); err != nil {
		t.Fatal(err)
	}
	cfg := readJSONObject(t, honcho)
	hosts, _ := cfg["hosts"].(map[string]any)
	if _, stale := hosts["icebath.ssi_health"]; stale {
		t.Fatalf("legacy host kept %v", hosts)
	}
	block, _ := hosts["icebath_heimdall"].(map[string]any)
	if block["aiPeer"] != "ssi_health" || block["peerName"] != "user-peer" {
		t.Fatalf("%v", hosts)
	}
}

func TestRenameProfileRenameMigratesSessionIdentityWithoutLiveGateway(t *testing.T) {
	tmp := manageEnv(t)
	captureStreams(t)
	ctx := context.Background()
	if _, err := CreateProfile(ctx, "oldname", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(tmp, ".icebath", "profiles", "oldname")
	pdb := openSessionDB(t, filepath.Join(oldDir, "state.db"))
	if err := pdb.CreateSession(state.NewSession{ID: "sess1", Source: "feishu", SessionKey: "agent:oldname:feishu:dm:chatA",
		ProfileName: "oldname", ChatID: "chatA", ChatType: "dm"}); err != nil {
		t.Fatal(err)
	}
	if err := pdb.Close(); err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(tmp, ".icebath", "sessions")
	rootPath := filepath.Join(tmp, ".icebath", "state.db")
	root := openSessionDB(t, rootPath)
	if err := root.SaveGatewayRoutingEntry(ctx, "agent:oldname:feishu:dm:chatA",
		`{"session_key": "agent:oldname:feishu:dm:chatA", "session_id": "sess1", "origin": {"platform": "feishu", "chat_id": "chatA", "profile": "oldname"}}`, scope); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	skipAliasCollision(t)
	oldLive := liveDefaultMultiplexer
	t.Cleanup(func() { liveDefaultMultiplexer = oldLive })
	liveDefaultMultiplexer = func() bool { return false }
	if _, err := RenameProfile(ctx, "oldname", "newname"); err != nil {
		t.Fatal(err)
	}
	moved := openSessionDB(t, filepath.Join(tmp, ".icebath", "profiles", "newname", "state.db"))
	key, profile, found := sessionIdentity(t, moved, "sess1")
	moved.Close()
	if !found || key != "agent:newname:feishu:dm:chatA" || profile != "newname" {
		t.Fatalf("%v %q %q", found, key, profile)
	}
	root2 := openSessionDB(t, rootPath)
	defer root2.Close()
	routing := routingKeys(t, root2, scope)
	if slices.Contains(routing, "agent:oldname:feishu:dm:chatA") || !slices.Contains(routing, "agent:newname:feishu:dm:chatA") {
		t.Fatalf("routing %v", routing)
	}
}

// JSON object of a utf-8-sig file
func readJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix([]byte(readText(t, path)), []byte("\xef\xbb\xbf")), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
