package tools

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

var glCtx = context.Background()

// Origin with an empty commit history and a depth one clone fetched twice past it
func glShallowFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origin, clone := filepath.Join(dir, "origin"), filepath.Join(dir, "clone")
	os.MkdirAll(origin, 0o755)
	glGit(t, origin, "init", "-q", "-b", "main")
	glGit(t, origin, "commit", "--allow-empty", "-qm", "c0")
	glGit(t, dir, "clone", "-q", "--depth", "1", "file://"+filepath.ToSlash(origin), clone)
	glGit(t, origin, "commit", "--allow-empty", "-qm", "c1")
	glGit(t, origin, "commit", "--allow-empty", "-qm", "c2")
	glGit(t, clone, "fetch", "-q", "--depth", "1", "origin", "main")
	glGit(t, origin, "commit", "--allow-empty", "-qm", "c3")
	glGit(t, clone, "fetch", "-q", "--depth", "1", "origin", "main")
	return clone
}

// Drops the first shallow line whose loss breaks a link and returns it
func glCorrupt(t *testing.T, clone string) string {
	t.Helper()
	path := filepath.Join(clone, ".git", "shallow")
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	for _, removed := range lines {
		var kept []string
		for _, l := range lines {
			if l != removed {
				kept = append(kept, l)
			}
		}
		os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o644)
		out, errOut, _ := glGitRC(t, clone, "fsck", "--connectivity-only")
		if strings.Contains(out+errOut, "broken link") || strings.Contains(out+errOut, "missing commit") {
			return removed
		}
	}
	t.Fatal("fixture did not create a broken shallow boundary")
	return ""
}

func glWalks(t *testing.T, clone string) bool {
	_, _, rc := glGitRC(t, clone, "rev-list", "--count", "--all", "--reflog")
	return rc == 0
}

func TestRepairRestoresBoundaryForReflogOnlyCommitWithUnfetchedParent(t *testing.T) {
	clone := glShallowFixture(t)
	glCorrupt(t, clone)
	if glWalks(t, clone) {
		t.Fatal("premise")
	}
	if out, _, _ := glGitRC(t, clone, "fsck", "--connectivity-only"); !strings.Contains(out, "broken link") {
		t.Fatal(out)
	}
	if n := RepairBrokenShallowBoundaries(glCtx, clone); n < 1 || !glWalks(t, clone) {
		t.Fatal(n)
	}
	if out, errOut, _ := glGitRC(t, clone, "fsck", "--connectivity-only"); strings.Contains(out+errOut, "broken link") {
		t.Fatal(out)
	}
	glGit(t, clone, "gc", "-q")
}

func TestRepairThenPruneLeavesRepoWalkable(t *testing.T) {
	clone := glShallowFixture(t)
	glCorrupt(t, clone)
	if RepairBrokenShallowBoundaries(glCtx, clone) < 1 || !glWalks(t, clone) {
		t.Fatal("repair")
	}
	PruneStaleShallowGrafts(glCtx, clone)
	if !glWalks(t, clone) {
		t.Fatal("prune re-broke the repo")
	}
	if out, errOut, _ := glGitRC(t, clone, "fsck", "--connectivity-only"); strings.Contains(out+errOut, "broken link") {
		t.Fatal(out)
	}
}

func TestRepairIsNoopOnHealthyShallowCheckout(t *testing.T) {
	clone := glShallowFixture(t)
	path := filepath.Join(clone, ".git", "shallow")
	before, _ := os.ReadFile(path)
	if n := RepairBrokenShallowBoundaries(glCtx, clone); n != 0 {
		t.Fatal(n)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("shallow file changed")
	}
}

func TestRepairIsNoopOnFullCloneWithoutShallowFile(t *testing.T) {
	repo := t.TempDir()
	glGit(t, repo, "init", "-q")
	if n := RepairBrokenShallowBoundaries(glCtx, repo); n != 0 {
		t.Fatal(n)
	}
}

func TestRepairNeverRaisesOnBrokenRepo(t *testing.T) {
	if n := RepairBrokenShallowBoundaries(glCtx, filepath.Join(t.TempDir(), "missing")); n != 0 {
		t.Fatal(n)
	}
}

func TestRepairDoesNotTouchReflogs(t *testing.T) {
	clone := glShallowFixture(t)
	glCorrupt(t, clone)
	before := glGit(t, clone, "reflog", "show", "--all")
	RepairBrokenShallowBoundaries(glCtx, clone)
	if after := glGit(t, clone, "reflog", "show", "--all"); after != before {
		t.Fatal(after)
	}
}

func TestRepairIsIdempotent(t *testing.T) {
	clone := glShallowFixture(t)
	glCorrupt(t, clone)
	if RepairBrokenShallowBoundaries(glCtx, clone) < 1 || RepairBrokenShallowBoundaries(glCtx, clone) != 0 {
		t.Fatal("not idempotent")
	}
}

func TestRepairIgnoresParentLinesInsideCommitMessages(t *testing.T) {
	dir := t.TempDir()
	origin, clone := filepath.Join(dir, "origin"), filepath.Join(dir, "clone")
	os.MkdirAll(origin, 0o755)
	glGit(t, origin, "init", "-q", "-b", "main")
	glGit(t, origin, "commit", "--allow-empty", "-qm", "c0")
	glGit(t, origin, "commit", "--allow-empty", "-qm", "c1")
	glGit(t, dir, "clone", "-q", "--depth", "2", "file://"+filepath.ToSlash(origin), clone)
	glGit(t, clone, "commit", "--allow-empty", "-m", "subject\n\nbody line\n\nparent ffffffffffffffffffffffffffffffffffffffff")
	head := glGit(t, clone, "rev-parse", "HEAD")
	if got := batchMissingParents(glCtx, clone, []string{head}); len(got) != 0 {
		t.Fatal(got)
	}
	if n := glGit(t, clone, "rev-list", "--count", "HEAD"); n != "3" {
		t.Fatal(n)
	}
}

func TestRepairDoesNotMaskUnrelatedObjectLoss(t *testing.T) {
	clone := glShallowFixture(t)
	glGit(t, clone, "commit", "--allow-empty", "-qm", "local1")
	glGit(t, clone, "commit", "--allow-empty", "-qm", "local2")
	victim := glGit(t, clone, "rev-parse", "HEAD~1")
	loose := filepath.Join(clone, ".git", "objects", victim[:2], victim[2:])
	if err := os.Remove(loose); err != nil {
		t.Fatalf("expected a loose object to delete: %v", err)
	}
	if n := RepairBrokenShallowBoundaries(glCtx, clone); n != 0 {
		t.Fatal(n)
	}
	if out, errOut, _ := glGitRC(t, clone, "fsck", "--connectivity-only"); !strings.Contains(out+errOut, "missing") && !strings.Contains(out+errOut, "broken link") {
		t.Fatal(out + errOut)
	}
}

func TestFailedShallowMaintenanceRestoresOriginalBytes(t *testing.T) {
	for _, op := range []string{"repair", "prune"} {
		t.Run(op, func(t *testing.T) {
			clone := glShallowFixture(t)
			if op == "repair" {
				glCorrupt(t, clone)
			}
			glGit(t, clone, "commit", "--allow-empty", "-qm", "local1")
			glGit(t, clone, "commit", "--allow-empty", "-qm", "local2")
			victim := glGit(t, clone, "rev-parse", "HEAD~1")
			os.Remove(filepath.Join(clone, ".git", "objects", victim[:2], victim[2:]))
			shallow := filepath.Join(clone, ".git", "shallow")
			raw, _ := os.ReadFile(shallow)
			original := bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
			os.WriteFile(shallow, original, 0o644)
			if glWalks(t, clone) {
				t.Fatal("premise")
			}
			logs := glCaptureLog(t)
			maintain := RepairBrokenShallowBoundaries
			if op == "prune" {
				maintain = PruneStaleShallowGrafts
			}
			if n := maintain(glCtx, clone); n != 0 {
				t.Fatal(n)
			}
			if !strings.Contains(logs.String(), "self-check failed") {
				t.Fatalf("logs %q", logs.String())
			}
			if after, _ := os.ReadFile(shallow); !bytes.Equal(after, original) || pathExists(shallow+".lock") {
				t.Fatalf("bytes %q", after)
			}
		})
	}
}

func TestBomShallowPruneRollsBackWithoutLosingBytes(t *testing.T) {
	for name, bom := range map[string][]byte{"plain": nil, "bom": []byte("\xef\xbb\xbf")} {
		t.Run(name, func(t *testing.T) {
			clone := glShallowFixture(t)
			shallow := filepath.Join(clone, ".git", "shallow")
			head, tip := glGit(t, clone, "rev-parse", "HEAD"), glGit(t, clone, "rev-parse", "origin/main")
			raw, _ := os.ReadFile(shallow)
			var middle string
			for _, l := range strings.Split(string(raw), "\n") {
				if l != "" && l != head && l != tip {
					middle = l
					break
				}
			}
			glGit(t, clone, "checkout", "-q", middle)
			glGit(t, clone, "checkout", "-q", "main")
			raw, _ = os.ReadFile(shallow)
			original := append(slices.Clone(bom), bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))...)
			os.WriteFile(shallow, original, 0o644)
			logs := glCaptureLog(t)
			if n := PruneStaleShallowGrafts(glCtx, clone); n != 0 {
				t.Fatal(n)
			}
			if !strings.Contains(logs.String(), "self-check failed") {
				t.Fatalf("logs %q", logs.String())
			}
			if after, _ := os.ReadFile(shallow); !bytes.Equal(after, original) {
				t.Fatalf("bytes %q", after)
			}
		})
	}
}

// Nonempty lines of the clone's shallow file
func glShallowLines(t *testing.T, repo string) []string {
	raw, _ := os.ReadFile(filepath.Join(repo, ".git", "shallow"))
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Depth one clone fetched at depth one after the given commit pairs, sparse when the middle parent is never fetched
func glGraftScenario(t *testing.T, sparse bool) string {
	dir := t.TempDir()
	origin, clone := filepath.Join(dir, "origin"), filepath.Join(dir, "clone")
	os.MkdirAll(origin, 0o755)
	glGit(t, origin, "init", "-q", "-b", "main")
	for i := range 3 {
		glGit(t, origin, "commit", "--allow-empty", "-q", "-m", "c"+strconv.Itoa(i))
	}
	glGit(t, dir, "clone", "-q", "--depth", "1", "file://"+filepath.ToSlash(origin), clone)
	steps := []int{3, 4}
	if sparse {
		steps = []int{3, 5}
	}
	for _, i := range steps {
		glGit(t, origin, "commit", "--allow-empty", "-q", "-m", "c"+strconv.Itoa(i))
		if sparse {
			glGit(t, origin, "commit", "--allow-empty", "-q", "-m", "c"+strconv.Itoa(i+1))
		}
		glGit(t, clone, "fetch", "-q", "--depth", "1", "origin", "main")
	}
	return clone
}

// Set of strings
func glSet(items ...string) map[string]bool {
	out := map[string]bool{}
	for _, s := range items {
		out[s] = true
	}
	return out
}

func glSameSet(a []string, b ...string) bool {
	x, y := glSet(a...), glSet(b...)
	if len(x) != len(y) {
		return false
	}
	for k := range x {
		if !y[k] {
			return false
		}
	}
	return true
}

func TestPrunesOrphanedGraftsKeepsReferencedBoundaries(t *testing.T) {
	clone := glGraftScenario(t, false)
	if n := len(glShallowLines(t, clone)); n != 3 {
		t.Fatal(n)
	}
	head, tip := glGit(t, clone, "rev-parse", "HEAD"), glGit(t, clone, "rev-parse", "origin/main")
	if n := PruneStaleShallowGrafts(glCtx, clone); n != 1 || !glSameSet(glShallowLines(t, clone), head, tip) {
		t.Fatal(n, glShallowLines(t, clone))
	}
	if glGit(t, clone, "rev-list", "--count", "HEAD") != "1" || glGit(t, clone, "rev-list", "--count", "origin/main") != "1" {
		t.Fatal("history")
	}
}

func TestPruneIsIdempotentAndNoopWithoutGrafts(t *testing.T) {
	clone := glGraftScenario(t, false)
	if PruneStaleShallowGrafts(glCtx, clone) != 1 || PruneStaleShallowGrafts(glCtx, clone) != 0 {
		t.Fatal("idempotence")
	}
	empty := t.TempDir()
	if PruneStaleShallowGrafts(glCtx, empty) != 0 {
		t.Fatal("not a repo")
	}
	noShallow := t.TempDir()
	os.MkdirAll(filepath.Join(noShallow, ".git"), 0o755)
	if PruneStaleShallowGrafts(glCtx, noShallow) != 0 {
		t.Fatal("no shallow file")
	}
}

func TestPruneExpiresFetchReflogsThatPinDroppedGrafts(t *testing.T) {
	clone := glGraftScenario(t, true)
	if n := len(glShallowLines(t, clone)); n != 3 {
		t.Fatal(n)
	}
	head, tip := glGit(t, clone, "rev-parse", "HEAD"), glGit(t, clone, "rev-parse", "origin/main")
	var middle string
	for _, l := range glShallowLines(t, clone) {
		if l != head && l != tip {
			middle = l
		}
	}
	if !slices.Contains(strings.Fields(glGit(t, clone, "reflog", "show", "--format=%H", "origin/main")), middle) {
		t.Fatal("premise")
	}
	if n := PruneStaleShallowGrafts(glCtx, clone); n != 1 || !glSameSet(glShallowLines(t, clone), head, tip) {
		t.Fatal(n)
	}
	if slices.Contains(strings.Fields(glGit(t, clone, "reflog", "show", "--format=%H", "origin/main")), middle) {
		t.Fatal("pinning reflog kept")
	}
	if glGit(t, clone, "reflog", "show", "--format=%H", "HEAD") == "" || glGit(t, clone, "rev-list", "--count", "HEAD") != "1" ||
		glGit(t, clone, "rev-list", "--count", "origin/main") != "1" || glGit(t, clone, "rev-list", "--count", "--all", "--reflog") != "2" {
		t.Fatal("history")
	}
}

func TestPruneStillRollsBackWhenUserReflogsPinTheGraft(t *testing.T) {
	clone := glGraftScenario(t, true)
	head, tip := glGit(t, clone, "rev-parse", "HEAD"), glGit(t, clone, "rev-parse", "origin/main")
	var middle string
	for _, l := range glShallowLines(t, clone) {
		if l != head && l != tip {
			middle = l
		}
	}
	glGit(t, clone, "checkout", "-q", middle)
	glGit(t, clone, "checkout", "-q", "main")
	headReflog := glGit(t, clone, "reflog", "show", "--format=%H", "HEAD")
	if n := PruneStaleShallowGrafts(glCtx, clone); n != 0 || len(glShallowLines(t, clone)) != 3 {
		t.Fatal(n)
	}
	if glGit(t, clone, "reflog", "show", "--format=%H", "HEAD") != headReflog {
		t.Fatal("user reflog expired")
	}
}

// Origin with commits rewriting one file and filter uploads allowed
func glHealOrigin(t *testing.T, dir string, commits int) string {
	origin := filepath.Join(dir, "origin")
	os.MkdirAll(origin, 0o755)
	glGit(t, origin, "init", "-q", "-b", "main")
	glGit(t, origin, "config", "uploadpack.allowFilter", "true")
	for i := range commits {
		os.WriteFile(filepath.Join(origin, "f.txt"), []byte(strconv.Itoa(i)+"\n"), 0o644)
		glGit(t, origin, "add", "f.txt")
		glGit(t, origin, "commit", "-qm", "c"+strconv.Itoa(i))
	}
	return origin
}

func TestDepth1CloneOfATaglessOriginGetsItsCommitGraph(t *testing.T) {
	dir := t.TempDir()
	origin, clone := glHealOrigin(t, dir, 3), filepath.Join(dir, "clone")
	glGit(t, dir, "clone", "-q", "--depth", "1", "file://"+filepath.ToSlash(origin), clone)
	if ok, err := HealShallowHistory(glCtx, clone, "main", pysubprocess.Options{}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if glGit(t, clone, "rev-parse", "--is-shallow-repository") != "false" || glGit(t, clone, "rev-list", "--count", "HEAD") != "3" ||
		glGit(t, clone, "config", "remote.origin.partialclonefilter") != "blob:none" {
		t.Fatal("not healed")
	}
	packs := glPacks(clone)
	for _, p := range packs {
		if !pathExists(strings.TrimSuffix(p, ".pack") + ".promisor") {
			t.Fatal(p)
		}
	}
	if len(packs) == 0 {
		t.Fatal("no packs")
	}
}

func TestDepthPrefetchOnAFullCloneRestoresAncestryAndKeepsItFull(t *testing.T) {
	dir := t.TempDir()
	origin, clone := glHealOrigin(t, dir, 2), filepath.Join(dir, "clone")
	glGit(t, dir, "clone", "-q", "file://"+filepath.ToSlash(origin), clone)
	os.WriteFile(filepath.Join(origin, "f.txt"), []byte("next\n"), 0o644)
	glGit(t, origin, "commit", "-qam", "next")
	glGit(t, clone, "fetch", "-q", "--depth", "1", "origin", "main")
	if _, _, rc := glGitRC(t, clone, "merge-base", "HEAD", "origin/main"); rc == 0 {
		t.Fatal("premise")
	}
	if ok, err := HealShallowHistory(glCtx, clone, "main", pysubprocess.Options{}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, _, rc := glGitRC(t, clone, "merge-base", "--is-ancestor", "HEAD", "origin/main"); rc != 0 {
		t.Fatal("ancestry")
	}
	if _, _, rc := glGitRC(t, clone, "config", "--get", "remote.origin.promisor"); rc == 0 {
		t.Fatal("full clone turned partial")
	}
}

func TestTreelessCheckoutGetsItsWholeHistoryOnceSoWalksStayOffline(t *testing.T) {
	tagOnly := func(t *testing.T, clone string) {
		glGit(t, clone, "fetch", "-q", "origin", "refs/tags/v9:refs/tags/v9")
		glGit(t, clone, "checkout", "-q", "--detach", "v9")
	}
	cases := []struct {
		name, ref string
		checkOut  func(*testing.T, string)
		history   int
	}{
		{"main", "refs/heads/side", func(*testing.T, string) {}, 5},
		{"tag_only_release", "refs/tags/v9", tagOnly, 7},
		{"clashing_local_tag", "refs/tags/v9", func(t *testing.T, clone string) {
			tagOnly(t, clone)
			glGit(t, clone, "tag", "-f", "v9", "origin/main")
		}, 7},
		{"branch_outside_the_refspec", "refs/heads/side", func(t *testing.T, clone string) {
			glGit(t, clone, "config", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
			glGit(t, clone, "fetch", "-q", "origin", "side:side")
			glGit(t, clone, "checkout", "-q", "side")
		}, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			origin, clone := glHealOrigin(t, dir, 5), filepath.Join(dir, "clone")
			glGit(t, origin, "checkout", "-q", "-b", "side", "main~1")
			for i := range 3 {
				os.WriteFile(filepath.Join(origin, "f.txt"), []byte("side "+strconv.Itoa(i)+"\n"), 0o644)
				glGit(t, origin, "commit", "-qam", "s"+strconv.Itoa(i))
			}
			if strings.HasPrefix(c.ref, "refs/tags/") {
				glGit(t, origin, "tag", strings.TrimPrefix(c.ref, "refs/tags/"))
			}
			glGit(t, origin, "checkout", "-q", "main")
			if strings.HasPrefix(c.ref, "refs/tags/") {
				glGit(t, origin, "branch", "-D", "side")
			}
			glGit(t, dir, "clone", "-q", "--filter=tree:0", "--no-tags", "file://"+filepath.ToSlash(origin), clone)
			c.checkOut(t, clone)
			walk := func() (string, int) {
				res, err := pysubprocess.RunWith(glCtx, []string{"git", "log", "--format=%H", "--", "f.txt"},
					pysubprocess.Options{Dir: clone, Env: glEnv("GIT_NO_LAZY_FETCH=1")})
				if err != nil {
					t.Fatal(err)
				}
				return string(res.Stdout), res.Returncode
			}
			if _, rc := walk(); rc == 0 {
				t.Fatal("premise: offline walk succeeded")
			}
			if ok, err := ConvertTreelessCheckout(glCtx, clone, pysubprocess.Options{}); !ok || err != nil {
				t.Fatal(ok, err)
			}
			if glGit(t, clone, "config", "remote.origin.partialclonefilter") != "blob:none" {
				t.Fatal("filter")
			}
			if out, rc := walk(); rc != 0 || len(strings.Fields(out)) != c.history {
				t.Fatalf("walk %d %q", rc, out)
			}
			if ok, _ := ConvertTreelessCheckout(glCtx, clone, pysubprocess.Options{}); ok {
				t.Fatal("converted twice")
			}
		})
	}
}

func TestFailedConversionKeepsTheCheckoutTreelessForARetry(t *testing.T) {
	dir := t.TempDir()
	origin, clone := glHealOrigin(t, dir, 2), filepath.Join(dir, "clone")
	glGit(t, dir, "clone", "-q", "--filter=tree:0", "file://"+filepath.ToSlash(origin), clone)
	glGit(t, clone, "remote", "set-url", "origin", "file://"+filepath.ToSlash(filepath.Join(dir, "gone")))
	_, err := ConvertTreelessCheckout(glCtx, clone, pysubprocess.Options{})
	var cpe *pysubprocess.CalledProcessError
	if !errors.As(err, &cpe) {
		t.Fatalf("err %v", err)
	}
	if glGit(t, clone, "config", "remote.origin.partialclonefilter") != "tree:0" {
		t.Fatal("filter recorded")
	}
}

// Server with two release tags, the second past an empty commit
func glTagServer(t *testing.T, dir string) string {
	root := filepath.Join(dir, "server")
	os.MkdirAll(root, 0o755)
	glGit(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "tracked"), []byte("release\n"), 0o644)
	glGit(t, root, "add", "tracked")
	glGit(t, root, "commit", "-qm", "release")
	glGit(t, root, "tag", "v0.21.5")
	glGit(t, root, "commit", "-q", "--allow-empty", "-m", "after release")
	glGit(t, root, "tag", "v0.21.6")
	return root
}

func glHasPromisor(t *testing.T, root string) bool {
	out, _, _ := glGitRC(t, root, "config", "--get-all", "remote.origin.promisor")
	for _, v := range strings.Fields(out) {
		if strings.ToLower(v) == "true" {
			return true
		}
	}
	return false
}

func TestTagFetchNeverWritesPromisorConfigOnAFullClone(t *testing.T) {
	dir := t.TempDir()
	server, checkout := glTagServer(t, dir), filepath.Join(dir, "full")
	glGit(t, dir, "clone", "-q", "--no-tags", "file://"+filepath.ToSlash(server), checkout)
	if glHasPromisor(t, checkout) {
		t.Fatal("premise")
	}
	if _, err := FetchFullCommitGraph(glCtx, checkout, nil, pysubprocess.Options{}); err != nil {
		t.Fatal(err)
	}
	if glHasPromisor(t, checkout) {
		t.Fatal("promisor written")
	}
	if out, _, _ := glGitRC(t, checkout, "config", "--get-all", "remote.origin.partialclonefilter"); out != "" {
		t.Fatal(out)
	}
	if !strings.Contains(glGit(t, checkout, "tag", "--list"), "v0.21.6") {
		t.Fatal("tags")
	}
}

func TestTagFetchPreservesANonDefaultPartialCloneFilter(t *testing.T) {
	dir := t.TempDir()
	server, checkout := glTagServer(t, dir), filepath.Join(dir, "blobless")
	glGit(t, server, "config", "uploadpack.allowFilter", "true")
	glGit(t, dir, "clone", "-q", "--no-tags", "--filter=blob:none", "file://"+filepath.ToSlash(server), checkout)
	if !glHasPromisor(t, checkout) {
		t.Fatal("premise")
	}
	if _, err := FetchFullCommitGraph(glCtx, checkout, nil, pysubprocess.Options{}); err != nil {
		t.Fatal(err)
	}
	if glGit(t, checkout, "config", "--get", "remote.origin.partialclonefilter") != "blob:none" ||
		!strings.Contains(glGit(t, checkout, "tag", "--list"), "v0.21.6") {
		t.Fatal("filter or tags")
	}
}

func TestATagRefreshIsBoundedShortAndAHistoryFillKeepsTheLongBudget(t *testing.T) {
	dir := t.TempDir()
	server := glTagServer(t, dir)
	glGit(t, server, "config", "uploadpack.allowFilter", "true")
	full, shallow := filepath.Join(dir, "full"), filepath.Join(dir, "shallow")
	glGit(t, dir, "clone", "-q", "--no-tags", "file://"+filepath.ToSlash(server), full)
	glGit(t, dir, "clone", "-q", "--depth", "1", "file://"+filepath.ToSlash(server), shallow)
	seen := map[string]float64{}
	real := GitlockRunGit
	GitlockRunGit = func(ctx context.Context, gitCmd, args []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
		if len(args) > 0 && args[0] == "fetch" {
			seen[o.Dir] = o.Timeout.Seconds()
		}
		return real(ctx, gitCmd, args, o)
	}
	t.Cleanup(func() { GitlockRunGit = real })
	if ok, err := FetchFullCommitGraph(glCtx, full, nil, pysubprocess.Options{}); ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := FetchFullCommitGraph(glCtx, shallow, nil, pysubprocess.Options{}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if seen[full] != 30 || seen[shallow] != 900 || tagRefreshTimeout.Seconds() != 30 || historyFetchTimeout.Seconds() != 900 {
		t.Fatal(seen)
	}
	if !strings.Contains(glGit(t, full, "tag", "--list"), "v0.21.6") {
		t.Fatal("tags")
	}
}

// Records every spawn with its attributes and answers through reply
func glRecordSpawns(t *testing.T, reply func(argv []string) *pysubprocess.Completed) *[][]any {
	var captured [][]any
	marker := &syscall.SysProcAttr{}
	prevHide, prevRun := gitlockHideAttr, gitlockSubprocessRun
	gitlockHideAttr = func() *syscall.SysProcAttr { return marker }
	gitlockSubprocessRun = func(ctx context.Context, argv []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
		captured = append(captured, []any{slices.Clone(argv), o.Attr == marker})
		return reply(argv), nil
	}
	t.Cleanup(func() { gitlockHideAttr, gitlockSubprocessRun = prevHide, prevRun })
	return &captured
}

func TestGitlockGitSpawnsHideConsoleWindow(t *testing.T) {
	captured := glRecordSpawns(t, func(argv []string) *pysubprocess.Completed {
		s := strings.Join(argv, " ")
		switch {
		case strings.Contains(s, "rev-list"):
			return &pysubprocess.Completed{Returncode: 1}
		case strings.Contains(s, "cat-file") && strings.Contains(s, "--batch-check"):
			return &pysubprocess.Completed{}
		case strings.Contains(s, "cat-file"):
			return &pysubprocess.Completed{Returncode: 1}
		}
		return &pysubprocess.Completed{Stdout: []byte("deadbeef\n")}
	})
	dir := t.TempDir()
	if got := GitlockStdoutLines(glCtx, dir, []string{"rev-parse", "--git-path", "shallow"}); !slices.Equal(got, []string{"deadbeef"}) {
		t.Fatal(got)
	}
	if got := batchMissingParents(glCtx, dir, []string{"abc123"}); len(got) != 0 {
		t.Fatal(got)
	}
	RepairBrokenShallowBoundaries(glCtx, dir)
	if len(*captured) == 0 {
		t.Fatal("expected git spawns")
	}
	for _, c := range *captured {
		if c[0].([]string)[0] == "git" && !c[1].(bool) {
			t.Fatalf("visible console %v", c[0])
		}
	}
}

func TestGitlockTasklistProbeHidesConsoleWindow(t *testing.T) {
	captured := glRecordSpawns(t, func([]string) *pysubprocess.Completed {
		return &pysubprocess.Completed{Stdout: []byte("\"git.exe\",\"1234\",\"Console\",\"1\",\"10,000 K\"\n")}
	})
	prev := gitlockWindows
	gitlockWindows = true
	t.Cleanup(func() { gitlockWindows = prev })
	if !gitProcRunning(nil) {
		t.Fatal("git.exe not seen")
	}
	if len(*captured) != 1 || (*captured)[0][0].([]string)[0] != "tasklist" || !(*captured)[0][1].(bool) {
		t.Fatal(*captured)
	}
}

func TestGitlockTagFetchHidesConsoleWindow(t *testing.T) {
	captured := glRecordSpawns(t, func(argv []string) *pysubprocess.Completed {
		if strings.Contains(strings.Join(argv, " "), "cat-file") {
			return &pysubprocess.Completed{Returncode: 1}
		}
		return &pysubprocess.Completed{Stdout: []byte("8192, 4096, deadbeef\n")}
	})
	if ok, err := FetchFullCommitGraph(glCtx, t.TempDir(), nil, pysubprocess.Options{}); ok || err != nil {
		t.Fatal(ok, err)
	}
	var fetches int
	for _, c := range *captured {
		if slices.Contains(c[0].([]string), "fetch") {
			fetches++
			if !c[1].(bool) {
				t.Fatal("visible fetch console")
			}
		}
	}
	if fetches != 1 {
		t.Fatal(*captured)
	}
}
