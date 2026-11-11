package selfupdate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const dblChildEntry = "desktop-build-lock-child"

// Child processes of the lock cases, dispatched before TestMain
func init() {
	if len(os.Args) > 2 && os.Args[1] == dblChildEntry {
		os.Exit(dblChild(os.Args[2], os.Args[3:]))
	}
}

func dblChild(mode string, args []string) int {
	ctx := context.Background()
	switch mode {
	case "desktop":
		ok, err := NewDesktopBuildLock(args[0]).Acquire(ctx, false)
		if err != nil || !ok {
			return 23
		}
		return 0
	case "update":
		ok, err := updaterecord.NewUpdateLock(args[1], args[0], true).Acquire(ctx)
		if err != nil || !ok {
			return 23
		}
		return 0
	case "update-holder":
		lock := updaterecord.NewUpdateLock(args[1], args[0], true)
		if ok, err := lock.Acquire(ctx); err != nil || !ok {
			return 1
		}
		fmt.Println("held")
		_, _ = io.ReadAll(os.Stdin)
		lock.Release(ctx)
		return 0
	case "sleep-holder":
		fmt.Println("ready")
		time.Sleep(300 * time.Second)
		return 0
	case "checkout":
		ok, err := updaterecord.NewUpdateLock("", args[0], false).AcquireCheckout(args[0])
		if err != nil || !ok {
			return 23
		}
		return 0
	}
	return 2
}

func dblRunChild(t *testing.T, mode string, args ...string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{dblChildEntry, mode}, args...)...)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child timed out")
	}
	return -1
}

func dblCheckout(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	root := filepath.Join(tmp, "icebath-agent")
	if err := os.MkdirAll(filepath.Join(root, "apps", "desktop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "apps", "desktop", "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func dblAcquire(t *testing.T, l *DesktopBuildLock, wait bool) bool {
	t.Helper()
	l.Out = io.Discard
	ok, err := l.Acquire(context.Background(), wait)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func dblLockFree(t *testing.T, root string) bool {
	path := NewDesktopBuildLock(root).Path
	if _, err := os.Stat(path); err != nil {
		return true
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	free := gateway.TryAcquireFileLock(f)
	if free {
		gateway.ReleaseFileLock(f)
	}
	return free
}

func TestDesktopBuildLockIsExclusiveAndReacquirable(t *testing.T) {
	root := dblCheckout(t)
	ctx := context.Background()
	first, contender := NewDesktopBuildLock(root), NewDesktopBuildLock(root)
	if !dblAcquire(t, first, false) {
		t.Fatal("first acquire refused")
	}
	if dblAcquire(t, contender, false) {
		t.Fatal("contender acquired a held lock")
	}
	first.Release(ctx)
	if !dblAcquire(t, contender, false) {
		t.Fatal("contender refused after release")
	}
	contender.Release(ctx)
}

func TestDesktopBuildLockExcludesAnotherProcess(t *testing.T) {
	root := dblCheckout(t)
	holder := NewDesktopBuildLock(root)
	if !dblAcquire(t, holder, false) {
		t.Fatal("holder refused")
	}
	code := dblRunChild(t, "desktop", root)
	holder.Release(context.Background())
	if code != 23 {
		t.Fatalf("child exit %d", code)
	}
}

func TestDesktopBuildLockReleasesAfterException(t *testing.T) {
	root := dblCheckout(t)
	ctx := context.Background()
	err := func() (err error) {
		l := NewDesktopBuildLock(root)
		l.Out = io.Discard
		if err := l.Enter(ctx); err != nil {
			return err
		}
		defer l.Release(ctx)
		return errors.New("build failed")
	}()
	if err == nil || err.Error() != "build failed" {
		t.Fatalf("err %v", err)
	}
	successor := NewDesktopBuildLock(root)
	if !dblAcquire(t, successor, false) {
		t.Fatal("successor refused")
	}
	successor.Release(ctx)
}

func TestDesktopBuildLockIsKeyedByCheckout(t *testing.T) {
	root := dblCheckout(t)
	other := filepath.Join(filepath.Dir(root), "icebath-agent-2")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	first := NewDesktopBuildLock(root)
	if !dblAcquire(t, first, false) {
		t.Fatal("first refused")
	}
	code := dblRunChild(t, "desktop", other)
	first.Release(context.Background())
	if code != 0 {
		t.Fatalf("child exit %d", code)
	}
	if first.Path == NewDesktopBuildLock(other).Path {
		t.Fatal("two checkouts share one lock")
	}
}

func TestAHeldDesktopBuildLockKeepsAnUpdaterOffTheCheckout(t *testing.T) {
	root := dblCheckout(t)
	build := NewDesktopBuildLock(root)
	if !dblAcquire(t, build, false) {
		t.Fatal("build refused")
	}
	code := dblRunChild(t, "update", root, filepath.Join(filepath.Dir(root), "marker"))
	build.Release(context.Background())
	if code != 23 {
		t.Fatalf("an updater took the checkout while a desktop build held it (exit %d)", code)
	}
}

func TestARunningUpdateRefusesADirectDesktopBuild(t *testing.T) {
	root := dblCheckout(t)
	holder := exec.Command(os.Args[0], dblChildEntry, "update-holder", root, filepath.Join(filepath.Dir(root), "marker"))
	stdin, _ := holder.StdinPipe()
	stdout, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	line := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- text
	}()
	select {
	case text := <-line:
		if strings.TrimSpace(text) != "held" {
			t.Fatalf("holder printed %q", text)
		}
	case <-time.After(60 * time.Second):
		_ = holder.Process.Kill()
		t.Fatal("child printed nothing within 60s")
	}
	build := NewDesktopBuildLock(root)
	refused := !dblAcquire(t, build, false)
	free := dblLockFree(t, root)
	stdin.Close()
	_ = holder.Wait()
	if !refused {
		t.Fatal("a desktop build started while an updater held the checkout")
	}
	if !free {
		t.Fatal("a refused build kept the desktop build lock")
	}
	after := NewDesktopBuildLock(root)
	if !dblAcquire(t, after, false) {
		t.Fatal("the build stayed refused after the update released")
	}
	after.Release(context.Background())
}

func TestTheUpdatesOwnDesktopBuildJoinsItsCheckoutLock(t *testing.T) {
	root := dblCheckout(t)
	ctx := context.Background()
	update := updaterecord.NewUpdateLock(filepath.Join(filepath.Dir(root), "marker"), root, true)
	if ok, err := update.Acquire(ctx); err != nil || !ok {
		t.Fatalf("update acquire %v %v", ok, err)
	}
	build := NewDesktopBuildLock(root)
	if !dblAcquire(t, build, true) {
		update.Release(ctx)
		t.Fatal("the update's build waited on itself")
	}
	build.Release(ctx)
	_, held := updaterecord.HeldCheckoutPath()
	stillHeld, err := updaterecord.CheckoutLockHeld(ctx, root)
	update.Release(ctx)
	if !held || err != nil || !stillHeld {
		t.Fatalf("the build released the update's hold (held %v, checkout %v, %v)", held, stillHeld, err)
	}
	if _, held := updaterecord.HeldCheckoutPath(); held {
		t.Fatal("the update release left a hold")
	}
}

func TestAHeldCheckoutLockAdmitsABuildOnlyForItsOwnCheckout(t *testing.T) {
	cases := []string{"another-checkout"}
	if runtime.GOOS != "windows" {
		cases = append(cases, "symlinked-alias")
	}
	for _, buildIn := range cases {
		t.Run(buildIn, func(t *testing.T) {
			ctx := context.Background()
			tmp := t.TempDir()
			t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
			a, b := filepath.Join(tmp, "checkout-a"), filepath.Join(tmp, "checkout-b")
			for _, root := range []string{a, b} {
				if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
			}
			outer := updaterecord.NewUpdateLock("", a, false)
			if ok, err := outer.AcquireCheckout(a); err != nil || !ok {
				t.Fatalf("outer %v %v", ok, err)
			}
			var contended string
			var build *DesktopBuildLock
			if buildIn == "symlinked-alias" {
				alias := filepath.Join(tmp, "alias")
				if err := os.Symlink(a, alias); err != nil {
					t.Fatal(err)
				}
				build = NewDesktopBuildLock(alias)
				if !dblAcquire(t, build, false) {
					t.Fatal("the alias build was refused")
				}
				contended = a
			} else {
				build = NewDesktopBuildLock(b)
				build.Out = io.Discard
				_, err := build.Acquire(ctx, false)
				if err == nil || !strings.Contains(err.Error(), "another checkout") || exceptionName(err) != "OSError" {
					t.Fatalf("err %v", err)
				}
				if !dblLockFree(t, b) {
					t.Fatal("a refused build kept the desktop build lock")
				}
				contended = b
			}
			code := dblRunChild(t, "checkout", contended)
			build.Release(ctx)
			held, err := updaterecord.CheckoutLockHeld(ctx, a)
			outer.Release(ctx)
			if err != nil || !held {
				t.Fatal("the build released the update's own hold")
			}
			want := 0
			if buildIn == "symlinked-alias" {
				want = 23
			}
			if code != want {
				t.Fatalf("updater exit %d want %d", code, want)
			}
		})
	}
}
