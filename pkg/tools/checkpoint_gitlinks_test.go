package tools

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Workspace with a nested repository whose uncommitted and untracked work the baseline cannot capture
func cpGitlinkCheckpoint(t *testing.T) (*cpFixture, *CheckpointManager, string, string) {
	t.Helper()
	f := newCPFixture(t)
	work := filepath.Join(f.tmp, "workspace")
	nested := filepath.Join(work, "tool")
	cpNestedRepo(t, nested, "main.py", "committed\n")
	f.write(filepath.Join(nested, "main.py"), "user work not captured\n")
	f.write(filepath.Join(nested, "untracked.txt"), "not captured either\n")
	f.write(filepath.Join(work, "notes.txt"), "before\n")
	m := f.mgr(true, 20, 500, 10)
	if !m.EnsureCheckpoint(work, "baseline") {
		t.Fatal("baseline not taken")
	}
	commit := f.newest(m, work)
	ok, tree, _ := runCheckpointGit(f.env, []string{"ls-tree", "-r", "-z", commit}, f.store(), work, checkpointGitOpts{})
	if !ok || !strings.Contains(tree, "160000 commit ") {
		t.Fatalf("baseline holds no gitlink: %q", tree)
	}
	f.write(filepath.Join(work, "notes.txt"), "after\n")
	f.write(filepath.Join(nested, "main.py"), "agent overwrite\n")
	return f, m, work, commit
}

// Durable files of the workspace and the store, refs, indexes, ledgers and working files
func cpDurableState(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || p == root {
				return nil
			}
			key := root + "\x00" + p[len(root):]
			if st, serr := os.Stat(p); serr == nil && st.Mode().IsRegular() {
				b, _ := os.ReadFile(p)
				out[key] = "file:" + string(b)
			} else {
				out[key] = "other"
			}
			return nil
		})
	}
	return out
}

func TestCheckpointGitlinkRefusalIsNonmutating(t *testing.T) {
	for _, safe := range []bool{false, true} {
		for _, spec := range []string{"", ".", "tool", "tool/", "tool/main.py", "*", "t*", ":(glob)*", ":(top,glob)**", ":(icase)TOOL"} {
			f, m, work, commit := cpGitlinkCheckpoint(t)
			before := cpDurableState(t, work, f.store())
			r := m.Restore(work, commit, spec, safe)
			if cpSuccess(r) || !reflect.DeepEqual(cpList(r, "nested_repositories"), []string{"tool"}) || !strings.Contains(r["error"].(string), "not captured") {
				t.Fatalf("safe=%v spec=%q not refused: %v", safe, spec, r)
			}
			if !reflect.DeepEqual(cpDurableState(t, work, f.store()), before) {
				t.Fatalf("safe=%v spec=%q mutated state", safe, spec)
			}
		}
	}
}

func TestCheckpointGitlinkUnrelatedSelectionStillRestores(t *testing.T) {
	for _, safe := range []bool{false, true} {
		for _, spec := range []string{"notes.txt", "*.txt", ":(glob)*.txt", ":(exclude)tool", ":(literal)notes.txt"} {
			f, m, work, commit := cpGitlinkCheckpoint(t)
			if r := m.Restore(work, commit, spec, safe); !cpSuccess(r) {
				t.Fatalf("safe=%v spec=%q refused: %v", safe, spec, r)
			}
			if f.read(filepath.Join(work, "notes.txt")) != "before\n" || f.read(filepath.Join(work, "tool", "main.py")) != "agent overwrite\n" {
				t.Fatalf("safe=%v spec=%q restored the wrong files", safe, spec)
			}
		}
	}
}

func TestCheckpointGitlinkUnusualFilenameDoesNotBreakTreeInspection(t *testing.T) {
	names := []string{"space name.txt", "unicode-雪.txt"}
	if runtime.GOOS != "windows" {
		names = append(names, "tab\tline\n.txt")
	}
	if runtime.GOOS == "linux" {
		names = append(names, "bad-\xff.txt")
	}
	for _, name := range names {
		f, m, work, _ := cpGitlinkCheckpoint(t)
		target := filepath.Join(work, name)
		f.write(target, "original\n")
		m.NewTurn()
		if !m.EnsureCheckpoint(work, "unusual filename") {
			t.Fatalf("%q not checkpointed", name)
		}
		commit := f.newest(m, work)
		f.write(target, "changed\n")
		if r := m.Restore(work, commit, name, false); !cpSuccess(r) || f.read(target) != "original\n" {
			t.Fatalf("%q not restored: %v", name, r)
		}
	}
}

func TestCheckpointGitlinkDeletedNestedRepositoryIsNotReportedRestored(t *testing.T) {
	for _, safe := range []bool{false, true} {
		for _, spec := range []string{"", "*", "tool/main.py"} {
			f, m, work, commit := cpGitlinkCheckpoint(t)
			if err := rmtreeReadonly(filepath.Join(work, "tool"), false); err != nil {
				t.Fatal(err)
			}
			before := cpDurableState(t, work, f.store())
			r := m.Restore(work, commit, spec, safe)
			if cpSuccess(r) || !reflect.DeepEqual(cpList(r, "nested_repositories"), []string{"tool"}) || f.exists(filepath.Join(work, "tool")) {
				t.Fatalf("safe=%v spec=%q wrong: %v", safe, spec, r)
			}
			if !reflect.DeepEqual(cpDurableState(t, work, f.store()), before) {
				t.Fatalf("safe=%v spec=%q mutated state", safe, spec)
			}
		}
	}
}

func TestCheckpointGitlinkEmptySelectionRefusesWithoutMutation(t *testing.T) {
	for _, notes := range []string{"before\n", "after\n"} {
		f, m, work, commit := cpGitlinkCheckpoint(t)
		f.write(filepath.Join(work, "notes.txt"), notes)
		os.Rename(filepath.Join(work, "tool"), filepath.Join(f.tmp, "moved-tool"))
		before := cpDurableState(t, work, f.store())
		r := m.Restore(work, commit, ":(exclude)*", false)
		if cpSuccess(r) || !strings.Contains(r["error"].(string), "matched no files") || f.exists(filepath.Join(work, "tool")) {
			t.Fatalf("empty selection not refused: %v", r)
		}
		if !reflect.DeepEqual(cpDurableState(t, work, f.store()), before) {
			t.Fatal("empty selection mutated state")
		}
	}
}

func TestCheckpointGitlinkNameRoundtripsInRefusal(t *testing.T) {
	names := []string{"nested space", "nested-雪", "vendor/tool"}
	if runtime.GOOS != "windows" {
		names = append(names, "nested\tline\n")
	}
	if runtime.GOOS == "linux" {
		names = append(names, "nested-\xff")
	}
	for _, name := range names {
		f, m, work, _ := cpGitlinkCheckpoint(t)
		dest := filepath.Join(work, filepath.FromSlash(name))
		f.mkdir(filepath.Dir(dest))
		if err := os.Rename(filepath.Join(work, "tool"), dest); err != nil {
			t.Fatal(err)
		}
		m.NewTurn()
		if !m.EnsureCheckpoint(work, "renamed nested repository") {
			t.Fatalf("%q not checkpointed", name)
		}
		commit := f.newest(m, work)
		before := cpDurableState(t, work, f.store())
		r := m.Restore(work, commit, "*", false)
		if cpSuccess(r) || !reflect.DeepEqual(cpList(r, "nested_repositories"), []string{name}) {
			t.Fatalf("%q refusal wrong: %v", name, r)
		}
		if !reflect.DeepEqual(cpDurableState(t, work, f.store()), before) {
			t.Fatalf("%q refusal mutated state", name)
		}
	}
}

func TestCheckpointListingDisclosesUncapturedNestedRepository(t *testing.T) {
	f, m, work, _ := cpGitlinkCheckpoint(t)
	entry := m.ListCheckpoints(work)[0]
	if entry.Reason != "baseline [nested git repos not captured: tool]" {
		t.Fatalf("reason wrong: %q", entry.Reason)
	}
	if !strings.Contains(FormatCheckpointList([]CheckpointEntry{entry}, work), "[nested git repos not captured: tool]") {
		t.Fatal("listing hides the disclosure")
	}
	plain := filepath.Join(f.tmp, "plain")
	f.write(filepath.Join(plain, "a.txt"), "a\n")
	if !m.EnsureCheckpoint(plain, "no nested repo") || m.ListCheckpoints(plain)[0].Reason != "no nested repo" {
		t.Fatal("plain project got a disclosure")
	}
}

func TestCheckpointMultilineReasonKeepsDisclosureInListing(t *testing.T) {
	f, m, work, _ := cpGitlinkCheckpoint(t)
	f.write(filepath.Join(work, "notes.txt"), "changed again\n")
	m.NewTurn()
	if !m.EnsureCheckpoint(work, "before terminal: rm -rf build\n\nprintf done") {
		t.Fatal("not taken")
	}
	entry := m.ListCheckpoints(work)[0]
	if entry.Reason != "before terminal: rm -rf build printf done [nested git repos not captured: tool]" {
		t.Fatalf("reason wrong: %q", entry.Reason)
	}
	if !strings.Contains(FormatCheckpointList([]CheckpointEntry{entry}, work), "[nested git repos not captured: tool]") {
		t.Fatal("listing hides the disclosure")
	}
}

func TestCheckpointDisclosureCapsListedRepositories(t *testing.T) {
	if got := checkpointUncapturedNote([]string{"a", "b", "c", "d", "e"}); got != " [nested git repos not captured: a, b, c (+2)]" {
		t.Fatalf("note wrong: %q", got)
	}
}

// Gitlink checkpoint with the nested repository moved one level deeper
func cpDeepCheckpoint(t *testing.T) (*cpFixture, *CheckpointManager, string, string) {
	t.Helper()
	f, m, work, _ := cpGitlinkCheckpoint(t)
	f.mkdir(filepath.Join(work, "packages"))
	os.Rename(filepath.Join(work, "tool"), filepath.Join(work, "packages", "nested"))
	f.write(filepath.Join(work, "notes.txt"), "before\n")
	m.NewTurn()
	if !m.EnsureCheckpoint(work, "deeper nested repository") {
		t.Fatal("not taken")
	}
	commit := f.newest(m, work)
	f.write(filepath.Join(work, "notes.txt"), "after\n")
	return f, m, work, commit
}

func TestCheckpointDeeperGitlinkScopeRefusesInAnySpelling(t *testing.T) {
	specs := []string{"packages", "packages/", "packages/nested", "packages/nested/", "packages/nested/main.py", "packages/./nested", "notes/../packages/nested"}
	if runtime.GOOS == "windows" {
		specs = append(specs, `packages\nested`, `packages\nested\main.py`)
	}
	for _, spec := range specs {
		f, m, work, commit := cpDeepCheckpoint(t)
		before := cpDurableState(t, work, f.store())
		r := m.Restore(work, commit, spec, false)
		if cpSuccess(r) || !reflect.DeepEqual(cpList(r, "nested_repositories"), []string{"packages/nested"}) {
			t.Fatalf("%q not refused: %v", spec, r)
		}
		if !reflect.DeepEqual(cpDurableState(t, work, f.store()), before) {
			t.Fatalf("%q mutated state", spec)
		}
	}
}

func TestCheckpointDeeperGitlinkLeavesUnrelatedRecoveryAvailable(t *testing.T) {
	for _, spec := range []string{"notes.txt", "packages/../notes.txt"} {
		f, m, work, commit := cpDeepCheckpoint(t)
		if r := m.Restore(work, commit, spec, false); !cpSuccess(r) {
			t.Fatalf("%q refused: %v", spec, r)
		}
		if f.read(filepath.Join(work, "notes.txt")) != "before\n" || f.read(filepath.Join(work, "packages", "nested", "main.py")) != "agent overwrite\n" {
			t.Fatalf("%q restored the wrong files", spec)
		}
	}
}

func TestCheckpointUnresolvableSelectionIsNonmutating(t *testing.T) {
	for _, spec := range []string{":(literal)tool/main.py", ":(glob)tool/*.py", ":(invalid)tool"} {
		f, m, work, commit := cpGitlinkCheckpoint(t)
		before := cpDurableState(t, work, f.store())
		if r := m.Restore(work, commit, spec, false); cpSuccess(r) {
			t.Fatalf("%q restored: %v", spec, r)
		}
		if !reflect.DeepEqual(cpDurableState(t, work, f.store()), before) {
			t.Fatalf("%q mutated state", spec)
		}
	}
}

func TestCheckpointAttributePathspecRestoresOnlyTheInspectedSelection(t *testing.T) {
	f, m, work, _ := cpGitlinkCheckpoint(t)
	attrs := filepath.Join(work, ".gitattributes")
	f.write(attrs, "tool restore\nnotes.txt restore\n")
	f.write(filepath.Join(work, "notes.txt"), "before\n")
	m.NewTurn()
	if !m.EnsureCheckpoint(work, "attributes") {
		t.Fatal("not taken")
	}
	commit := f.newest(m, work)
	os.Rename(filepath.Join(work, "tool"), filepath.Join(f.tmp, "moved-tool"))
	os.Remove(attrs)
	f.write(filepath.Join(work, "notes.txt"), "after\n")
	if r := m.Restore(work, commit, ":(attr:!restore)*", false); !cpSuccess(r) {
		t.Fatalf("attribute selection refused: %v", r)
	}
	if f.read(attrs) != "tool restore\nnotes.txt restore\n" || f.read(filepath.Join(work, "notes.txt")) != "after\n" || f.exists(filepath.Join(work, "tool")) {
		t.Fatal("checkout re-evaluated the attribute pathspec")
	}
}

func TestCheckpointInspectionFailureLeavesFilesAndHistoryUntouched(t *testing.T) {
	for _, command := range []string{"read-tree", "ls-files"} {
		f, m, work, commit := cpGitlinkCheckpoint(t)
		real := runCheckpointGit
		cpSwap(t, &runCheckpointGit, func(env *Env, args []string, store, wd string, o checkpointGitOpts) (bool, string, string) {
			if args[0] == command {
				return false, "", "inspection unavailable"
			}
			return real(env, args, store, wd, o)
		})
		before := cpDurableState(t, work, f.store())
		r := m.Restore(work, commit, "*", false)
		if cpSuccess(r) || !strings.Contains(r["error"].(string), "inspection unavailable") {
			t.Fatalf("%s failure not surfaced: %v", command, r)
		}
		if !reflect.DeepEqual(cpDurableState(t, work, f.store()), before) {
			t.Fatalf("%s failure mutated state", command)
		}
		if leftovers, _ := filepath.Glob(filepath.Join(f.store(), "restore-inspect-*")); len(leftovers) != 0 {
			t.Fatalf("inspection scratch left behind: %v", leftovers)
		}
		runCheckpointGit = real
	}
}

func TestCheckpointCapturedFileReplacedBySymlinkIntoNestedRepoRestores(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	for _, safe := range []bool{false, true} {
		f, m, work, commit := cpGitlinkCheckpoint(t)
		notes := filepath.Join(work, "notes.txt")
		os.Remove(notes)
		if err := os.Symlink(filepath.Join(work, "tool", "main.py"), notes); err != nil {
			t.Fatal(err)
		}
		if r := m.Restore(work, commit, "notes.txt", safe); !cpSuccess(r) {
			t.Fatalf("safe=%v refused: %v", safe, r)
		}
		if st, _ := os.Lstat(notes); st.Mode()&os.ModeSymlink != 0 || f.read(notes) != "before\n" || f.read(filepath.Join(work, "tool", "main.py")) != "agent overwrite\n" {
			t.Fatalf("safe=%v symlink not replaced by the captured file", safe)
		}
	}
}

func TestCheckpointGitlinkReplacedByUnrelatedSymlinkStillRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	for _, spec := range []string{"", "tool", "tool/main.py"} {
		f, m, work, commit := cpGitlinkCheckpoint(t)
		os.Rename(filepath.Join(work, "tool"), filepath.Join(f.tmp, "moved-tool"))
		other := filepath.Join(work, "other")
		f.write(filepath.Join(other, "main.py"), "unrelated\n")
		if err := os.Symlink(other, filepath.Join(work, "tool")); err != nil {
			t.Fatal(err)
		}
		before := cpDurableState(t, work, f.store())
		r := m.Restore(work, commit, spec, false)
		if cpSuccess(r) || !reflect.DeepEqual(cpList(r, "nested_repositories"), []string{"tool"}) {
			t.Fatalf("%q not refused: %v", spec, r)
		}
		if st, _ := os.Lstat(filepath.Join(work, "tool")); st.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%q replaced the symlink", spec)
		}
		if !reflect.DeepEqual(cpDurableState(t, work, f.store()), before) {
			t.Fatalf("%q mutated state", spec)
		}
	}
}
