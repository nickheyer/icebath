//go:build !windows

package state

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Pid of a reaped child that no process holds any more
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("reaped pid %d unexpectedly reused", cmd.Process.Pid)
	}
	return cmd.Process.Pid
}

func record(kv ...any) pyObject {
	obj := pyObject{}
	for i := 0; i < len(kv); i += 2 {
		obj = append(obj, pyKeyValue{kv[i].(string), kv[i+1]})
	}
	return obj
}

func provablyDead(t *testing.T, rec pyObject) bool {
	t.Helper()
	dead, err := lockHolderProvablyDead(rec)
	if err != nil {
		t.Fatal(err)
	}
	return dead
}

func TestFlockHolderRecordQualifiesPidNamespaces(t *testing.T) {
	ours := LocalPidNamespace{ID: "111", Supported: true}
	pinPidNamespace(t, &ours, func() LocalPidNamespace { return ours })
	dead := deadPid(t)
	handle, err := os.OpenFile(filepath.Join(t.TempDir(), "lock"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	writeLockHolderRecord(handle)
	if ns, _ := readLockHolderRecord(handle).get("pidns"); ns != "111" {
		t.Fatalf("stamped namespace %v", ns)
	}
	deadPidNumber := jsonInt(dead)
	if !provablyDead(t, record("pid", deadPidNumber, "pidns", "111", "start_ticks", jsonInt(1), "acquired_at", 0.0)) {
		t.Fatal("same namespace dead holder must break")
	}
	if provablyDead(t, record("pid", deadPidNumber, "pidns", "222", "start_ticks", jsonInt(1), "acquired_at", 0.0)) {
		t.Fatal("foreign namespace must defer")
	}
	if !provablyDead(t, record("pid", deadPidNumber, "start_ticks", jsonInt(1), "acquired_at", 0.0)) {
		t.Fatal("unstamped record keeps probing")
	}
	if provablyDead(t, nil) {
		t.Fatal("no record defers")
	}
	resolved := LocalPidNamespace{ID: "", Supported: true}
	pinPidNamespace(t, nil, func() LocalPidNamespace { return resolved })
	if provablyDead(t, record("pid", deadPidNumber, "pidns", "111", "start_ticks", jsonInt(1), "acquired_at", 0.0)) {
		t.Fatal("stamped record with a failed lookup defers")
	}
	if !provablyDead(t, record("pid", deadPidNumber, "start_ticks", jsonInt(1), "acquired_at", 0.0)) {
		t.Fatal("unstamped record still breaks")
	}
	resolved = ours
	if !provablyDead(t, record("pid", deadPidNumber, "pidns", "111", "start_ticks", jsonInt(1), "acquired_at", 0.0)) {
		t.Fatal("resolved lookup breaks a stamped record")
	}
}

func jsonInt(n int) any { return json.Number(strconv.Itoa(n)) }

func TestLockHolderLivenessAndRecycling(t *testing.T) {
	self := os.Getpid()
	ticks, ok := procStartTicks(int64(self))
	if _, err := os.Stat("/proc/self/stat"); err == nil && !ok {
		t.Fatal("own start ticks unreadable")
	}
	cases := []struct {
		rec  pyObject
		dead bool
	}{
		{record("pidns", nil), false},
		{record("pid", "abc"), false},
		{record("pid", nil), false},
		{record("pid", jsonInt(0)), false},
		{record("pid", jsonInt(-4)), false},
		{record("pid", jsonInt(self)), false},
		{record("pid", jsonInt(self), "start_ticks", nil), false},
	}
	if ok {
		cases = append(cases,
			struct {
				rec  pyObject
				dead bool
			}{record("pid", jsonInt(self), "start_ticks", json.Number(strconv.FormatInt(ticks, 10))), false},
			struct {
				rec  pyObject
				dead bool
			}{record("pid", strconv.Itoa(self), "start_ticks", float64(ticks)), false},
			struct {
				rec  pyObject
				dead bool
			}{record("pid", jsonInt(self), "start_ticks", jsonInt(int(ticks)+1)), true},
			struct {
				rec  pyObject
				dead bool
			}{record("pid", jsonInt(self), "start_ticks", strconv.FormatInt(ticks, 10)), true},
		)
	}
	for _, tc := range cases {
		if got := provablyDead(t, tc.rec); got != tc.dead {
			t.Fatalf("%v -> %v", tc.rec, got)
		}
	}
	for _, rec := range []pyObject{record("pid", json.Number("99999999999")), record("pid", math.Inf(1))} {
		if _, err := lockHolderProvablyDead(rec); err == nil {
			t.Fatalf("%v must raise OverflowError", rec)
		}
	}
}

func TestAdvisoryLockContention(t *testing.T) {
	for err, want := range map[error]bool{
		syscall.EAGAIN: true, syscall.EACCES: true, syscall.EDEADLK: true, syscall.EINPROGRESS: true,
		&os.PathError{Op: "flock", Path: "x", Err: syscall.EAGAIN}: true,
		syscall.ENOLCK: false, syscall.EIO: false, syscall.ENOENT: false, errors.New("x"): false,
	} {
		if IsAdvisoryLockContention(err) != want {
			t.Fatalf("%v", err)
		}
	}
}

func readLockFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestFTSRebuildAdmissionSerializesAndReleases(t *testing.T) {
	zero := 0.0
	if err := FTSRebuildAdmission("", &zero, func(admitted bool) error {
		if !admitted {
			t.Fatal("in memory store is always admitted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "state.db")
	lockPath := dbPath + ".fts_rebuild.lock"
	sentinel := errors.New("body failed")
	err := FTSRebuildAdmission(dbPath, &zero, func(admitted bool) error {
		if !admitted {
			t.Fatal("first rebuild must be admitted")
		}
		body := readLockFile(t, lockPath)
		if !strings.HasPrefix(body, `{"acquired_at": `) || !strings.Contains(body, `"pid": `+strconv.Itoa(os.Getpid())+`, "pidns": `) {
			t.Fatalf("holder record %q", body)
		}
		nested := FTSRebuildAdmission(dbPath, &zero, func(inner bool) error {
			if inner {
				t.Fatal("a live holder must defer a second rebuild")
			}
			return nil
		})
		if nested != nil {
			t.Fatal(nested)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("body error %v", err)
	}
	if body := readLockFile(t, lockPath); body != "" {
		t.Fatalf("normal release clears the record, got %q", body)
	}
	if err := FTSRebuildAdmission(dbPath, nil, func(admitted bool) error {
		if !admitted {
			t.Fatal("released lock is admitted again")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := FTSRebuildAdmission(filepath.Join(t.TempDir(), "missing", "state.db"), nil, func(admitted bool) error {
		if admitted {
			t.Fatal("unopenable lock file fails closed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFTSRebuildAdmissionBreaksOrphanedDescriptor(t *testing.T) {
	dead := deadPid(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	lockPath := dbPath + ".fts_rebuild.lock"
	orphan, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	defer orphan.Close()
	if err := syscall.Flock(int(orphan.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	rewriteLockFile(orphan, []byte(`{"acquired_at": 0.0, "pid": `+strconv.Itoa(dead)+`, "start_ticks": 1}`))
	before, _ := StatDBFileIdentity(lockPath)
	zero := 0.0
	if err := FTSRebuildAdmission(dbPath, &zero, func(admitted bool) error {
		if !admitted {
			t.Fatal("dead holder's orphaned descriptor must be broken")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, _ := StatDBFileIdentity(lockPath)
	if before == after {
		t.Fatal("lock must be retaken on a fresh inode")
	}
}

func TestProcStartTicksOfMissingProcess(t *testing.T) {
	if _, ok := procStartTicks(int64(deadPid(t))); ok {
		t.Fatal("reaped process has no start ticks")
	}
}

func TestDescribeLockHolder(t *testing.T) {
	for _, tc := range []struct {
		rec  pyObject
		want string
	}{
		{nil, "unknown (no holder record; pre-fix writer or non-icebath)"},
		{record("acquired_at", 1.0), "unknown (no holder record; pre-fix writer or non-icebath)"},
		{record("pid", json.Number("42")), "pid 42"},
		{record("pid", "x", "acquired_at", "soon"), "pid x"},
		{record("pid", []any{json.Number("1"), "a"}), "pid [1, 'a']"},
		{record("pid", nil, "acquired_at", nil), "pid None"},
	} {
		if got, err := describeLockHolder(tc.rec); err != nil || got != tc.want {
			t.Fatalf("%v -> %q %v", tc.rec, got, err)
		}
	}
	got, err := describeLockHolder(record("pid", json.Number("7"), "acquired_at", float64(time.Now().Unix()-30)))
	if err != nil || (got != "pid 7, acquired 30s ago" && got != "pid 7, acquired 31s ago") {
		t.Fatalf("aged holder %q %v", got, err)
	}
	if _, err := describeLockHolder(record("pid", json.Number("1"), "acquired_at", json.Number("1"+strings.Repeat("0", 400)))); err == nil {
		t.Fatal("float overflow propagates")
	}
}
