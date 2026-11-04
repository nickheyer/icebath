package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Process facts from a table of pid to birth, with denied and zombie pids
type fakeProber struct {
	mu          sync.Mutex
	procs       map[int]float64
	denied      map[int]bool
	zombies     map[int]bool
	ppid        map[int]int
	environ     map[int]map[string]string
	zombieCalls []int
	kills       []int
}

func newFakeProber(procs map[int]float64) *fakeProber {
	return &fakeProber{procs: procs, denied: map[int]bool{}, zombies: map[int]bool{}, ppid: map[int]int{}, environ: map[int]map[string]string{}}
}

func (f *fakeProber) Birth(pid int) (float64, error) {
	if f.denied[pid] {
		return 0, errors.New("permission denied")
	}
	t, ok := f.procs[pid]
	if !ok {
		return 0, errNoSuchProcess
	}
	return t, nil
}

func (f *fakeProber) WorkerBirth(pid int) (float64, error) { return f.Birth(pid) }
func (f *fakeProber) BootStart(pid int) (float64, bool)    { return 0, false }
func (f *fakeProber) Running(pid int) bool {
	_, ok := f.procs[pid]
	return ok || f.denied[pid]
}
func (f *fakeProber) Zombie(pid int) bool {
	f.mu.Lock()
	f.zombieCalls = append(f.zombieCalls, pid)
	f.mu.Unlock()
	return f.zombies[pid]
}
func (f *fakeProber) Environ(pid int) (map[string]string, error) {
	if env, ok := f.environ[pid]; ok {
		return env, nil
	}
	return nil, errNoSuchProcess
}
func (f *fakeProber) Cmdline(pid int) ([]string, error) { return []string{"fake", "argv"}, nil }
func (f *fakeProber) PPID(pid int) (int, bool) {
	p, ok := f.ppid[pid]
	return p, ok
}
func (f *fakeProber) Children(pid int, recursive bool) []int { return nil }
func (f *fakeProber) Terminate(pid int) error {
	f.mu.Lock()
	f.kills = append(f.kills, pid)
	f.mu.Unlock()
	return nil
}
func (f *fakeProber) Kill(pid int) error { return f.Terminate(pid) }

// Installs a fake prober, a fixed pid and a temp ledger for the duration of the test
func identityFixture(t *testing.T, procs map[int]float64, pid int) (*fakeProber, string) {
	t.Helper()
	fake := newFakeProber(procs)
	prevProber, prevPid, prevLedger := identityProber, currentPid, ledgerPathFn
	identityProber = fake
	if pid != 0 {
		currentPid = func() int { return pid }
	}
	ledger := filepath.Join(t.TempDir(), "spawn-ledger.json")
	ledgerPathFn = func() string { return ledger }
	t.Cleanup(func() { identityProber, currentPid, ledgerPathFn = prevProber, prevPid, prevLedger })
	return fake, ledger
}

func readLedgerFile(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("ledger json: %v", err)
	}
	return out
}

func ledgerPids(entries []map[string]any) map[int]bool {
	out := map[int]bool{}
	for _, e := range entries {
		out[int(e["pid"].(float64))] = true
	}
	return out
}

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestSpawnTagRoundtrip(t *testing.T) {
	identityFixture(t, map[int]float64{999: 1234.5}, 999)
	raw := BuildSpawnTag("serve", "/x/install")
	tag := ParseSpawnTag(raw)
	if tag == nil {
		t.Fatal("tag did not parse")
	}
	if tag.Purpose != "serve" || tag.SpawnerPid != 999 {
		t.Fatalf("unexpected tag %+v", tag)
	}
	if tag.SpawnerCreate == nil || !approx(*tag.SpawnerCreate, 1234.5, 0.01) {
		t.Fatalf("spawner create %v", tag.SpawnerCreate)
	}
	if tag.Install != InstallID("/x/install") {
		t.Fatalf("install %q", tag.Install)
	}
}

func TestParseSpawnTagRejectsMalformed(t *testing.T) {
	for _, raw := range []any{nil, "", "v0:abc:serve:1:2.0", "v1:abc:serve:notanint:2.0", "v1:abc:serve:-4:2.0",
		"v1::serve:10:2.0", "v1:abc::10:2.0", "v1:abc:serve:10", "v1:abc:serve:10:zzz"} {
		if ParseSpawnTag(raw) != nil {
			t.Errorf("%v parsed", raw)
		}
	}
}

func TestParseSpawnTagDashCreateIsNil(t *testing.T) {
	tag := ParseSpawnTag("v1:abcdef:serve:42:-")
	if tag == nil || tag.SpawnerCreate != nil {
		t.Fatalf("tag %+v", tag)
	}
}

func TestDesktopStyleTagParses(t *testing.T) {
	tag := ParseSpawnTag("v1:-:serve:5100:1755689000.123")
	if tag == nil || tag.SpawnerPid != 5100 {
		t.Fatalf("tag %+v", tag)
	}
}

func TestInstallIDStableAndPathScoped(t *testing.T) {
	a := InstallID("/opt/icebath")
	if a != InstallID("/opt/icebath") || a == InstallID("/opt/other") || len(a) != 12 {
		t.Fatalf("install id %q", a)
	}
}

func ledgerEntry(pid int, create float64, purpose string, install string, spawnerPid *int, spawnerCreate *float64) map[string]any {
	if install == "" {
		install = InstallID("/x/install")
	}
	e := map[string]any{"pid": pid, "create_time": create, "purpose": purpose, "install": install,
		"spawner_pid": nil, "spawner_create": nil, "registered_at": 0.0, "argv": ""}
	if spawnerPid != nil {
		e["spawner_pid"] = *spawnerPid
	}
	if spawnerCreate != nil {
		e["spawner_create"] = *spawnerCreate
	}
	return e
}

func writeLedgerFile(t *testing.T, path string, entries []map[string]any) {
	t.Helper()
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func intp(v int) *int           { return &v }
func floatp(v float64) *float64 { return &v }

func TestRegisterSelfWritesAndPrunesDead(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{400: 2.0, 999: 50.0}, 999)
	writeLedgerFile(t, ledger, []map[string]any{ledgerEntry(300, 1.0, "serve", "", nil, nil), ledgerEntry(400, 2.0, "serve", "", nil, nil)})
	if !RegisterSelf("serve", "/x/install", nil) {
		t.Fatal("register_self failed")
	}
	entries := readLedgerFile(t, ledger)
	pids := ledgerPids(entries)
	if len(pids) != 2 || !pids[400] || !pids[999] {
		t.Fatalf("pids %v", pids)
	}
	for _, e := range entries {
		if e["pid"].(float64) == 999 {
			if e["purpose"] != "serve" || !approx(e["create_time"].(float64), 50.0, 0.01) {
				t.Fatalf("entry %v", e)
			}
		}
	}
}

func TestRegisterSelfSurvivesNonUTF8Argv(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{999: 50.0}, 999)
	prev := os.Args
	os.Args = []string{"icebath", "serve", "/tmp/project-\xff"}
	t.Cleanup(func() { os.Args = prev })
	if !RegisterSelf("serve", "/x/install", nil) {
		t.Fatal("register_self failed")
	}
	raw, _ := os.ReadFile(ledger)
	if !strings.Contains(string(raw), `icebath serve /tmp/project-\udcff`) {
		t.Fatalf("argv not surrogate escaped: %s", raw)
	}
	if len(readLedgerFile(t, ledger)) != 1 {
		t.Fatal("ledger unreadable after the non UTF-8 argv")
	}
}

func TestRegisterSelfInheritsSpawnTagLineage(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{999: 50.0}, 999)
	t.Setenv(SpawnEnvVar, "v1:-:serve:777:123.000")
	RegisterSelf("serve", "/x/install", nil)
	me := readLedgerFile(t, ledger)[0]
	if me["spawner_pid"].(float64) != 777 || !approx(me["spawner_create"].(float64), 123.0, 0.01) {
		t.Fatalf("entry %v", me)
	}
}

func TestRegisterSelfFallsBackToDesktopParentEnv(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{999: 50.0}, 999)
	t.Setenv(SpawnEnvVar, "")
	os.Unsetenv(SpawnEnvVar)
	t.Setenv("ICEBATH_PARENT_PID", "888")
	t.Setenv("ICEBATH_PARENT_START_MARKER", "winms:1755689000123")
	RegisterSelf("serve", "/x/install", nil)
	me := readLedgerFile(t, ledger)[0]
	if me["spawner_pid"].(float64) != 888 || !approx(me["spawner_create"].(float64), 1755689000.123, 0.01) {
		t.Fatalf("entry %v", me)
	}
}

func TestRegisterSelfRecordsIsolatedServeMarker(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{999: 50.0}, 999)
	RegisterSelf("serve", "/x/install", map[string]any{"host": "127.0.0.1", "port": 4321, "isolated": true})
	rows := readLedgerFile(t, ledger)
	if rows[0]["isolated"] != true || rows[0]["port"].(float64) != 4321 || rows[0]["host"] != "127.0.0.1" {
		t.Fatalf("isolated row %v", rows[0])
	}
	RegisterSelf("serve", "/x/install", map[string]any{"host": "127.0.0.1", "port": 4322})
	rows = readLedgerFile(t, ledger)
	if rows[len(rows)-1]["isolated"] != false {
		t.Fatalf("ordinary row %v", rows[len(rows)-1])
	}
}

func TestCorruptLedgerQuarantinedNotRewrittenBlind(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{999: 50.0}, 999)
	os.WriteFile(ledger, []byte("{ not json"), 0o600)
	if !RegisterSelf("serve", "/x/install", nil) {
		t.Fatal("register_self failed")
	}
	parked, err := os.ReadFile(ledger + ".corrupt")
	if err != nil || string(parked) != "{ not json" {
		t.Fatalf("parked %q %v", parked, err)
	}
	entries := readLedgerFile(t, ledger)
	if len(entries) != 1 || entries[0]["pid"].(float64) != 999 {
		t.Fatalf("entries %v", entries)
	}
}

func TestLedgerEntriesFiltersDeadReusedAndForeign(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{100: 10.0, 200: 9999.0, 400: 40.0}, 0)
	writeLedgerFile(t, ledger, []map[string]any{
		ledgerEntry(100, 10.0, "serve", "", nil, nil),
		ledgerEntry(200, 20.0, "serve", "", nil, nil),
		ledgerEntry(300, 30.0, "serve", "", nil, nil),
		ledgerEntry(400, 40.0, "serve", "ffffffffffff", nil, nil),
	})
	live := LedgerEntries(LedgerOptions{ProjectRoot: "/x/install"})
	if len(live) != 1 {
		t.Fatalf("live %v", live)
	}
	if pid, _ := entryInt(live[0], "pid"); pid != 100 {
		t.Fatalf("pid %d", pid)
	}
}

func TestSpawnerIsDeadTristate(t *testing.T) {
	identityFixture(t, map[int]float64{500: 5.0}, 0)
	check := func(entry map[string]any, wantDead, wantKnown bool) {
		t.Helper()
		dead, known := SpawnerIsDead(entry)
		if dead != wantDead || known != wantKnown {
			t.Fatalf("entry %v: dead=%v known=%v", entry, dead, known)
		}
	}
	check(ledgerEntry(1, 1.0, "serve", "", intp(500), floatp(5.0)), false, true)
	check(ledgerEntry(1, 1.0, "serve", "", intp(600), floatp(6.0)), true, true)
	check(ledgerEntry(1, 1.0, "serve", "", intp(500), floatp(999.0)), true, true)
	check(ledgerEntry(1, 1.0, "serve", "", nil, nil), false, false)
}

func TestDesktopSSHBackendSpawnShapeIsDesktopOwned(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "")
	os.Unsetenv("ICEBATH_DASHBOARD_SESSION_TOKEN")
	sshArgv := []string{"serve", "--isolated", "--host", "127.0.0.1", "--port", "0", "--ssh-session-token-file", "/home/u/.icebath/desktop-ssh/abc.token"}
	if !IsDesktopOwnedBackend(sshArgv) {
		t.Fatal("ssh spawn shape not desktop owned")
	}
	prev := os.Args
	os.Args = append([]string{"icebath"}, sshArgv...)
	t.Cleanup(func() { os.Args = prev })
	if !IsDesktopOwnedBackend(nil) {
		t.Fatal("own argv not desktop owned")
	}
	if IsDesktopOwnedBackend([]string{"serve", "--host", "127.0.0.1", "--port", "0"}) {
		t.Fatal("bare inherited flag counted as ownership")
	}
}

// Runs the reclaim predicate against a faked process world
func reapFixture(t *testing.T, fake *fakeProber, entry map[string]any, pid int, create float64) (int, bool, []int) {
	t.Helper()
	prevLedger := ledgerEntriesFn
	ledgerEntriesFn = func(LedgerOptions) []map[string]any { return []map[string]any{entry} }
	t.Cleanup(func() { ledgerEntriesFn = prevLedger })
	kills := []int{}
	reaped, ok := ReapOrphanedBackendOwner(pid, floatp(create), func(p int) error { kills = append(kills, p); return nil })
	return reaped, ok, kills
}

func TestReclaimReapsDeadSpawnerOwner(t *testing.T) {
	fake, _ := identityFixture(t, map[int]float64{555: 55.0}, 0)
	reaped, ok, kills := reapFixture(t, fake, ledgerEntry(555, 55.0, "serve", "", intp(700), floatp(7.0)), 555, 55.0)
	if !ok || reaped != 555 || len(kills) != 1 || kills[0] != 555 {
		t.Fatalf("reaped=%d ok=%v kills=%v", reaped, ok, kills)
	}
}

func TestReclaimSparesLiveSpawnerOwner(t *testing.T) {
	fake, _ := identityFixture(t, map[int]float64{555: 55.0, 500: 5.0}, 0)
	_, ok, kills := reapFixture(t, fake, ledgerEntry(555, 55.0, "serve", "", intp(500), floatp(5.0)), 555, 55.0)
	if ok || len(kills) != 0 {
		t.Fatalf("ok=%v kills=%v", ok, kills)
	}
}

func TestReclaimSparesUnprovableSpawner(t *testing.T) {
	fake, _ := identityFixture(t, map[int]float64{555: 55.0}, 0)
	fake.denied[500] = true
	_, ok, kills := reapFixture(t, fake, ledgerEntry(555, 55.0, "serve", "", intp(500), floatp(5.0)), 555, 55.0)
	if ok || len(kills) != 0 {
		t.Fatalf("ok=%v kills=%v", ok, kills)
	}
}

func TestReclaimReapsNullSpawnerOrphanToInit(t *testing.T) {
	fake, _ := identityFixture(t, map[int]float64{555: 1000.0}, 0)
	fake.ppid[555] = 1
	prev := lockOwnedServePidsFn
	lockOwnedServePidsFn = func() map[int]bool { return map[int]bool{} }
	t.Cleanup(func() { lockOwnedServePidsFn = prev })
	reaped, ok, kills := reapFixture(t, fake, ledgerEntry(555, 1000.0, "serve", "", nil, nil), 555, 1000.0)
	if !ok || reaped != 555 || len(kills) != 1 {
		t.Fatalf("reaped=%d ok=%v kills=%v", reaped, ok, kills)
	}
}

func TestReclaimSparesNullSpawnerWithLiveParent(t *testing.T) {
	fake, _ := identityFixture(t, map[int]float64{555: 55.0}, 0)
	fake.ppid[555] = 1234
	_, ok, kills := reapFixture(t, fake, ledgerEntry(555, 55.0, "serve", "", nil, nil), 555, 55.0)
	if ok || len(kills) != 0 {
		t.Fatalf("ok=%v kills=%v", ok, kills)
	}
}

func TestReclaimSparesLockClaimedOrphan(t *testing.T) {
	fake, _ := identityFixture(t, map[int]float64{555: 1000.0}, 0)
	fake.ppid[555] = 1
	prev := lockOwnedServePidsFn
	lockOwnedServePidsFn = func() map[int]bool { return map[int]bool{555: true} }
	t.Cleanup(func() { lockOwnedServePidsFn = prev })
	_, ok, kills := reapFixture(t, fake, ledgerEntry(555, 1000.0, "serve", "", nil, nil), 555, 1000.0)
	if ok || len(kills) != 0 {
		t.Fatalf("ok=%v kills=%v", ok, kills)
	}
}

func TestReclaimSparesInteractivePurposesAndSelf(t *testing.T) {
	fake, _ := identityFixture(t, map[int]float64{555: 55.0}, 0)
	_, ok, kills := reapFixture(t, fake, ledgerEntry(555, 55.0, "chat", "", intp(700), floatp(7.0)), 555, 55.0)
	if ok || len(kills) != 0 {
		t.Fatalf("chat purpose: ok=%v kills=%v", ok, kills)
	}
	prev := currentPid
	currentPid = func() int { return 555 }
	t.Cleanup(func() { currentPid = prev })
	_, ok, kills = reapFixture(t, fake, ledgerEntry(555, 55.0, "serve", "", intp(700), floatp(7.0)), 555, 55.0)
	if ok || len(kills) != 0 {
		t.Fatalf("self: ok=%v kills=%v", ok, kills)
	}
}

func TestReclaimIgnoresUnknownPidAndNeverRaises(t *testing.T) {
	fake, _ := identityFixture(t, map[int]float64{555: 55.0}, 0)
	if _, ok, _ := reapFixture(t, fake, ledgerEntry(555, 55.0, "serve", "", nil, nil), 999, 9.0); ok {
		t.Fatal("unknown pid reaped")
	}
	prev := ledgerEntriesFn
	ledgerEntriesFn = func(LedgerOptions) []map[string]any { panic("boom") }
	t.Cleanup(func() { ledgerEntriesFn = prev })
	if _, ok := ReapOrphanedBackendOwner(555, floatp(55.0), func(int) error { return nil }); ok {
		t.Fatal("ledger failure reaped")
	}
}

func TestLocalDesktopClientNeedsDeclarationAndNativeTicket(t *testing.T) {
	native := map[string]any{"provider": "local", "native_bootstrap": true}
	if !LocalDesktopClient(native, true) {
		t.Fatal("declared native client refused")
	}
	if LocalDesktopClient(native, false) || LocalDesktopClient(map[string]any{"provider": "local"}, true) || LocalDesktopClient(nil, true) {
		t.Fatal("undeclared or non native client accepted")
	}
	ctx := WithServingLocalDesktop(context.Background(), true)
	if !ServesLocalDesktopNow(ctx) || ServesLocalDesktopNow(context.Background()) {
		t.Fatal("serving marker not carried by the context")
	}
	if !ServesLocalDesktopNow(WithServingLocalDesktop(ctx, false)) {
		t.Fatal("an inner false cleared an outer true")
	}
}

func TestSpawnedByThisProcessChecksTagAndBirth(t *testing.T) {
	fake, _ := identityFixture(t, map[int]float64{999: 50.0, 1000: 60.0}, 999)
	fake.environ[1000] = map[string]string{SpawnEnvVar: "v1:abc:serve:999:50.000"}
	if !SpawnedByThisProcess(1000) {
		t.Fatal("tagged child not recognised")
	}
	fake.environ[1000][SpawnEnvVar] = "v1:abc:serve:999:9999.000"
	if SpawnedByThisProcess(1000) {
		t.Fatal("stale spawner birth accepted")
	}
	fake.environ[1000][SpawnEnvVar] = "v1:abc:serve:998:50.000"
	if SpawnedByThisProcess(1000) || SpawnedByThisProcess(2000) {
		t.Fatal("foreign or unreadable child accepted")
	}
}

// Collects warnings the identity layer logs
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r)
	h.mu.Unlock()
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) warnings(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, substr) {
			n++
		}
	}
	return n
}
