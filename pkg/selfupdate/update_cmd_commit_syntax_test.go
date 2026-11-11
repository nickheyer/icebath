package selfupdate

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Replaces the lookup of an interpreter the target admits for one test
func uoPatchAdmittedPython(t *testing.T, python string) {
	t.Helper()
	saved := admittedPythonProbe
	admittedPythonProbe = func(context.Context, string) (string, bool, error) { return python, python != "", nil }
	t.Cleanup(func() { admittedPythonProbe = saved })
}

// A fresh repository on main with a committer identity
func uoInitRepo(t *testing.T, repo string) {
	t.Helper()
	ugEnv(t)
	ugGit(t, repo, "init", "-b", "main")
	ugGit(t, repo, "config", "user.email", "test@example.invalid")
	ugGit(t, repo, "config", "user.name", "Test")
}

func uoHead(t *testing.T, repo string) string {
	t.Helper()
	return strings.TrimSpace(ugGit(t, repo, "rev-parse", "HEAD"))
}

// Commits newer, requiring a future Python with the given constants source, and same, the file under this Python
func uoPythonBumpRepo(t *testing.T, repo, brokenSource string) (newer, same string) {
	t.Helper()
	uoInitRepo(t, repo)
	ugWrite(t, filepath.Join(repo, "icebath_constants.py"), brokenSource)
	pyproject := filepath.Join(repo, "pyproject.toml")
	ugWrite(t, pyproject, "[project]\nname = \"x\"\nrequires-python = \">=3.99\"\n")
	ugGit(t, repo, "add", ".")
	ugGit(t, repo, "commit", "-m", "requires a future python")
	newer = uoHead(t, repo)
	ugWrite(t, pyproject, "[project]\nname = \"x\"\nrequires-python = \">=3.8\"\n")
	ugGit(t, repo, "commit", "-am", "same file, this python")
	return newer, uoHead(t, repo)
}

func TestAPythonBumpWithNoAdmittedInterpreterIsStillSyntaxChecked(t *testing.T) {
	uoFresh(t)
	uoPatchAdmittedPython(t, "")
	repo := t.TempDir()
	newer, same := uoPythonBumpRepo(t, repo, "def broken(:\n")
	ctx := context.Background()
	critical := []string{"icebath_constants.py"}
	refused, err := targetSyntaxError(ctx, &bytes.Buffer{}, []string{"git"}, repo, newer, critical)
	if err != nil {
		t.Fatal(err)
	}
	if refused == nil || refused.Path != "icebath_constants.py" {
		t.Fatalf("refused = %+v", refused)
	}
	if !strings.Contains(refused.Error, "uv python install '>=3.99'") {
		t.Fatalf("the refusal does not name the Python to install: %q", refused.Error)
	}
	judged, err := targetSyntaxError(ctx, &bytes.Buffer{}, []string{"git"}, repo, same, critical)
	if err != nil || judged == nil || judged.Path != "icebath_constants.py" {
		t.Fatalf("judged = %+v, err = %v", judged, err)
	}

	ucPatchRoot(t, repo)
	ugGit(t, repo, "checkout", "-q", newer)
	if err := rollbackIfPulledSyntaxError(ctx, &bytes.Buffer{}, []string{"git"}, same, nil, nil); !isUpdateExit(err) {
		t.Fatalf("err = %v, want an exit", err)
	}
	if head := uoHead(t, repo); head != same {
		t.Fatalf("HEAD = %s, want %s", head, same)
	}
}

func TestAMalformedBootstrapIsRefusedBeforeTheMove(t *testing.T) {
	repo := t.TempDir()
	uoInitRepo(t, repo)
	ugWrite(t, filepath.Join(repo, "icebath_bootstrap.py"), "def broken(:\n")
	ugWrite(t, filepath.Join(repo, "icebath_constants.py"), "ok = 1\n")
	ugGit(t, repo, "add", ".")
	ugGit(t, repo, "commit", "-m", "broken bootstrap")
	refused, err := targetSyntaxError(context.Background(), &bytes.Buffer{}, []string{"git"}, repo, "HEAD", updateCriticalFiles)
	if err != nil {
		t.Fatal(err)
	}
	if refused == nil || refused.Path != "icebath_bootstrap.py" {
		t.Fatalf("refused = %+v", refused)
	}
}

func TestThePreflightReadsEveryCriticalFileInOneGitSpawn(t *testing.T) {
	repo := t.TempDir()
	uoInitRepo(t, repo)
	for _, rel := range []string{"icebath_constants.py", "cli.py"} {
		ugWrite(t, filepath.Join(repo, rel), "ok = 1\n")
	}
	ugWrite(t, filepath.Join(repo, "icebath_cli", "main.py"), "x = (\n")
	ugGit(t, repo, "add", ".")
	ugGit(t, repo, "commit", "-m", "c")
	var spawns [][]string
	real := runInCustody
	runInCustody = func(ctx context.Context, argv []string, inheritLock bool, opts ChildOptions) (*CompletedProcess, error) {
		spawns = append(spawns, slices.Clone(argv))
		return real(ctx, argv, inheritLock, opts)
	}
	t.Cleanup(func() { runInCustody = real })
	refused, err := targetSyntaxError(context.Background(), &bytes.Buffer{}, []string{"git"}, repo, "HEAD", updateCriticalFiles)
	if err != nil {
		t.Fatal(err)
	}
	if refused == nil || refused.Path != "icebath_cli/main.py" {
		t.Fatalf("refused = %+v", refused)
	}
	if len(spawns) != 1 || len(spawns[0]) < 2 || !slices.Equal(spawns[0][len(spawns[0])-2:], []string{"cat-file", "--batch"}) {
		t.Fatalf("spawns = %v, want one cat-file --batch", spawns)
	}
}

func TestAPythonBumpNeverAdmitsAConflictMarker(t *testing.T) {
	uoFresh(t)
	uoPatchAdmittedPython(t, "")
	repo := t.TempDir()
	uoInitRepo(t, repo)
	ugWrite(t, filepath.Join(repo, "icebath_constants.py"), "good = True\n")
	ugWrite(t, filepath.Join(repo, "pyproject.toml"), "[project]\nname = \"x\"\nrequires-python = \">=3.8\"\n")
	ugGit(t, repo, "add", ".")
	ugGit(t, repo, "commit", "-m", "pre")
	pre := uoHead(t, repo)
	ugWrite(t, filepath.Join(repo, "icebath_constants.py"), "<<<<<<< HEAD\na = 1\n=======\na = 2\n>>>>>>> topic\n")
	ugWrite(t, filepath.Join(repo, "pyproject.toml"), "[project]\nname = \"x\"\nrequires-python = \">=3.99\"\n")
	ugGit(t, repo, "commit", "-am", "conflicted, and bumps python")
	broken := uoHead(t, repo)
	ctx := context.Background()
	refused, err := targetSyntaxError(ctx, &bytes.Buffer{}, []string{"git"}, repo, broken, []string{"icebath_constants.py"})
	if err != nil || refused == nil {
		t.Fatalf("refused = %+v, err = %v", refused, err)
	}
	ucPatchRoot(t, repo)
	if err := rollbackIfPulledSyntaxError(ctx, &bytes.Buffer{}, []string{"git"}, pre, nil, nil); !isUpdateExit(err) {
		t.Fatalf("err = %v, want an exit", err)
	}
	if head := uoHead(t, repo); head != pre {
		t.Fatalf("HEAD = %s, want %s", head, pre)
	}
}

func TestTheClosureIsPresCommittedBytesReadInOneCatFileSpawn(t *testing.T) {
	ugEnv(t)
	root := filepath.Join(t.TempDir(), "checkout")
	for _, rel := range RecoveryClosure {
		ugWrite(t, filepath.Join(root, filepath.FromSlash(rel)), "# "+rel+" at pre\n")
	}
	ugGit(t, root, "init", "-q", "-b", "main")
	ugGit(t, root, "add", "-A")
	ugGit(t, root, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qm", "pre")
	pre := uoHead(t, root)
	ugWrite(t, filepath.Join(root, filepath.FromSlash(RecoveryClosure[0])), "# a working-tree edit, never published\n")
	var spawns []string
	real := runInCustody
	runInCustody = func(ctx context.Context, argv []string, inheritLock bool, opts ChildOptions) (*CompletedProcess, error) {
		for _, arg := range argv {
			if arg == "ls-tree" || arg == "cat-file" {
				spawns = append(spawns, arg)
				break
			}
		}
		return real(ctx, argv, inheritLock, opts)
	}
	t.Cleanup(func() { runInCustody = real })

	closure, err := publishRecoveryClosure(context.Background(), []string{"git"}, root, pre)
	if err != nil {
		t.Fatal(err)
	}

	for _, rel := range RecoveryClosure {
		if got := ugRead(t, filepath.Join(closure, filepath.FromSlash(rel))); got != "# "+rel+" at pre\n" {
			t.Fatalf("%s = %q", rel, got)
		}
	}
	if !slices.Equal(spawns, []string{"ls-tree", "cat-file"}) {
		t.Fatalf("spawns = %v", spawns)
	}
}
