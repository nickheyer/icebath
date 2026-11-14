package provider

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const auxLlamacppAliasModel = "SmolLM2-135M-Instruct-Q4_K_M"

// Real supervised child process whose server.json the write callback points at a port and key
func auxManagedServer(t *testing.T) (context.Context, func(port int, key string)) {
	t.Helper()
	auxIsolateClientCache(t)
	prevPorts := DefaultProbePorts
	DefaultProbePorts = nil
	t.Cleanup(func() { DefaultProbePorts = prevPorts })
	t.Setenv("GEMINI_API_KEY", "cloud-key-must-not-be-used")
	ctx := auxMainTestHome(t, "")
	auxResetManagedLocalCache()
	t.Cleanup(auxResetManagedLocalCache)
	child := exec.Command("sleep", "120")
	child.Stdin = nil
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
		_ = os.Remove(localRuntimeStatePath())
	})
	proc, err := openPSProcess(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	created, err := proc.CreateTime()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := proc.Exe()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := openPSProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	ownerCreated, err := owner.CreateTime()
	if err != nil {
		t.Fatal(err)
	}
	write := func(port int, key string) {
		t.Helper()
		path := localRuntimeStatePath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]any{"base_url": "http://127.0.0.1:" + strconv.Itoa(port) + "/v1", "api_key": key, "pid": child.Process.Pid,
			"create_time": created, "executable": exe, "owner_pid": os.Getpid(), "owner_create_time": ownerCreated})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		auxResetManagedLocalCache()
	}
	return ctx, write
}

func TestFallbackResolutionUsesLiveManagedServerOrNothing(t *testing.T) {
	ctx, write := auxManagedServer(t)
	write(18001, "key-a")
	client, model, err := ResolveProviderClient(ctx, "llamacpp", auxLlamacppAliasModel, ProviderClientOptions{RawCodex: true})
	if err != nil || client == nil || !strings.HasPrefix(client.BaseURL(), "http://127.0.0.1:18001/v1") {
		t.Fatalf("got (%v, %q, %v)", client, model, err)
	}
	if client.APIKey() != "key-a" || model != auxLlamacppAliasModel {
		t.Fatalf("key %q model %q", client.APIKey(), model)
	}
	if err := os.Remove(localRuntimeStatePath()); err != nil {
		t.Fatal(err)
	}
	auxResetManagedLocalCache()
	client, model, err = ResolveProviderClient(ctx, "llamacpp", auxLlamacppAliasModel, ProviderClientOptions{RawCodex: true})
	if err != nil || client != nil || model != "" {
		t.Fatalf("server off must resolve to nothing, got (%v, %q, %v)", client, model, err)
	}
}

func TestCachedClientFollowsManagedEndpointRestart(t *testing.T) {
	ctx, write := auxManagedServer(t)
	write(18001, "key-a")
	req := auxCachedClientRequest{Provider: "llama.cpp", Model: auxLlamacppAliasModel, Task: "compression"}
	first, _, err := auxGetCachedClient(ctx, req)
	if err != nil || first == nil || !strings.HasPrefix(first.BaseURL(), "http://127.0.0.1:18001/v1") {
		t.Fatalf("got (%v, %v)", first, err)
	}
	if again, _, err := auxGetCachedClient(ctx, req); err != nil || again != first {
		t.Fatalf("cache missed a live managed endpoint: (%v, %v)", again, err)
	}
	write(18002, "key-b")
	second, _, err := auxGetCachedClient(ctx, req)
	if err != nil || second == nil || !strings.HasPrefix(second.BaseURL(), "http://127.0.0.1:18002/v1") || second.APIKey() != "key-b" {
		t.Fatalf("restart not followed: (%v, %v)", second, err)
	}
	if err := os.Remove(localRuntimeStatePath()); err != nil {
		t.Fatal(err)
	}
	auxResetManagedLocalCache()
	if client, model, err := auxGetCachedClient(ctx, req); err != nil || client != nil || model != "" {
		t.Fatalf("server off must yield nothing, got (%v, %q, %v)", client, model, err)
	}
}
