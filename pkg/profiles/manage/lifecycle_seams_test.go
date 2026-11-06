package manage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestRmtreeFailureRaises(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	dir, err := CreateProfile(ctx, "coder", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := profiles.SetActiveProfile("coder"); err != nil {
		t.Fatal(err)
	}
	oldClean, oldSleep, oldRm := cleanupGatewayService, rmtreeSleep, rmtreeProfile
	cleanupGatewayService = func(context.Context, string, string) bool { return false }
	rmtreeSleep = func(time.Duration) {}
	rmtreeProfile = func(string, func(pyfs.RmTreeOp, string, error) error) error {
		return fmt.Errorf("locked: %w", fs.ErrPermission)
	}
	t.Cleanup(func() { cleanupGatewayService, rmtreeSleep, rmtreeProfile = oldClean, oldSleep, oldRm })
	_, err = DeleteProfile(ctx, "coder", true)
	var rt *RuntimeError
	if !errors.As(err, &rt) || !strings.Contains(err.Error(), "Could not remove profile directory") {
		t.Fatalf("delete: %v", err)
	}
	if !isDir(dir) || profiles.GetActiveProfile("") != "default" {
		t.Fatal("profile dir gone or active profile not reset")
	}
}

// Swaps the multiplexer liveness and notification and restores them at cleanup
func swapMultiplexer(t *testing.T, live bool, notify func(string)) {
	t.Helper()
	oldLive, oldNotify := liveDefaultMultiplexer, notifyMultiplexer
	liveDefaultMultiplexer = func() bool { return live }
	notifyMultiplexer = notify
	t.Cleanup(func() { liveDefaultMultiplexer, notifyMultiplexer = oldLive, oldNotify })
}

func TestMultiplexedRenameUnroutesOldThenHotServesNew(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	if _, err := CreateProfile(ctx, "oldname", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(tmp, ".icebath", "profiles", "oldname")
	newDir := filepath.Join(tmp, ".icebath", "profiles", "newname")
	type signal struct {
		name                          string
		oldExists, newExists, oldTomb bool
	}
	var calls []signal
	swapMultiplexer(t, true, func(name string) {
		calls = append(calls, signal{name, pathExists(oldDir), pathExists(newDir), home.ProfileDeleted(oldDir)})
		if name == "oldname" && pathExists(oldDir) {
			var missing *home.ErrProfileMissing
			if _, err := home.Mkdir(filepath.Join(oldDir, "logs")); !errors.As(err, &missing) {
				t.Errorf("mkdir under the unrouted home: %v", err)
			}
		}
	})
	if _, err := RenameProfile(ctx, "oldname", "newname"); err != nil {
		t.Fatal(err)
	}
	if len(calls) < 2 || calls[0] != (signal{"oldname", true, false, true}) || calls[len(calls)-1] != (signal{"newname", false, true, false}) {
		t.Fatalf("signals %+v", calls)
	}
	if pathExists(oldDir) || !isDir(newDir) || home.ProfileDeleted(oldDir) {
		t.Fatal("rename left the old home or its tombstone")
	}
}

func TestUnmultiplexedRenameDoesNotSignalMultiplexer(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	if _, err := CreateProfile(ctx, "oldname", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(tmp, ".icebath", "profiles", "oldname")
	notified := false
	swapMultiplexer(t, false, func(string) { notified = true })
	newDir, err := RenameProfile(ctx, "oldname", "newname")
	if err != nil {
		t.Fatal(err)
	}
	if notified || exists(filepath.Join(tmp, ".icebath", "profiles", ".deleted")) {
		t.Fatal("unmultiplexed rename signalled or tombstoned")
	}
	if pathExists(oldDir) || !isDir(newDir) {
		t.Fatal("rename did not move the directory")
	}
}
