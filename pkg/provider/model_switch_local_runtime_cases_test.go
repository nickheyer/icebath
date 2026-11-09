package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Writes the managed llama-server state owned by this test process pointing at the port
func writeManagedLocalState(t *testing.T, port int, apiKey string) {
	t.Helper()
	self, err := openPSProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	created, err := self.CreateTime()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := self.Exe()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := openPSProcess(os.Getppid())
	if err != nil {
		t.Fatal(err)
	}
	ownerCreated, err := owner.CreateTime()
	if err != nil {
		t.Fatal(err)
	}
	path := localRuntimeStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"base_url": "http://127.0.0.1:" + strconv.Itoa(port) + "/v1", "api_key": apiKey, "pid": os.Getpid(),
		"create_time": created, "executable": exe, "owner_pid": os.Getppid(), "owner_create_time": ownerCreated})
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	auxResetManagedLocalCache()
	t.Cleanup(auxResetManagedLocalCache)
}

func stageLocalGGUF(t *testing.T, h, modelID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(h, "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "models", modelID+".gguf"), []byte("GGUF"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func noDefaultProbePorts(t *testing.T) {
	t.Helper()
	prev := DefaultProbePorts
	DefaultProbePorts = nil
	t.Cleanup(func() { DefaultProbePorts = prev })
}

func TestSwitchModelExplicitLlamacppProvider(t *testing.T) {
	port := lr2LlamaServer(t, 200, map[string]any{}, map[string]any{"data": []any{map[string]any{"id": "stub-model-a", "owned_by": "llamacpp"}}})
	ctx, _ := switchAliasHome(t, "")
	writeManagedLocalState(t, port, "sk-managed")
	fakeSwitchPipeline(t, false)
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "stub-model-a", CurrentProvider: "nous", ExplicitProvider: "llamacpp"})
	if !res.Success {
		t.Fatal(res.ErrorMessage)
	}
	if !strings.Contains(res.BaseURL, "127.0.0.1:"+strconv.Itoa(port)) || res.APIKey != "sk-managed" {
		t.Fatalf("result %+v", res)
	}
}

func TestStagedLocalModelResolvesWithoutARunningServer(t *testing.T) {
	ctx, h := switchAliasHome(t, "")
	stageLocalGGUF(t, h, "Qwen3.8-27B-IQ3_S-mtp")
	noDefaultProbePorts(t)
	pdef := ResolveProviderFull(ctx, LlamacppProviderID, map[string]any{}, []any{})
	if pdef == nil {
		t.Fatal("staged model, but the picker's own provider id does not resolve")
	}
	if pdef.ID != LlamacppProviderID {
		t.Fatalf("pdef %+v", pdef)
	}
	fakeSwitchPipeline(t, false)
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "Qwen3.8-27B-IQ3_S-mtp", CurrentProvider: "nous", ExplicitProvider: LlamacppProviderID})
	if res.Success {
		t.Fatalf("result %+v", res)
	}
	if strings.Contains(res.ErrorMessage, "Unknown provider") || !strings.Contains(strings.ToLower(res.ErrorMessage), "local model server") {
		t.Fatal(res.ErrorMessage)
	}
}

func TestExternalServerOnAConfiguredDetectPortIsUsed(t *testing.T) {
	port := lr2LlamaServer(t, 200, map[string]any{"build_info": "b10964-test", "model_path": "/models/ext-model.gguf",
		"default_generation_settings": map[string]any{"n_ctx": 4096}}, map[string]any{"data": []any{map[string]any{"id": "ext-model", "owned_by": "llamacpp"}}})
	ctx, h := switchAliasHome(t, "local_runtime:\n  enabled: false\n  detect_ports: ["+strconv.Itoa(port)+"]\n")
	stageLocalGGUF(t, h, "ext-model")
	noDefaultProbePorts(t)
	fakeSwitchPipeline(t, false)
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "ext-model", CurrentProvider: "nous", ExplicitProvider: "llamacpp"})
	if !res.Success {
		t.Fatal(res.ErrorMessage)
	}
	if res.BaseURL != "http://127.0.0.1:"+strconv.Itoa(port)+"/v1" {
		t.Fatalf("result %+v", res)
	}
}
