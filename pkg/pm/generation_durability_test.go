package pm

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Replaces the filesystem flush with one recording what each flush saw on disk
func svFlushProbe(t *testing.T, observe func() bool) *[]bool {
	t.Helper()
	seen := &[]bool{}
	peSwap(t, &filesystemSync, func() error {
		*seen = append(*seen, observe())
		return nil
	})
	return seen
}

func TestSVInstallSelectionIsCommittedOnlyAfterItsTreeIsFlushed(t *testing.T) {
	tmp := svRooted(t)
	ctx := context.Background()
	repo := sttMkdir(t, filepath.Join(tmp, "repo"))
	peSwap(t, &repoRoot, func() string { return repo })
	candidate := filepath.Join(InstallStateDir(ctx, repo), "environments", "fresh", "venv")
	pubSwapVenv(t, &pubVenv{
		stamp: func(context.Context, []string, []MemberDir) (string, error) { return "stamp", nil },
		prepare: func(context.Context, []string, VenvApplyOptions) (*VenvGeneration, error) {
			if err := os.MkdirAll(candidate, 0o777); err != nil {
				return nil, err
			}
			return &VenvGeneration{Environment: candidate}, os.WriteFile(filepath.Join(candidate, "pyvenv.cfg"), []byte("home = fixture\n"), 0o666)
		},
	})
	committed := func() bool {
		selected, err := SelectedVenv(ctx, repo)
		return isRegularFile(EnvironmentRuntimeFactsPath(ctx, repo)) && err == nil && selected == resolvePath(candidate)
	}
	flushes := svFlushProbe(t, func() bool { return isRegularFile(filepath.Join(candidate, "pyvenv.cfg")) && !committed() })
	if err := syncVenv(ctx, SyncVenvOptions{Extras: []string{}, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*flushes, []bool{true}) {
		t.Fatalf("the tree must be flushed after it is complete and before it is selected, flushes %v", *flushes)
	}
	if selected, err := SelectedVenv(ctx, repo); err != nil || selected != resolvePath(candidate) {
		t.Fatalf("selected %q %v", selected, err)
	}
}

func TestSVSideEnvironmentSelectionIsCommittedOnlyAfterItsTreeIsFlushed(t *testing.T) {
	tmp := isolateHome(t)
	base := sttTouch(t, filepath.Join(sttMkdir(t, filepath.Join(tmp, "base")), "python"))
	peSwap(t, &uvToolchain, func(context.Context, bool, bool) (string, string, bool, error) { return "", base, true, nil })
	root := filepath.Join(tmp, "side")
	var built []string
	build := func(generation, _ string) (string, error) {
		environment := filepath.Join(generation, "venv")
		python := VenvPython(environment)
		if err := os.MkdirAll(filepath.Dir(python), 0o777); err != nil {
			return "", err
		}
		if err := os.WriteFile(python, nil, 0o666); err != nil {
			return "", err
		}
		built = append(built, environment)
		return python, os.WriteFile(filepath.Join(environment, "pyvenv.cfg"), []byte("home = fixture\n"), 0o666)
	}
	flushes := svFlushProbe(t, func() bool {
		_, err := os.Stat(filepath.Join(root, "active.json"))
		return len(built) > 0 && os.IsNotExist(err)
	})
	python, err := ensureGeneration(context.Background(), "proof", root, map[string]any{}, build, map[string]any{}, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*flushes, []bool{true}) {
		t.Fatalf("the tree must be flushed after it is complete and before it is selected, flushes %v", *flushes)
	}
	if got, ok, err := EnvironmentPython("proof", root); err != nil || !ok || got != python {
		t.Fatalf("environment python %q %v %v, built %q", got, ok, err, python)
	}
}
