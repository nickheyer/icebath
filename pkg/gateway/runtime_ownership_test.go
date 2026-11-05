//go:build linux

package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func ownershipConflict(err error) bool {
	var c *OwnershipConflict
	return errors.As(err, &c)
}

func TestProfileReservationsUnwindWithoutReleasingAnotherOwner(t *testing.T) {
	tmp := t.TempDir()
	a, b := filepath.Join(tmp, "a"), filepath.Join(tmp, "b")
	os.Mkdir(a, 0o755)
	os.Mkdir(b, 0o755)
	blocker := NewProfileOwnership()
	if err := blocker.Reserve([]string{b}, ""); err != nil {
		t.Fatal(err)
	}
	contender := NewProfileOwnership()
	if err := contender.Reserve([]string{b, a}, ""); !ownershipConflict(err) {
		t.Fatalf("contender %v", err)
	}
	free := NewProfileOwnership()
	if err := free.Reserve([]string{a}, ""); err != nil {
		t.Fatalf("unwound home still held %v", err)
	}
	contender.Close()
	if err := contender.Reserve([]string{b}, ""); !ownershipConflict(err) {
		t.Fatal("contender took a held home")
	}
	blocker.Close()
	if err := contender.Reserve([]string{b}, ""); err != nil {
		t.Fatal(err)
	}
	blocker.Close()
	if err := blocker.Reserve([]string{b}, ""); !ownershipConflict(err) {
		t.Fatal("late cleanup released the replacement")
	}
	free.Close()
	contender.Close()
}

func TestStaleUnopenableGatewayLockIsReplacedOnBoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("permission semantics need an unprivileged user")
	}
	h := filepath.Join(t.TempDir(), "home")
	os.Mkdir(h, 0o700)
	t.Setenv("ICEBATH_HOME", h)
	lock := filepath.Join(h, "gateway.lock")
	os.WriteFile(lock, []byte(`{"pid": 1}`), 0o644)
	os.Chmod(lock, 0o400)
	if !AcquireGatewayRuntimeLock() {
		t.Fatal("stale unwritable lock not replaced")
	}
	defer ReleaseGatewayRuntimeLock()
	if r := readRecord(t, lock); !pyEq(recGet(r, "pid"), int64(os.Getpid())) {
		t.Fatal("record not ours")
	}
}

func TestUnusableGatewayLockHeldOrLinkedIsNeverUnlinked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("permission semantics need an unprivileged user")
	}
	tmp := t.TempDir()
	homes := map[string]string{}
	for _, kind := range []string{"held", "held_readable", "hardlink", "symlink"} {
		homes[kind] = filepath.Join(tmp, kind)
		os.Mkdir(homes[kind], 0o700)
	}
	for _, kind := range []string{"held", "held_readable"} {
		p := filepath.Join(homes[kind], "gateway.lock")
		os.WriteFile(p, nil, 0o644)
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
	}
	os.Chmod(filepath.Join(homes["held"], "gateway.lock"), 0)
	os.Chmod(filepath.Join(homes["held_readable"], "gateway.lock"), 0o400)
	os.WriteFile(filepath.Join(tmp, "other"), nil, 0o644)
	os.Link(filepath.Join(tmp, "other"), filepath.Join(homes["hardlink"], "gateway.lock"))
	os.Chmod(filepath.Join(homes["hardlink"], "gateway.lock"), 0)
	os.Symlink(filepath.Join(tmp, "target"), filepath.Join(homes["symlink"], "gateway.lock"))
	for kind, h := range homes {
		before, _ := os.Lstat(filepath.Join(h, "gateway.lock"))
		err := NewProfileOwnership().Reserve([]string{h}, "")
		if err == nil || !isOSError(err) {
			t.Fatalf("%s reserve error %v", kind, err)
		}
		after, _ := os.Lstat(filepath.Join(h, "gateway.lock"))
		if !os.SameFile(before, after) {
			t.Fatalf("%s inode replaced", kind)
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "target")); err == nil {
		t.Fatal("symlink target created")
	}
}

func TestExclusiveMaintenanceNestsAndRefusesARuntimeOwner(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	ctx, release, err := ExclusiveMaintenance(context.Background(), []string{h}, "browse_schema_reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if MaintenanceHolder(h, "browse_schema_reconcile") == nil || LockRecordUnpublished(h) == false {
		t.Fatal("maintenance record")
	}
	if pid, live, _ := LiveRuntimeOwner(h); live {
		t.Fatalf("maintenance read as owner %d", pid)
	}
	_, inner, err := ExclusiveMaintenance(ctx, []string{h}, "")
	if err != nil {
		t.Fatal("nested maintenance refused")
	}
	inner()
	if _, _, err := ExclusiveMaintenance(context.Background(), []string{h}, ""); !ownershipConflict(err) {
		t.Fatalf("second outermost %v", err)
	}
	release()
	_, again, err := ExclusiveMaintenance(context.Background(), []string{h}, "")
	if err != nil {
		t.Fatal(err)
	}
	again()
}
