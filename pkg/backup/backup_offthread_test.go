package backup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestOffThreadCopyReturnsTheCopyError(t *testing.T) {
	want := errors.New("copy failed")
	c := newOffThreadCopy(func() error { return want })
	if c.running() {
		t.Fatal("running before start")
	}
	if err := c.run(context.Background()); err != want {
		t.Fatalf("err %v", err)
	}
	if c.running() {
		t.Fatal("running after the copy returned")
	}
}

func TestOffThreadCopyReraisesTheCopyPanicInTheWaiter(t *testing.T) {
	c := newOffThreadCopy(func() error { panic("boom") })
	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("recovered %v", r)
		}
	}()
	c.run(context.Background())
	t.Fatal("run returned without the panic")
}

func TestOffThreadCopyStopReturnsWhileTheCopyRuns(t *testing.T) {
	release := make(chan struct{})
	c := newOffThreadCopy(func() error { <-release; return nil })
	ctx, cancel := context.WithCancelCause(context.Background())
	stop := errors.New("SIGTERM")
	go func() { time.Sleep(20 * time.Millisecond); cancel(stop) }()
	err := c.run(ctx)
	var interrupted *Interrupted
	if !errors.As(err, &interrupted) || interrupted.Cause != stop {
		t.Fatalf("err %v", err)
	}
	if !c.running() {
		t.Fatal("copy reported finished while still inside the call")
	}
	close(release)
	<-c.done
	if c.running() {
		t.Fatal("copy still reported running after it returned")
	}
}

func TestPerProfileConfigModelRestoreRestoresFromOwnSnapshot(t *testing.T) {
	profiles := profilesFixture(t)
	cfg := filepath.Join(profiles["work"], "config.yaml")
	writeText(t, cfg, userConfig)
	snaps := preUpdateSnaps(t, profiles["default"])
	snaps.Set("ghost", "20990101-000000-pre-update")
	writeText(t, cfg, "_config_version: 39\nmodel:\n  provider: deepseek\n  default: deepseek-chat\n")
	restored := RestoreConfigModelSettingsAllProfiles(context.Background(), snaps, profiles["default"])
	if len(restored) != 1 {
		t.Fatalf("restored %v", restored)
	}
	profile, _ := restored[0].Get("profile")
	ok, _ := restored[0].Get("restored")
	if profile != "work" || ok != true {
		t.Fatal(restored[0])
	}
	if !strings.Contains(readText(t, cfg), "zyphra/zamba-3-large") {
		t.Fatal("work config model settings not restored")
	}
	if again := RestoreConfigModelSettingsAllProfiles(context.Background(), snaps, profiles["default"]); len(again) != 0 {
		t.Fatalf("second pass restored %v", again)
	}
	if none := RestoreConfigModelSettingsAllProfiles(context.Background(), pyjson.NewObject(), ""); none == nil || len(none) != 0 {
		t.Fatalf("empty map restored %v", none)
	}
}
