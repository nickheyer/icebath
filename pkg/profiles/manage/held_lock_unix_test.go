//go:build unix

package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Swaps the lifecycle side effects the held lock cases neutralize and restores them at cleanup
func neutralizeLifecycle(t *testing.T) {
	t.Helper()
	oldClean, oldUnreg, oldBackends := cleanupGatewayService, maybeUnregisterGatewayService, stopProfileBackends
	oldNotify, oldStop, oldReg := notifyMultiplexer, stopGatewayProcess, maybeRegisterGatewayService
	oldLive, oldWait := liveDefaultMultiplexer, OwnerReleaseWait
	cleanupGatewayService = func(context.Context, string, string) bool { return false }
	maybeUnregisterGatewayService = func(string) {}
	stopProfileBackends = func(string, string) {}
	notifyMultiplexer = func(string) {}
	stopGatewayProcess = func(string) {}
	maybeRegisterGatewayService = func(string) {}
	t.Cleanup(func() {
		cleanupGatewayService, maybeUnregisterGatewayService, stopProfileBackends = oldClean, oldUnreg, oldBackends
		notifyMultiplexer, stopGatewayProcess, maybeRegisterGatewayService = oldNotify, oldStop, oldReg
		liveDefaultMultiplexer, OwnerReleaseWait = oldLive, oldWait
	})
}

// Reserves the profile's gateway.lock from its own open file description like process_ownership.reserve
func holdGatewayLock(t *testing.T, profileDir string) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(profileDir, "gateway.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestProfileHomeWaitsForTheLiveOwnerToReleaseIt(t *testing.T) {
	cases := []struct {
		id, op   string
		released bool
	}{
		{"delete-owner-releases", "delete", true},
		{"delete-owner-keeps-it", "delete", false},
		{"rename-owner-keeps-it", "rename", false},
		{"recreate-over-owned-shell", "recreate", false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			manageEnv(t)
			captureOut(t)
			neutralizeLifecycle(t)
			ctx := context.Background()
			profile, err := CreateProfile(ctx, "owned-delete", CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			OwnerReleaseWait = 1500 * time.Millisecond
			if c.released {
				OwnerReleaseWait = 30 * time.Second
			}
			rename := c.op == "rename"
			liveDefaultMultiplexer = func() bool { return rename }
			if c.op == "recreate" {
				if err := os.RemoveAll(profile); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(profile, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := home.MarkProfileDeleted(profile); err != nil {
					t.Fatal(err)
				}
			}
			holder := holdGatewayLock(t, profile)
			released := false
			release := func() {
				if !released {
					released = true
					holder.Close()
				}
			}
			defer release()
			done := make(chan error, 1)
			go func() {
				var err error
				switch c.op {
				case "rename":
					_, err = RenameProfile(ctx, "owned-delete", "owned-renamed")
				case "recreate":
					_, err = CreateProfile(ctx, "owned-delete", CreateOptions{})
				default:
					_, err = DeleteProfile(ctx, "owned-delete", true)
				}
				done <- err
			}()
			var outcome error
			finished := false
			select {
			case outcome = <-done:
				finished = true
			case <-time.After(time.Second):
			}
			if st, err := os.Stat(profile); err != nil || !st.IsDir() {
				t.Fatal("the profile was removed while a live gateway still owned it")
			}
			if c.released {
				release()
			}
			if !finished {
				select {
				case outcome = <-done:
				case <-time.After(30 * time.Second):
					t.Fatal("the operation is still running")
				}
			}
			if c.released {
				if outcome != nil || pathExists(profile) {
					t.Fatalf("outcome %v, profile exists %v", outcome, pathExists(profile))
				}
				return
			}
			var rt *RuntimeError
			var exists *profiles.ExistsError
			if outcome == nil || !(errors.As(outcome, &rt) || errors.As(outcome, &exists)) {
				t.Fatalf("outcome %#v", outcome)
			}
			if !strings.Contains(outcome.Error(), "still owns") {
				t.Fatalf("outcome %q", outcome.Error())
			}
			if st, err := os.Stat(filepath.Join(profile, "gateway.lock")); err != nil || !st.Mode().IsRegular() {
				t.Fatalf("gateway.lock gone %v", err)
			}
		})
	}
}
