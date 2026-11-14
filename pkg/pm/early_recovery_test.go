package pm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Temp project with a pyproject, an isolated home, the argv hook and a recorded repairer
func erProject(t *testing.T) (context.Context, string, *[]string, *bytes.Buffer) {
	t.Helper()
	tmp := isolateHome(t)
	root := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(root, 0o777); err != nil {
		t.Fatal(err)
	}
	root = resolvePath(root)
	body := "[project]\nname = \"x\"\ndependencies = []\n"
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte(body), 0o666); err != nil {
		t.Fatal(err)
	}
	prevHook := commandArgvHook
	prevRepair := repairDependencies
	SetCommandArgv(func(argv []string) []string { return argv })
	calls := &[]string{}
	repairDependencies = func(_ context.Context, project string) error {
		*calls = append(*calls, project)
		return nil
	}
	t.Cleanup(func() { SetCommandArgv(prevHook); repairDependencies = prevRepair })
	stderr := &bytes.Buffer{}
	return WithCLIStderr(context.Background(), stderr), root, calls, stderr
}

func erWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o666); err != nil {
		t.Fatal(err)
	}
}

func erRecover(t *testing.T, ctx context.Context, root string, argv []string) bool {
	t.Helper()
	ok, err := recoverIfNeeded(ctx, root, argv, false)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestERMarkerRequestsPMRepairThenClears(t *testing.T) {
	for _, name := range []string{".update-incomplete", ".lazy-refresh-incomplete"} {
		t.Run(name, func(t *testing.T) {
			ctx, root, calls, _ := erProject(t)
			marker := filepath.Join(root, name)
			erWrite(t, marker, "interrupted")
			if !erRecover(t, ctx, root, []string{}) {
				t.Fatal("recovery did not run")
			}
			if len(*calls) != 1 || (*calls)[0] != root {
				t.Fatalf("calls %v", *calls)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("marker survived")
			}
		})
	}
}

func TestERFailedRepairKeepsMarkerAndStopsAtRetryLimit(t *testing.T) {
	for _, body := range []string{"", "not json", `{"attempts": 1}`, "started=1\npid=0\n"} {
		t.Run(body, func(t *testing.T) {
			ctx, root, calls, stderr := erProject(t)
			marker := filepath.Join(root, ".update-incomplete")
			erWrite(t, marker, body)
			attempts := readMarkerAttempts(marker).Int64()
			repairDependencies = func(_ context.Context, project string) error {
				*calls = append(*calls, project)
				return errors.New("dependency build failed")
			}
			for expected := attempts + 1; expected <= earlyCoreInstallMaxAttempts; expected++ {
				if erRecover(t, ctx, root, []string{}) {
					t.Fatal("failed repair reported success")
				}
				if got := readMarkerAttempts(marker).Int64(); got != expected {
					t.Fatalf("attempts %d want %d", got, expected)
				}
				if strings.Contains(body, "pid=") {
					data, _ := os.ReadFile(marker)
					if !strings.HasPrefix(string(data), body) {
						t.Fatalf("legacy body lost %q", data)
					}
				}
			}
			before := len(*calls)
			if erRecover(t, ctx, root, []string{}) || len(*calls) != before {
				t.Fatal("retry limit did not stop the repair")
			}
			if !strings.Contains(stderr.String(), "icebath pm repair") {
				t.Fatalf("stderr %q", stderr.String())
			}
		})
	}
}

func TestERRecoveryObeysLiveOwnerAndSingleFlight(t *testing.T) {
	ctx, root, calls, _ := erProject(t)
	marker := filepath.Join(root, ".update-incomplete")
	erWrite(t, marker, fmt.Sprintf("started=1\npid=%d\n", os.Getpid()))
	if erRecover(t, ctx, root, []string{}) || len(*calls) != 0 || !isRegularFile(marker) {
		t.Fatal("live owner did not block recovery")
	}
	erWrite(t, marker, "interrupted")
	lock, err := claimRecoveryLock(ctx, root)
	if err != nil || lock == nil {
		t.Fatalf("lock %v %v", lock, err)
	}
	if erRecover(t, ctx, root, []string{}) || len(*calls) != 0 || !isRegularFile(marker) {
		lock.Close()
		t.Fatal("held lock did not block recovery")
	}
	lock.Close()
	if !erRecover(t, ctx, root, []string{"update"}) || len(*calls) != 1 || (*calls)[0] != root {
		t.Fatalf("recovery after release %v", *calls)
	}
	if !UpdateRetryRecovered() {
		t.Fatal("update retry flag not set")
	}
}

func TestERMissingEnvironmentCannotWriteARetryMarkerWithoutLock(t *testing.T) {
	ctx, root, _, _ := erProject(t)
	state := InstallStateDir(ctx, root)
	facts, err := NewFacts(EnvironmentRuntimeFactsPath(ctx, root), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.RecordState("venv", "old", []string{}, filepath.Join(state, "environments", "old", "venv"), ""); err != nil {
		t.Fatal(err)
	}
	lock, err := claimRecoveryLock(ctx, root)
	if err != nil || lock == nil {
		t.Fatalf("lock %v %v", lock, err)
	}
	defer lock.Close()
	if erRecover(t, ctx, root, []string{}) {
		t.Fatal("recovery ran without the lock")
	}
	if isRegularFile(filepath.Join(state, ".repair-incomplete")) {
		t.Fatal("retry marker written without the lock")
	}
}

func TestERPMCommandsAndHealthyStartupDoNotRepair(t *testing.T) {
	ctx, root, _, _ := erProject(t)
	repairDependencies = func(context.Context, string) error {
		t.Fatal("unexpected install")
		return nil
	}
	if erRecover(t, ctx, root, []string{}) {
		t.Fatal("healthy startup repaired")
	}
	marker := filepath.Join(root, ".update-incomplete")
	erWrite(t, marker, "interrupted")
	if erRecover(t, ctx, root, []string{"pm", "repair"}) || !isRegularFile(marker) {
		t.Fatal("pm command repaired")
	}
}

func TestERPIDLivenessRecognizesCurrentProcess(t *testing.T) {
	if live, err := pidRunning(big.NewInt(int64(os.Getpid()))); err != nil || !live {
		t.Fatalf("own pid %v %v", live, err)
	}
	if live, _ := pidRunning(big.NewInt(0)); live {
		t.Fatal("pid 0 live")
	}
}

func TestERMarkerOwnerLivenessUsesRecordedPID(t *testing.T) {
	marker := filepath.Join(t.TempDir(), ".update-incomplete")
	erWrite(t, marker, "started=1\npid=4321\n")
	var seen []string
	prev := pidIsRunning
	pidIsRunning = func(pid *big.Int) (bool, error) { seen = append(seen, pid.String()); return true, nil }
	defer func() { pidIsRunning = prev }()
	if live, err := markerOwnerIsLive(marker); err != nil || !live || len(seen) != 1 || seen[0] != "4321" {
		t.Fatalf("live %v err %v seen %v", live, err, seen)
	}
}

func TestERMarkerAttemptsFromAHandEditedBodyAreANonnegativeInt(t *testing.T) {
	for _, body := range []string{`{"attempts": Infinity}`, `{"attempts": -Infinity}`, `{"attempts": NaN}`, `{"attempts": true}`, `{"attempts": -4}`} {
		marker := filepath.Join(t.TempDir(), ".update-incomplete")
		erWrite(t, marker, body)
		if got := readMarkerAttempts(marker); got.Sign() < 0 {
			t.Fatalf("%s gave %s", body, got)
		}
	}
}

func TestERAFailedRepairCountsItsAttemptEvenOnAnUndecodableMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), ".update-incomplete")
	erWrite(t, marker, "pid=1\nstash=\xff\xfe\n")
	countFailedAttempt(marker)
	if got := readMarkerAttempts(marker); got.Int64() != 1 {
		t.Fatalf("attempts %s", got)
	}
}

func TestExportedRecoveryWrappers(t *testing.T) {
	marker := filepath.Join(t.TempDir(), ".update-incomplete")
	if err := os.WriteFile(marker, []byte(fmt.Sprintf("pid=%d\n", os.Getpid())), 0o666); err != nil {
		t.Fatal(err)
	}
	if live, err := MarkerOwnerIsLive(marker); err != nil || !live {
		t.Fatalf("own pid marker live %v %v", live, err)
	}
	prevHook := commandArgvHook
	SetCommandArgv(func(argv []string) []string { return argv })
	defer SetCommandArgv(prevHook)
	if recovered, err := RecoverIfNeeded(context.Background(), t.TempDir(), []string{}, false); err != nil || recovered {
		t.Fatalf("tree without a project recovered %v %v", recovered, err)
	}
}
