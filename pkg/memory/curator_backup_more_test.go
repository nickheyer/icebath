package memory

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func curTarNames(t *testing.T, snap string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(snap, "skills.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
}

func curHas(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func curRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func curWrite(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func curSnap(t *testing.T, ctx context.Context, reason string) string {
	t.Helper()
	snap, ok, err := SnapshotSkills(ctx, reason, nil)
	if !ok || err != nil {
		t.Fatalf("snapshot %s: %v", reason, err)
	}
	return snap
}

func curRollback(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	if ok, msg, _ := RollbackCuratorSnapshot(ctx, id); !ok {
		t.Fatalf("rollback failed: %s", msg)
	}
}

func TestCuratorRealRunTakesPreSnapshot(t *testing.T) {
	ctx, dir := curBackupHome(t, nil)
	curWriteSkill(t, dir, "alpha", "body")
	on := true
	if _, err := RunCuratorReview(ctx, CuratorRunOptions{Synchronous: true, Consolidate: &on}); err != nil {
		t.Fatal(err)
	}
	rows, err := ListCuratorBackups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []any
	for _, r := range rows {
		reasons = append(reasons, curGet(r, "reason"))
	}
	for _, r := range reasons {
		if r == "pre-curator-run" {
			return
		}
	}
	t.Fatalf("expected a pre-curator-run snapshot, got %v", reasons)
}

func TestCuratorRollbackRecoversCleanlyFromPartialExtract(t *testing.T) {
	ctx, dir := curBackupHome(t, nil)
	curWriteSkill(t, dir, "alpha", "snapshot copy")
	curSnap(t, ctx, "before")
	curWriteSkill(t, dir, "alpha", "current copy")
	curWriteSkill(t, dir, "beta", "current only")
	prev := curatorExtractArchive
	curatorExtractArchive = func(archive, dest string) error {
		curWrite(t, filepath.Join(dest, "alpha", "SKILL.md"), "half written")
		if err := os.MkdirAll(filepath.Join(dest, "gamma"), 0o755); err != nil {
			t.Fatal(err)
		}
		return &os.PathError{Op: "write", Path: dest, Err: syscall.ENOSPC}
	}
	t.Cleanup(func() { curatorExtractArchive = prev })
	if ok, _, _ := RollbackCuratorSnapshot(ctx, ""); ok {
		t.Fatal("rollback reported success over a failed extract")
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha", "alpha")); err == nil {
		t.Fatal("staged skill was nested, not restored")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var present []string
	for _, e := range entries {
		if !curatorExcludeTopLevel[e.Name()] {
			present = append(present, e.Name())
		}
	}
	sort.Strings(present)
	if strings.Join(present, ",") != "alpha,beta" {
		t.Fatalf("tree not restored: %v", present)
	}
	if !strings.Contains(curRead(t, filepath.Join(dir, "alpha", "SKILL.md")), "current copy") || !strings.Contains(curRead(t, filepath.Join(dir, "beta", "SKILL.md")), "current only") {
		t.Fatal("live content not restored")
	}
}

func TestCuratorSnapshotExcludesGitCuratorBackupsAndHub(t *testing.T) {
	ctx, dir := curBackupHome(t, nil)
	curWrite(t, filepath.Join(dir, ".git", "config"), "[core]\nrepositoryformatversion = 0\n")
	curWrite(t, filepath.Join(dir, ".hub", "lock.json"), "{}")
	curWriteSkill(t, dir, "alpha", "alpha body")
	curWrite(t, filepath.Join(dir, "alpha", ".git", "HEAD"), "ref: refs/heads/main\n")
	names := curTarNames(t, curSnap(t, ctx, "test-exclude-git"))
	for _, n := range names {
		for _, part := range strings.Split(n, "/") {
			if part == ".git" || part == ".curator_backups" || part == ".hub" {
				t.Fatalf("%s found in archive", n)
			}
		}
	}
	if !curHas(names, "alpha/SKILL.md") {
		t.Fatalf("members %v", names)
	}
}

func TestCuratorSnapshotAndRollbackLeaveLedgerAndArchiveAlone(t *testing.T) {
	ctx, dir := curBackupHome(t, nil)
	curWriteSkill(t, dir, "alpha", "v1")
	curWrite(t, filepath.Join(dir, ".curator_ledger.jsonl"), "{\"id\": \"old\"}\n")
	curWriteSkill(t, filepath.Join(dir, ".archive"), "pruned", "archived body")
	snap := curSnap(t, ctx, "snap-v1")
	names := curTarNames(t, snap)
	for _, n := range names {
		if top := strings.Split(n, "/")[0]; top == ".archive" || top == ".curator_ledger.jsonl" {
			t.Fatalf("members %v", names)
		}
	}
	if !curHas(names, "alpha/SKILL.md") {
		t.Fatalf("members %v", names)
	}
	curWrite(t, filepath.Join(dir, ".curator_ledger.jsonl"), "{\"id\": \"old\"}\n{\"id\": \"new\"}\n")
	curWriteSkill(t, filepath.Join(dir, ".archive"), "pruned-later", "archived later")
	curRollback(t, ctx, filepath.Base(snap))
	if strings.Count(curRead(t, filepath.Join(dir, ".curator_ledger.jsonl")), "\n") != 2 {
		t.Fatal("ledger rolled back")
	}
	if _, err := os.Stat(filepath.Join(dir, ".archive", "pruned-later", "SKILL.md")); err != nil {
		t.Fatal("archive rolled back")
	}
}

func TestCuratorSnapshotSkipsNestedVenvAndRollbackCarriesItBack(t *testing.T) {
	ctx, dir := curBackupHome(t, nil)
	curWriteSkill(t, dir, "alpha", "v1")
	curWrite(t, filepath.Join(dir, "alpha", "venv", "lib", "big.so"), strings.Repeat("x", 4096))
	curWrite(t, filepath.Join(dir, "alpha", "scripts", "venv"), "#!/bin/sh\n")
	snap := curSnap(t, ctx, "snap-v1")
	names := curTarNames(t, snap)
	if !curHas(names, "alpha/scripts/venv") {
		t.Fatalf("members %v", names)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "alpha/venv") {
			t.Fatalf("venv archived: %v", names)
		}
	}
	curWriteSkill(t, dir, "alpha", "v2")
	curRollback(t, ctx, filepath.Base(snap))
	if !strings.Contains(curRead(t, filepath.Join(dir, "alpha", "SKILL.md")), "v1") {
		t.Fatal("skill not rolled back")
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha", "venv", "lib", "big.so")); err != nil {
		t.Fatal("live venv not carried back after rollback")
	}
}

func TestCuratorRollbackPreservesTopLevelGit(t *testing.T) {
	ctx, dir := curBackupHome(t, nil)
	head := filepath.Join(dir, ".git", "HEAD")
	curWrite(t, head, "ref: refs/heads/main\n")
	curWriteSkill(t, dir, "alpha", "v1")
	snap := curSnap(t, ctx, "snap-v1")
	curWriteSkill(t, dir, "alpha", "v2")
	curWriteSkill(t, dir, "beta", "new")
	curWrite(t, head, "ref: refs/heads/feature\n")
	curRollback(t, ctx, filepath.Base(snap))
	if !strings.Contains(curRead(t, filepath.Join(dir, "alpha", "SKILL.md")), "v1") {
		t.Fatal("skill not rolled back")
	}
	if _, err := os.Stat(filepath.Join(dir, "beta")); !os.IsNotExist(err) {
		t.Fatal("new skill survived the rollback")
	}
	if curRead(t, head) != "ref: refs/heads/feature\n" {
		t.Fatal("top level .git rolled back")
	}
}

func TestCuratorRollbackPreservesNestedGitInsideSkill(t *testing.T) {
	ctx, dir := curBackupHome(t, nil)
	curWriteSkill(t, dir, "alpha", "v1")
	head := filepath.Join(dir, "alpha", ".git", "HEAD")
	curWrite(t, head, "ref: refs/heads/main\n")
	snap := curSnap(t, ctx, "snap-v1")
	curWriteSkill(t, dir, "alpha", "v2")
	curWrite(t, head, "ref: refs/heads/feature\n")
	curRollback(t, ctx, filepath.Base(snap))
	if !strings.Contains(curRead(t, filepath.Join(dir, "alpha", "SKILL.md")), "v1") || curRead(t, head) != "ref: refs/heads/feature\n" {
		t.Fatal("nested .git not preserved across rollback")
	}
	staging, _ := filepath.Glob(filepath.Join(dir, ".curator_backups", ".rollback-*"))
	if len(staging) != 0 {
		t.Fatalf("staging dir left behind: %v", staging)
	}
}

func TestCuratorRollbackPreservesNestedGitFilePointer(t *testing.T) {
	ctx, dir := curBackupHome(t, nil)
	curWriteSkill(t, dir, "alpha", "v1")
	ptr := filepath.Join(dir, "alpha", ".git")
	curWrite(t, ptr, "gitdir: ../../.git/modules/alpha\n")
	snap := curSnap(t, ctx, "snap-v1")
	if curHas(curTarNames(t, snap), "alpha/.git") {
		t.Fatal(".git pointer archived")
	}
	curWriteSkill(t, dir, "alpha", "v2")
	curRollback(t, ctx, filepath.Base(snap))
	if !strings.Contains(curRead(t, filepath.Join(dir, "alpha", "SKILL.md")), "v1") || !strings.HasPrefix(curRead(t, ptr), "gitdir:") {
		t.Fatal(".git file pointer not preserved")
	}
}

func TestCuratorSnapshotCronJobsUTF8BOMCountedAndBackupBOMless(t *testing.T) {
	ctx, dir := curBackupHome(t, nil)
	curWriteSkill(t, dir, "alpha", "body")
	payload := `{"jobs": [{"id": "job-a"}, {"id": "job-b"}]}`
	curWrite(t, filepath.Join(filepath.Dir(dir), "cron", "jobs.json"), "\ufeff"+payload)
	snap := curSnap(t, ctx, "test")
	var mf map[string]any
	if err := json.Unmarshal([]byte(curRead(t, filepath.Join(snap, "manifest.json"))), &mf); err != nil {
		t.Fatal(err)
	}
	cj := mf["cron_jobs"].(map[string]any)
	if _, warned := cj["parse_warning"]; cj["backed_up"] != true || cj["jobs_count"] != float64(2) || warned {
		t.Fatalf("manifest cron_jobs %v", cj)
	}
	backup := curRead(t, filepath.Join(snap, curatorCronJobsFilename))
	var got, want any
	if strings.HasPrefix(backup, "\ufeff") || json.Unmarshal([]byte(backup), &got) != nil || json.Unmarshal([]byte(payload), &want) != nil {
		t.Fatalf("backup %q", backup)
	}
	if g, _ := json.Marshal(got); string(g) != `{"jobs":[{"id":"job-a"},{"id":"job-b"}]}` {
		t.Fatalf("backup content %s", g)
	}
}
