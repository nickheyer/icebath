package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Supervisor double answering a fixed model listing
type lmFakeSupervisor struct {
	models RouterModels
}

func (s *lmFakeSupervisor) Models(time.Duration) (RouterModels, error) { return s.models, nil }

func (s *lmFakeSupervisor) UnloadModel(string) error { return nil }

// Router listing of model ids to status values
func lmRouterModels(statuses map[string]any) RouterModels {
	out := RouterModels{Status: map[string]any{}}
	for id, status := range statuses {
		out.IDs = append(out.IDs, id)
		out.Status[id] = status
	}
	return out
}

func TestActivateRescansThroughStateEndpointWhenSupervisorIsNone(t *testing.T) {
	dir := filepath.Dir(lmTestHome(t, ".icebath"))
	lmWriteFakeGGUF(t, filepath.Join(dir, "models", "Qwen3-4B-Q4_K_M.gguf"), 4)
	refreshes := 0
	known := []any{map[string]any{"id": "some-other-model"}}
	job := lmNewJob("model-activate", "Qwen3-4B-Q4_K_M", "Qwen3-4B-Q4_K_M")
	lmDeps.ensureLocalRuntime = func(context.Context, map[string]any, bool) (localModelsSupervisor, error) { return nil, nil }
	lmDeps.stateEndpoint = func() (map[string]any, error) {
		return map[string]any{"base_url": "http://127.0.0.1:18434/v1", "api_key": "k"}, nil
	}
	lmDeps.refreshLocalRuntime = func(context.Context) (bool, error) { refreshes++; return false, nil }
	lmDeps.routerRequest = func(endpoint map[string]any, path string, timeout time.Duration, payload []byte) (any, error) {
		if path != "/models" {
			t.Fatalf("router path %q", path)
		}
		return map[string]any{"data": known}, nil
	}
	lmDeps.setRuntimeEnabled = func(context.Context, bool) (map[string]any, error) { return map[string]any{}, nil }
	lmDeps.applyAssignment = func(context.Context, string, string, string, string, string, string) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}
	cfg := map[string]any{"local_runtime": map[string]any{"enabled": true}}
	if err := lmEnsureServer(context.Background(), job, cfg, "Qwen3-4B-Q4_K_M", "server failed", "skipped"); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 {
		t.Fatalf("the router must be bounced when its listing lacks the model, %d bounces", refreshes)
	}
	lmJobsMu.Lock()
	job["status"] = "done"
	lmJobsMu.Unlock()
}

func TestActivateSkipsRescanWhenOwnedServerAlreadyListsTheModel(t *testing.T) {
	lmTestHome(t, ".icebath")
	refreshes := 0
	job := lmNewJob("model-activate", "Qwen3-4B-Q4_K_M", "Qwen3-4B-Q4_K_M")
	sup := &lmFakeSupervisor{models: lmRouterModels(map[string]any{"Qwen3-4B-Q4_K_M": "loaded"})}
	lmDeps.ensureLocalRuntime = func(context.Context, map[string]any, bool) (localModelsSupervisor, error) { return sup, nil }
	lmDeps.refreshLocalRuntime = func(context.Context) (bool, error) { refreshes++; return false, nil }
	cfg := map[string]any{"local_runtime": map[string]any{"enabled": true}}
	if err := lmEnsureServer(context.Background(), job, cfg, "Qwen3-4B-Q4_K_M", "server failed", "skipped"); err != nil {
		t.Fatal(err)
	}
	if refreshes != 0 {
		t.Fatalf("no bounce when the listing already knows the model, %d bounces", refreshes)
	}
	lmJobsMu.Lock()
	job["status"] = "done"
	lmJobsMu.Unlock()
}

// local_runtime.enabled of the temp home's config
func lmRuntimeEnabled(t *testing.T) any {
	t.Helper()
	cfg, err := configLoadFn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cfg["local_runtime"].(map[string]any)["enabled"]
}

func TestStopDisablesAndTearsDown(t *testing.T) {
	lmTestHome(t, ".icebath")
	stopped := false
	lmDeps.getSupervisor = func() localModelsSupervisor { return &lmFakeSupervisor{} }
	lmDeps.shutdownLocalRuntime = func() error { stopped = true; return nil }
	if _, err := LocalModelsServer(context.Background(), "stop"); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("server not torn down")
	}
	if got := lmRuntimeEnabled(t); got != false {
		t.Fatalf("enabled %v", got)
	}
}

func TestStartEnablesAndBoots(t *testing.T) {
	lmTestHome(t, ".icebath")
	booted := false
	lmDeps.ensureLocalRuntime = func(ctx context.Context, cfg map[string]any, force bool) (localModelsSupervisor, error) {
		booted = true
		if !force {
			t.Fatal("start must force the boot")
		}
		return &lmFakeSupervisor{}, nil
	}
	if _, err := LocalModelsServer(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	if !booted {
		t.Fatal("server not booted")
	}
	if got := lmRuntimeEnabled(t); got != true {
		t.Fatalf("enabled %v", got)
	}
}

func TestBogusActionRejected(t *testing.T) {
	lmTestHome(t, ".icebath")
	if _, err := LocalModelsServer(context.Background(), "reboot"); lmStatusOf(err) != 400 {
		t.Fatalf("reboot %v", err)
	}
}

func TestStatusReportsLoadedModelsFromLiveRouter(t *testing.T) {
	lmTestHome(t, ".icebath")
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{"data": []any{
			map[string]any{"id": "m-loaded", "status": map[string]any{"value": "loaded"}},
			map[string]any{"id": "m-loading", "status": map[string]any{"value": "loading"}},
			map[string]any{"id": "m-cold", "status": map[string]any{"value": "unloaded"}},
		}})
		_, _ = w.Write(body)
	}))
	defer router.Close()
	lmDeps.stateEndpoint = func() (map[string]any, error) {
		return map[string]any{"base_url": router.URL + "/v1", "api_key": "k"}, nil
	}
	payload, err := LocalModelsStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if payload["server_running"] != true {
		t.Fatalf("server running %v", payload["server_running"])
	}
	want := map[string]any{"m-loaded": "loaded", "m-loading": "loading"}
	if !reflect.DeepEqual(payload["loaded_models"], want) {
		t.Fatalf("loaded %v", payload["loaded_models"])
	}
}

func TestStopForwardsRecoveryAndPreservesConflict(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "refuse=False", true: "refuse=True"}[refuse], func(t *testing.T) {
			lmTestHome(t, ".icebath")
			if err := os.MkdirAll(RuntimesRoot(), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(localRuntimeStatePath(), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			lmDeps.getSupervisor = func() localModelsSupervisor { return nil }
			lmDeps.stateEndpoint = func() (map[string]any, error) { return nil, nil }
			var called []bool
			var disabled []bool
			lmDeps.terminateStatePID = func() error {
				called = append(called, true)
				if refuse {
					return httpException(409, "Another Icebath process owns this server, or its ownership could not be verified")
				}
				return nil
			}
			lmDeps.setRuntimeEnabled = func(ctx context.Context, enabled bool) (map[string]any, error) {
				disabled = append(disabled, enabled)
				return map[string]any{}, nil
			}
			_, err := LocalModelsServer(context.Background(), "stop")
			wantStatus := 0
			if refuse {
				wantStatus = 409
			}
			if lmStatusOf(err) != wantStatus || (!refuse && err != nil) {
				t.Fatalf("stop %v", err)
			}
			if refuse && !strings.Contains(err.(*HTTPException).Detail, "ownership could not be verified") {
				t.Fatalf("detail %v", err)
			}
			if len(called) != 1 {
				t.Fatalf("recovery calls %v", called)
			}
			want := []bool{false}
			if refuse {
				want = nil
			}
			if !reflect.DeepEqual(disabled, want) {
				t.Fatalf("disabled %v", disabled)
			}
		})
	}
}
