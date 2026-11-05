//go:build !windows

package provider

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestLocalRuntimeSupervisorWithPresetsDoesNotScanUnadmittedFiles(t *testing.T) {
	dir := lr5Home(t)
	lr5ResetSupervisorSeams(t)
	ini := filepath.Join(dir, "presets.ini")
	lr5Touch(t, ini, "[allowed]\nmodel = allowed.gguf\nctx-size = 65536\n")
	binary, _ := lr5HelpScript(t, "--load-mode MODE", false)
	var argv, envs [][]string
	supSpawnServer = lr5RecordingSpawn(t.TempDir(), &argv, &envs)
	sup, err := NewLlamaServerSupervisor(binary, dir, 4, 1234, nil, "", ini)
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.spawn(); err != nil {
		t.Fatal(err)
	}
	_ = sup.Stop()
	cmd := argv[0]
	if cmd[0] != binary || !slices.Contains(cmd, "--models-preset") || slices.Contains(cmd, "--models-dir") ||
		!slices.Contains(cmd, "--no-ui") || slices.Contains(cmd, "--no-webui") {
		t.Fatal(cmd)
	}
}

// Live router over a fake listing that counts preset reloads, its process a spawned sleeper
type lr6Router struct {
	sup     *LlamaServerSupervisor
	mu      sync.Mutex
	status  string
	reloads int
}

func lr6NewRouter(t *testing.T, path, status string) *lr6Router {
	t.Helper()
	lr5ResetSupervisorSeams(t)
	r := &lr6Router{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Query().Get("reload") == "1" {
			r.reloads++
			_, _ = w.Write([]byte(`{"success": true}`))
			return
		}
		raw, _ := json.Marshal(lr5Models([2]string{lr6Model27B, r.status}))
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	var argv, envs [][]string
	supDirectIOArgs = func(string) ([]string, error) { return []string{}, nil }
	supSpawnServer = lr5RecordingSpawn(t.TempDir(), &argv, &envs)
	sup, err := NewLlamaServerSupervisor(filepath.Join(t.TempDir(), "llama-server"), localModelsDir(), 1,
		srv.Listener.Addr().(*net.TCPAddr).Port, nil, "", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.spawn(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Stop() })
	r.sup = sup
	return r
}

func lr6Refit(t *testing.T, r *lr6Router) bool {
	t.Helper()
	ok, err := refitIdlePresets(r.sup)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// Boots presets for the 27B model against a card where others GiB are held
func lr6Booted(t *testing.T, others float64) (string, string) {
	t.Helper()
	dir, mdir, _ := lr6Stage27B(t)
	lr6Card(others, lr6CardTotal())
	path := filepath.Join(dir, "presets.ini")
	generateBootPresets(mdir, path, 1, "")
	return path, mdir
}

func TestLocalRuntimeIdleRouterGetsWindowsForTheCurrentDesktop(t *testing.T) {
	lr5ResetBoot(t)
	path, _ := lr6Booted(t, 6.5)
	lr6CapacityProbe(t)
	if readPresetDecisions(path)[lr6Model27B].Window != 96*1024 {
		t.Fatal("boot window")
	}
	r := lr6NewRouter(t, path, "unloaded")
	lr6Card(2.0, lr6CardTotal())
	if !lr6Refit(t, r) || r.reloads != 1 || readPresetDecisions(path)[lr6Model27B].Window != 216*1024 {
		t.Fatal(r.reloads, readPresetDecisions(path)[lr6Model27B])
	}
	if lr6Refit(t, r) || r.reloads != 1 {
		t.Fatal("an unchanged desktop replanned")
	}
}

func TestLocalRuntimeAReplanKeepsTheDraftTheServingEngineLoads(t *testing.T) {
	lr5ResetBoot(t)
	dir, mdir, _ := lr6Stage27B(t)
	lr6CapacityProbe(t)
	lr6Touch(t, filepath.Join(localAssetsDir(), CatalogByID()["qwen3.8-27b"].Draft.LocalName()))
	bootMu.Lock()
	localServingEngine = &Engine{Tag: "b11671"}
	bootMu.Unlock()
	lr6Card(2.0, lr6CardTotal())
	path := filepath.Join(dir, "presets.ini")
	generateBootPresets(mdir, path, 1, "b11671")
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "spec-type = draft-dflash") {
		t.Fatal(string(raw))
	}
	lr6Card(0.0, lr6CardTotal())
	lr6Refit(t, lr6NewRouter(t, path, "unloaded"))
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "spec-type = draft-dflash") || !strings.Contains(string(raw), "parallel = 1") {
		t.Fatal(string(raw))
	}
}

func TestLocalRuntimeNeverRewritesWhileAModelHoldsMemory(t *testing.T) {
	for _, status := range []string{"loaded", "loading", "sleeping"} {
		t.Run(status, func(t *testing.T) {
			lr5ResetBoot(t)
			path, _ := lr6Booted(t, 6.5)
			lr6CapacityProbe(t)
			before, _ := os.ReadFile(path)
			r := lr6NewRouter(t, path, status)
			lr6Card(2.0, lr6CardTotal())
			if lr6Refit(t, r) || r.reloads != 0 {
				t.Fatal("rewrote under a resident model")
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Fatal("preset file changed")
			}
		})
	}
}

func TestLocalRuntimeSmallChangesInFreeMemoryDoNotReplan(t *testing.T) {
	lr5ResetBoot(t)
	path, _ := lr6Booted(t, 5.4)
	lr6CapacityProbe(t)
	r := lr6NewRouter(t, path, "unloaded")
	if lr6Refit(t, r) {
		t.Fatal("the boot reading must give the same file")
	}
	presetReadHeader = func(string) (*GGUFHeader, error) { t.Fatal("re-planned"); return nil, nil }
	lr6Card(5.5, lr6CardTotal())
	if lr6Refit(t, r) {
		t.Fatal("a small change replanned")
	}
}
