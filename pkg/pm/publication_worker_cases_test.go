package pm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Routes the next client sync of project through a fault worker child of this test binary
func pubFaultWorker(t *testing.T, tmp string, args map[string]string) {
	t.Helper()
	uv, python, _, _ := uvToolchain(context.Background(), true, true)
	args["uv"], args["python"] = uv, python
	encoded, _ := json.Marshal(args)
	t.Setenv(pubHelperEnv, "fault-worker")
	t.Setenv(pubHelperEnv+"_ARGS", string(encoded))
	peSwap(t, &repoRoot, func() string { return filepath.Join(tmp, "elsewhere") })
	peSwap(t, &workerArgv, func() ([]string, error) {
		return []string{os.Args[0], "-test.run=^TestPubHelperProcess$", "-test.count=1"}, nil
	})
}

func TestWorkerSyncDiscoversProfileMembersAfterWorkerAcquiresLock(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(fmt.Sprintf("explicit=%v", explicit), func(t *testing.T) {
			tmp, homeDir := pubClient(t)
			ctx := context.Background()
			sibling := filepath.Join(homeDir, "profiles", "sibling", "plugins", "dependency")
			peWrite(t, filepath.Join(sibling, "plugin.yaml"), "name: dependency\npython_dependencies: [fixture-dep==1]\n")
			repo := pubCurrentEnvironment(t, tmp, []string{sibling})
			peToolchain(t, peUV(t), peHostPython(t))
			wsHost(t, "")
			ready := make(chan struct{})
			var once bool
			original := venvRuntimeLock
			peSwap(t, &venvRuntimeLock, func(ctx context.Context, project string, timeout time.Duration) (bool, func(), error) {
				if !once {
					once = true
					close(ready)
				}
				return original(ctx, project, timeout)
			})
			release := pubRuntimeLock(t, ctx, repo)
			done := make(chan error, 1)
			go func() {
				done <- SyncVenv(ctx, SyncVenvOptions{Explicit: explicit, Plugins: &Selection{Data: map[string]any{
					"home": homeDir, "enabled": pubList("plain"), "disabled": pubList(),
				}}})
			}()
			select {
			case <-ready:
			case <-time.After(15 * time.Second):
				release()
				t.Fatal("worker never reached the install lock")
			}
			time.Sleep(50 * time.Millisecond)
			select {
			case err := <-done:
				release()
				t.Fatalf("worker ignored the install lock %v", err)
			default:
			}
			peWrite(t, filepath.Join(homeDir, "profiles", "sibling", "config.yaml"), "plugins:\n  enabled: [dependency]\n")
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("sync never finished")
			}
			if current, err := VenvIsCurrent(ctx, VenvIsCurrentOptions{}); err != nil || !current {
				t.Fatalf("current %v %v", current, err)
			}
		})
	}
}

func TestWorkerInvalidSelectionWaitsForFailedReceiptAndLockRelease(t *testing.T) {
	tmp, homeDir := pubClient(t)
	ctx := context.Background()
	repo := pubCurrentEnvironment(t, tmp, nil)
	peWrite(t, filepath.Join(homeDir, "config.yaml"), "plugins: []\n")
	err := SyncVenv(ctx, SyncVenvOptions{Extras: []string{}, Explicit: true, Plugins: &Selection{Data: map[string]any{
		"home": homeDir, "enabled": pubList(), "disabled": pubList(),
	}}})
	pubWantValueError(t, err, "plugins must be a mapping")
	receipts, _ := filepath.Glob(filepath.Join(homeDir, "logs", "update_receipts", "pm_*.json"))
	if len(receipts) != 1 {
		t.Fatalf("receipts %v", receipts)
	}
	var receipt map[string]any
	if json.Unmarshal([]byte(peRead(t, receipts[0])), &receipt); receipt["outcome"] != "failed" {
		t.Fatalf("receipt %v", receipt)
	}
	held, release, err := RuntimeLock(ctx, repo, 0)
	if err != nil || !held {
		t.Fatalf("lock still held %v %v", held, err)
	}
	release()
}

func TestWorkerResolutionConflictSurvivesWorkerAndReceipt(t *testing.T) {
	tmp, _ := pubClient(t)
	repo := pubCurrentEnvironment(t, tmp, nil)
	peWrite(t, filepath.Join(repo, "uv.lock"), "version = 2\n")
	peToolchain(t, peUV(t), peHostPython(t))
	pubFaultWorker(t, tmp, map[string]string{"fault": "conflict"})
	ctx := WithReceiptScope(context.Background())
	id := "conflict-update"
	var err error
	var result map[string]any
	stderr := carriedStderr(t, func() {
		restore := WorkerContext(ctx, &id)
		err = SyncVenv(ctx, SyncVenvOptions{ProjectRoot: repo, Extras: []string{}, Explicit: true, Plugins: &Members{Dirs: []MemberDir{}}})
		result = LastForUpdate(ctx, id, true)
		restore()
	})
	var conflict *ResolutionConflict
	if !errors.As(err, &conflict) || conflict.Package != "venv" || conflict.Cause != "impossible union" || conflict.Remedy != "change member" {
		t.Fatalf("conflict %T %v", err, err)
	}
	if result == nil || result["outcome"] != "failed" {
		t.Fatalf("receipt %v", result)
	}
	if !strings.Contains(stderr, "engine stdout") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestWorkerFailedFactsWriteRestoresExactConfigBeforeReporting(t *testing.T) {
	tmp, homeDir := pubClient(t)
	repo := pubCurrentEnvironment(t, tmp, nil)
	config := filepath.Join(homeDir, "config.yaml")
	peWrite(t, config, "# preserve me\nplugins: {enabled: [old]}\n")
	previous := peRead(t, config)
	facts := peRead(t, pubState(t, repo, "facts.json"))
	peWrite(t, filepath.Join(repo, "uv.lock"), "version = 2\n")
	peToolchain(t, peUV(t), peHostPython(t))
	pubFaultWorker(t, tmp, map[string]string{"fault": "facts", "config": config})
	err := SyncVenv(context.Background(), SyncVenvOptions{ProjectRoot: repo, Explicit: true, Plugins: &Selection{Data: map[string]any{
		"home": homeDir, "enabled": pubList("new"), "disabled": pubList(),
	}}})
	var py *PyError
	if !errors.As(err, &py) || py.Class != "OSError" || !strings.Contains(err.Error(), "facts disk full") {
		t.Fatalf("facts failure %T %v", err, err)
	}
	if peRead(t, config) != previous || peRead(t, pubState(t, repo, "facts.json")) != facts || pathExists(pubState(t, repo, "publication.json")) {
		t.Fatalf("config %q", peRead(t, config))
	}
}
