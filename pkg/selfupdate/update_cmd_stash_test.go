package selfupdate

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Replaces the updater's line input for one test
func usPatchInput(t *testing.T, input func(prompt string) (string, error)) {
	t.Helper()
	saved := updateInput
	updateInput = input
	t.Cleanup(func() { updateInput = saved })
}

// A clone one commit behind its origin with a tracked edit, a colliding untracked file and a stray directory
func usBehindClone(t *testing.T) string {
	t.Helper()
	ugEnv(t)
	tmp := t.TempDir()
	up, work := filepath.Join(tmp, "up"), filepath.Join(tmp, "work")
	if err := os.Mkdir(up, 0o755); err != nil {
		t.Fatal(err)
	}
	ugGit(t, up, "init", "-q", "-b", "main")
	ugGit(t, up, "config", "user.email", "t@t")
	ugGit(t, up, "config", "user.name", "t")
	ugWrite(t, filepath.Join(up, "a.txt"), "a\n")
	ugGit(t, up, "add", ".")
	ugGit(t, up, "commit", "-qm", "init")
	ugGit(t, tmp, "clone", "-q", up, work)
	ugWrite(t, filepath.Join(up, "pkg", "new.py"), "upstream\n")
	ugGit(t, up, "add", ".")
	ugGit(t, up, "commit", "-qm", "adds pkg/new.py")
	ugGit(t, work, "fetch", "-q", "origin")
	ugGit(t, work, "config", "status.showUntrackedFiles", "no")
	ugWrite(t, filepath.Join(work, "a.txt"), "a\nmine\n")
	ugWrite(t, filepath.Join(work, "pkg", "new.py"), "my copy\n")
	ugWrite(t, filepath.Join(work, "stray", "big.bin"), strings.Repeat("x", 4096))
	return work
}

var usTargets = []string{"origin/main", "refs/heads/main"}

func TestOnlyCollidingUntrackedFilesAreStashed(t *testing.T) {
	work := usBehindClone(t)

	ref, err := stashLocalChangesIfNeeded(context.Background(), &bytes.Buffer{}, []string{"git"}, work, nil, usTargets, false)
	if err != nil || ref == nil {
		t.Fatalf("ref = %v, err = %v", ref, err)
	}

	stashed := strings.Fields(ugGit(t, work, "stash", "show", "--include-untracked", "--name-only", *ref))
	slices.Sort(stashed)
	if want := []string{"a.txt", "pkg/new.py"}; !slices.Equal(stashed, want) {
		t.Fatalf("stashed = %v, want %v", stashed, want)
	}
	if got := ugRead(t, filepath.Join(work, "stray", "big.bin")); got != strings.Repeat("x", 4096) {
		t.Fatalf("stray file changed: %d bytes", len(got))
	}
	ugGit(t, work, "merge", "--ff-only", "-q", "origin/main")
}

func TestDecliningUpFrontDiscardsWithoutAStashEntry(t *testing.T) {
	work := usBehindClone(t)
	usPatchInput(t, func(string) (string, error) { return "n", nil })

	ref, err := stashLocalChangesIfNeeded(context.Background(), &bytes.Buffer{}, []string{"git"}, work, nil, usTargets, true)
	if err != nil {
		t.Fatal(err)
	}

	if ref != nil {
		t.Fatalf("ref = %q, want none", *ref)
	}
	if listed := ugGit(t, work, "stash", "list"); listed != "" {
		t.Fatalf("stash list = %q", listed)
	}
	if got := ugRead(t, filepath.Join(work, "a.txt")); got != "a\n" {
		t.Fatalf("a.txt = %q", got)
	}
	if statExists(filepath.Join(work, "pkg", "new.py")) {
		t.Fatal("the colliding untracked file is still there")
	}
	if !statExists(filepath.Join(work, "stray", "big.bin")) {
		t.Fatal("the stray file is gone")
	}
	ugGit(t, work, "merge", "--ff-only", "-q", "origin/main")
}

func TestUpToDateCheckoutNeverOffersToDelete(t *testing.T) {
	work := usBehindClone(t)
	ugGit(t, work, "stash", "push", "-q", "--include-untracked")
	ugGit(t, work, "merge", "--ff-only", "-q", "origin/main")
	ugWrite(t, filepath.Join(work, "a.txt"), "a\nmine\n")
	ucPatchRoot(t, work)
	t.Chdir(work)
	savedIn, savedOut := updateStdinIsTTY, updateStdoutIsTTY
	updateStdinIsTTY, updateStdoutIsTTY = func() bool { return true }, func(io.Writer) bool { return true }
	t.Cleanup(func() { updateStdinIsTTY, updateStdoutIsTTY = savedIn, savedOut })
	usPatchInput(t, func(string) (string, error) {
		t.Error("asked keep/delete on an update with nothing to pull")
		return "", nil
	})

	plan, err := prepareCheckoutForUpdate(context.Background(), &bytes.Buffer{}, []string{"git"}, "main", "main", prepareOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if plan.CommitCount != 0 {
		t.Fatalf("commit count = %d", plan.CommitCount)
	}
	if shown := ugGit(t, work, "stash", "show", "--name-only", plan.AutoStashRef); !strings.Contains(shown, "a.txt") {
		t.Fatalf("stash show = %q", shown)
	}
}

// Runtime state at the home root of a flat install, one representative per ignored class
var usFlatInstallRuntimeState = []string{
	"state.db",
	"state.db-wal",
	"state.db-shm",
	"state.db-journal",
	"state.db.retired-wal-20260914T000000Z-1234/manifest.json",
	"state.db.quarantine.lock",
	"state.db.repair.lock",
	"state.db.fts_rebuild.lock",
	"state.db.auto-maintenance.lock",
	"state.db.repair-attempts.json",
	"state.db.malformed-backup-20260914_060000",
	"kanban.db",
	"kanban.db.init.lock",
	"kanban.db.dispatch.lock",
	"response_store.db",
	"response_store.db-wal",
	"gateway/discord_message_recovery.db",
	"state-snapshots/2026-09-14T06-00-00-pre-update/state.db",
	"sessions/2026-09-14_06-00-00_abcd123d.jsonl",
	"browser-profile/Cookies",
	"cron/executions.db",
	"cron/executions.db-wal",
	"cron/executions.db-shm",
	"cron/deliveries.db",
	"cron/deliveries.db-wal",
	"cron/notepad.db",
	"cron/jobs.json",
	"cron/.jobs.lock",
	"cron/ticker_heartbeat",
	"cron/output/job1/2026-09-14T06-00-00.md",
	"cron.pid",
	"gateway.lock",
	"gateway.pid",
	"gateway_state.json",
	"processes.json",
	"gateway-starts.log",
	".update_check",
	".clean_shutdown",
	"active_profile",
	".icebath_history",
	"slack_tokens.json",
	"hook_outputs/2026-09-14_06-00-00/tool.json",
	"hooks/on_session_end.sh",
	"cache/banner_snapshot.json",
	"checkpoints/abcd123d/0001.json",
	"pending_messages/telegram.json",
	"plugin-data/example/state.json",
	"kanban/boards/x",
	"config.yaml",
	"auth.json",
	"auth.lock",
	"auth/google_oauth.json",
	".anthropic_oauth.json",
	"google_token.json",
	"google_oauth_pending.json",
	"webhook_subscriptions.json",
	"bws_cache.json",
	"bws_cache.enc.json",
	"op_cache.json",
	"photon-sidecar.json",
	"channel_directory.json",
	"channel_aliases.json",
	"feishu_comment_pairing.json",
	"memories/MEMORY.md",
	"profiles/work/config.yaml",
	"credentials/github_token",
	"mcp-tokens/server.json",
	"webapp/session-token",
	"pairing/telegram.json",
	"platforms/pairing/x.json",
	"backups/2026-09-14T06-00-00-pre-update/state.db",
	"vault.key",
	"vault.json.enc",
}

// A real git repository standing in for a flat install, with the tracked .gitignore and the runtime state present
func usFlatInstallRepo(t *testing.T) string {
	t.Helper()
	ugEnv(t)
	repo := filepath.Join(t.TempDir(), "flat-install-checkout")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	ugGit(t, repo, "init", "-q", repo)
	ignore, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), ignore, 0o644); err != nil {
		t.Fatal(err)
	}
	ugWrite(t, filepath.Join(repo, "app.py"), "print('icebath')\n")
	ugGit(t, repo, "add", ".gitignore", "app.py")
	ugGit(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init")
	for _, rel := range usFlatInstallRuntimeState {
		ugWrite(t, filepath.Join(repo, filepath.FromSlash(rel)), "runtime state")
	}
	return repo
}

func TestFlatInstallRuntimeStateIsIgnored(t *testing.T) {
	repo := usFlatInstallRepo(t)
	if status := ugGit(t, repo, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("status is not empty:\n%s", status)
	}
}

func TestUntrackedAutostashCannotSweepRuntimeState(t *testing.T) {
	repo := usFlatInstallRepo(t)
	ugWrite(t, filepath.Join(repo, "app.py"), "print('changed')\n")
	ugGit(t, repo, "stash", "push", "--include-untracked", "-m", "icebath-update-autostash")
	if got := ugRead(t, filepath.Join(repo, "app.py")); got != "print('icebath')\n" {
		t.Fatalf("app.py = %q", got)
	}
	var missing []string
	for _, rel := range usFlatInstallRuntimeState {
		if !statExists(filepath.Join(repo, filepath.FromSlash(rel))) {
			missing = append(missing, rel)
		}
	}
	if len(missing) != 0 {
		t.Fatalf("swept runtime state: %v", missing)
	}
}

func TestUntrackedAutostashLeavesOpenWalDatabaseReadable(t *testing.T) {
	repo := usFlatInstallRepo(t)
	dbPath := filepath.Join(repo, "cron", "executions.db")
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	for _, statement := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE executions (id INTEGER PRIMARY KEY, status TEXT)",
		"INSERT INTO executions (status) VALUES ('ok')"} {
		if _, err := conn.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if !statExists(filepath.Join(repo, "cron", "executions.db-wal")) {
		t.Fatal("the database has no WAL sidecar")
	}
	ugWrite(t, filepath.Join(repo, "app.py"), "print('changed')\n")

	stashRef, err := stashLocalChangesIfNeeded(context.Background(), &bytes.Buffer{}, []string{"git"}, repo, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	if stashRef == nil || *stashRef == "" {
		t.Fatal("nothing was stashed")
	}
	reader, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rows, err := reader.Query("SELECT status FROM executions")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var statuses []string
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil || !slices.Equal(statuses, []string{"ok"}) {
		t.Fatalf("statuses = %v, err = %v", statuses, err)
	}
}

// A repository with one committed file and a local change to it
func usRepoWithLocalChange(t *testing.T, change string) string {
	t.Helper()
	ugEnv(t)
	repo := t.TempDir()
	ugGit(t, repo, "init", "-q", "-b", "main")
	ugGit(t, repo, "config", "user.email", "t@example.com")
	ugGit(t, repo, "config", "user.name", "t")
	ugWrite(t, filepath.Join(repo, "tracked.txt"), "v1\n")
	ugGit(t, repo, "add", "-A")
	ugGit(t, repo, "commit", "-qm", "init")
	ugWrite(t, filepath.Join(repo, "tracked.txt"), change)
	return repo
}

// A repository with one update autostash entry aged the given number of days, and that entry's name
func usRepoWithAutostash(t *testing.T, age time.Duration) (string, string) {
	t.Helper()
	repo := usRepoWithLocalChange(t, "local change\n")
	name := autostashNamePrefix + time.Now().UTC().Add(-age).Format("20060102-150405")
	ugGit(t, repo, "stash", "push", "--include-untracked", "-m", name)
	return repo, name
}

func TestOldAutostashIsSurfaced(t *testing.T) {
	repo, name := usRepoWithAutostash(t, 9*24*time.Hour)
	var out bytes.Buffer
	count := warnOrphanedUpdateAutostashes(context.Background(), &out, []string{"git"}, repo)
	if count != 1 {
		t.Fatalf("count = %d", count)
	}
	for _, want := range []string{"leftover update autostash", name, "git stash apply"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if listed := ugGit(t, repo, "stash", "list"); !strings.Contains(listed, name) {
		t.Fatalf("stash list = %q", listed)
	}
}

func TestFreshAutostashIsNotFlagged(t *testing.T) {
	repo, _ := usRepoWithAutostash(t, 24*time.Hour)
	var out bytes.Buffer
	if count := warnOrphanedUpdateAutostashes(context.Background(), &out, []string{"git"}, repo); count != 0 {
		t.Fatalf("count = %d", count)
	}
	if strings.Contains(out.String(), "leftover update autostash") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestNonHermesStashIsIgnored(t *testing.T) {
	repo := usRepoWithLocalChange(t, "user's own WIP\n")
	ugGit(t, repo, "stash", "push", "-m", "my own stash from 20200101-000000")
	var out bytes.Buffer
	if count := warnOrphanedUpdateAutostashes(context.Background(), &out, []string{"git"}, repo); count != 0 {
		t.Fatalf("count = %d", count)
	}
	if strings.Contains(out.String(), "leftover update autostash") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestUnparseableAutostashTimestampLeftAlone(t *testing.T) {
	repo := usRepoWithLocalChange(t, "change\n")
	ugGit(t, repo, "stash", "push", "-m", autostashNamePrefix+"notadate")
	if count := warnOrphanedUpdateAutostashes(context.Background(), &bytes.Buffer{}, []string{"git"}, repo); count != 0 {
		t.Fatalf("count = %d", count)
	}
}

func TestGitFailureIsNonfatal(t *testing.T) {
	ugEnv(t)
	if count := warnOrphanedUpdateAutostashes(context.Background(), &bytes.Buffer{}, []string{"git"}, t.TempDir()); count != 0 {
		t.Fatalf("count = %d", count)
	}
}

func TestStashSelectorIsABareIndexNeverABraceSelector(t *testing.T) {
	ugEnv(t)
	repo := t.TempDir()
	ugGit(t, repo, "init", "-q", "-b", "main")
	ugGit(t, repo, "config", "user.email", "t@example.com")
	ugGit(t, repo, "config", "user.name", "t")
	ugWrite(t, filepath.Join(repo, "f.txt"), "v1\n")
	ugGit(t, repo, "add", "-A")
	ugGit(t, repo, "commit", "-qm", "init")
	ugWrite(t, filepath.Join(repo, "f.txt"), "older\n")
	ugGit(t, repo, "stash", "push", "-q", "-m", "older")
	targetSHA := strings.TrimSpace(ugGit(t, repo, "rev-parse", "refs/stash"))
	ugWrite(t, filepath.Join(repo, "f.txt"), "newer\n")
	ugGit(t, repo, "stash", "push", "-q", "-m", "newer")

	selector, found, err := resolveStashSelector(context.Background(), []string{"git"}, repo, targetSHA)
	if err != nil || !found || selector != "1" {
		t.Fatalf("selector = %q, found = %v, err = %v", selector, found, err)
	}
	ugGit(t, repo, "stash", "drop", selector)
	if listed := ugGit(t, repo, "stash", "list", "--format=%H"); strings.Contains(listed, targetSHA) {
		t.Fatalf("the entry is still listed:\n%s", listed)
	}
}

func TestGatewayRestorePromptDefaultsToKeepStash(t *testing.T) {
	var prompts [][2]string
	var out bytes.Buffer
	restored, err := restoreStashedChanges(context.Background(), &out, []string{"git"}, t.TempDir(), "stash@{0}", true,
		func(prompt, def string) (string, error) {
			prompts = append(prompts, [2]string{prompt, def})
			return "", nil
		}, nil)
	if err != nil || restored {
		t.Fatalf("restored = %v, err = %v", restored, err)
	}
	if want := [][2]string{{"Restore local changes now? [y/N]", "n"}}; !slices.Equal(prompts, want) {
		t.Fatalf("prompts = %v", prompts)
	}
	if !strings.Contains(out.String(), "still preserved in git stash") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestPruneOrphanRescueRefsWithRealGitUnpinsObjects(t *testing.T) {
	ugEnv(t)
	repo := t.TempDir()
	objectPresent := func(sha string) bool {
		cmd := exec.Command("git", "cat-file", "-e", sha)
		cmd.Dir = repo
		return cmd.Run() == nil
	}
	ugGit(t, repo, "init", "-q", "-b", "main")
	ugGit(t, repo, "config", "user.email", "t@example.com")
	ugGit(t, repo, "config", "user.name", "t")
	ugGit(t, repo, "config", "gc.auto", "0")
	ugWrite(t, filepath.Join(repo, "f.txt"), "base\n")
	ugGit(t, repo, "add", "-A")
	ugGit(t, repo, "commit", "-qm", "init")
	big := make([]byte, 512*1024)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	ugGit(t, repo, "add", "-A")
	ugGit(t, repo, "commit", "-qm", "snapshot")
	snapSHA := strings.TrimSpace(ugGit(t, repo, "rev-parse", "HEAD"))
	ugGit(t, repo, "reset", "-q", "--hard", "HEAD~1")
	oldRef := "refs/icebath-update-backups/orphan-main-20200101-000000-" + snapSHA[:12]
	ugGit(t, repo, "update-ref", oldRef, snapSHA)
	ugGit(t, repo, "reflog", "expire", "--expire=now", "--all")
	ugGit(t, repo, "gc", "-q", "--prune=now")
	if !objectPresent(snapSHA) {
		t.Fatal("the rescue ref does not pin its snapshot")
	}

	if err := pruneOrphanRescueRefs(context.Background(), []string{"git"}, repo, "main", orphanRescueRefsToKeep, orphanRescueRefMaxAgeDays); err != nil {
		t.Fatal(err)
	}

	if remaining := ugGit(t, repo, "for-each-ref", "refs/icebath-update-backups/"); strings.Contains(remaining, oldRef) {
		t.Fatalf("the expired ref is still there:\n%s", remaining)
	}
	ugGit(t, repo, "gc", "-q", "--prune=now")
	if objectPresent(snapSHA) {
		t.Fatal("the snapshot is still pinned")
	}
}
