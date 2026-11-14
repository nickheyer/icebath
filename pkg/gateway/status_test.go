package gateway

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestWritePidFileRecordsGatewayMetadataAndIsExclusive(t *testing.T) {
	dir := statusHome(t)
	if err := WritePidFile(context.Background()); err != nil {
		t.Fatal(err)
	}
	payload := readRecord(t, filepath.Join(dir, "gateway.pid"))
	if !pyEq(recGet(payload, "pid"), int64(os.Getpid())) || recGet(payload, "kind") != "icebath-gateway" {
		t.Fatalf("payload %v", payload.Keys)
	}
	if argv, ok := recGet(payload, "argv").([]any); !ok || len(argv) == 0 {
		t.Fatalf("argv %v", recGet(payload, "argv"))
	}
	if err := WritePidFile(context.Background()); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second write error = %v", err)
	}
}

func TestGatewayIdentityFilesUseProcessHome(t *testing.T) {
	processHome := filepath.Join(t.TempDir(), "default")
	profileHome := filepath.Join(filepath.Dir(processHome), "profiles", "cfo")
	os.MkdirAll(processHome, 0o755)
	os.MkdirAll(profileHome, 0o755)
	t.Setenv("ICEBATH_HOME", processHome)
	if err := WritePidFile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(processHome, "gateway.pid")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(profileHome, "gateway.pid")); err == nil {
		t.Fatal("pid file written into the profile home")
	}
}

func TestRuntimeLockClaimsAndReleasesLiveness(t *testing.T) {
	statusHome(t)
	if active, _ := IsGatewayRuntimeLockActive(""); active {
		t.Fatal("active before acquire")
	}
	if !AcquireGatewayRuntimeLock(context.Background()) {
		t.Fatal("acquire failed")
	}
	if active, _ := IsGatewayRuntimeLockActive(""); !active {
		t.Fatal("inactive after acquire")
	}
	ReleaseGatewayRuntimeLock()
	if active, _ := IsGatewayRuntimeLockActive(""); active {
		t.Fatal("active after release")
	}
}

func TestGetRunningPidCachedInvalidatesWhenPidFileChanges(t *testing.T) {
	dir := statusHome(t)
	write := func(pid, start int64) {
		r := rec("pid", pid, "kind", "icebath-gateway", "argv", strs("python", "-m", "icebath_cli.main", "gateway"), "start_time", start)
		writeRecord(t, filepath.Join(dir, "gateway.pid"), r)
		writeRecord(t, filepath.Join(dir, "gateway.lock"), r)
	}
	write(111, 123)
	var calls atomic.Int32
	patch(t, &statusIsGatewayRuntimeLockActive, func(string) (bool, error) { calls.Add(1); return true, nil })
	patch(t, &statusPidExists, func(int) bool { return true })
	patch(t, &statusProcessStartTime, func(pid int) any {
		if pid == 111 {
			return int64(123)
		}
		return int64(456)
	})
	patch(t, &statusReadProcessCmdline, noCmdline)
	if pid, ok, _ := GetRunningPidCached("", true, time.Minute); !ok || pid != 111 {
		t.Fatalf("pid %d %v", pid, ok)
	}
	write(2222, 456)
	if pid, ok, _ := GetRunningPidCached("", true, time.Minute); !ok || pid != 2222 {
		t.Fatalf("pid %d %v", pid, ok)
	}
	if calls.Load() != 2 {
		t.Fatalf("lock probes %d", calls.Load())
	}
}

func TestGetRunningPidFallsBackToLiveLockRecord(t *testing.T) {
	dir := statusHome(t)
	writeRecord(t, filepath.Join(dir, "gateway.pid"), rec("pid", int64(99999), "kind", "icebath-gateway",
		"argv", strs("python", "-m", "icebath_cli.main", "gateway"), "start_time", int64(123)))
	patch(t, &statusProcessStartTime, fixedInt(int64(123)))
	patch(t, &statusReadProcessCmdline, noCmdline)
	patch(t, &statusBuildPidRecord, func() *pyjson.Object {
		return rec("pid", int64(os.Getpid()), "kind", "icebath-gateway", "argv", strs("python", "-m", "icebath_cli.main", "gateway"), "start_time", int64(123))
	})
	patch(t, &statusPidExists, func(pid int) bool { return pid != 99999 })
	if !AcquireGatewayRuntimeLock(context.Background()) {
		t.Fatal("acquire failed")
	}
	defer ReleaseGatewayRuntimeLock()
	if pid, ok, err := GetRunningPid("", true); err != nil || !ok || pid != os.Getpid() {
		t.Fatalf("pid %d %v %v", pid, ok, err)
	}
}

func liveLockSeams(t *testing.T, cmd string) {
	patch(t, &statusIsGatewayRuntimeLockActive, func(string) (bool, error) { return true, nil })
	patch(t, &statusPidExists, func(int) bool { return true })
	patch(t, &statusProcessStartTime, fixedInt(int64(123)))
	patch(t, &statusReadProcessCmdline, cmdline(cmd))
	patch(t, &statusUnscopedRuntimeStatusRunningPid, func() (int, bool, error) { return 0, false, nil })
}

func TestUnscopedLiveIdentityMismatchNeverUnlinksActiveFiles(t *testing.T) {
	dir := statusHome(t)
	resolved, _ := canonicalHermesHome(dir)
	r := rec("pid", int64(4242), "kind", "icebath-gateway", "start_time", int64(123),
		"argv", strs("python", "-m", "icebath_cli.main", "gateway", "run"), "hermes_home", resolved)
	writeRecord(t, filepath.Join(dir, "gateway.pid"), r)
	writeRecord(t, filepath.Join(dir, "gateway.lock"), r)
	liveLockSeams(t, "python -m icebath_cli.main chat")
	if _, ok, _ := GetRunningPid("", true); ok {
		t.Fatal("chat process reported as gateway")
	}
	for _, name := range []string{"gateway.pid", "gateway.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s unlinked", name)
		}
	}
}

func TestUnscopedLiveCrossHomePidFileIsUnlinkedButHeldLockStays(t *testing.T) {
	dir := statusHome(t)
	other, _ := canonicalHermesHome(filepath.Join(dir, "other-home"))
	writeRecord(t, filepath.Join(dir, "gateway.pid"), rec("pid", int64(4242), "kind", "icebath-gateway", "start_time", int64(123),
		"argv", strs("python", "-m", "icebath_cli.main", "gateway", "run"), "hermes_home", other))
	os.WriteFile(filepath.Join(dir, "gateway.lock"), nil, 0o644)
	liveLockSeams(t, "python -m icebath_cli.main gateway run")
	if _, ok, _ := GetRunningPid("", true); ok {
		t.Fatal("foreign record reported")
	}
	if _, err := os.Stat(filepath.Join(dir, "gateway.pid")); err == nil {
		t.Fatal("cross home pid file kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "gateway.lock")); err != nil {
		t.Fatal("held lock unlinked")
	}
}

func writeScopedProfile(t *testing.T, root string) (string, string) {
	profileDir := filepath.Join(root, "profiles", "wiki")
	resolved, _ := canonicalHermesHome(profileDir)
	r := rec("pid", int64(4242), "kind", "icebath-gateway", "argv", strs("python", "-m", "icebath_cli.main", "gateway", "--profile", "wiki"),
		"start_time", int64(123), "hermes_home", resolved)
	writeRecord(t, filepath.Join(profileDir, "gateway.pid"), r)
	writeRecord(t, filepath.Join(profileDir, "gateway.lock"), r)
	return profileDir, filepath.Join(profileDir, "gateway.pid")
}

func TestScopedQueryReportsLiveForeignProfilePid(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "default-home"))
	profileDir, pidFile := writeScopedProfile(t, root)
	liveLockSeams(t, "python -m icebath_cli.main gateway --profile wiki")
	if pid, ok, _ := GetRunningPid(pidFile, true); !ok || pid != 4242 {
		t.Fatalf("pid %d %v", pid, ok)
	}
	for _, p := range []string{pidFile, filepath.Join(profileDir, "gateway.lock")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScopedQueryStillCleansDeadPidRecord(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "default-home"))
	profileDir, pidFile := writeScopedProfile(t, root)
	patch(t, &statusIsGatewayRuntimeLockActive, func(string) (bool, error) { return true, nil })
	patch(t, &statusPidExists, func(int) bool { return false })
	if _, ok, _ := GetRunningPid(pidFile, true); ok {
		t.Fatal("dead pid reported")
	}
	if _, err := os.Stat(pidFile); err == nil {
		t.Fatal("dead pid file kept")
	}
	if _, err := os.Stat(filepath.Join(profileDir, "gateway.lock")); err != nil {
		t.Fatal("lock unlinked")
	}
}

func mustWrite(t *testing.T, u RuntimeStatusUpdate) {
	t.Helper()
	if ok, err := WriteRuntimeStatus(context.Background(), u, false, nil); err != nil || !ok {
		t.Fatalf("write %v %v", ok, err)
	}
}

func platformsOf(t *testing.T) *pyjson.Object {
	t.Helper()
	return recGet(ReadRuntimeStatus(""), "platforms").(*pyjson.Object)
}

func TestClearProfilePlatformsPreservesPrimaryEntries(t *testing.T) {
	dir := statusHome(t)
	os.WriteFile(filepath.Join(dir, "gateway_state.json"), []byte(`{"platforms": {"telegram": {"state": "connected"}, "reviewer:discord": {"state": "fatal", "error_code": "duplicate_credential"}}}`), 0o644)
	mustWrite(t, RuntimeStatusUpdate{ClearProfilePlatforms: true})
	if got, _ := pyjson.Dumps(platformsOf(t)); got != `{"telegram": {"state": "connected"}}` {
		t.Fatalf("platforms %s", got)
	}
}

func TestClearProfilePlatformsAndWriteAreOneAtomicUpdate(t *testing.T) {
	dir := statusHome(t)
	os.WriteFile(filepath.Join(dir, "gateway_state.json"), []byte(`{"platforms": {"old:discord": {"state": "fatal"}, "telegram": {"state": "connected"}}}`), 0o644)
	mustWrite(t, RuntimeStatusUpdate{Fields: map[string]any{"platform": "reviewer:slack", "platform_state": "connected"}, ClearProfilePlatforms: true})
	p := platformsOf(t)
	if strings.Join(p.Keys, ",") != "telegram,reviewer:slack" {
		t.Fatalf("keys %v", p.Keys)
	}
	if got, _ := pyjson.Dumps(recGet(p, "telegram")); got != `{"state": "connected"}` {
		t.Fatal(got)
	}
	if recGet(recGet(p, "reviewer:slack").(*pyjson.Object), "state") != "connected" {
		t.Fatal("slack state")
	}
}

func TestPlatformWritesAreStampedWithWriterIdentity(t *testing.T) {
	statusHome(t)
	mustWrite(t, RuntimeStatusUpdate{Fields: map[string]any{"platform": "telegram", "platform_state": "connected"}})
	entry := recGet(platformsOf(t), "telegram").(*pyjson.Object)
	if !pyEq(recGet(entry, "writer_pid"), int64(os.Getpid())) || !pyEq(recGet(entry, "writer_start_time"), GetProcessStartTime(os.Getpid())) {
		t.Fatalf("writer %v %v", recGet(entry, "writer_pid"), recGet(entry, "writer_start_time"))
	}
}

func TestClearProfilePlatformsRepairsMalformedPlatforms(t *testing.T) {
	dir := statusHome(t)
	os.WriteFile(filepath.Join(dir, "gateway_state.json"), []byte(`{"platforms": ["not", "a", "mapping"]}`), 0o644)
	mustWrite(t, RuntimeStatusUpdate{ClearProfilePlatforms: true})
	if platformsOf(t).Len() != 0 {
		t.Fatal("platforms not repaired")
	}
}

func TestWriteRuntimeStatusOverwritesStalePidOnRestart(t *testing.T) {
	dir := statusHome(t)
	os.WriteFile(filepath.Join(dir, "gateway_state.json"), []byte(`{"pid": 99999, "start_time": 1000.0, "kind": "icebath-gateway", "platforms": {}, "updated_at": "2025-01-01T00:00:00Z"}`), 0o644)
	mustWrite(t, RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "running"}})
	payload := ReadRuntimeStatus("")
	if !pyEq(recGet(payload, "pid"), int64(os.Getpid())) || pyEq(recGet(payload, "start_time"), 1000.0) {
		t.Fatalf("pid %v start %v", recGet(payload, "pid"), recGet(payload, "start_time"))
	}
}

func TestRuntimeStatusRunningPidRejectsPidReusedByOtherProfile(t *testing.T) {
	payload := rec("pid", int64(139), "gateway_state", "running", "kind", "icebath-gateway", "argv", strs("icebath", "gateway", "run"))
	patch(t, &statusPidExists, func(int) bool { return true })
	patch(t, &statusProcessStartTime, fixedInt(nil))
	patch(t, &statusReadProcessCmdline, cmdline("icebath gateway run --replace"))
	if _, ok, _ := GetRuntimeStatusRunningPid(payload, "/opt/data/profiles/coder"); ok {
		t.Fatal("reused pid reported for coder")
	}
}

func TestRuntimeStatusRunningPidAcceptsMatchingProfileCmdline(t *testing.T) {
	payload := rec("pid", int64(139), "gateway_state", "running", "kind", "icebath-gateway", "argv", strs("icebath", "gateway", "run"), "start_time", int64(1000))
	patch(t, &statusPidExists, func(int) bool { return true })
	patch(t, &statusProcessStartTime, fixedInt(int64(1000)))
	for _, c := range []string{"icebath -p coder gateway run --replace", "/opt/icebath/.venv/bin/icebath --profile coder gateway run --replace",
		"icebath_home=/opt/data/profiles/coder icebath gateway run --replace"} {
		patch(t, &statusReadProcessCmdline, cmdline(c))
		if pid, ok, _ := GetRuntimeStatusRunningPid(payload, "/opt/data/profiles/coder"); !ok || pid != 139 {
			t.Fatalf("%s: %d %v", c, pid, ok)
		}
	}
}

func TestCommandLineBelongsToProfile(t *testing.T) {
	cases := []struct {
		cmd, home string
		want      bool
	}{
		{"icebath --profile default gateway run --replace --external-supervisor", "/opt/icebath-data", true},
		{"/opt/icebath/.venv/bin/icebath -p default gateway run", "/opt/icebath-data", true},
		{"icebath -p coder gateway run", "/opt/icebath-data", false},
		{`icebath_home=c:\opt\data\profiles\coder icebath gateway run --replace`, "c:/opt/data/profiles/coder", true},
		{"ICEBATH_HOME=/fixture/profiles/ops2 icebath gateway run", "/fixture/profiles/ops", false},
		{"ICEBATH_HOME=/fixture/profiles/ops-backup icebath gateway run", "/fixture/profiles/ops", false},
		{"ICEBATH_HOME=/fixture/profiles/ops/2 icebath gateway run", "/fixture/profiles/ops", false},
		{"ICEBATH_HOME=/opt/icebath-data2 icebath gateway run", "/opt/icebath-data", false},
		{"ICEBATH_HOME=/opt/icebath-data icebath gateway run", "/opt/icebath-data", true},
		{"icebath gateway run", "/opt/icebath-data", true},
		{`icebath_home="/opt/data/profiles/coder with space" icebath gateway run`, "/opt/data/profiles/coder with space", true},
		{"ICEBATH_HOME=/opt/data/profiles/coder with space icebath gateway run", "/opt/data/profiles/coder with space", true},
		{`ICEBATH_HOME=C:\Users\John Doe\.icebath icebath gateway run`, `C:\Users\John Doe\.icebath`, true},
		{"ICEBATH_HOME=/opt/data/profiles/coder with spaces icebath gateway run", "/opt/data/profiles/coder with space", false},
		{"ICEBATH_HOME=/fixture/profiles/ops/ icebath gateway run", "/fixture/profiles/ops", true},
		{"ICEBATH_HOME=/opt/icebath-data/ icebath gateway run", "/opt/icebath-data", true},
		{"FOO=icebath_home=/fixture/profiles/ops icebath gateway run", "/fixture/profiles/ops", false},
	}
	for _, c := range cases {
		if got := commandLineBelongsToProfile(c.cmd, c.home); got != c.want {
			t.Errorf("%q in %q = %v", c.cmd, c.home, got)
		}
	}
}

func TestWriteRuntimeStatusExplicitNoneClearsStaleFields(t *testing.T) {
	statusHome(t)
	mustWrite(t, RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "startup_failed", "exit_reason": "stale error",
		"platform": "discord", "platform_state": "fatal", "error_code": "discord_timeout", "error_message": "stale platform error"}})
	mustWrite(t, RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "running", "exit_reason": nil,
		"platform": "discord", "platform_state": "connected", "error_code": nil, "error_message": nil}})
	payload := ReadRuntimeStatus("")
	discord := recGet(recGet(payload, "platforms").(*pyjson.Object), "discord").(*pyjson.Object)
	if recGet(payload, "gateway_state") != "running" || recGet(payload, "exit_reason") != nil || recGet(discord, "state") != "connected" ||
		recGet(discord, "error_code") != nil || recGet(discord, "error_message") != nil {
		t.Fatal("stale fields kept")
	}
}

func TestConnectedClearsNeedsAttentionFromStartupStamp(t *testing.T) {
	statusHome(t)
	mustWrite(t, RuntimeStatusUpdate{Fields: map[string]any{"platform": "telegram", "platform_state": "retrying", "needs_attention": true,
		"retrying_since": "2026-08-30T07:53:47+00:00"}})
	mustWrite(t, RuntimeStatusUpdate{Fields: map[string]any{"platform": "telegram", "platform_state": "connected", "error_code": nil, "error_message": nil}})
	if !FlushRuntimeStatus(2 * time.Second) {
		t.Fatal("flush")
	}
	entry := recGet(platformsOf(t), "telegram").(*pyjson.Object)
	if recGet(entry, "state") != "connected" || recGet(entry, "needs_attention") != false || recGet(entry, "retrying_since") != nil {
		t.Fatal("needs_attention sticky")
	}
}

func TestBlockedWriteDoesNotBlockPublishAndBurstCoalesces(t *testing.T) {
	statusHome(t)
	writeStarted, releaseWrite, publishReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var writes []*pyjson.Object
	var count atomic.Int32
	writer := NewRuntimeStatusWriter(func(_ string, payload *pyjson.Object) error {
		writes = append(writes, payload)
		if count.Add(1) == 1 {
			close(writeStarted)
			<-releaseWrite
		}
		return nil
	})
	runtimeStatusStateMu.Lock()
	old := runtimeStatusWriter
	runtimeStatusWriter = writer
	runtimeStatusStateMu.Unlock()
	t.Cleanup(func() { runtimeStatusStateMu.Lock(); runtimeStatusWriter = old; runtimeStatusStateMu.Unlock() })
	go func() {
		PublishRuntimeStatus(context.Background(), RuntimeStatusUpdate{Fields: map[string]any{"platform": "reviewer:slack", "platform_state": "fatal"}})
		close(publishReturned)
	}()
	<-writeStarted
	<-publishReturned
	PublishRuntimeStatus(context.Background(), RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "running", "active_work": []string{"telegram:dm:1"}, "multiplex_standalone_reason": "single-profile"}})
	PublishRuntimeStatus(context.Background(), RuntimeStatusUpdate{Fields: map[string]any{"platform": "telegram", "platform_state": "connected",
		"ingress_url": "https://example.test/p/default/telegram", "listener_base": "http://127.0.0.1:8080"}})
	PublishRuntimeStatus(context.Background(), RuntimeStatusUpdate{DropProfilePlatforms: "reviewer"})
	for i := range 50 {
		PublishRuntimeStatus(context.Background(), RuntimeStatusUpdate{Fields: map[string]any{"active_agents": i}})
	}
	close(releaseWrite)
	if !writer.Flush(2 * time.Second) {
		t.Fatal("flush")
	}
	if len(writes) != 2 {
		t.Fatalf("writes %d", len(writes))
	}
	final := writes[1]
	plats := recGet(final, "platforms").(*pyjson.Object)
	telegram := recGet(plats, "telegram").(*pyjson.Object)
	work, _ := pyjson.Dumps(recGet(final, "active_work"))
	if recGet(final, "gateway_state") != "running" || !pyEq(recGet(final, "active_agents"), 49) || work != `["telegram:dm:1"]` ||
		recGet(final, "multiplex_standalone_reason") != "single-profile" || recGet(plats, "reviewer:slack") != nil ||
		!strings.HasSuffix(recGet(telegram, "ingress_url").(string), "/telegram") || recGet(telegram, "listener_base") != "http://127.0.0.1:8080" {
		t.Fatal("final snapshot wrong")
	}
}

func TestSyncWriteCanBoundABlockedPersistenceWait(t *testing.T) {
	statusHome(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	writer := NewRuntimeStatusWriter(func(string, *pyjson.Object) error {
		if once.CompareAndSwap(false, true) {
			close(started)
		}
		<-release
		return nil
	})
	runtimeStatusStateMu.Lock()
	old := runtimeStatusWriter
	runtimeStatusWriter = writer
	runtimeStatusStateMu.Unlock()
	t.Cleanup(func() { runtimeStatusStateMu.Lock(); runtimeStatusWriter = old; runtimeStatusStateMu.Unlock() })
	timeout := 50 * time.Millisecond
	persisted, err := WriteRuntimeStatus(context.Background(), RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "starting"}}, false, &timeout)
	<-started
	close(release)
	if err != nil || persisted {
		t.Fatalf("persisted %v %v", persisted, err)
	}
	if !writer.Flush(2 * time.Second) {
		t.Fatal("flush")
	}
}

func TestWriteRuntimeStatusRecordsPlatformMetricsAndActiveAgents(t *testing.T) {
	statusHome(t)
	metrics := rec("last_heartbeat", "2026-06-25T00:00:00+00:00", "metrics_today", rec("requests", int64(3), "tokens", int64(42)))
	mustWrite(t, RuntimeStatusUpdate{Fields: map[string]any{"platform": "api_server", "platform_state": "connected", "platform_metrics": metrics}})
	api := recGet(platformsOf(t), "api_server").(*pyjson.Object)
	if got, _ := pyjson.Dumps(recGet(api, "metrics")); got != `{"last_heartbeat": "2026-06-25T00:00:00+00:00", "metrics_today": {"requests": 3, "tokens": 42}}` {
		t.Fatal(got)
	}
	mustWrite(t, RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "running", "active_agents": 0}})
	mustWrite(t, RuntimeStatusUpdate{Fields: map[string]any{"active_agents": 2}})
	r := ReadRuntimeStatus("")
	if !pyEq(recGet(r, "active_agents"), 2) || recGet(r, "gateway_state") != "running" {
		t.Fatal("active agents write clobbered state")
	}
	stamp := recGet(r, "updated_at").(string)
	if _, aware, err := pydatetime.ParseISOFormat(stamp); err != nil || !aware {
		t.Fatalf("updated_at %q", stamp)
	}
	if norm, ok, _ := NormalizeUpdatedAt(stamp); !ok || norm != stamp {
		t.Fatalf("normalized %q", norm)
	}
}

func TestLiveProcessStartTimeIsStableInt(t *testing.T) {
	cmd := exec.Command("sleep", "20")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	a := GetProcessStartTime(cmd.Process.Pid)
	time.Sleep(200 * time.Millisecond)
	b := GetProcessStartTime(cmd.Process.Pid)
	if _, ok := a.(int64); !ok || a != b {
		t.Fatalf("start times %v %v", a, b)
	}
}

func TestTerminatePidWindowsForceGuards(t *testing.T) {
	patch(t, &statusIsWindows, true)
	var calls []int
	patch(t, &statusTaskkill, func(pid int) (int, string, string, error) { calls = append(calls, pid); return 0, "", "", nil })
	patch(t, &statusProcessStartTime, fixedInt(int64(456)))
	if err := TerminatePid(123, true, int64(456)); err != nil || len(calls) != 1 || calls[0] != 123 {
		t.Fatalf("taskkill %v %v", calls, err)
	}
	calls = nil
	if err := TerminatePid(123, true, nil); err == nil || !strings.Contains(err.Error(), "without a process start-time guard") || calls != nil {
		t.Fatalf("unguarded %v", err)
	}
	patch(t, &statusProcessStartTime, fixedInt(int64(999)))
	if err := TerminatePid(123, true, int64(456)); err == nil || !strings.Contains(err.Error(), "process identity changed") || calls != nil {
		t.Fatalf("reused %v", err)
	}
}

func TestPidExistsReportsZombiesDead(t *testing.T) {
	if !pidExistsLive(os.Getpid()) {
		t.Fatal("self not alive")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && pidExistsLive(cmd.Process.Pid) {
		time.Sleep(10 * time.Millisecond)
	}
	if pidExistsLive(cmd.Process.Pid) {
		t.Fatal("zombie reported alive")
	}
}
