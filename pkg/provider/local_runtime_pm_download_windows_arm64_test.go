//go:build windows && arm64

package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Answers the engine probes the acceptance run makes when this test binary runs as llama-server.exe
func init() {
	if !strings.EqualFold(filepath.Base(os.Args[0]), "llama-server.exe") || len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "--version":
		fmt.Println("version: 11671 (fixture)")
		os.Exit(0)
	case "--list-devices":
		fmt.Println("Available devices:")
		os.Exit(0)
	}
}

func TestLivePinnedCudaDownloadPauseResumeAndExecute(t *testing.T) {
	dir := t.TempDir()
	h := lmTestHome(t, "home")
	store := filepath.Join(h, "tools")
	t.Setenv(pm.EnvRuntimeDir, store)
	target, err := pm.CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	shipped := pm.NewLockfile(pm.LockfilePath())
	version, _ := shipped.Version("llamacpp-cuda")
	srv := newLMRangeServer(t)
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	contents := [][]string{{"llama-server.exe", "ggml-cuda.dll"}, {"cudart64_13.dll"}}
	bodies := map[string][]byte{"llama-server.exe": self, "ggml-cuda.dll": bytes.Repeat([]byte("ggml"), 1024),
		"cudart64_13.dll": bytes.Repeat(lmBytes256(), 64*1024)}
	var pins []any
	var pinned []string
	for i, row := range shipped.Artifacts("llamacpp-cuda", target) {
		name := filepath.Base(fmt.Sprint(row["url"]))
		files := map[string][]byte{}
		for _, f := range contents[i] {
			files[f] = bodies[f]
		}
		payload := lmArchive(t, files, contents[i])
		srv.set("/"+name, payload)
		sum := sha256.Sum256(payload)
		pinned = append(pinned, hex.EncodeToString(sum[:]))
		pins = append(pins, map[string]any{"url": srv.url("/" + name), "sha256": pinned[i]})
	}
	lockPath := filepath.Join(dir, "lock.json")
	lock := pm.NewLockfile(lockPath)
	lock.SetPin("llamacpp-cuda", version, map[string]any{target: pins})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	lmSwapLockfile(t, lockPath)
	var mu sync.Mutex
	var snapshots [][2]int64
	flowing, release, once := make(chan struct{}), make(chan struct{}), &sync.Once{}
	stageHook, byteHook := lmDeps.runtimeProgressHook, lmDeps.downloadProgressHook
	lmDeps.downloadProgressHook = func(job map[string]any) pm.ProgressFunc {
		original := byteHook(job)
		return func(done, total int64, ranges []pm.RangeSet) error {
			if err := original(done, total, ranges); err != nil {
				return err
			}
			mu.Lock()
			snapshots = append(snapshots, [2]int64{done, total})
			mu.Unlock()
			return nil
		}
	}
	lmDeps.runtimeProgressHook = func(job map[string]any) pm.InstallProgress {
		original := stageHook(job)
		var firstMu sync.Mutex
		var firstArchiveBytes int64
		return func(stage string, done, total int64, label string) error {
			if err := original(stage, done, total, label); err != nil {
				return err
			}
			firstMu.Lock()
			if stage == "download" && label == "1/2" {
				firstArchiveBytes = done
			}
			first := firstArchiveBytes
			firstMu.Unlock()
			if stage == "download" && label == "2/2" && first < done && done < total {
				opened := false
				once.Do(func() { opened = true })
				if opened {
					close(flowing)
					select {
					case <-release:
					case <-time.After(30 * time.Second):
						t.Error("release never came")
					}
				}
			}
			return nil
		}
	}
	ctx := context.Background()
	resp, err := LocalModelsRuntimeInstall(ctx, "cuda", "")
	if err != nil {
		t.Fatal(err)
	}
	jobID := resp["job_id"].(string)
	func() {
		defer close(release)
		select {
		case <-flowing:
		case <-time.After(240 * time.Second):
			view, _ := LocalModelsJob(ctx, jobID)
			t.Fatalf("download never flowed: %v", view)
		}
		paused, err := LocalModelsDownloadPause(ctx, jobID)
		if err != nil || paused["paused"] != true {
			t.Fatalf("pause %v %v", paused, err)
		}
	}()
	pausedJob := lmPoll(t, jobID, 240*time.Second, "paused", "done", "error")
	if pausedJob["status"] != "paused" {
		t.Fatalf("job %v", pausedJob)
	}
	if engine, err := InstalledEngine(ctx, "cuda", true); err != nil || engine != nil {
		t.Fatalf("paused install left an engine %v %v", engine, err)
	}
	if resumed, err := LocalModelsDownloadResume(ctx, jobID); err != nil || resumed["resumed"] != true {
		t.Fatalf("resume %v %v", resumed, err)
	}
	completed := lmPoll(t, jobID, 240*time.Second, "done", "error")
	if completed["status"] != "done" {
		t.Fatalf("job %v", completed)
	}
	engine, err := InstalledEngine(ctx, "cuda", false)
	if err != nil || engine == nil {
		t.Fatalf("engine %v %v", engine, err)
	}
	factsPath, err := pm.FactsPath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := pm.NewFacts(factsPath, false)
	if err != nil {
		t.Fatal(err)
	}
	fact, _ := facts.Get("llamacpp-cuda")
	var artifacts []string
	for _, a := range fact["artifacts"].([]any) {
		artifacts = append(artifacts, a.(string))
	}
	if !reflect.DeepEqual(artifacts, pinned) {
		t.Fatalf("artifacts %v", artifacts)
	}
	mu.Lock()
	dones := make([]int64, len(snapshots))
	for i, s := range snapshots {
		dones[i] = s[0]
	}
	mu.Unlock()
	if !sort.SliceIsSorted(dones, func(i, j int) bool { return dones[i] < dones[j] }) {
		t.Fatalf("byte counter went backwards %v", dones)
	}
	if completed["done_bytes"] != completed["total_bytes"] || completed["done_bytes"].(int64) <= 0 {
		t.Fatalf("bytes %v of %v", completed["done_bytes"], completed["total_bytes"])
	}
	receipt := map[string]any{"target": target, "tag": engine.Tag, "download_bytes": completed["done_bytes"],
		"pause_bytes": pausedJob["done_bytes"], "artifacts": pins, "binary": engine.Binary, "commands": map[string]any{}}
	for _, argument := range []string{"--version", "--list-devices"} {
		cmd := exec.Command(engine.Binary, argument)
		cmd.Dir = filepath.Dir(engine.Binary)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v %s", engine.Binary, argument, err, out)
		}
		receipt["commands"].(map[string]any)[argument] = string(out)
	}
	dlls, err := filepath.Glob(filepath.Join(filepath.Dir(engine.Binary), "*.dll"))
	if err != nil || len(dlls) == 0 {
		t.Fatalf("dlls %v %v", dlls, err)
	}
	sort.Strings(dlls)
	var names []string
	for _, dll := range dlls {
		names = append(names, filepath.Base(dll))
	}
	receipt["dlls"] = names
	evidence, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "acceptance.json"), evidence, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Log(string(evidence))
}
