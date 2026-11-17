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
	raw := BuildSpawnTag("gateway", "/x/install")
	tag := ParseSpawnTag(raw)
	if tag == nil {
		t.Fatal("tag did not parse")
	}
	if tag.Purpose != "gateway" || tag.SpawnerPid != 999 {
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
	for _, raw := range []any{nil, "", "v0:abc:gateway:1:2.0", "v1:abc:gateway:notanint:2.0", "v1:abc:gateway:-4:2.0",
		"v1::gateway:10:2.0", "v1:abc::10:2.0", "v1:abc:gateway:10", "v1:abc:gateway:10:zzz"} {
		if ParseSpawnTag(raw) != nil {
			t.Errorf("%v parsed", raw)
		}
	}
}

func TestParseSpawnTagDashCreateIsNil(t *testing.T) {
	tag := ParseSpawnTag("v1:abcdef:gateway:42:-")
	if tag == nil || tag.SpawnerCreate != nil {
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
	writeLedgerFile(t, ledger, []map[string]any{ledgerEntry(300, 1.0, "gateway", "", nil, nil), ledgerEntry(400, 2.0, "gateway", "", nil, nil)})
	if !RegisterSelf(context.Background(), "gateway", "/x/install") {
		t.Fatal("register_self failed")
	}
	entries := readLedgerFile(t, ledger)
	pids := ledgerPids(entries)
	if len(pids) != 2 || !pids[400] || !pids[999] {
		t.Fatalf("pids %v", pids)
	}
	for _, e := range entries {
		if e["pid"].(float64) == 999 {
			if e["purpose"] != "gateway" || !approx(e["create_time"].(float64), 50.0, 0.01) {
				t.Fatalf("entry %v", e)
			}
		}
	}
}

func TestRegisterSelfSurvivesNonUTF8Argv(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{999: 50.0}, 999)
	prev := os.Args
	os.Args = []string{"icebath", "gateway", "/tmp/project-\xff"}
	t.Cleanup(func() { os.Args = prev })
	if !RegisterSelf(context.Background(), "gateway", "/x/install") {
		t.Fatal("register_self failed")
	}
	raw, _ := os.ReadFile(ledger)
	if !strings.Contains(string(raw), `icebath gateway /tmp/project-\udcff`) {
		t.Fatalf("argv not surrogate escaped: %s", raw)
	}
	if len(readLedgerFile(t, ledger)) != 1 {
		t.Fatal("ledger unreadable after the non UTF-8 argv")
	}
}

func TestRegisterSelfInheritsSpawnTagLineage(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{999: 50.0}, 999)
	t.Setenv(SpawnEnvVar, "v1:-:gateway:777:123.000")
	RegisterSelf(context.Background(), "gateway", "/x/install")
	me := readLedgerFile(t, ledger)[0]
	if me["spawner_pid"].(float64) != 777 || !approx(me["spawner_create"].(float64), 123.0, 0.01) {
		t.Fatalf("entry %v", me)
	}
}

func TestCorruptLedgerQuarantinedNotRewrittenBlind(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{999: 50.0}, 999)
	os.WriteFile(ledger, []byte("{ not json"), 0o600)
	if !RegisterSelf(context.Background(), "gateway", "/x/install") {
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
		ledgerEntry(100, 10.0, "gateway", "", nil, nil),
		ledgerEntry(200, 20.0, "gateway", "", nil, nil),
		ledgerEntry(300, 30.0, "gateway", "", nil, nil),
		ledgerEntry(400, 40.0, "gateway", "ffffffffffff", nil, nil),
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
	check(ledgerEntry(1, 1.0, "gateway", "", intp(500), floatp(5.0)), false, true)
	check(ledgerEntry(1, 1.0, "gateway", "", intp(600), floatp(6.0)), true, true)
	check(ledgerEntry(1, 1.0, "gateway", "", intp(500), floatp(999.0)), true, true)
	check(ledgerEntry(1, 1.0, "gateway", "", nil, nil), false, false)
}

func TestSpawnedByThisProcessChecksTagAndBirth(t *testing.T) {
	fake, _ := identityFixture(t, map[int]float64{999: 50.0, 1000: 60.0}, 999)
	fake.environ[1000] = map[string]string{SpawnEnvVar: "v1:abc:gateway:999:50.000"}
	if !SpawnedByThisProcess(1000) {
		t.Fatal("tagged child not recognised")
	}
	fake.environ[1000][SpawnEnvVar] = "v1:abc:gateway:999:9999.000"
	if SpawnedByThisProcess(1000) {
		t.Fatal("stale spawner birth accepted")
	}
	fake.environ[1000][SpawnEnvVar] = "v1:abc:gateway:998:50.000"
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

func TestProcessCreateTimeAndPidAliveMatchesWrappers(t *testing.T) {
	identityFixture(t, map[int]float64{4242: 1000.5}, 4242)
	if ct := ProcessCreateTime(0); ct == nil || *ct != 1000.5 {
		t.Fatalf("self create time %v", ct)
	}
	if ProcessCreateTime(5151) != nil {
		t.Fatal("absent pid has a create time")
	}
	birth := 1000.5
	if alive, known := PidAliveMatches(4242, &birth, false, nil); !alive || !known {
		t.Fatalf("live pid %v %v", alive, known)
	}
	other := 990.0
	if alive, known := PidAliveMatches(4242, &other, false, nil); alive || !known {
		t.Fatalf("recycled pid %v %v", alive, known)
	}
}

func TestLedgerRewriteKeepsRowOrderAndNumberTypes(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{400: 2.0, 999: 50.0}, 999)
	onDisk := `[{"zeta": 1, "pid": 400, "create_time": 2.0, "purpose": "gateway", "extra": 1e2, "argv": "a<b é"}, 7]`
	if err := os.WriteFile(ledger, []byte(onDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	if !RegisterSelf(context.Background(), "gateway", "/x/install") {
		t.Fatal("register_self failed")
	}
	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	kept := "[\n  {\n    \"zeta\": 1,\n    \"pid\": 400,\n    \"create_time\": 2.0,\n    \"purpose\": \"gateway\",\n    \"extra\": 100.0,\n    \"argv\": \"a<b \\u00e9\"\n  },\n  {\n    \"pid\": 999,\n"
	if !strings.HasPrefix(string(raw), kept) {
		t.Fatalf("kept row rewritten as\n%s", raw)
	}
	var order []string
	for _, line := range strings.Split(string(raw)[len(kept)-len("    \"pid\": 999,\n"):], "\n") {
		if name, _, ok := strings.Cut(strings.TrimPrefix(line, "    \""), "\": "); ok && strings.HasPrefix(line, "    \"") {
			order = append(order, name)
		}
	}
	want := "pid create_time purpose install spawner_pid spawner_create registered_at argv hermes_home boot_start spawner_boot boot_id"
	if got := strings.Join(order, " "); got != want {
		t.Fatalf("new row key order %q, want %q", got, want)
	}
}
