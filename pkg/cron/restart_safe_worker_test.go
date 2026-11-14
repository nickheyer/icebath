package cron

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Worker double that never exits on its own, the Popen stand in of the launch cases
type fakeWorkerProcess struct{}

func (fakeWorkerProcess) Wait(time.Duration) (*int, bool) { return nil, false }
func (fakeWorkerProcess) Poll() *int                      { return nil }

type spawnedWorker struct {
	argv []string
	dir  string
	env  map[string]string
}

// Spawn double that records the launch, reads its payload and publishes the ownership ack
func fakeExternalWorkerLaunch(t *testing.T, executionID string) *[]spawnedWorker {
	t.Helper()
	var spawned []spawnedWorker
	prevSpawn, prevGet := spawnExternalWorkerSeam, getExecution
	t.Cleanup(func() { spawnExternalWorkerSeam, getExecution = prevSpawn, prevGet })
	spawnExternalWorkerSeam = func(argv []string, dir string, env map[string]string, _ string) (workerProcess, int, error) {
		spawned = append(spawned, spawnedWorker{argv, dir, env})
		ack := ""
		for i, arg := range argv {
			if arg == "--ack-file" {
				ack = argv[i+1]
			}
		}
		body, _ := json.Marshal(map[string]any{"pid": 4321, "execution_id": executionID})
		if err := os.WriteFile(ack, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return fakeWorkerProcess{}, 4321, nil
	}
	statuses := []string{"running", "completed"}
	getExecution = func(context.Context, *Store, string) (map[string]any, error) {
		status := statuses[0]
		if len(statuses) > 1 {
			statuses = statuses[1:]
		}
		return map[string]any{"id": executionID, "status": status}, nil
	}
	return &spawned
}

// Commits a PM generation for the tree in the sandboxed home and returns its venv
func commitGeneration(t *testing.T, ctx context.Context, repoRoot, name string, withSitePackages bool) string {
	t.Helper()
	venv := filepath.Join(pm.InstallStateDir(ctx, repoRoot), "environments", name, "venv")
	if err := os.MkdirAll(filepath.Join(venv, "lib", "python3.12"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("version = 3.12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(venv), ".lease-managed"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if withSitePackages {
		selected, err := pm.SitePackages(venv)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(selected, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(selected, "_generation_sentinel.py"), []byte("NAME = '"+name+"'\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	facts := pm.EnvironmentRuntimeFactsPath(ctx, repoRoot)
	body, _ := json.Marshal(map[string]any{"packages": map[string]any{"venv": map[string]any{"environment": venv}}, "schema": 1})
	if err := os.MkdirAll(filepath.Dir(facts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(facts, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return venv
}

func TestLaunchExternalWorkerPinExtendsTheSanitizedEnvNotOsEnviron(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	ctx := context.Background()
	tmp := t.TempDir()
	execution, err := openExecutionsLedger(s).Create(ctx, "job-1", "direct", nil)
	if err != nil {
		t.Fatal(err)
	}
	executionID := execution["id"].(string)
	job := &Job{ID: "job-1", ExecutionID: &executionID, Prompt: "work"}
	prevArgv, prevBuild := restartSafeChildArgv, buildWorkerSubprocessEnv
	t.Cleanup(func() { restartSafeChildArgv, buildWorkerSubprocessEnv = prevArgv, prevBuild })
	restartSafeChildArgv = func(command []string, _ string, _, _ bool) (tools.GatewayChildDispatch, error) {
		return tools.GatewayChildDispatch{Mode: "degraded", Argv: command}, nil
	}
	t.Setenv("PYTHONPATH", filepath.Join(tmp, "raw-environ-only"))
	buildWorkerSubprocessEnv = func(context.Context, *tools.Env, tools.SubprocessEnvOptions) (map[string]string, error) {
		return map[string]string{"PATH": os.Getenv("PATH"), "PYTHONPATH": filepath.Join(tmp, "kept-by-sanitizer")}, nil
	}
	spawned := fakeExternalWorkerLaunch(t, executionID)
	key := s.inflightKey(job.ID, "")
	t.Cleanup(func() { inflight.Lock(); delete(inflight.workerPIDs, key); inflight.Unlock() })
	repoRoot := pm.RepoRoot()
	handed, err := s.launchExternalCronWorker(ctx, job)
	if err != nil || !handed {
		t.Fatalf("handed %v err %v", handed, err)
	}
	entries := strings.Split((*spawned)[0].env["PYTHONPATH"], string(os.PathListSeparator))
	if want := []string{repoRoot, filepath.Join(tmp, "kept-by-sanitizer")}; !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries %v want %v", entries, want)
	}
	prevPurelib := installedPurelib
	t.Cleanup(func() { installedPurelib = prevPurelib })
	installedPurelib = func() (string, bool) { return repoRoot, true }
	untouched := map[string]string{"PYTHONPATH": filepath.Join(tmp, "kept-by-sanitizer")}
	got, err := pinHermesTreeOnPythonpath(s, ctx, map[string]string{"PYTHONPATH": untouched["PYTHONPATH"]}, repoRoot)
	if err != nil || !reflect.DeepEqual(got, untouched) {
		t.Fatalf("purelib tree pinned %v err %v", got, err)
	}
	empty, err := pinHermesTreeOnPythonpath(s, ctx, map[string]string{}, repoRoot)
	if _, present := empty["PYTHONPATH"]; err != nil || present {
		t.Fatalf("purelib tree pinned %v err %v", empty, err)
	}
}

func TestPinRestoresTheCommittedGenerationSitePackages(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	ctx := context.Background()
	tmp := t.TempDir()
	repoRoot := filepath.Join(tmp, "icebath-agent")
	if err := os.Mkdir(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	venv := commitGeneration(t, ctx, repoRoot, "gen1", true)
	selected, err := pm.SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	env, err := pinHermesTreeOnPythonpath(s, ctx, map[string]string{"PYTHONPATH": filepath.Join(tmp, "kept")}, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Split(env["PYTHONPATH"], string(os.PathListSeparator)), []string{repoRoot, selected, filepath.Join(tmp, "kept")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("entries %v want %v", got, want)
	}
	if err := os.Remove(pm.EnvironmentRuntimeFactsPath(ctx, repoRoot)); err != nil {
		t.Fatal(err)
	}
	env, err = pinHermesTreeOnPythonpath(s, ctx, map[string]string{}, repoRoot)
	if err != nil || !reflect.DeepEqual(env, map[string]string{"PYTHONPATH": repoRoot}) {
		t.Fatalf("tree only pin %v err %v", env, err)
	}
}

func TestBootedWorkerDropsTheMarkerBeforeAnyJobChild(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prev := WorkerProcessBoot
	WorkerProcessBoot = func() {}
	t.Cleanup(func() { WorkerProcessBoot = prev })
	t.Setenv(workerMarker, "1")
	finishWorkerBoot()
	if _, present := os.LookupEnv(workerMarker); present {
		t.Fatal("finish_worker_boot left the marker for job children")
	}
}
