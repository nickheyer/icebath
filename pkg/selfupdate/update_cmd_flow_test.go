package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Runs git in a fixture repository and returns the finished command without failing the test
func ucRun(t *testing.T, cwd string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return strings.TrimSpace(out.String()), exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out.String()), 0
}

// Keeps the update check offline and admitted, the two fakes the hermes check cases install
func ucOfflineCheck(t *testing.T, root string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ucPatchRoot(t, root)
	savedAdmission, savedBehind := updateAdmission, updateCompareBehind
	t.Cleanup(func() { updateAdmission, updateCompareBehind = savedAdmission, savedBehind })
	updateAdmission = func(context.Context, string) (*UpdateRefusal, error) { return nil, nil }
	updateCompareBehind = func(context.Context, string, string, string) int { return -1 }
}

func TestSourceUpdateChannelExplicitBranchAlwaysWins(t *testing.T) {
	ctx := context.Background()
	if got, err := sourceUpdateChannel(ctx, map[string]any{"branch": "bb/gui", "channel": "stable"}, nil, false); err != nil || got != "main" {
		t.Fatalf("%v %v", got, err)
	}
	canary := "canary"
	if got, err := sourceUpdateChannel(ctx, nil, &canary, true); err != nil || got != "main" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestSourceUpdateChannelTransientChannelFlagWins(t *testing.T) {
	h := chHome(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("update: [broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, channel := range []string{"stable", "main", "canary"} {
		if got, err := sourceUpdateChannel(ctx, map[string]any{"branch": nil, "channel": channel}, nil, false); err != nil || got != channel {
			t.Fatalf("%v %v", got, err)
		}
		if got, err := sourceUpdateChannel(ctx, nil, &channel, false); err != nil || got != channel {
			t.Fatalf("%v %v", got, err)
		}
	}
}

func TestSourceUpdateChannelPerInstallRecordActivates(t *testing.T) {
	h := chHome(t)
	root := filepath.Join(filepath.Dir(h), "install")
	chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": "self"})
	ucPatchRoot(t, root)
	data, _ := json.Marshal(chConfigFor(root, "stable"))
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := sourceUpdateChannel(context.Background(), map[string]any{"branch": nil, "channel": nil}, nil, false); err != nil || got != "stable" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestSourceUpdateChannelNoRecordStaysMain(t *testing.T) {
	h := chHome(t)
	root := filepath.Join(filepath.Dir(h), "install")
	chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": "self"})
	ucPatchRoot(t, root)
	if got, err := sourceUpdateChannel(context.Background(), map[string]any{"branch": nil, "channel": nil}, nil, false); err != nil || got != "main" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestSourceUpdateChannelConfigFailureNeverChangesTheSubscription(t *testing.T) {
	h := chHome(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("update: [broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	ucPatchRoot(t, t.TempDir())
	_, err := sourceUpdateChannel(context.Background(), map[string]any{"branch": nil, "channel": nil}, nil, false)
	if err == nil || !strings.Contains(err.Error(), "formatting error") {
		t.Fatalf("got %v", err)
	}
}

// Origin with a main branch and a tag, and a depth 1 single branch clone pinned to the tag only
func ucNarrowClone(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	ucGit(t, origin, "init", "-q", "-b", "main")
	ucGit(t, origin, "commit", "--allow-empty", "-q", "-m", "c0")
	ucGit(t, origin, "tag", "v9.9.9")
	ucGit(t, origin, "commit", "--allow-empty", "-q", "-m", "c1")
	clone := filepath.Join(tmp, "clone")
	ucGit(t, tmp, "clone", "-q", "--depth", "1", "--single-branch", "--branch", "v9.9.9", "file://"+origin, clone)
	return clone, origin
}

func TestCheckFetchMaterialisesTrackingRefOnNarrowClone(t *testing.T) {
	clone, origin := ucNarrowClone(t)
	if strings.Contains(ucGit(t, clone, "config", "--get-all", "remote.origin.fetch"), "refs/heads/*") {
		t.Fatal("the fixture clone maps branch refs")
	}
	tipSHA := ucGit(t, origin, "rev-parse", "main")
	ucGit(t, clone, "fetch", "-q", "origin", "main")
	if _, code := ucRun(t, clone, "rev-parse", "--verify", "--quiet", "origin/main"); code == 0 {
		t.Fatal("a by-name fetch already materialised origin/main")
	}
	ucOfflineCheck(t, clone)
	var out bytes.Buffer
	if err := cmdUpdateCheck(context.Background(), &out, "main", false, nil); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := ucGit(t, clone, "rev-parse", "origin/main"); got != tipSHA {
		t.Fatalf("origin/main = %s, want %s", got, tipSHA)
	}
	if strings.Contains(out.String(), "not found on origin") || !strings.Contains(out.String(), "Update available") {
		t.Fatalf("output:\n%s", out.String())
	}
	kept := false
	for _, line := range strings.Split(ucGit(t, clone, "config", "--get-all", "remote.origin.fetch"), "\n") {
		kept = kept || strings.HasPrefix(strings.TrimSpace(line), "+refs/tags/v9.9.9")
	}
	if !kept {
		t.Fatal("the check rewrote the remote's fetch config")
	}
}

func TestForcedRefspecUpdatesShallowCloneToNewTip(t *testing.T) {
	tmp := t.TempDir()
	origin, seed, work, adv := filepath.Join(tmp, "origin.git"), filepath.Join(tmp, "seed"), filepath.Join(tmp, "work"), filepath.Join(tmp, "advance")
	ucGit(t, tmp, "init", "-q", "--bare", "-b", "main", origin)
	ucGit(t, tmp, "clone", "-q", "file://"+origin, seed)
	os.WriteFile(filepath.Join(seed, "f.txt"), []byte("a\n"), 0o644)
	ucGit(t, seed, "add", "f.txt")
	ucGit(t, seed, "commit", "-qm", "init")
	ucGit(t, seed, "tag", "v1")
	ucGit(t, seed, "push", "-q", "origin", "HEAD:refs/heads/main", "v1")
	ucGit(t, tmp, "clone", "-q", "--depth", "1", "--single-branch", "--branch", "v1", "file://"+origin, work)
	if got := ucGit(t, work, "config", "--get-all", "remote.origin.fetch"); got != "+refs/tags/v1:refs/tags/v1" {
		t.Fatalf("fixture refspec = %q", got)
	}
	ucGit(t, tmp, "clone", "-q", "file://"+origin, adv)
	ucGit(t, work, "fetch", "--depth", "1", "origin", "refs/heads/main:refs/remotes/origin/main")
	stale := ucGit(t, work, "rev-parse", "--verify", "--quiet", "origin/main")
	if stale == "" {
		t.Fatal("no stale tracking ref was seeded")
	}
	os.WriteFile(filepath.Join(adv, "f.txt"), []byte("b\n"), 0o644)
	ucGit(t, adv, "add", "f.txt")
	ucGit(t, adv, "commit", "-qm", "second")
	ucGit(t, adv, "push", "-q", "origin", "main")
	newTip := ucGit(t, adv, "rev-parse", "HEAD")
	if newTip == "" || newTip == stale {
		t.Fatalf("origin did not advance: %s", newTip)
	}
	var out bytes.Buffer
	fetchResult, compareBranch, err := fetchCompareBranch(context.Background(), &out, []string{"git"}, work, "main", []string{"--depth", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if fetchResult.ReturnCode != 0 {
		t.Fatalf("fetch failed: %s", fetchResult.Stderr)
	}
	if compareBranch != "origin/main" {
		t.Fatalf("compare branch = %q", compareBranch)
	}
	if landed := ucGit(t, work, "rev-parse", "--verify", "--quiet", "origin/main"); landed != newTip {
		t.Fatalf("origin/main = %s, want %s", landed, newTip)
	}
}

// Lines of a shallow checkout's graft file
func ucShallowLines(t *testing.T, repo string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, ".git", "shallow"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestUpdateCheckPrunesAndReportsCount(t *testing.T) {
	tmp := t.TempDir()
	origin, clone := filepath.Join(tmp, "origin"), filepath.Join(tmp, "clone")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	ucGit(t, origin, "init", "-q", "-b", "main")
	for _, message := range []string{"c0", "c1", "c2"} {
		ucGit(t, origin, "commit", "--allow-empty", "-q", "-m", message)
	}
	ucGit(t, tmp, "clone", "-q", "--depth", "1", "file://"+origin, clone)
	for _, message := range []string{"c3", "c4"} {
		ucGit(t, origin, "commit", "--allow-empty", "-q", "-m", message)
		ucGit(t, clone, "fetch", "-q", "--depth", "1", "origin", "main")
	}
	if got := ucShallowLines(t, clone); len(got) != 3 {
		t.Fatalf("fixture grafts = %v", got)
	}
	headSHA := ucGit(t, clone, "rev-parse", "HEAD")
	previousTip := ucGit(t, clone, "rev-parse", "origin/main")
	ucGit(t, origin, "commit", "--allow-empty", "-q", "-m", "c5")
	tipSHA := ucGit(t, origin, "rev-parse", "HEAD")
	ucOfflineCheck(t, clone)
	var out bytes.Buffer
	if err := cmdUpdateCheck(context.Background(), &out, "main", false, nil); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := ucGit(t, clone, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD moved to %s", got)
	}
	if got := ucGit(t, clone, "rev-parse", "origin/main"); got != tipSHA || got == previousTip {
		t.Fatalf("origin/main = %s", got)
	}
	if got := ucGit(t, clone, "rev-parse", "FETCH_HEAD"); got != tipSHA {
		t.Fatalf("FETCH_HEAD = %s", got)
	}
	grafts := ucShallowLines(t, clone)
	slices.Sort(grafts)
	want := []string{headSHA, tipSHA}
	slices.Sort(want)
	if !slices.Equal(slices.Compact(grafts), want) {
		t.Fatalf("grafts = %v, want %v", grafts, want)
	}
	if got := ucGit(t, clone, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("HEAD history = %s", got)
	}
	if got := ucGit(t, clone, "rev-list", "--count", "origin/main"); got != "1" {
		t.Fatalf("origin/main history = %s", got)
	}
	if !strings.Contains(out.String(), "pruned 2 stale shallow graft(s)") || !strings.Contains(out.String(), "Update available (behind origin/main).") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestCalledProcessErrorMessageDepInstallFailureLeadAndNextStep(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := installRepairIsWindows
	installRepairIsWindows = func() bool { return false }
	t.Cleanup(func() { installRepairIsWindows = saved })
	exc := ucCPE([]string{"uv", "pip", "install", "-e", ".[all]"}, 1, "error: disk full", "")
	var out bytes.Buffer
	err := handleUpdateCalledProcessError(context.Background(), &out, exc, map[string]any{}, false, false, "", "", nil)
	var exit *pm.SystemExit
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "disk full") {
		t.Fatalf("output:\n%s", out.String())
	}
}
