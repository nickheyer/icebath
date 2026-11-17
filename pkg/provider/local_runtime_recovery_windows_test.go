//go:build windows

package provider

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Copies this test binary to dst so a process can run it under the server's name
func lr7CopyExe(t *testing.T, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, src); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// Whether the state file decodes to the same JSON object as state
func lr7StateIs(t *testing.T, path string, state map[string]any) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk, want map[string]any
	if json.Unmarshal(raw, &onDisk) != nil || json.Unmarshal(encoded, &want) != nil {
		return false
	}
	return reflect.DeepEqual(onDisk, want)
}

func TestStartupPreservesRecordedTrees(t *testing.T) {
	for _, c := range []string{"modern", "legacy", "live-owner", "pid-reused", "unknown-owner", "wrong-exe",
		"legacy-key", "legacy-models", "busy", "wrong-parent"} {
		t.Run(c, func(t *testing.T) { lr7StartupCase(t, c) })
	}
}

func lr7StartupCase(t *testing.T, c string) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "managed runtime home"))
	lr5ResetBoot(t)
	bootPresetsStaleFn = func() bool { return false }
	bootNvidiaQuery = func() (*NvidiaGPUQuery, error) { return nil, nil }
	bootGPUClass = func() string { return "" }
	exe := filepath.Join(RuntimesRoot(), "test-build", "cpu", "llama-server.exe")
	lr7CopyExe(t, exe)
	mdir := localModelsDir()
	receipts := filepath.Join(tmp, "receipts")
	for _, d := range []string{mdir, receipts} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seconds := "1"
	if c == "live-owner" {
		seconds = "60"
	}
	owner := lr3HelperCmd(receipts, "lr7owner")
	owner.Env = append(owner.Env, "ICEBATH_LRT_ROUTER_EXE="+exe, "ICEBATH_LRT_MODELS_DIR="+mdir, "ICEBATH_LRT_OWNER_SECONDS="+seconds)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = owner.Wait(); close(exited) }()
	var tree []lr3Identity
	t.Cleanup(func() {
		for _, id := range tree {
			lr3Kill(id)
		}
		_ = owner.Process.Kill()
		<-exited
	})
	ownerID := lr3Read(t, filepath.Join(receipts, "lr7owner.json"))
	router := lr3Read(t, filepath.Join(receipts, "lr7router.json"))
	child := lr3Read(t, filepath.Join(receipts, "lr7child.json"))
	tree = []lr3Identity{router, child}
	if c != "live-owner" {
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Fatal("owner did not exit")
		}
	}
	lr5Touch(t, filepath.Join(mdir, "model.gguf"), "GGUF")
	routerExe, err := proc.Exe(router.PID)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"pid": router.PID, "create_time": router.Created, "owner_pid": ownerID.PID,
		"owner_create_time": ownerID.Created, "executable": routerExe, "base_url": "http://127.0.0.1:59999/v1", "api_key": "test-only"}
	switch c {
	case "legacy", "legacy-key", "legacy-models":
		state = map[string]any{"pid": state["pid"], "base_url": state["base_url"], "api_key": state["api_key"]}
	}
	switch c {
	case "pid-reused":
		state["create_time"] = router.Created - 10
	case "unknown-owner":
		delete(state, "owner_create_time")
	case "wrong-exe":
		state["executable"] = filepath.Join(tmp, "other.exe")
	case "legacy-key":
		state["api_key"] = "different-key"
	case "legacy-models":
		prev := lrModelsDir
		t.Cleanup(func() { lrModelsDir = prev })
		lrModelsDir = func() string { return filepath.Join(tmp, "other models") }
	case "wrong-parent":
		state["owner_pid"] = os.Getpid()
	}
	statePath := lrStatePath()
	lr4WriteState(t, statePath, state)
	alive := func() bool { return lr3Alive(router) && lr3Alive(child) }
	for range 2 {
		if _, err := EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true}}, false); err != nil {
			t.Fatal(err)
		}
		if !alive() {
			t.Fatalf("startup killed: %s", c)
		}
		if !lr7StateIs(t, statePath, state) {
			t.Fatalf("startup rewrote the state: %s", c)
		}
	}
}
