package tools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Safe restore across unusual names keeps every exact path and the one user edit
func cpAssertSafeRestore(t *testing.T, names []string) {
	t.Helper()
	f := newCPFixture(t)
	work := filepath.Join(f.tmp, "rtproject")
	f.write(filepath.Join(work, "AGENTS.md"), "Project instructions\n")
	for _, name := range names {
		f.write(filepath.Join(work, name), "original\n")
	}
	preserved := filepath.Join(work, " 用户修改.txt")
	f.write(preserved, "original\n")
	m := f.mgr(true, 20, 500, 10)
	if !m.EnsureCheckpoint(work, "auto") {
		t.Fatal("not taken")
	}
	target := f.newest(m, work)
	for _, name := range names {
		f.write(filepath.Join(work, name), "agent edit\n")
		m.RecordAgentWrite(filepath.Join(work, name))
	}
	f.write(preserved, "agent edit\n")
	m.RecordAgentWrite(preserved)
	f.write(preserved, "user edit\n")
	r := m.Restore(work, target, "", true)
	if !cpSuccess(r) {
		t.Fatalf("restore failed: %v", r)
	}
	got := append([]string{}, cpList(r, "restored_files")...)
	want := append([]string{}, names...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("restored set wrong: %q want %q", got, want)
	}
	if skipped := cpList(r, "skipped_user_edits"); len(skipped) != 1 || skipped[0] != filepath.Base(preserved) {
		t.Fatalf("skipped wrong: %q", skipped)
	}
	for _, name := range names {
		if f.read(filepath.Join(work, name)) != "original\n" {
			t.Fatalf("%q not restored", name)
		}
	}
	if f.read(preserved) != "user edit\n" {
		t.Fatal("user edit lost")
	}
}

func TestCheckpointSafeRestorePreservesExactPathsAndUserEdits(t *testing.T) {
	cpAssertSafeRestore(t, []string{" leading.txt", "报告.txt", "ordinary.txt"})
}

func TestCheckpointSafeRestorePreservesPosixBytePaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("raw byte names are a Linux shape")
	}
	cpAssertSafeRestore(t, []string{" raw-\xff.txt", "carriage\rreturn.txt", "crlf\r\nname.txt"})
}

// The per file cap leaves an oversized unusual name out of the snapshot and keeps the small file
func cpAssertSizeCap(t *testing.T, name string) {
	t.Helper()
	f := newCPFixture(t)
	work := filepath.Join(f.tmp, "capproject")
	f.write(filepath.Join(work, "small.txt"), "keep\n")
	f.write(filepath.Join(work, name), strings.Repeat("x", 1024*1024+1))
	m := f.mgr(true, 20, 500, 1)
	if !m.EnsureCheckpoint(work, "auto") {
		t.Fatal("not taken")
	}
	target := f.newest(m, work)
	included, _, _ := runCheckpointGit(f.env, []string{"cat-file", "-e", target + ":small.txt"}, f.store(), work, checkpointGitOpts{})
	oversized, _, _ := runCheckpointGit(f.env, []string{"cat-file", "-e", target + ":" + name}, f.store(), work, checkpointGitOpts{Allowed: map[int]bool{128: true}})
	if !included || oversized {
		t.Fatalf("size cap wrong: small=%v oversized=%v", included, oversized)
	}
}

func TestCheckpointSizeCapAppliesToLeadingSpacePaths(t *testing.T) {
	cpAssertSizeCap(t, " oversized.bin")
}

func TestCheckpointSizeCapAppliesToPosixBytePaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("raw byte names are a Linux shape")
	}
	cpAssertSizeCap(t, " oversized-\xff.bin")
}

func TestCheckpointFollowsSelectedGitEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper is POSIX only")
	}
	f := newCPFixture(t)
	git, err := whichGit()
	if err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(f.tmp, "selected-tools")
	observed := filepath.Join(f.tmp, "git-environment")
	f.write(filepath.Join(selected, "git"), "#!/bin/sh\n"+
		"test \"$ICEBATH_GIT_HELPER\" = \"selected\" || exit 77\n"+
		"printf \"%s\\n\" selected >> "+shQuote(observed)+"\n"+
		"exec "+shQuote(git)+" \"$@\"\n")
	os.Chmod(filepath.Join(selected, "git"), 0o755)
	repo := filepath.Join(f.tmp, "repo")
	f.mkdir(repo)
	cpGit(t, repo, "init", "-q")
	f.write(filepath.Join(repo, "new.txt"), "new content\n")
	cpSwap(t, &checkpointPMEnsure, func(name string, base map[string]string) (map[string]string, error) {
		if name != "git" {
			t.Fatalf("selected %q", name)
		}
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		out["PATH"] = selected
		out["ICEBATH_GIT_HELPER"] = "selected"
		return out, nil
	})
	t.Setenv("PATH", filepath.Join(f.tmp, "missing-path"))
	m := f.mgr(true, 20, 500, 10)
	if !m.EnsureCheckpoint(repo, "auto") {
		t.Fatalf("checkpoint through the selected git failed\n%s", f.logs.String())
	}
	if strings.TrimSpace(f.read(observed)) == "" {
		t.Fatal("selected git never ran")
	}
	checkpointPMEnsure = func(name string, base map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		out["PATH"] = filepath.Join(f.tmp, "missing-path")
		return out, nil
	}
	m.NewTurn()
	if m.EnsureCheckpoint(repo, "auto") {
		t.Fatal("a changed selection did not affect this process")
	}
}

func TestCheckpointFallsBackToSystemGitWhenSelectionFails(t *testing.T) {
	for _, failure := range []string{"unsupported-target", "acquisition"} {
		f := newCPFixture(t)
		repo := filepath.Join(f.tmp, "repo")
		f.mkdir(repo)
		cpGit(t, repo, "init", "-q")
		f.write(filepath.Join(repo, "new.txt"), "system git content\n")
		calls := 0
		cpSwap(t, &checkpointPMEnsure, func(string, map[string]string) (map[string]string, error) {
			calls++
			if failure == "unsupported-target" {
				return nil, errors.New("unsupported architecture")
			}
			return nil, errors.New("managed Git unavailable")
		})
		if !f.mgr(true, 20, 500, 10).EnsureCheckpoint(repo, "auto") || calls == 0 {
			t.Fatalf("%s did not fall back to system git", failure)
		}
	}
}

// Path of the system git binary
func whichGit() (string, error) {
	p := whichInPath("git", os.Getenv("PATH"))
	if p == "" {
		return "", errors.New("git not on PATH")
	}
	return p, nil
}

func TestCheckpointListAllAggregatesProjectsAndLabelsWorkdir(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	a, b := filepath.Join(f.tmp, "proj-a"), filepath.Join(f.tmp, "proj-b")
	f.write(filepath.Join(a, "x"), "a")
	f.write(filepath.Join(b, "y"), "b")
	m.EnsureCheckpoint(a, "before write_file")
	m.NewTurn()
	m.EnsureCheckpoint(b, "before patch")
	all, err := m.ListAllCheckpoints()
	if err != nil || len(all) != 2 {
		t.Fatalf("aggregate wrong: %v %v", all, err)
	}
	seen := map[string]string{}
	for _, e := range all {
		seen[e.Workdir] = e.Reason
	}
	if seen[a] != "before write_file" || seen[b] != "before patch" {
		t.Fatalf("labels wrong: %v", seen)
	}
	if all[0].Timestamp < all[1].Timestamp {
		t.Fatal("not newest first")
	}
}

func TestCheckpointListAllEmptyStoreReturnsEmpty(t *testing.T) {
	f := newCPFixture(t)
	all, err := f.enabled().ListAllCheckpoints()
	if err != nil || len(all) != 0 {
		t.Fatalf("missing store not empty: %v %v", all, err)
	}
}

func TestCheckpointFormatWorkdirLabelShownInAllDirectoriesView(t *testing.T) {
	cps := []CheckpointEntry{{Hash: strings.Repeat("b", 40), ShortHash: "bbbbbbb", Timestamp: "2026-08-19T10:00:00", Reason: "before patch", Workdir: "/tmp/qa-repo"}}
	if out := FormatCheckpointList(cps, "all directories"); !strings.Contains(out, "[qa-repo]") {
		t.Fatalf("label missing: %s", out)
	}
}

func TestCheckpointFormatSingleDirectoryViewUnchanged(t *testing.T) {
	cps := []CheckpointEntry{{Hash: strings.Repeat("c", 40), ShortHash: "ccccccc", Timestamp: "2026-08-19T10:00:00", Reason: "before patch"}}
	out := FormatCheckpointList(cps, "/tmp/qa-repo")
	if strings.Contains(out, "[qa-repo]") || !strings.Contains(out, "ccccccc") {
		t.Fatalf("single view wrong: %s", out)
	}
	if !strings.Contains(out, "  1. ccccccc  2026-08-19 10:00  before patch") {
		t.Fatalf("row format wrong: %s", out)
	}
	if FormatCheckpointList(nil, "/x") != "No checkpoints found for /x" {
		t.Fatal("empty listing wrong")
	}
	stat := FormatCheckpointList([]CheckpointEntry{{ShortHash: "d", Timestamp: "2026-08-19T10:00:00+02:00", Reason: "r", FilesChanged: 2, Insertions: 3, Deletions: 1}}, "/x")
	if !strings.Contains(stat, "2026-08-19 10:00  r  (2 files, +3/-1)") {
		t.Fatalf("stat format wrong: %s", stat)
	}
}

func TestRelativeFileCheckpointUsesTaskWorkspace(t *testing.T) {
	f := newCPFixture(t)
	processCwd := filepath.Join(f.tmp, "opt", "icebath")
	workspace := filepath.Join(f.tmp, "opt", "data", "workspace")
	f.write(filepath.Join(processCwd, "pyproject.toml"), "[project]\nname = 'icebath'\n")
	f.write(filepath.Join(workspace, "pyproject.toml"), "[project]\nname = 'workspace'\n")
	f.write(filepath.Join(workspace, "existing.txt"), "before\n")
	f.env.Cwd = processCwd
	t.Setenv("TERMINAL_CWD", workspace)
	m := f.mgr(true, 20, 500, 10)
	m.EnsureFileCheckpoint("write_file", map[string]any{"path": "test_permissions2.txt"}, "gateway-session")
	if len(m.ListCheckpoints(workspace)) == 0 || len(m.ListCheckpoints(processCwd)) != 0 {
		t.Fatal("checkpoint taken for the wrong directory")
	}
}

func TestContainerBackendTaskLeavesHostStoreUntouched(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	hostFile := filepath.Join(f.tmp, "workspace", "project", "a.txt")
	f.write(hostFile, "host content\n")
	container := "container-checkpoint-task"
	termPool.Lock()
	termPool.active[container] = &fakePathEnv{envType: "docker"}
	termPool.Unlock()
	t.Cleanup(func() {
		termPool.Lock()
		delete(termPool.active, container)
		termPool.Unlock()
	})
	m.EnsureFileCheckpoint("write_file", map[string]any{"path": hostFile}, container)
	m.EnsureTerminalCheckpoint(filepath.Dir(hostFile), "before terminal: rm -rf "+filepath.Dir(hostFile), container)
	m.RecordLandedWrites([]string{hostFile}, container)
	if len(m.ListCheckpoints(filepath.Dir(hostFile))) != 0 || f.exists(f.base) {
		t.Fatal("container task touched the host store")
	}
	if reason := m.UnsupportedBackendReason(container); !strings.Contains(reason, "terminal.backend=docker") {
		t.Fatalf("backend reason wrong: %q", reason)
	}
	m.EnsureFileCheckpoint("write_file", map[string]any{"path": hostFile}, "local-task")
	if len(m.ListCheckpoints(filepath.Dir(hostFile))) != 1 {
		t.Fatal("local task not checkpointed")
	}
	if reason := m.UnsupportedBackendReason("local-task"); reason != "" {
		t.Fatalf("local task refused: %q", reason)
	}
}

func TestCheckpointTerminalCheckpointFallsBackToScopedThenProcessCwd(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	t.Setenv("TERMINAL_CWD", f.work)
	m.EnsureTerminalCheckpoint("", "before terminal: rm -rf build", "default")
	if r := cpReasons(m, f.work); len(r) != 1 || r[0] != "before terminal: rm -rf build" {
		t.Fatalf("scoped cwd not used: %v", r)
	}
	other := filepath.Join(f.tmp, "procdir")
	f.write(filepath.Join(other, "x"), "x")
	t.Setenv("TERMINAL_CWD", "")
	f.env.Cwd = other
	m.NewTurn()
	m.EnsureTerminalCheckpoint("", "before terminal: rm x", "default")
	if r := cpReasons(m, other); len(r) != 1 {
		t.Fatalf("process cwd not used: %v", r)
	}
}

func TestCheckpointOptionsFromConfig(t *testing.T) {
	o, err := CheckpointOptionsFromConfig(map[string]any{"checkpoints": true})
	if err != nil || !o.Enabled || o.MaxSnapshots != 20 || o.MaxTotalSizeMB != 500 || o.MaxFileSizeMB != 10 {
		t.Fatalf("legacy bool wrong: %+v %v", o, err)
	}
	o, _ = CheckpointOptionsFromConfig(map[string]any{"checkpoints": map[string]any{"enabled": false, "max_snapshots": 3, "max_file_size_mb": "4"}})
	if o.Enabled || o.MaxSnapshots != 3 || o.MaxFileSizeMB != 4 || o.MaxTotalSizeMB != 500 {
		t.Fatalf("section wrong: %+v", o)
	}
	if _, err := CheckpointOptionsFromConfig(map[string]any{"checkpoints": map[string]any{"max_snapshots": "many"}}); err == nil {
		t.Fatal("bad integer accepted")
	}
	m := NewCheckpointManager(nil, CheckpointManagerOptions{MaxSnapshots: 0, MaxTotalSizeMB: -5, MaxFileSizeMB: -1})
	if m.MaxSnapshots != 1 || m.MaxTotalSizeMB != 0 || m.MaxFileSizeMB != 0 {
		t.Fatalf("clamps wrong: %+v", m)
	}
}

func TestCheckpointValidators(t *testing.T) {
	for in, want := range map[string]string{
		"":          "Empty commit hash",
		"   ":       "Empty commit hash",
		"-x":        "Invalid commit hash (must not start with '-'): '-x'",
		"abc":       "Invalid commit hash (expected 4-64 hex characters): 'abc'",
		"abcd":      "",
		"ABCDEF012": "",
	} {
		if got := checkpointValidateCommitHash(in); got != want {
			t.Errorf("hash %q: %q want %q", in, got, want)
		}
	}
	work := cpTempDir(t)
	if got := checkpointValidateFilePath(" ", work); got != "Empty file path" {
		t.Errorf("blank path: %q", got)
	}
	if got := checkpointValidateFilePath("a/../../b", work); !strings.Contains(got, "escapes the working directory") {
		t.Errorf("traversal: %q", got)
	}
	if got := checkpointValidateFilePath("a/./b", work); got != "" {
		t.Errorf("inside path: %q", got)
	}
}

func TestCheckpointGitlinkPathsParsing(t *testing.T) {
	z := "100644 blob abc\tnotes.txt\x00160000 commit def\tvendor/tool\x00160000 commit 012\tsp ace\x00"
	got := checkpointGitlinkPaths(z)
	if len(got) != 2 || got[0] != "vendor/tool" || got[1] != "sp ace" {
		t.Fatalf("gitlinks wrong: %q", got)
	}
}

func TestCheckpointLedgerCapKeepsNewestEntries(t *testing.T) {
	store := cpTempDir(t)
	ledger := map[string]any{}
	for i := 0; i < checkpointLedgerMaxItems+5; i++ {
		ledger["/p/"+strconv.Itoa(i)] = map[string]any{"sha256": "s", "ts": float64(i)}
	}
	if err := saveCheckpointLedger(nil, store, "h", ledger); err != nil {
		t.Fatal(err)
	}
	loaded := loadCheckpointLedger(store, "h")
	if len(loaded) != checkpointLedgerMaxItems {
		t.Fatalf("cap wrong: %d", len(loaded))
	}
	for k, v := range loaded {
		ts, _ := checkpointMetaFloat(v.(map[string]any)["ts"])
		if ts < 5 {
			t.Fatalf("old entry %s kept", k)
		}
	}
}
