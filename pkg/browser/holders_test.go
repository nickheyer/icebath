package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestContentionIsRetriedUntilTheDeadlineThenFailsNamingTheDir(t *testing.T) {
	dir := t.TempDir()
	for _, code := range []syscall.Errno{syscall.EACCES, syscall.EDEADLK} {
		busy := error(code)
		if ntLockFailure(busy, dir, false) != nil {
			t.Fatalf("%v not retried", code)
		}
		var timeout *recordsLockTimeoutError
		failure := ntLockFailure(busy, dir, true)
		if !errors.As(failure, &timeout) || !strings.Contains(failure.Error(), dir) {
			t.Fatalf("%v failure %v", code, failure)
		}
	}
}

func TestAnyOtherLockErrorIsRaisedAtOnce(t *testing.T) {
	for _, code := range []syscall.Errno{syscall.EBADF, syscall.EINVAL, syscall.EIO} {
		broken := error(code)
		if ntLockFailure(broken, t.TempDir(), false) != broken {
			t.Fatalf("%v not raised", code)
		}
	}
}

func TestALockHeldElsewhereIsNotTakenAndTimesOut(t *testing.T) {
	dir := t.TempDir()
	held, unlock, err := recordsLock(dir, true)
	if err != nil || !held {
		t.Fatalf("first lock %v %v", held, err)
	}
	if runtime.GOOS == "windows" {
		pin(t, &ntLockTimeout, 0)
		var timeout *recordsLockTimeoutError
		if _, _, err := recordsLock(dir, true); !errors.As(err, &timeout) {
			t.Fatalf("hung holder err %v", err)
		}
	}
	if again, release, err := recordsLock(dir, false); err != nil || again {
		t.Fatalf("contended lock taken %v %v", again, err)
	} else {
		release()
	}
	unlock()
	if again, release, err := recordsLock(dir, false); err != nil || !again {
		t.Fatalf("released lock not taken %v %v", again, err)
	} else {
		release()
	}
	if _, err := os.Stat(filepath.Join(dir, holdersLockName)); err != nil {
		t.Fatal(err)
	}
}

func lease(holderPID int64, holderStart, startTime any, ns any) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("holder_pid", holderPID)
	o.Set("holder_start_time", holderStart)
	o.Set("pid", int64(4242))
	o.Set("start_time", startTime)
	o.Set("user_data_dir", "/p")
	o.Set("ns", ns)
	return o
}

func writeJSON(t *testing.T, path string, o *pyjson.Object) {
	t.Helper()
	text, _ := pyjson.Dumps(o)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLeasesNameOnlyOtherLiveHoldersOfTheSameLaunch(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "abc.json")
	alive := map[int]bool{os.Getpid(): true, 111: true}
	pin(t, &launcherPidAlive, func(pid int) bool { return alive[pid] })
	pin(t, &launcherStartTime, func(pid int) (int64, bool) { return int64(pid) * 10, true })
	start := int64(77)
	if err := writeLease(context.Background(), record, &LaunchedBrowser{PID: new(int64), StartTime: &start, UserDataDir: "/p"}); err != nil {
		t.Fatal(err)
	}
	ns := nsValue()
	writeJSON(t, filepath.Join(dir, "abc@111.lease"), lease(111, int64(1110), int64(77), ns))
	writeJSON(t, filepath.Join(dir, "abc@222.lease"), lease(222, int64(2220), int64(77), ns))
	writeJSON(t, filepath.Join(dir, "abc@333.lease"), lease(333, nil, int64(76), ns))
	writeJSON(t, filepath.Join(dir, "abc@444.lease"), lease(444, nil, int64(77), "other-namespace"))
	writeJSON(t, filepath.Join(dir, "abc@555.lease"), lease(555, nil, nil, "other-namespace"))
	os.WriteFile(filepath.Join(dir, "abc@666.lease"), []byte("[1]"), 0o600)
	holders := otherLiveHolders(record, int64(77))
	var pids []any
	for _, h := range holders {
		pids = append(pids, objGet(h, "holder_pid"))
	}
	if len(pids) != 3 || pids[0] != int64(111) || pids[1] != int64(444) || pids[2] != int64(555) {
		t.Fatalf("holders %v", pids)
	}
	for _, gone := range []string{"abc@222.lease", "abc@333.lease", "abc@666.lease"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s kept", gone)
		}
	}
	if s := successor(holders); objGet(s, "holder_pid") != int64(111) {
		t.Fatalf("successor %v", s)
	}
	if s := successor(holders[1:]); s != nil {
		t.Fatalf("foreign successor %v", s)
	}
	dropLease(record)
	if _, err := os.Stat(leasePath(record, os.Getpid())); err == nil {
		t.Fatal("own lease kept")
	}
}

func TestDeadLeasesAreDroppedAndAHandOverNamesTheNewOwner(t *testing.T) {
	dir := t.TempDir()
	pin(t, &launcherPidAlive, func(pid int) bool { return pid == 111 })
	pin(t, &launcherStartTime, func(pid int) (int64, bool) { return 5, true })
	writeJSON(t, filepath.Join(dir, "r@111.lease"), lease(111, int64(5), nil, nsValue()))
	writeJSON(t, filepath.Join(dir, "r@112.lease"), lease(111, int64(6), nil, nsValue()))
	writeJSON(t, filepath.Join(dir, "r@113.lease"), lease(113, nil, nil, nsValue()))
	dropDeadLeases(dir)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "r@111.lease" {
		t.Fatalf("entries %v", entries)
	}
	record := filepath.Join(dir, "r.json")
	data := pyjson.NewObject()
	data.Set("pid", int64(9))
	data.Set("owner_pid", int64(1))
	data.Set("ns", nsValue())
	if err := handOver(context.Background(), record, data, lease(111, int64(5), nil, nsValue())); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(record)
	got, _ := pyjson.LoadsBytes(raw)
	obj := got.(*pyjson.Object)
	if strings.Join(obj.Keys, ",") != "pid,owner_pid,ns,owner_start_time" || objGet(obj, "owner_pid") != int64(111) || objGet(obj, "owner_start_time") != int64(5) {
		t.Fatalf("record %s", raw)
	}
}

func TestProcessAliveNeedsTheRecordedStartTime(t *testing.T) {
	pin(t, &launcherPidAlive, func(pid int) bool { return pid == 7 })
	pin(t, &launcherStartTime, func(pid int) (int64, bool) { return 70, true })
	cases := []struct {
		pid, start any
		want       bool
	}{{int64(7), nil, true}, {int64(7), int64(70), true}, {int64(7), float64(70), true}, {int64(7), int64(71), false},
		{int64(8), nil, false}, {"7", nil, false}, {float64(7), nil, false}, {nil, nil, false}}
	for _, c := range cases {
		if processAlive(c.pid, c.start) != c.want {
			t.Errorf("%+v", c)
		}
	}
}

func TestNamespaceTagFollowsTheIdentity(t *testing.T) {
	identity, ok := nsIdentity()
	if runtime.GOOS == "linux" && !ok {
		t.Fatal("no namespace identity on linux")
	}
	if ok != (nsTag() != "") || (ok && (len(nsTag()) != 14 || !strings.Contains(identity, ":"))) {
		t.Fatalf("tag %q identity %q", nsTag(), identity)
	}
	if !sameNamespace(lease(1, nil, nil, nsValue())) || sameNamespace(lease(1, nil, nil, "x:y")) {
		t.Fatal("namespace match")
	}
}
