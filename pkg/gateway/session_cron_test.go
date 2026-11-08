package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCronExecutionBindingIsScopedToItsContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := CurrentCronExecution(ctx); ok {
		t.Fatal("unbound context reports an execution")
	}
	bound := BindCronExecution(ctx, CronOwnerExecution{DBPath: "/h/state.db", SessionID: "s", JobID: "j", ExecutionID: "e"})
	got, ok := CurrentCronExecution(bound)
	if !ok || got.DBPath != "/h/state.db" || got.SessionID != "s" || got.JobID != "j" || got.ExecutionID != "e" {
		t.Fatalf("%+v %v", got, ok)
	}
	if _, ok := CurrentCronExecution(ctx); ok {
		t.Fatal("binding leaked into the parent context")
	}
}

type fakeCronAuthority struct{ path string }

func (a *fakeCronAuthority) DBPath() string { return a.path }

func TestCronOwnerForHomeBindsResolvedStoreHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	if _, ok := CronOwnerForHome(home); ok {
		t.Fatal("unbound home reports an owner")
	}
	loop, closeLoop := context.WithCancel(context.Background())
	defer closeLoop()
	authority := &fakeCronAuthority{path: filepath.Join(link, "state.db")}
	bindCronOwner(loop, authority)
	defer unbindCronOwner(authority)
	for _, h := range []string{home, link, home + "/."} {
		got, ok := CronOwnerForHome(h)
		if !ok || got != authority {
			t.Fatalf("%s: got %v %v", h, got, ok)
		}
	}
	if _, ok := CronOwnerForHome(root); ok {
		t.Fatal("parent of the store home reports an owner")
	}
}

func TestCronOwnerForHomeIgnoresClosedLoopAndForeignPid(t *testing.T) {
	home := t.TempDir()
	loop, closeLoop := context.WithCancel(context.Background())
	authority := &fakeCronAuthority{path: filepath.Join(home, "state.db")}
	bindCronOwner(loop, authority)
	defer unbindCronOwner(authority)
	closeLoop()
	if _, ok := CronOwnerForHome(home); ok {
		t.Fatal("closed loop still owns cron")
	}
	resolved := resolvedCronHome(home)
	cronOwnersMu.Lock()
	cronOwners[resolved] = boundCronOwner{pid: os.Getpid() + 1, authority: authority, loop: context.Background()}
	cronOwnersMu.Unlock()
	if _, ok := CronOwnerForHome(home); ok {
		t.Fatal("binding of another pid owns cron")
	}
}

func TestUnbindCronOwnerOnlyDropsItsOwnBinding(t *testing.T) {
	home := t.TempDir()
	first := &fakeCronAuthority{path: filepath.Join(home, "state.db")}
	second := &fakeCronAuthority{path: filepath.Join(home, "state.db")}
	bindCronOwner(context.Background(), first)
	bindCronOwner(context.Background(), second)
	defer unbindCronOwner(second)
	unbindCronOwner(first)
	if got, ok := CronOwnerForHome(home); !ok || got != second {
		t.Fatalf("stale unbind dropped the live owner: %v %v", got, ok)
	}
	unbindCronOwner(second)
	if _, ok := CronOwnerForHome(home); ok {
		t.Fatal("unbound owner still reported")
	}
}
