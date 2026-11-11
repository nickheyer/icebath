package tuiserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/skills"
)

// One captured global broadcast
type watchEvent struct {
	event   string
	payload map[string]any
}

// Captured broadcasts of the change watcher
type watchEvents struct {
	sync.Mutex
	list []watchEvent
}

func (e *watchEvents) has(event string) bool {
	e.Lock()
	defer e.Unlock()
	for _, item := range e.list {
		if item.event == event && reflect.DeepEqual(item.payload, map[string]any{}) {
			return true
		}
	}
	return false
}

func (e *watchEvents) named(event string) []watchEvent {
	e.Lock()
	defer e.Unlock()
	out := []watchEvent{}
	for _, item := range e.list {
		if item.event == event {
			out = append(out, item)
		}
	}
	return out
}

func (e *watchEvents) all() []watchEvent {
	e.Lock()
	defer e.Unlock()
	return slices.Clone(e.list)
}

func (e *watchEvents) clear() {
	e.Lock()
	e.list = nil
	e.Unlock()
}

// Temp launch home with fresh watcher state and a broadcast capture
func watcherHomeFixture(t *testing.T) (string, *watchEvents) {
	t.Helper()
	h := t.TempDir()
	watchWriteFile(t, filepath.Join(h, "config.yaml"), "display: {}\n")
	if err := os.Mkdir(filepath.Join(h, "cron"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	prevHome, prevBroadcast := hermesHome, broadcastGlobalEvent
	servedProfileMu.Lock()
	prevServed := servedProfileHomes
	servedProfileHomes = map[string]bool{}
	servedProfileMu.Unlock()
	hermesHome = h
	changeState.Lock()
	changeState.sigs, changeState.checkedAt, changeState.broadcastAt = map[string]any{}, map[string]float64{}, map[string]float64{}
	changeState.Unlock()
	botRelayOutboxSeen.Lock()
	botRelayOutboxSeen.ns = 0
	botRelayOutboxSeen.Unlock()
	pairingRootsCache.Lock()
	pairingRootsCache.set = false
	pairingRootsCache.Unlock()
	sessionsDBSigCache.Lock()
	sessionsDBSigCache.byPath = map[string]sessionsDBSigEntry{}
	sessionsDBSigCache.Unlock()
	events := &watchEvents{}
	broadcastGlobalEvent = func(event string, payload map[string]any) {
		events.Lock()
		events.list = append(events.list, watchEvent{event, payload})
		events.Unlock()
	}
	t.Cleanup(func() {
		hermesHome, broadcastGlobalEvent = prevHome, prevBroadcast
		servedProfileMu.Lock()
		servedProfileHomes = prevServed
		servedProfileMu.Unlock()
	})
	return h, events
}

func watchWriteFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirs(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func execSQL(t *testing.T, dbPath string, statements ...string) {
	t.Helper()
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, statement := range statements {
		if _, err := conn.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func writeSessionChange(t *testing.T, dbPath, title string) {
	t.Helper()
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec("CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, title TEXT, last_activity_at REAL, message_count INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("INSERT INTO sessions(id, title, last_activity_at, message_count) VALUES ('s1', ?, 1, 1) ON CONFLICT(id) DO UPDATE SET title = excluded.title", title); err != nil {
		t.Fatal(err)
	}
}

func seedWatcherStore(t *testing.T, dbPath string) {
	t.Helper()
	execSQL(t, dbPath,
		"CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT, started_at REAL, message_count INTEGER, last_activity_at REAL, last_activity_description TEXT)",
		"CREATE TABLE gateway_heartbeats (backend_id TEXT PRIMARY KEY, last_heartbeat REAL)",
		"INSERT INTO sessions VALUES ('s1', 'hello', 1, 3, 1, NULL)",
		"INSERT INTO gateway_heartbeats VALUES ('backend-1', 1)")
}

func watchPass(now float64) { broadcastWatchedChanges(context.Background(), now) }

func TestFirstSightingSeedsWithoutBroadcasting(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchWriteFile(t, filepath.Join(h, "cron", "jobs.json"), "[]")
	watchWriteFile(t, filepath.Join(h, "state.db"), "x")
	watchPass(0)
	if got := events.all(); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
}

func TestCronJobsFileMoveBroadcastsCronChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchPass(0)
	watchWriteFile(t, filepath.Join(h, "cron", "jobs.json"), "[]")
	watchPass(10)
	if !events.has("cron.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestStateDbMoveBroadcastsSessionsChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchPass(0)
	writeSessionChange(t, filepath.Join(h, "state.db"), "created")
	watchPass(10)
	if !events.has("sessions.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestActivityHeartbeatsDoNotBroadcastSessionsChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	db := filepath.Join(h, "state.db")
	seedWatcherStore(t, db)
	watchPass(0)
	for tick := 1; tick < 4; tick++ {
		time.Sleep(20 * time.Millisecond)
		conn, err := sql.Open("sqlite", db)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec("UPDATE gateway_heartbeats SET last_heartbeat = ?", float64(tick)*60); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec("UPDATE sessions SET last_activity_at = ?, last_activity_description = ? WHERE id = 's1'", float64(tick)*60, "tool "+string(rune('0'+tick))); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		watchPass(float64(tick) * 10)
	}
	if events.has("sessions.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestSessionRowChangesBroadcastSessionsChanged(t *testing.T) {
	for _, statement := range []string{
		"INSERT INTO sessions VALUES ('s2', 'new', 2, 0, 2, NULL)",
		"UPDATE sessions SET title = 'renamed' WHERE id = 's1'",
		"UPDATE sessions SET message_count = 4 WHERE id = 's1'",
		"DELETE FROM sessions WHERE id = 's1'",
	} {
		t.Run(statement, func(t *testing.T) {
			h, events := watcherHomeFixture(t)
			db := filepath.Join(h, "state.db")
			seedWatcherStore(t, db)
			watchPass(0)
			time.Sleep(20 * time.Millisecond)
			execSQL(t, db, statement)
			watchPass(10)
			if !events.has("sessions.changed") {
				t.Fatalf("events = %v", events.all())
			}
		})
	}
}

func TestUnreadableStoreKeepsLastDigest(t *testing.T) {
	h, events := watcherHomeFixture(t)
	db := filepath.Join(h, "state.db")
	seedWatcherStore(t, db)
	watchPass(0)
	watchWriteFile(t, db, "not-sqlite")
	watchPass(10)
	if events.has("sessions.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestProjectsDbMoveBroadcastsProjectsChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchPass(0)
	watchWriteFile(t, filepath.Join(h, "projects.db"), "x")
	watchPass(10)
	if !events.has("projects.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestServedProfileProjectsDbMoveBroadcastsProjectsChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	coder := filepath.Join(h, "profiles", "coder")
	watchWriteFile(t, filepath.Join(coder, "config.yaml"), "{}\n")
	if got, err := profileHome("coder"); err != nil || got != coder {
		t.Fatalf("profileHome = %q, %v", got, err)
	}
	watchPass(0)
	watchWriteFile(t, filepath.Join(coder, "projects.db"), "x")
	watchPass(10)
	if !events.has("projects.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestProjectsSigDoesNotTrackStateDbWrites(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchPass(0)
	watchWriteFile(t, filepath.Join(h, "state.db"), "x")
	watchPass(10)
	if events.has("projects.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestServedProfileStoreMoveBroadcastsSessionsChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	bot := filepath.Join(h, "profiles", "bot")
	watchWriteFile(t, filepath.Join(bot, "config.yaml"), "{}\n")
	if got, err := profileHome("bot"); err != nil || got != bot {
		t.Fatalf("profileHome = %q, %v", got, err)
	}
	watchPass(0)
	writeSessionChange(t, filepath.Join(bot, "state.db"), "created")
	watchPass(10)
	if !events.has("sessions.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestGatewayStateMoveBroadcastsPlatformsChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchPass(0)
	watchWriteFile(t, filepath.Join(h, "gateway_state.json"), `{"platforms": {}}`)
	watchPass(10)
	if !events.has("platforms.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestPendingPairingRequestBroadcastsPairingChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	store := filepath.Join(h, "platforms", "pairing")
	mkdirs(t, store)
	watchPass(0)
	watchWriteFile(t, filepath.Join(store, "telegram-pending.json"), `{"abc": {"user_id": "1"}}`)
	watchPass(10)
	if !events.has("pairing.changed") || events.has("platforms.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestPairingSignalFollowsAProfileStore(t *testing.T) {
	h, events := watcherHomeFixture(t)
	store := filepath.Join(h, "profiles", "work", "platforms", "pairing")
	mkdirs(t, store)
	watchWriteFile(t, filepath.Join(h, "profiles", "work", "config.yaml"), "{}\n")
	watchPass(0)
	watchWriteFile(t, filepath.Join(store, "telegram-approved.json"), `{"u1": {"user_id": "u1"}}`)
	watchPass(10)
	if !events.has("pairing.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestPairingProbeReusesLiveProfileRootsUntilTheProfileSetMoves(t *testing.T) {
	h, events := watcherHomeFixture(t)
	var liveCalls []string
	prevLive := namedProfileIsLive
	namedProfileIsLive = func(p string) bool {
		liveCalls = append(liveCalls, filepath.Base(p))
		return prevLive(p)
	}
	t.Cleanup(func() { namedProfileIsLive = prevLive })
	profile := func(name string) {
		mkdirs(t, filepath.Join(h, "profiles", name, "platforms", "pairing"))
		watchWriteFile(t, filepath.Join(h, "profiles", name, "config.yaml"), "{}\n")
	}
	profile("work")
	watchPass(0)
	watchWriteFile(t, filepath.Join(h, "profiles", "work", "platforms", "pairing", "telegram-pending.json"), "{}")
	watchPass(10)
	pairingOnly := func(n int) bool {
		got := events.all()
		if len(got) != n {
			return false
		}
		for _, e := range got {
			if e.event != "pairing.changed" || len(e.payload) != 0 {
				return false
			}
		}
		return true
	}
	if !pairingOnly(1) || !slices.Equal(liveCalls, []string{"work"}) {
		t.Fatalf("events = %v, live calls = %v", events.all(), liveCalls)
	}
	profile("play")
	bump := time.Unix(0, 1e18)
	if err := os.Chtimes(filepath.Join(h, "profiles"), time.Unix(0, 0), bump); err != nil {
		t.Fatal(err)
	}
	watchPass(20)
	watchWriteFile(t, filepath.Join(h, "profiles", "play", "platforms", "pairing", "discord-approved.json"), "{}")
	watchPass(30)
	sorted := slices.Clone(liveCalls)
	sort.Strings(sorted)
	if !pairingOnly(2) || !slices.Equal(sorted, []string{"play", "work", "work"}) {
		t.Fatalf("events = %v, live calls = %v", events.all(), liveCalls)
	}
}

func TestSecondLedgerWithTheSameMtimeStillBroadcastsPairingChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	store := filepath.Join(h, "platforms", "pairing")
	first := filepath.Join(store, "telegram-pending.json")
	watchWriteFile(t, first, "{}")
	watchPass(0)
	second := filepath.Join(store, "discord-pending.json")
	watchWriteFile(t, second, "{}")
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(second, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	watchPass(10)
	if got := events.all(); len(got) != 1 || got[0].event != "pairing.changed" {
		t.Fatalf("events = %v", got)
	}
}

func TestSiblingProjectsDbWriteInTheSameMtimeTickBroadcasts(t *testing.T) {
	h, events := watcherHomeFixture(t)
	coder := filepath.Join(h, "profiles", "coder")
	mkdirs(t, coder)
	servedProfileMu.Lock()
	servedProfileHomes = map[string]bool{coder: true}
	servedProfileMu.Unlock()
	watchWriteFile(t, filepath.Join(h, "projects.db"), "x")
	watchPass(0)
	watchWriteFile(t, filepath.Join(coder, "projects.db"), "x")
	info, err := os.Stat(filepath.Join(h, "projects.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(coder, "projects.db"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	watchPass(10)
	if !events.has("projects.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestRateLimitChurnDoesNotBroadcastPairingChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	store := filepath.Join(h, "platforms", "pairing")
	watchWriteFile(t, filepath.Join(store, "telegram-pending.json"), "{}")
	watchPass(0)
	watchWriteFile(t, filepath.Join(store, "_rate_limits.json"), `{"telegram:1": 123}`)
	watchPass(10)
	if events.has("pairing.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestSessionsFloorCoalescesBurstButKeepsTrailingEdge(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchPass(0)
	writeSessionChange(t, filepath.Join(h, "state.db"), "first")
	watchPass(10)
	events.clear()
	time.Sleep(20 * time.Millisecond)
	writeSessionChange(t, filepath.Join(h, "state.db"), "second")
	watchPass(11)
	if got := events.all(); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
	watchPass(13)
	if !events.has("sessions.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestPetSigStaysOffWithoutARenderablePet(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchPass(0)
	watchWriteFile(t, filepath.Join(h, "config.yaml"), "display:\n  pet:\n    enabled: true\n    slug: boba\n")
	watchPass(10)
	if got := events.named("pet.changed"); len(got) != 0 {
		t.Fatalf("pet events = %v", got)
	}
}

func TestRenderablePetBroadcastsMetaPayload(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchWriteFile(t, filepath.Join(h, "config.yaml"), "display:\n  pet:\n    enabled: true\n    slug: boba\n")
	watchPass(0)
	sheet := filepath.Join(h, "sheet.png")
	watchWriteFile(t, sheet, "png")
	prev := petActiveSelection
	petActiveSelection = func(context.Context) (bool, *pet.InstalledPet, float64, error) {
		return true, &pet.InstalledPet{Slug: "boba", DisplayName: "Boba", Spritesheet: sheet}, 0.33, nil
	}
	t.Cleanup(func() { petActiveSelection = prev })
	watchPass(10)
	got := events.named("pet.changed")
	if len(got) == 0 {
		t.Fatalf("events = %v", events.all())
	}
	payload := got[0].payload
	if payload["enabled"] != true || payload["slug"] != "boba" || payload["spritesheetRevision"] == "" {
		t.Fatalf("payload = %v", payload)
	}
}

func TestEnqueuedEnvelopeBroadcastsOutboxPending(t *testing.T) {
	h, events := watcherHomeFixture(t)
	outbox := filepath.Join(h, "bot_relay", "outbox")
	mkdirs(t, outbox)
	watchPass(0)
	id := string(slices.Repeat([]byte("a"), 32))
	watchWriteFile(t, filepath.Join(outbox, id+".json"), `{"id": "`+id+`"}`)
	watchPass(10)
	if !events.has("bot_relay.outbox.pending") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestDrainedOutboxDoesNotRebroadcastPending(t *testing.T) {
	h, events := watcherHomeFixture(t)
	envelope := filepath.Join(h, "bot_relay", "outbox", string(slices.Repeat([]byte("b"), 32))+".json")
	watchWriteFile(t, envelope, "{}")
	watchPass(0)
	if err := os.Remove(envelope); err != nil {
		t.Fatal(err)
	}
	watchPass(10)
	watchPass(20)
	if got := events.named("bot_relay.outbox.pending"); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
}

func TestNewEnvelopeAfterDrainFiresPendingAgain(t *testing.T) {
	h, events := watcherHomeFixture(t)
	outbox := filepath.Join(h, "bot_relay", "outbox")
	first := filepath.Join(outbox, string(slices.Repeat([]byte("c"), 32))+".json")
	watchWriteFile(t, first, "{}")
	watchPass(0)
	watchWriteFile(t, first, "{}")
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	bump := info.ModTime().Add(time.Millisecond)
	if err := os.Chtimes(first, bump, bump); err != nil {
		t.Fatal(err)
	}
	watchPass(10)
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	watchPass(20)
	second := filepath.Join(outbox, string(slices.Repeat([]byte("d"), 32))+".json")
	watchWriteFile(t, second, "{}")
	newer := bump.Add(time.Millisecond)
	if err := os.Chtimes(second, newer, newer); err != nil {
		t.Fatal(err)
	}
	watchPass(30)
	got := events.named("bot_relay.outbox.pending")
	if len(got) != 2 || len(got[0].payload) != 0 || len(got[1].payload) != 0 {
		t.Fatalf("events = %v", got)
	}
}

func TestNoOutboxDirNeverFiresPending(t *testing.T) {
	_, events := watcherHomeFixture(t)
	watchPass(0)
	watchPass(10)
	if got := events.named("bot_relay.outbox.pending"); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
}

func TestBrokenProbeNeverKillsThePass(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchPass(0)
	prev := slices.Clone(changeWatches)
	for i := range changeWatches {
		if changeWatches[i].event == "cron.changed" {
			changeWatches[i] = changeWatch{"cron.changed", 1.0, func(context.Context) any { panic(errors.New("boom")) }, emptyChangePayload}
		}
	}
	t.Cleanup(func() { changeWatches = prev })
	writeSessionChange(t, filepath.Join(h, "state.db"), "created")
	watchPass(10)
	if !events.has("sessions.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestSkillInstalledFromAnotherTerminalBroadcastsSkillsChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	skillsDir := filepath.Join(h, "skills")
	mkdirs(t, skillsDir)
	watchPass(0)
	if got := events.named("skills.changed"); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
	watchWriteFile(t, filepath.Join(skillsDir, "new-skill", "SKILL.md"), "---\nname: new-skill\ndescription: d\n---\nbody\n")
	future := time.Now().Add(5 * time.Second)
	if err := os.Chtimes(skillsDir, future, future); err != nil {
		t.Fatal(err)
	}
	watchPass(10)
	if !events.has("skills.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

func TestASkillInstalledInAnotherSessionsRepoBroadcastsSkillsChanged(t *testing.T) {
	h, events := watcherHomeFixture(t)
	mkdirs(t, filepath.Join(h, "skills"))
	root := t.TempDir()
	repoA, repoB := filepath.Join(root, "repo-a"), filepath.Join(root, "repo-b")
	for _, repo := range []string{repoA, repoB} {
		mkdirs(t, filepath.Join(repo, ".git"))
		mkdirs(t, filepath.Join(repo, ".icebath", "skills"))
	}
	cfg, _ := json.Marshal(map[string]any{"skills": map[string]any{"trusted_project_dirs": []string{repoA, repoB}}})
	watchWriteFile(t, filepath.Join(h, "config.yaml"), string(cfg))
	t.Chdir(repoA)
	sessionScope := func() *skills.Scope {
		scope := skills.NewScope(h, nil)
		scope.Cwd = repoB
		return scope
	}
	if _, found := sessionScope().GetSkillCommands()["/workspace-only"]; found {
		t.Fatal("workspace-only listed before install")
	}
	watchPass(0)
	if got := events.named("skills.changed"); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
	watchWriteFile(t, filepath.Join(repoB, ".icebath", "skills", "workspace-only", "SKILL.md"), "---\nname: workspace-only\ndescription: d\n---\nbody\n")
	watchPass(10)
	if !events.has("skills.changed") {
		t.Fatalf("events = %v", events.all())
	}
	if _, found := sessionScope().GetSkillCommands()["/workspace-only"]; !found {
		t.Fatal("workspace-only missing after install")
	}
}

// Session store with the heartbeat table and one session row of the signature cases
func seedSignatureStore(t *testing.T, dbPath string) {
	t.Helper()
	execSQL(t, dbPath,
		"CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT, last_activity_at REAL, message_count INTEGER, archived INTEGER DEFAULT 0, pinned INTEGER DEFAULT 0)",
		"CREATE TABLE gateway_heartbeats (backend_id TEXT PRIMARY KEY, last_heartbeat REAL)",
		"INSERT INTO sessions(id, title, last_activity_at, message_count) VALUES ('session-1', 'Original', 100, 1)",
		"INSERT INTO gateway_heartbeats(backend_id, last_heartbeat) VALUES ('backend-1', 100)")
}

// Watcher over a temp store with no served profiles and a fresh digest cache
func signatureFixture(t *testing.T) (string, context.Context) {
	t.Helper()
	tmp := t.TempDir()
	db := filepath.Join(tmp, "state.db")
	seedSignatureStore(t, db)
	_, _ = watcherHomeFixture(t)
	hermesHome = tmp
	return db, context.Background()
}

func TestSessionsSignatureIgnoresGatewayHeartbeatOnlyWrites(t *testing.T) {
	db, ctx := signatureFixture(t)
	before := sessionsSig(ctx)
	execSQL(t, db, "UPDATE gateway_heartbeats SET last_heartbeat = 200")
	if after := sessionsSig(ctx); !reflect.DeepEqual(after, before) {
		t.Fatalf("signature moved: %v -> %v", before, after)
	}
}

func TestSessionsSignatureChangesForSessionMetadataUpdates(t *testing.T) {
	db, ctx := signatureFixture(t)
	before := sessionsSig(ctx)
	execSQL(t, db, "UPDATE sessions SET title = 'Renamed' WHERE id = 'session-1'")
	if after := sessionsSig(ctx); reflect.DeepEqual(after, before) {
		t.Fatalf("signature did not move: %v", after)
	}
}

func TestSessionsSignatureSeesACommitInsideTheSameMtimeTick(t *testing.T) {
	db, ctx := signatureFixture(t)
	frozen := time.Now().UnixNano()
	prev := newestMtimeNS
	newestMtimeNS = func(...string) (int64, bool) { return frozen, true }
	t.Cleanup(func() { newestMtimeNS = prev })
	before := sessionsSig(ctx)
	execSQL(t, db, "UPDATE sessions SET title = 'Renamed' WHERE id = 'session-1'")
	if after := sessionsSig(ctx); reflect.DeepEqual(after, before) {
		t.Fatalf("signature did not move: %v", after)
	}
}
