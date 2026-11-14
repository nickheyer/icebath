package pm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Venv double whose build fails whenever a broken extra is selected and commits a fresh generation otherwise
type carriedVenv struct {
	*pubVenv
	broken  map[string]bool
	applied [][]string
}

// Registers a carried venv double over a fresh checkout
func carriedSetup(t *testing.T, broken ...string) *carriedVenv {
	t.Helper()
	tmp := isolateHome(t)
	repo := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(repo, 0o777); err != nil {
		t.Fatal(err)
	}
	peSwap(t, &repoRoot, func() string { return repo })
	c := &carriedVenv{broken: map[string]bool{}}
	for _, extra := range broken {
		c.broken[extra] = true
	}
	c.pubVenv = pubSwapVenv(t, &pubVenv{
		stamp: func(ctx context.Context, extras []string, _ []MemberDir) (string, error) {
			sorted := append([]string{}, extras...)
			sort.Strings(sorted)
			return "stamp:" + strings.Join(sorted, ","), nil
		},
	})
	c.prepare = c.build
	return c
}

// Builds a generation unless a broken extra is selected
func (c *carriedVenv) build(ctx context.Context, extras []string, _ VenvApplyOptions) (*VenvGeneration, error) {
	sorted := append([]string{}, extras...)
	sort.Strings(sorted)
	c.applied = append(c.applied, sorted)
	for _, extra := range extras {
		if c.broken[extra] {
			return nil, &BuildFailure{NewInstallError("venv", "uv sync exited 1: Failed to build `python-olm==3.2.16`", "")}
		}
	}
	environment := filepath.Join(InstallStateDir(ctx, RepoRoot()), "environments", strconv.Itoa(len(c.applied)), "venv")
	if err := os.MkdirAll(environment, 0o777); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(environment, "pyvenv.cfg"), []byte("home = fixture\n"), 0o666); err != nil {
		return nil, err
	}
	return &VenvGeneration{Environment: environment}, nil
}

// Extras of the recorded venv fact, nil when none is recorded
func carriedRecorded(t *testing.T) []string {
	t.Helper()
	facts, err := NewFacts(RuntimeFactsPath(context.Background()), false)
	if err != nil {
		t.Fatal(err)
	}
	fact, _ := facts.Get("venv")
	if fact == nil {
		return nil
	}
	return svList(fact["extras"])
}

// Runs body with the process stderr captured and returns what it wrote
func carriedStderr(t *testing.T, body func()) string {
	t.Helper()
	capture, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = capture
	defer func() { os.Stderr = previous }()
	body()
	capture.Close()
	return peRead(t, capture.Name())
}

func TestCarriedExtraThatCannotBuildIsLeftOutAndNotCarriedAgain(t *testing.T) {
	c := carriedSetup(t, "matrix")
	ctx := context.Background()
	var err error
	notice := carriedStderr(t, func() {
		err = syncVenv(ctx, SyncVenvOptions{Extras: []string{"all", "fal", "matrix"}, Explicit: true, Carried: []string{"fal", "matrix"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := carriedRecorded(t); !reflect.DeepEqual(got, []string{"all", "fal"}) {
		t.Fatalf("recorded %v", got)
	}
	declined, err := declinedExtras(ctx, "")
	if err != nil || !reflect.DeepEqual(declined, map[string]bool{"matrix": true}) {
		t.Fatalf("declined %v %v", declined, err)
	}
	if reasons := declinedReasons(ctx, ""); !reflect.DeepEqual(reasons, map[string]string{"matrix": "it no longer builds on this machine"}) {
		t.Fatalf("reasons %v", reasons)
	}
	if !strings.Contains(notice, "'matrix'") || !strings.Contains(notice, "icebath pm install --extra matrix") {
		t.Fatalf("notice %q", notice)
	}
	builds := len(c.applied)
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if len(c.applied) != builds {
		t.Fatalf("a current environment rebuilt %v", c.applied)
	}
	if err := syncVenv(ctx, SyncVenvOptions{Extras: []string{"matrix"}}); err == nil || !strings.Contains(err.Error(), "an update left 'matrix' out") {
		t.Fatalf("lazy use %v", err)
	}
}

func TestCarriedRequestedExtraOrCoreThatCannotBuildStillFails(t *testing.T) {
	for _, row := range []struct {
		broken  string
		carried []string
	}{{"matrix", nil}, {"all", []string{"matrix"}}} {
		t.Run(row.broken, func(t *testing.T) {
			carriedSetup(t, row.broken)
			ctx := context.Background()
			err := syncVenv(ctx, SyncVenvOptions{Extras: []string{"all", "matrix"}, Explicit: true, Carried: row.carried})
			var failure *BuildFailure
			if !errors.As(err, &failure) {
				t.Fatalf("sync %T %v", err, err)
			}
			if carriedRecorded(t) != nil {
				t.Fatal("a failed sync recorded facts")
			}
			if declined, err := declinedExtras(ctx, ""); err != nil || len(declined) != 0 {
				t.Fatalf("declined %v %v", declined, err)
			}
		})
	}
}

func TestCarriedExtraThatFailsForTheMomentIsRetriedByTheNextUpdate(t *testing.T) {
	c := carriedSetup(t)
	ctx := context.Background()
	failures := []error{&BuildFailure{NewInstallError("venv", "uv sync exited 1: Failed to build `review-optional`", "")}}
	unavailable := NewInstallError("venv", "uv sync exited 2: `review-optional` does not appear to be a Python project", "")
	sourceDown := true
	c.prepare = func(ctx context.Context, extras []string, opts VenvApplyOptions) (*VenvGeneration, error) {
		if slicesContain(extras, "matrix") && sourceDown {
			sorted := append([]string{}, extras...)
			sort.Strings(sorted)
			c.applied = append(c.applied, sorted)
			if len(failures) > 0 {
				failure := failures[len(failures)-1]
				failures = failures[:len(failures)-1]
				return nil, failure
			}
			return nil, unavailable
		}
		return c.build(ctx, extras, opts)
	}
	err := syncVenv(ctx, SyncVenvOptions{Extras: []string{"all", "fal", "matrix"}, Explicit: true, Carried: []string{"fal", "matrix"}})
	var refused *InstallError
	if !errors.As(err, &refused) || !strings.Contains(refused.Error(), "['matrix']") || !strings.Contains(refused.Error(), "could not be prepared") ||
		strings.Index(refused.Error(), "['matrix']") > strings.Index(refused.Error(), "could not be prepared") {
		t.Fatalf("refusal %v", err)
	}
	if !strings.Contains(refused.Remedy, "icebath pm install --without matrix") {
		t.Fatalf("remedy %q", refused.Remedy)
	}
	if carriedRecorded(t) != nil {
		t.Fatal("a transient refusal recorded facts")
	}
	if declined, err := declinedExtras(ctx, ""); err != nil || len(declined) != 0 {
		t.Fatalf("declined %v %v", declined, err)
	}
	sourceDown = false
	if err := syncVenv(ctx, SyncVenvOptions{Extras: []string{"all", "fal", "matrix"}, Explicit: true, Carried: []string{"fal", "matrix"}}); err != nil {
		t.Fatal(err)
	}
	if got := carriedRecorded(t); !reflect.DeepEqual(got, []string{"all", "fal", "matrix"}) {
		t.Fatalf("recorded %v", got)
	}
}
