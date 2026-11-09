package selfupdate

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Child mode of the killed mid erase case, dying right after a pack payload is unlinked
func init() {
	repo := os.Getenv("ICEBATH_TEST_TIDY_KILLED_MID_ERASE")
	if repo == "" {
		return
	}
	tidyUnlinkFn = func(path string) error {
		err := os.Remove(path)
		if filepath.Ext(path) == ".pack" {
			os.Exit(9)
		}
		return err
	}
	tools.GitlockRunGit = GitlockRunGit
	TidyPartialClonePacks(context.Background(), repo, TidyBudgetSeconds)
	os.Exit(0)
}

func ptEnv(offline bool) []string {
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if offline {
		env = append(env, "GIT_NO_LAZY_FETCH=1")
	}
	return env
}

func ptGit(t *testing.T, cwd string, offline bool, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	cmd.Dir = cwd
	cmd.Env = ptEnv(offline)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func ptPackDir(repo string) string { return filepath.Join(repo, ".git", "objects", "pack") }

func ptPacks(t *testing.T, repo string) []string {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(ptPackDir(repo), "pack-*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(packs)
	return packs
}

func ptObjects(t *testing.T, repo string) string {
	t.Helper()
	names := strings.Fields(ptGit(t, repo, true, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)"))
	sort.Strings(names)
	return strings.Join(names, "\n")
}

func ptAge(t *testing.T, repo string) {
	t.Helper()
	old := time.Now().Add(-2 * time.Hour)
	entries, err := os.ReadDir(ptPackDir(repo))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(ptPackDir(repo), e.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func ptWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func ptUpstream(t *testing.T, tmp string, contents [][]byte) string {
	t.Helper()
	seed, up := filepath.Join(tmp, "seed"), filepath.Join(tmp, "up.git")
	ptGit(t, tmp, false, "init", "-q", "-b", "main", seed)
	for i, data := range contents {
		ptWrite(t, filepath.Join(seed, fmt.Sprintf("f%d", i)), data)
		ptGit(t, seed, false, "add", "-A")
		ptGit(t, seed, false, "commit", "-qm", fmt.Sprintf("c%d", i))
	}
	ptGit(t, tmp, false, "clone", "-q", "--bare", seed, up)
	for _, key := range []string{"uploadpack.allowFilter", "uploadpack.allowAnySHA1InWant"} {
		ptGit(t, up, false, "config", key, "true")
	}
	return up
}

func ptHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return tmp
}

func ptClone(t *testing.T) string {
	t.Helper()
	tmp := ptHome(t)
	contents := make([][]byte, 6)
	for i := range contents {
		contents[i] = []byte(fmt.Sprintf("v%d\n", i))
	}
	up := ptUpstream(t, tmp, contents)
	repo := filepath.Join(tmp, "clone")
	ptGit(t, tmp, false, "clone", "-q", "--filter=tree:0", "--no-checkout", "-b", "main", "file://"+filepath.ToSlash(up), repo)
	ptGit(t, repo, false, "config", "maintenance.auto", "false")
	ptGit(t, repo, false, "config", "gc.auto", "0")
	for i := 0; i < 6; i++ {
		ptGit(t, repo, false, "cat-file", "-p", fmt.Sprintf("HEAD:f%d", i))
	}
	ptGit(t, repo, false, "update-ref", "refs/heads/local", ptGit(t, repo, false, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "local"))
	ptGit(t, repo, false, "fetch", "-q", "--refetch", "--filter=blob:none", "origin", "main")
	ptGit(t, up, false, "update-ref", "refs/heads/main", ptGit(t, up, false, "commit-tree", "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "-m", "rewrite"))
	ptGit(t, up, false, "gc", "-q", "--prune=now")
	ptAge(t, repo)
	return repo
}

func ptTidy(repo string) TidyResult {
	return TidyPartialClonePacks(context.Background(), repo, TidyBudgetSeconds)
}

func TestErasesOnlyPacksWhoseEveryObjectHasAnotherCopy(t *testing.T) {
	clone := ptClone(t)
	before := ptPacks(t, clone)
	for _, pack := range before {
		ptWrite(t, withSuffix(pack, ".keep"), []byte("fetch-pack 4242 on host\n"))
	}
	head := ptGit(t, clone, false, "rev-parse", "HEAD")
	listings := map[string]string{}
	for _, p := range before {
		listings[p] = ptGit(t, clone, true, "verify-pack", "-v", withSuffix(p, ".idx"))
	}
	clonePack := ""
	for _, p := range before {
		if strings.Contains(listings[p], head) && (clonePack == "" || len(listings[p]) < len(listings[clonePack])) {
			clonePack = p
		}
	}
	if clonePack == "" {
		t.Fatal("fixture: no pack lists HEAD")
	}
	ptWrite(t, withSuffix(clonePack, ".keep"), []byte("pinned by hand\n"))
	ptAge(t, clone)
	held := ptObjects(t, clone)
	biggest := ""
	var biggestSize int64 = -1
	for _, p := range before {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > biggestSize {
			biggest, biggestSize = withSuffix(p, ".idx"), st.Size()
		}
	}
	realOpen := openPackIndexFn
	openPackIndexFn = func(idx string) (*packIndex, error) {
		if idx == biggest {
			return nil, errors.New("busy")
		}
		return realOpen(idx)
	}
	ptTidy(clone)
	openPackIndexFn = realOpen
	result := ptTidy(clone)
	if result.Erased <= 0 || len(ptPacks(t, clone)) != len(before)-result.Erased || result.FreedBytes <= 0 {
		t.Fatalf("result %+v, packs %d of %d", result, len(ptPacks(t, clone)), len(before))
	}
	if _, err := os.Stat(clonePack); err != nil {
		t.Fatal("a pack pinned by hand was erased")
	}
	if ptObjects(t, clone) != held {
		t.Fatal("an erased pack held the only copy of an object")
	}
	if got := ptGit(t, clone, true, "cat-file", "-p", "local:f3"); got != "v3" {
		t.Fatalf("local:f3 %q", got)
	}
	if again := ptTidy(clone); again.Erased != 0 {
		t.Fatalf("second tidy erased %d", again.Erased)
	}
}

func TestMergesDownToTheTargetIntoOnePackWhateverTheSizeLimit(t *testing.T) {
	tmp := ptHome(t)
	contents := make([][]byte, 6)
	for i := range contents {
		blob := make([]byte, 400*1024)
		if _, err := rand.Read(blob); err != nil {
			t.Fatal(err)
		}
		contents[i] = blob
	}
	up := ptUpstream(t, tmp, contents)
	repo := filepath.Join(tmp, "clone")
	ptGit(t, tmp, false, "clone", "-q", "--filter=blob:none", "--no-checkout", "-b", "main", "file://"+filepath.ToSlash(up), repo)
	ptGit(t, repo, false, "config", "maintenance.auto", "false")
	ptGit(t, repo, false, "config", "pack.packSizeLimit", "1m")
	for i := 0; i < 6; i++ {
		ptGit(t, repo, false, "cat-file", "-s", fmt.Sprintf("HEAD:f%d", i))
	}
	ptAge(t, repo)
	held := ptObjects(t, repo)
	prevTarget := PackCountTarget
	t.Cleanup(func() { PackCountTarget = prevTarget })
	PackCountTarget = 1
	type spawn struct {
		sub         string
		inheritLock bool
	}
	var spawned []spawn
	realRun := runInCustody
	t.Cleanup(func() { runInCustody = realRun })
	runInCustody = func(ctx context.Context, argv []string, inheritLock bool, opts ChildOptions) (*CompletedProcess, error) {
		spawned = append(spawned, spawn{updaterecord.GitSubcommand(argv[1:]), inheritLock})
		return realRun(ctx, argv, inheritLock, opts)
	}
	result := ptTidy(repo)
	packs := ptPacks(t, repo)
	if result.PacksLeft != 1 || len(packs) != 1 || !tidyExists(withSuffix(packs[0], ".promisor")) {
		t.Fatalf("result %+v packs %v", result, packs)
	}
	if ptObjects(t, repo) != held {
		t.Fatal("the merge lost an object")
	}
	found := false
	for _, s := range spawned {
		if s == (spawn{"pack-objects", true}) {
			found = true
		}
	}
	if !found {
		t.Fatalf("pack-objects did not run in the update's custody holding the lock: %v", spawned)
	}
}

func TestARefusedOrKilledEraseLeavesGitReadingAndIsFinishedLater(t *testing.T) {
	for _, layout := range [][]string{nil, {"--incremental"}} {
		t.Run(strings.Join(append([]string{"midx"}, layout...), " "), func(t *testing.T) {
			clone := ptClone(t)
			packDir := ptPackDir(clone)
			ptGit(t, clone, false, append([]string{"multi-pack-index", "write"}, layout...)...)
			held, before := ptObjects(t, clone), ptPacks(t, clone)
			realUnlink := tidyUnlinkFn
			tidyUnlinkFn = func(path string) error {
				if filepath.Dir(path) == packDir && filepath.Ext(path) == ".pack" {
					return &os.PathError{Op: "unlink", Path: path, Err: syscall.EACCES}
				}
				return realUnlink(path)
			}
			erased := ptTidy(clone).Erased
			tidyUnlinkFn = realUnlink
			if erased != 0 {
				t.Fatalf("erased %d packs while their payload could not go", erased)
			}
			after := ptPacks(t, clone)
			if strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Fatalf("packs changed: %v -> %v", before, after)
			}
			for _, p := range before {
				if !tidyExists(withSuffix(p, ".idx")) {
					t.Fatalf("index of %s gone", p)
				}
			}
			ptGit(t, clone, false, append([]string{"multi-pack-index", "write"}, layout...)...)
			if midx, _ := filepath.Glob(filepath.Join(packDir, "multi-pack-index*")); len(midx) == 0 {
				t.Fatal("fixture: no multi-pack-index written")
			}
			child := exec.Command(os.Args[0])
			child.Env = append(ptEnv(false), "ICEBATH_TEST_TIDY_KILLED_MID_ERASE="+clone)
			out, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 9 {
				t.Fatalf("child exit %v\n%s", err, out)
			}
			if midx, _ := filepath.Glob(filepath.Join(packDir, "multi-pack-index*")); len(midx) != 0 {
				t.Fatalf("a killed erase left a multi-pack-index naming a lost pack: %v", midx)
			}
			if ptObjects(t, clone) != held {
				t.Fatal("the killed erase lost an object")
			}
			if got := ptGit(t, clone, true, "cat-file", "-p", "local:f3"); got != "v3" {
				t.Fatalf("local:f3 %q", got)
			}
			holder, err := takeTidyLock(filepath.Join(clone, ".git"))
			if err != nil {
				t.Fatal(err)
			}
			if holder == nil {
				t.Fatal("a killed tidy left its lock held")
			}
			if got := ptTidy(clone); got != (TidyResult{}) {
				holder.Close()
				t.Fatalf("ran beside a live lock holder: %+v", got)
			}
			holder.Close()
			ptTidy(clone)
			parts, _ := filepath.Glob(filepath.Join(packDir, "pack-*.*"))
			for _, part := range parts {
				if !tidyExists(withSuffix(part, ".pack")) {
					t.Fatalf("leftovers of an interrupted erase survived the next run: %s", part)
				}
			}
			if ptObjects(t, clone) != held {
				t.Fatal("the finishing run lost an object")
			}
		})
	}
}
