package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Points the home at a temp dir and resets the per engagement log flags, the hermes fixture
func estopHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	estopLogMu.Lock()
	clear(estopLoggedComponents)
	estopLogMu.Unlock()
	return dir
}

// Whether a path exists on disk
func estopExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestEngageCreatesSentinelAndIsEngaged(t *testing.T) {
	ctx := context.Background()
	dir := estopHome(t)
	if IsEngaged(ctx) {
		t.Fatal("engaged before any pause")
	}
	Engage(ctx, "")
	if !estopExists(filepath.Join(dir, "ESTOP")) || !IsEngaged(ctx) {
		t.Fatal("engage left no sentinel")
	}
}

func TestDisengageRemovesSentinel(t *testing.T) {
	ctx := context.Background()
	dir := estopHome(t)
	Engage(ctx, "")
	if !Disengage(ctx) {
		t.Fatal("disengage reported nothing lifted")
	}
	if estopExists(filepath.Join(dir, "ESTOP")) || IsEngaged(ctx) {
		t.Fatal("sentinel survived disengage")
	}
	if Disengage(ctx) {
		t.Fatal("a second disengage reported a lift")
	}
}

func TestReasonAndTimestampStored(t *testing.T) {
	ctx := context.Background()
	dir := estopHome(t)
	Engage(ctx, "runaway cron fan-out")
	state := GetState(ctx)
	if state == nil || state.Reason != "runaway cron fan-out" || state.EngagedAt == "" {
		t.Fatalf("state %+v", state)
	}
	data, err := os.ReadFile(filepath.Join(dir, "ESTOP"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil || raw["reason"] != "runaway cron fan-out" {
		t.Fatalf("sentinel body %s %v", data, err)
	}
}

func TestCorruptSentinelStillEngages(t *testing.T) {
	ctx := context.Background()
	dir := estopHome(t)
	if err := os.WriteFile(filepath.Join(dir, "ESTOP"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsEngaged(ctx) {
		t.Fatal("a corrupt sentinel did not pause")
	}
	state := GetState(ctx)
	if state == nil || state.Reason != "" {
		t.Fatalf("state %+v", state)
	}
}

func TestPausedReplySurfacesReasonAndResumeHint(t *testing.T) {
	ctx := context.Background()
	estopHome(t)
	Engage(ctx, "deploy window")
	notice := PausedReply(ctx)
	if notice == "" || !strings.Contains(strings.ToLower(notice), "paused") || !strings.Contains(notice, "deploy window") {
		t.Fatalf("notice %q", notice)
	}
}

func TestIsEngagedFailsSafeOnStatError(t *testing.T) {
	ctx := context.Background()
	estopHome(t)
	saved := estopStat
	estopStat = func(string) (os.FileInfo, error) { return nil, errors.New("permission denied") }
	t.Cleanup(func() { estopStat = saved })
	if !IsEngaged(ctx) {
		t.Fatal("a stat failure reported not engaged")
	}
}

func TestProfileGatewayHonorsCanonicalRootEstop(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "hermes-root")
	profile := filepath.Join(root, "profiles", "fleet-analyst")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profile)
	estopLogMu.Lock()
	clear(estopLoggedComponents)
	estopLogMu.Unlock()
	if IsEngaged(ctx) {
		t.Fatal("engaged before any pause")
	}
	if err := os.WriteFile(filepath.Join(root, "ESTOP"), []byte("{\"reason\": \"thundering herd\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsEngaged(ctx) {
		t.Fatal("the fleet root sentinel was ignored")
	}
	if reply := PausedReply(ctx); reply == "" || !strings.Contains(strings.ToLower(reply), "paused") {
		t.Fatalf("reply %q", reply)
	}
	Engage(ctx, "local")
	if !estopExists(filepath.Join(profile, "ESTOP")) || !IsEngaged(ctx) {
		t.Fatal("profile local engage failed")
	}
	if err := os.Remove(filepath.Join(root, "ESTOP")); err != nil {
		t.Fatal(err)
	}
	if !IsEngaged(ctx) {
		t.Fatal("the profile sentinel alone did not hold the pause")
	}
	Disengage(ctx)
	if IsEngaged(ctx) {
		t.Fatal("engaged after disengage")
	}
}
