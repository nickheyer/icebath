package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func lockDir(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "locks")
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", dir)
	os.MkdirAll(dir, 0o755)
	return dir
}

func TestAcquireScopedLockRejectsLiveOtherProcess(t *testing.T) {
	dir := lockDir(t)
	lock := filepath.Join(dir, "telegram-bot-token-2bb80d537b1da3e3.lock")
	writeRecord(t, lock, rec("pid", int64(99999), "start_time", int64(123), "kind", "icebath-gateway"))
	patch(t, &statusPidExists, func(int) bool { return true })
	patch(t, &statusProcessStartTime, fixedInt(int64(123)))
	acquired, existing, err := AcquireScopedLock(context.Background(), "telegram-bot-token", "secret", rec("platform", "telegram"))
	if err != nil || acquired || !pyEq(recGet(existing, "pid"), 99999) {
		t.Fatalf("%v %v", acquired, err)
	}
}

func TestAcquireScopedLockReplacesPidReusedByUnrelatedProcess(t *testing.T) {
	dir := lockDir(t)
	lock := filepath.Join(dir, "telegram-bot-token-2bb80d537b1da3e3.lock")
	writeRecord(t, lock, rec("pid", int64(873), "start_time", nil, "kind", "icebath-gateway",
		"argv", strs("/Users/user/.icebath/icebath-agent/icebath_cli/main.py", "gateway", "run", "--replace")))
	patch(t, &statusPidExists, func(int) bool { return true })
	patch(t, &statusProcessStartTime, fixedInt(nil))
	patch(t, &statusLooksLikeGatewayProcess, func(int) bool { return false })
	patch(t, &statusReadProcessCmdline, cmdline("/usr/libexec/bluetoothuserd"))
	acquired, _, err := AcquireScopedLock(context.Background(), "telegram-bot-token", "secret", rec("platform", "telegram"))
	payload := readRecord(t, lock)
	if err != nil || !acquired || !pyEq(recGet(payload, "pid"), int64(os.Getpid())) || recGet(recGet(payload, "metadata").(*pyjson.Object), "platform") != "telegram" {
		t.Fatalf("%v %v", acquired, err)
	}
}

func TestAcquireScopedLockSelfReacquiresWhateverDiskStartTime(t *testing.T) {
	for _, disk := range []any{nil, int64(111)} {
		dir := lockDir(t)
		lock := filepath.Join(dir, "discord-bot-token-2bb80d537b1da3e3.lock")
		writeRecord(t, lock, rec("pid", int64(os.Getpid()), "start_time", disk, "kind", "icebath-gateway",
			"argv", strs("icebath_cli/main.py", "gateway", "run", "--replace"), "scope", "discord-bot-token"))
		patch(t, &statusProcessStartTime, fixedInt(int64(987654321)))
		patch(t, &statusPidExists, func(int) bool { return true })
		patch(t, &statusLooksLikeGatewayProcess, func(int) bool { return true })
		acquired, existing, err := AcquireScopedLock(context.Background(), "discord-bot-token", "secret", rec("platform", "discord"))
		payload := readRecord(t, lock)
		if err != nil || !acquired || !pyEq(recGet(existing, "pid"), int64(os.Getpid())) || !pyEq(recGet(payload, "start_time"), 987654321) {
			t.Fatalf("disk %v: %v %v", disk, acquired, err)
		}
	}
}

func TestReleaseScopedLockAllowsNullDiskStartTime(t *testing.T) {
	dir := lockDir(t)
	lock := filepath.Join(dir, "discord-bot-token-2bb80d537b1da3e3.lock")
	writeRecord(t, lock, rec("pid", int64(os.Getpid()), "start_time", nil, "kind", "icebath-gateway"))
	patch(t, &statusProcessStartTime, fixedInt(int64(987654321)))
	ReleaseScopedLock("discord-bot-token", "secret")
	if _, err := os.Stat(lock); err == nil {
		t.Fatal("lock kept")
	}
}

func TestAcquireScopedLockRaceSecondAcquirerLoses(t *testing.T) {
	dir := lockDir(t)
	lock := filepath.Join(dir, "telegram-bot-token-2bb80d537b1da3e3.lock")
	writeRecord(t, lock, rec("pid", int64(99999), "start_time", int64(123), "kind", "icebath-gateway"))
	patch(t, &statusPidExists, func(int) bool { return false })
	winner := rec("pid", int64(424242), "start_time", int64(456), "kind", "icebath-gateway", "scope", "telegram-bot-token")
	patch(t, &statusReplace, func(src, dst string) error {
		if src == lock {
			writeRecord(t, lock, winner)
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.ENOENT}
		}
		return os.Rename(src, dst)
	})
	acquired, existing, err := AcquireScopedLock(context.Background(), "telegram-bot-token", "secret", rec("platform", "telegram"))
	if err != nil || acquired || !pyEq(recGet(existing, "pid"), 424242) || !pyEq(recGet(readRecord(t, lock), "pid"), 424242) {
		t.Fatalf("%v %v", acquired, err)
	}
}

func TestAcquireScopedLockReplacesStaleRecordAndStampsLabel(t *testing.T) {
	dir := lockDir(t)
	lock := filepath.Join(dir, "telegram-bot-token-2bb80d537b1da3e3.lock")
	writeRecord(t, lock, rec("pid", int64(99999), "start_time", int64(123), "kind", "icebath-gateway"))
	patch(t, &statusPidExists, func(int) bool { return false })
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "profiles", "lead-gen-outreach"))
	acquired, _, err := AcquireScopedLock(context.Background(), "telegram-bot-token", "secret", rec("platform", "telegram"))
	payload := readRecord(t, lock)
	if err != nil || !acquired || !pyEq(recGet(payload, "pid"), int64(os.Getpid())) || recGet(payload, "profile") != "lead-gen-outreach" {
		t.Fatalf("%v %v %v", acquired, err, recGet(payload, "profile"))
	}
	if filepath.Base(lock) != "telegram-bot-token-"+scopeHash("secret")+".lock" {
		t.Fatal("scope hash")
	}
}

func TestAcquireScopedLockOmitsProfileWhenNotInferable(t *testing.T) {
	dir := lockDir(t)
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "some-custom-dir"))
	patch(t, &statusProfileLabelForHome, func(string) string { return "" })
	acquired, _, err := AcquireScopedLock(context.Background(), "telegram-bot-token", "secret", nil)
	payload := readRecord(t, filepath.Join(dir, "telegram-bot-token-"+scopeHash("secret")+".lock"))
	if _, has := payload.Get("profile"); err != nil || !acquired || has {
		t.Fatal("profile stamped")
	}
}

func TestReleaseAllScopedLocksCanTargetSingleOwner(t *testing.T) {
	dir := lockDir(t)
	target, other := filepath.Join(dir, "telegram-bot-token-target.lock"), filepath.Join(dir, "slack-app-token-other.lock")
	writeRecord(t, target, rec("pid", int64(111), "start_time", int64(222), "kind", "icebath-gateway"))
	writeRecord(t, other, rec("pid", int64(999), "start_time", int64(333), "kind", "icebath-gateway"))
	owner := 111
	if n := ReleaseAllScopedLocks(&owner, int64(222)); n != 1 {
		t.Fatalf("removed %d", n)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("target kept")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("other removed")
	}
}

func TestScopedLockAcquireReportsUnusableLockDir(t *testing.T) {
	dir := lockDir(t)
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o755)
	ResetUnusableLockDirReports()
	_, _, err := AcquireScopedLock(context.Background(), "telegram-bot-token", "secret", nil)
	var unusable *ScopedLockDirUnusable
	if !errors.As(err, &unusable) || !UnusableLockDirReported(dir) || !strings.Contains(err.Error(), "gateway lock directory is not usable") {
		t.Fatalf("error %v", err)
	}
	os.Chmod(dir, 0o755)
	if acquired, _, err := AcquireScopedLock(context.Background(), "telegram-bot-token", "secret", nil); err != nil || !acquired || UnusableLockDirReported(dir) {
		t.Fatalf("recovery %v %v", acquired, err)
	}
}

func TestScopedLockOwnerLabel(t *testing.T) {
	tmp := t.TempDir()
	if got := profileLabelForHomeLive(filepath.Join(tmp, "profiles", "lead-gen-outreach")); got != "lead-gen-outreach" {
		t.Fatal(got)
	}
	if got := profileLabelForHomeLive("/opt/data/profiles/zerocool"); got != "zerocool" {
		t.Fatal(got)
	}
	t.Setenv("ICEBATH_HOME", "/opt/data")
	if got := profileLabelForHomeLive("/opt/data"); got != "default" {
		t.Fatal(got)
	}
	os.Unsetenv("ICEBATH_HOME")
	if got := profileLabelForHomeLive("/somewhere/else"); got != "" {
		t.Fatal(got)
	}
	if got := profileLabelForHomeLive(filepath.Join(tmp, "profiles", "Bad Name!")); got != "" {
		t.Fatal(got)
	}
	cases := []struct {
		record any
		want   string
	}{
		{rec("profile", "coder", "hermes_home", "/opt/data/profiles/other"), "coder"},
		{rec("profile", "Bad Name!"), ""},
		{rec("profile", "  "), ""},
		{rec("profile", int64(42)), ""},
		{rec("profile", "bad; rm -rf /", "hermes_home", "/opt/data/profiles/zerocool"), "zerocool"},
		{rec("pid", int64(559), "hermes_home", "/opt/data/profiles/zerocool"), "zerocool"},
		{rec("pid", int64(99999)), ""},
		{nil, ""},
		{"not-a-dict", ""},
	}
	for _, c := range cases {
		if got := ScopedLockOwnerLabel(c.record); got != c.want {
			t.Errorf("%v = %q", c.record, got)
		}
	}
}

func TestTakeoverMarker(t *testing.T) {
	dir := statusHome(t)
	marker := filepath.Join(dir, ".gateway-takeover.json")
	patch(t, &statusProcessStartTime, fixedInt(int64(42)))
	if !WriteTakeoverMarker(context.Background(), 12345, "", nil, false) {
		t.Fatal("write")
	}
	p := readRecord(t, marker)
	if !pyEq(recGet(p, "target_pid"), 12345) || !pyEq(recGet(p, "target_start_time"), 42) || !pyEq(recGet(p, "replacer_pid"), int64(os.Getpid())) ||
		recGet(p, "written_at") == nil || recGet(p, "replacer_hermes_home") != dir {
		t.Fatal("marker fields")
	}
	patch(t, &statusProcessStartTime, fixedInt(nil))
	if !WriteTakeoverMarker(context.Background(), os.Getpid(), "", nil, false) || recGet(readRecord(t, marker), "target_start_time") != nil {
		t.Fatal("write without start time")
	}
	if ok, err := ConsumeTakeoverMarkerForSelf(); err != nil || !ok {
		t.Fatalf("consume %v %v", ok, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("marker kept")
	}
	patch(t, &statusProcessStartTime, fixedInt(int64(100)))
	now := utcNowISO()
	writeRecord(t, marker, rec("target_pid", int64(os.Getpid()), "target_start_time", int64(100), "replacer_pid", int64(99999),
		"replacer_hermes_home", filepath.Join(dir, "profiles", "other"), "written_at", now))
	if ok, _ := ConsumeTakeoverMarkerForSelf(); ok {
		t.Fatal("foreign profile marker consumed")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("foreign marker removed")
	}
	writeRecord(t, marker, rec("target_pid", int64(os.Getpid()), "target_start_time", int64(100), "replacer_pid", int64(99999), "written_at", now))
	if ok, _ := ConsumeTakeoverMarkerForSelf(); !ok {
		t.Fatal("legacy marker rejected")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("legacy marker kept")
	}
}

func ownerRecord(t *testing.T, targetHome string, pid int64) *pyjson.Object {
	r := rec("pid", pid, "kind", "icebath-gateway", "argv", strs("python", "-m", "icebath_cli.main", "gateway", "run"),
		"start_time", int64(123), "hermes_home", targetHome)
	writeRecord(t, filepath.Join(targetHome, "gateway.pid"), r)
	return r
}

func TestVerifiedDistinctHomeHandoffMarksTargetBeforeSigterm(t *testing.T) {
	tmp := t.TempDir()
	replacer, target := filepath.Join(tmp, "replacer"), filepath.Join(tmp, "target")
	os.MkdirAll(replacer, 0o755)
	t.Setenv("ICEBATH_HOME", replacer)
	record := ownerRecord(t, target, 4242)
	alive := []bool{true, true, false}
	patch(t, &statusPidExists, func(int) bool { v := alive[0]; alive = alive[1:]; return v })
	patch(t, &statusProcessStartTime, fixedInt(int64(123)))
	patch(t, &statusReadProcessCmdline, cmdline("python -m icebath_cli.main gateway run"))
	patch(t, &statusSnapshotGatewayChildren, func(int) []gatewayChild { return nil })
	patch(t, &statusSleep, func(time.Duration) {})
	var calls [][2]any
	patch(t, &statusTerminatePid, func(pid int, force bool, _ any) error {
		m := readRecord(t, filepath.Join(target, ".gateway-takeover.json"))
		if recGet(m, "target_hermes_home") != target || recGet(m, "replacer_hermes_home") != replacer {
			t.Error("marker homes")
		}
		calls = append(calls, [2]any{pid, force})
		return nil
	})
	if pid, ok := TakeOverScopedLockHolder(context.Background(), record, 1, 20); !ok || pid != 4242 {
		t.Fatalf("owner %d %v", pid, ok)
	}
	if len(calls) != 1 || calls[0] != [2]any{4242, false} {
		t.Fatalf("calls %v", calls)
	}
	for _, h := range []string{target, replacer} {
		if _, err := os.Stat(filepath.Join(h, ".gateway-takeover.json")); err == nil {
			t.Fatal("marker kept")
		}
	}
}

func TestHandoffRejectsUncorroboratedTargetHome(t *testing.T) {
	statusHome(t)
	target := filepath.Join(t.TempDir(), "target")
	record := ownerRecord(t, target, 4242)
	ownerRecord(t, target, 9999)
	patch(t, &statusPidExists, func(int) bool { return true })
	patch(t, &statusProcessStartTime, fixedInt(int64(123)))
	patch(t, &statusReadProcessCmdline, cmdline("python -m icebath_cli.main gateway run"))
	called := false
	patch(t, &statusTerminatePid, func(int, bool, any) error { called = true; return nil })
	if _, ok := TakeOverScopedLockHolder(context.Background(), record, 20, 20); ok || called {
		t.Fatal("uncorroborated owner signalled")
	}
	if _, err := os.Stat(filepath.Join(target, ".gateway-takeover.json")); err == nil {
		t.Fatal("marker written")
	}
}

func TestPlannedStopMarker(t *testing.T) {
	dir := statusHome(t)
	marker := filepath.Join(dir, ".gateway-planned-stop.json")
	patch(t, &statusProcessStartTime, fixedInt(int64(42)))
	if !WritePlannedStopMarker(context.Background(), 12345) {
		t.Fatal("write")
	}
	p := readRecord(t, marker)
	if !pyEq(recGet(p, "target_pid"), 12345) || !pyEq(recGet(p, "target_start_time"), 42) || !pyEq(recGet(p, "stopper_pid"), int64(os.Getpid())) || recGet(p, "written_at") == nil {
		t.Fatal("fields")
	}
	patch(t, &statusProcessStartTime, fixedInt(nil))
	WritePlannedStopMarker(context.Background(), os.Getpid())
	if targets, err := PlannedStopMarkerTargetsSelf(); err != nil || !targets {
		t.Fatal("probe")
	}
	if ok, err := ConsumePlannedStopMarkerForSelf(); err != nil || !ok {
		t.Fatalf("consume %v %v", ok, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("marker kept")
	}
	patch(t, &statusProcessStartTime, fixedInt(int64(100)))
	WritePlannedStopMarker(context.Background(), os.Getpid())
	patch(t, &statusProcessStartTime, fixedInt(int64(9999)))
	if ok, _ := ConsumePlannedStopMarkerForSelf(); ok {
		t.Fatal("mismatched start time consumed")
	}
}
