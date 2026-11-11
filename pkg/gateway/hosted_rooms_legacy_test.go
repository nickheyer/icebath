package gateway

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const hrLegacyReadChildEnv = "ICEBATH_TEST_HOSTED_LEGACY_READ"

func init() {
	if path := os.Getenv(hrLegacyReadChildEnv); path != "" {
		gatewayID, seq, err := hrReadLegacyState(path)
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Printf("%s %d\n", gatewayID, seq)
		os.Exit(0)
	}
}

func hrReadLegacyState(path string) (any, any, error) {
	roomState, err := RoomState(context.Background(), path, "room-1", false)
	if err != nil {
		return nil, nil, err
	}
	return roomState["authority_gateway_id"], roomState["latest_seq"], nil
}

// Draft database from before the actor and authority columns
func hrCreatePreActorDatabase(t *testing.T, path string) {
	t.Helper()
	conn := hrRawConn(t, path)
	defer conn.Close()
	for _, statement := range []string{`CREATE TABLE hosted_rooms (
                room_id TEXT PRIMARY KEY,
                name TEXT NOT NULL,
                members_json TEXT NOT NULL,
                next_seq INTEGER NOT NULL,
                revision INTEGER NOT NULL,
                created_at REAL NOT NULL,
                updated_at REAL NOT NULL,
                disbanded_at REAL
            )`, `CREATE TABLE hosted_room_events (
                room_id TEXT NOT NULL,
                seq INTEGER NOT NULL,
                event_id TEXT NOT NULL,
                kind TEXT NOT NULL,
                payload_json TEXT NOT NULL,
                created_at REAL NOT NULL,
                PRIMARY KEY (room_id, seq),
                UNIQUE (room_id, event_id)
            )`, `INSERT INTO hosted_rooms
               VALUES ('room-1', 'Legacy', '[]', 2, 1, 1, 1, NULL)`, `INSERT INTO hosted_room_events
               VALUES ('room-1', 1, 'legacy-event', 'message.created', '{}', 1)`} {
		if _, err := conn.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func hrRawExec(t *testing.T, db string, statements ...string) {
	t.Helper()
	conn := hrRawConn(t, db)
	defer conn.Close()
	for _, statement := range statements {
		if _, err := conn.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func hrEventIDs(page map[string]any) []any {
	var out []any
	for _, event := range page["events"].([]any) {
		out = append(out, event.(map[string]any)["event_id"])
	}
	return out
}

func TestPreActorDraftDatabaseMigratesWithExplicitLegacyIdentity(t *testing.T) {
	db := hrDB(t)
	hrCreatePreActorDatabase(t, db)
	replay := hrOK(hrRead(db, "room-1", 0, 100, false))
	if !pyjson.Equal(replay["events"].([]any)[0].(map[string]any)["actor"], map[string]any{"kind": "system", "id": "legacy"}) {
		t.Fatalf("replay %v", replay)
	}
	roomState := hrOK(RoomState(context.Background(), db, "room-1", false))
	if roomState["authority_gateway_id"] != "legacy" || roomState["authority_epoch"] != int64(1) {
		t.Fatalf("state %v", roomState)
	}
	adopted := hrOK(CreateRoom(context.Background(), db, CreateRoomInput{RoomID: "room-1", Name: "Legacy", Members: []any{}, AuthorityGatewayID: "gateway-a", Now: hrFloatPtr(2)}))
	claim := adopted["claim_event"].(map[string]any)
	if adopted["authority_gateway_id"] != "gateway-a" || adopted["authority_epoch"] != int64(2) || adopted["adopted"] != true ||
		claim["seq"] != int64(2) || claim["payload"].(map[string]any)["previous_gateway_id"] != "legacy" {
		t.Fatalf("adopted %v", adopted)
	}
}

func TestMigrationReservesExistingDisbandedRoomIdBeforePruning(t *testing.T) {
	db := hrDB(t)
	hrCreatePreActorDatabase(t, db)
	hrRawExec(t, db, "UPDATE hosted_rooms SET disbanded_at=20 WHERE room_id='room-1'")
	if pruned := hrOK(PruneDisbandedRooms(context.Background(), db, hrFloatPtr(20+disbandedRoomRetentionSeconds+1))); pruned != 1 {
		t.Fatalf("pruned %d", pruned)
	}
	hrAssertRetiredIdentityStaysReserved(t, db, "room-1", "room-new")
}

func TestLegacyAdoptionFillsMissingTargetsButRejectsTargetChanges(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	legacy := map[string]any{"member_id": "ops", "profile": "ops", "handle": "ops"}
	targeted := map[string]any{"member_id": "ops", "profile": "ops", "handle": "ops", "target": map[string]any{"kind": "local", "profile": "ops"}}
	hrOK(CreateRoom(ctx, db, CreateRoomInput{RoomID: "legacy-targets", Name: "Legacy targets", Members: []any{legacy}, AuthorityGatewayID: "legacy", Now: hrFloatPtr(1)}))
	adopted := hrOK(CreateRoom(ctx, db, CreateRoomInput{RoomID: "legacy-targets", Name: "Legacy targets", Members: []any{targeted}, AuthorityGatewayID: "gateway-a", Now: hrFloatPtr(2)}))
	if !pyjson.Equal(adopted["members"], []any{targeted}) {
		t.Fatalf("adopted %v", adopted)
	}
	peer := map[string]any{"member_id": "ops", "profile": "ops", "handle": "ops", "target": map[string]any{"kind": "peer", "installation_id": "install-b", "profile": "ops"}}
	_, err := CreateRoom(ctx, db, CreateRoomInput{RoomID: "legacy-targets", Name: "Legacy targets", Members: []any{peer}, AuthorityGatewayID: "gateway-a", Now: hrFloatPtr(3)})
	hrRequireErr[RoomConflictError](t, err, "different state")
}

func TestDraftSchemaMigrationIsSafeAcrossProcesses(t *testing.T) {
	db := hrDB(t)
	hrCreatePreActorDatabase(t, db)
	commands := make([]*exec.Cmd, 4)
	outputs := make([]*bytes.Buffer, 4)
	for i := range commands {
		outputs[i] = &bytes.Buffer{}
		commands[i] = exec.Command(os.Args[0], "-test.run=^$")
		commands[i].Env = append(os.Environ(), hrLegacyReadChildEnv+"="+db)
		commands[i].Stdout = outputs[i]
		commands[i].Stderr = outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("child %d: %v %s", i, err, outputs[i])
		}
		if got := strings.TrimSpace(outputs[i].String()); !strings.HasSuffix(got, "legacy 1") {
			t.Fatalf("child %d read %q", i, got)
		}
	}
	replay := hrOK(hrRead(db, "room-1", 0, 100, false))
	if !pyjson.Equal(replay["events"].([]any)[0].(map[string]any)["actor"], map[string]any{"kind": "system", "id": "legacy"}) {
		t.Fatalf("replay %v", replay)
	}
}

func TestLegacyRemoteRunReceiptMigratesWithoutCurrentLineageAccess(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	hrRawExec(t, db, `CREATE TABLE hosted_room_remote_runs (
                room_id TEXT NOT NULL,
                member_id TEXT NOT NULL,
                task_id TEXT NOT NULL,
                execution_generation INTEGER NOT NULL,
                target_install_id TEXT NOT NULL,
                target_profile TEXT NOT NULL,
                run_id TEXT NOT NULL,
                session_id TEXT NOT NULL,
                created_at REAL NOT NULL,
                updated_at REAL NOT NULL,
                PRIMARY KEY (task_id, execution_generation)
            )`, `INSERT INTO hosted_room_remote_runs VALUES(
                'room-1', 'member-reviewer', 'task-1', 1, 'install-peer',
                'reviewer', 'run-legacy', 'session-legacy', 1, 1
            )`)
	current := map[string]any{"room_id": "room-1", "home_install_id": "install-home", "authority_gateway_id": "gateway-home", "authority_epoch": 2,
		"member_id": "member-reviewer", "target_install_id": "install-peer", "target_profile": "reviewer", "task_id": "task-1", "execution_generation": 1}
	if receipt := hrOK(RemoteRunReceipt(ctx, db, current)); receipt != nil {
		t.Fatalf("receipt %v", receipt)
	}
	legacy := hrOK(ListRemoteRunReceipts(ctx, db, nil, nil, nil))
	if legacy[0]["home_install_id"] != "legacy" || legacy[0]["authority_gateway_id"] != "legacy" {
		t.Fatalf("legacy %v", legacy)
	}
	record := map[string]any{"run_id": "run-current", "session_id": "session-current"}
	for key, value := range current {
		record[key] = value
	}
	if err := UpsertRemoteRunReceipt(ctx, db, record, nil); err != nil {
		t.Fatal(err)
	}
	if runID := hrOK(RemoteRunReceipt(ctx, db, current))["run_id"]; runID != "run-current" {
		t.Fatal(runID)
	}
}

func TestRoomLogPageBoundCountsBytesNotCharacters(t *testing.T) {
	db := hrDB(t)
	hrOK(hrCreate(db, "room-1"))
	text := strings.Repeat("\U0001F600", 200)
	for index := range 2 {
		hrOK(hrAppend(db, AppendEventInput{RoomID: "room-1", EventID: fmt.Sprintf("bytes-%d", index), Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": text, "index": index}}))
	}
	pageJSON := func(page map[string]any) string {
		return hrOK(pyjson.Dumps(page, pyjson.EnsureASCII(false), pyjson.Separators(",", ":")))
	}
	oneEvent := hrOK(hrRead(db, "room-1", 0, 1, false))
	unboundedTwo := hrOK(hrRead(db, "room-1", 0, 2, false))
	budget := int64(len(pageJSON(oneEvent)) + 64)
	if int64(len([]rune(pageJSON(unboundedTwo)))) > budget || int64(len(pageJSON(unboundedTwo))) <= budget {
		t.Fatal("budget does not discriminate characters from bytes")
	}
	hrSetBound(t, &maxLogPageBytes, budget)
	page := hrOK(hrRead(db, "room-1", 0, 2, false))
	if !pyjson.Equal(hrSeqs(page), []any{1}) || int64(len(pageJSON(page))) > budget || page["has_more"] != true {
		t.Fatalf("page %v", page)
	}
}

func TestDefaultDbPathNeverNamesTheMasterSessionStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	profileHome := filepath.Join(root, "profiles", "bot1")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profileHome)
	fromProfile := HostedRoomsDefaultDBPath(context.Background())
	t.Setenv("ICEBATH_HOME", root)
	fromRoot := HostedRoomsDefaultDBPath(context.Background())
	if fromProfile != fromRoot || filepath.Dir(fromRoot) != root || filepath.Base(fromRoot) == "state.db" {
		t.Fatalf("profile %s root %s", fromProfile, fromRoot)
	}
}

func TestUpgradeKeepsRoomsFromBeforeTheSharedStateDbSplit(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	legacy := filepath.Join(dir, "state.db")
	ctx := context.Background()
	hrOK(hrCreate(legacy, "room-1"))
	hrOK(hrAppend(legacy, AppendEventInput{RoomID: "room-1", EventID: "event-1", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "before the upgrade"}, Now: hrFloatPtr(11)}))
	store := filepath.Join(dir, "shared-state.db")
	if listed := hrOK(ListRooms(ctx, store, false, int64(MaxRoomListLimit), int64(0))); len(listed) != 1 || listed[0]["room_id"] != "room-1" {
		t.Fatalf("listed %v", listed)
	}
	if latest := hrOK(RoomState(ctx, store, "room-1", false))["latest_seq"]; latest != int64(1) {
		t.Fatal(latest)
	}
	if ids := hrEventIDs(hrOK(hrRead(store, "room-1", 0, 100, false))); !pyjson.Equal(ids, []any{"event-1"}) {
		t.Fatal(ids)
	}
}

func TestLegacyImportIsAOneShotAndSkipsDriverLivenessState(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	ctx := context.Background()
	legacy := filepath.Join(dir, "state.db")
	hrOK(hrCreate(legacy, "room-1"))
	now := func() any { return 100.0 }
	identity := hrOK(NewTaskIdentity("room-1", "task-1", "thread-1", "turn-1"))
	hrOK(DriverAdmitTask(ctx, legacy, identity, map[string]any{"target_profile": "ops", "prompt": "ping", "source_event_seq": 1}, now))
	hrOK(DriverAcquireLease(ctx, legacy, "room-1", "gateway-a", 1, "process-a", 30, now))
	store := filepath.Join(dir, "shared-state.db")
	if listed := hrOK(ListRooms(ctx, store, false, int64(MaxRoomListLimit), int64(0))); len(listed) != 1 || listed[0]["room_id"] != "room-1" {
		t.Fatalf("listed %v", listed)
	}
	if hrInt(hrRawRow(t, store, "SELECT COUNT(*) AS n FROM hosted_room_driver_tasks"), "n") != 1 ||
		hrInt(hrRawRow(t, store, "SELECT COUNT(*) AS n FROM sqlite_master WHERE type='table' AND name='hosted_room_driver_leases'"), "n") != 0 {
		t.Fatal("durable work or liveness state crossed wrongly")
	}
	hrOK(RenameRoom(ctx, store, "room-1", "rename-1", "Renamed", hrFloatPtr(12)))
	hrRawExec(t, store, "DELETE FROM hosted_room_legacy_imports")
	if name := hrOK(RoomState(ctx, store, "room-1", false))["name"]; name != "Renamed" {
		t.Fatal(name)
	}
	hrRawExec(t, store, "DELETE FROM hosted_rooms")
	if listed := hrOK(ListRooms(ctx, store, false, int64(MaxRoomListLimit), int64(0))); len(listed) != 0 {
		t.Fatalf("listed %v", listed)
	}
}

func TestLegacyImportSkipsARoomThisStoreAlreadyOwnsAsAUnit(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	ctx := context.Background()
	legacy := filepath.Join(dir, "state.db")
	hrOK(hrCreate(legacy, "same"))
	for index := range 5 {
		hrOK(hrAppend(legacy, AppendEventInput{RoomID: "same", EventID: fmt.Sprintf("legacy-%d", index), Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": fmt.Sprint(index)}, Now: hrFloatPtr(float64(11 + index))}))
	}
	own := filepath.Join(dir, "scratch.db")
	hrOK(hrCreate(own, "same"))
	hrOK(hrAppend(own, AppendEventInput{RoomID: "same", EventID: "own-1", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "s"}, Now: hrFloatPtr(11)}))
	store := filepath.Join(dir, "shared-state.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(own + suffix); err == nil {
			if err := os.Rename(own+suffix, store+suffix); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ids := hrEventIDs(hrOK(hrRead(store, "same", 0, 100, false))); !pyjson.Equal(ids, []any{"own-1"}) {
		t.Fatal(ids)
	}
	hrOK(hrAppend(store, AppendEventInput{RoomID: "same", EventID: "own-2", Kind: "message.user", Actor: hrUser, Payload: map[string]any{"text": "t"}, Now: hrFloatPtr(30)}))
	if latest := hrOK(RoomState(ctx, store, "same", false))["latest_seq"]; latest != int64(2) {
		t.Fatal(latest)
	}
}

func TestLegacyImportReadsLayoutsFromBeforeTheActorAndAuthorityColumns(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	hrCreatePreActorDatabase(t, filepath.Join(dir, "state.db"))
	store := filepath.Join(dir, "shared-state.db")
	gatewayID, seq, err := hrReadLegacyState(store)
	if err != nil || gatewayID != "legacy" || seq != int64(1) {
		t.Fatalf("state %v %v %v", gatewayID, seq, err)
	}
	if ids := hrEventIDs(hrOK(hrRead(store, "room-1", 0, 100, false))); !pyjson.Equal(ids, []any{"legacy-event"}) {
		t.Fatal(ids)
	}
	if hrInt(hrRawRow(t, store, "SELECT rooms FROM hosted_room_legacy_imports"), "rooms") != 1 || hrInt(hrRawRow(t, store, "SELECT event_bytes FROM hosted_rooms"), "event_bytes") <= 0 {
		t.Fatal("legacy rows did not import")
	}
}

func TestUnreadableLegacyStoreIsReportedOncePerProcess(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.db"), bytes.Repeat([]byte("not a sqlite file"), 100), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	store := filepath.Join(dir, "shared-state.db")
	for range 4 {
		if listed := hrOK(ListRooms(context.Background(), store, false, int64(MaxRoomListLimit), int64(0))); len(listed) != 0 {
			t.Fatalf("listed %v", listed)
		}
	}
	if count := strings.Count(logs.String(), "could not import"); count != 1 {
		t.Fatalf("warned %d times: %s", count, logs.String())
	}
	if hrRawRow(t, store, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='hosted_room_legacy_imports'") != nil {
		t.Fatal("marker recorded for an unreadable store")
	}
}
