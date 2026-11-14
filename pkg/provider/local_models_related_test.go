package provider

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Stored zip archive holding the given files, zipfile's default of no compression
func lmArchive(t *testing.T, files map[string][]byte, order []string) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, name := range order {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Replaces a registered PM package for the rest of the test
func lmRegisterPackage(t *testing.T, p pm.Package) {
	t.Helper()
	original, err := pm.GetPackage(p.Base().Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Register(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pm.Register(original) })
}

// Points PM at a disposable lockfile for the rest of the test
func lmSwapLockfile(t *testing.T, path string) {
	t.Helper()
	prev := pm.SwapLockfilePath(func() string { return path })
	t.Cleanup(func() { pm.SwapLockfilePath(prev) })
}

func TestComponentJobPauseRetainsPinAndStopsSequence(t *testing.T) {
	for _, endpoint := range []string{"runtime/install", "quickstart"} {
		t.Run(endpoint, func(t *testing.T) {
			lmDownloadJobsClient(t)
			srv := newLMRangeServer(t)
			dir := t.TempDir()
			shipped, err := pm.GetPackage("llamacpp-cpu")
			if err != nil {
				t.Fatal(err)
			}
			cpu := shipped.(*pm.LlamaCpp)
			fixture := &pm.LlamaCpp{
				BinaryPackage: pm.BinaryPackage{
					BasePackage: pm.BasePackage{Name: "llamacpp-cpu", Optional: true, OffPath: true},
					BinaryRel:   map[string]string{"win32": "llama-server.exe", "posix": "llama-server"},
					NoFlatten:   true, ProbeCwd: true, NoProbe: true,
				},
				Backend: "cpu", Assets: cpu.Assets, Libgomp: cpu.Libgomp,
			}
			lmRegisterPackage(t, fixture)
			root := filepath.Join(dir, "store")
			lockPath := filepath.Join(dir, "lock.json")
			t.Setenv(pm.EnvRuntimeDir, root)
			lmSwapLockfile(t, lockPath)
			lmDeps.runtimeSection = func(context.Context) any { return map[string]any{"backend": "cpu"} }
			target, err := pm.CurrentTarget()
			if err != nil {
				t.Fatal(err)
			}
			binaryName := filepath.Base(fixture.Binary(root, target))
			files := map[string][]byte{binaryName: []byte("archive engine fixture"), "runtime.dll": bytes.Repeat(lmBytes256(), 32*1024)}
			order := []string{binaryName, "runtime.dll"}
			var pins []any
			var shas []string
			for index, name := range order {
				payload := lmArchive(t, map[string][]byte{name: files[name]}, []string{name})
				path := "/component-" + string(rune('0'+index)) + ".zip"
				srv.set(path, payload)
				sum := sha256.Sum256(payload)
				shas = append(shas, hex.EncodeToString(sum[:]))
				pins = append(pins, map[string]any{"url": srv.url(path), "sha256": shas[len(shas)-1]})
			}
			lock := pm.NewLockfile(lockPath)
			lock.SetPin(fixture.Name, "10362", map[string]any{target: pins})
			if err := lock.Save(); err != nil {
				t.Fatal(err)
			}
			entry := Catalog()[0]
			variant := entry.Variants[0]
			lmDeps.quickstartTarget = func(context.Context, string, HardwareBudget) (CatalogEntry, QuantVariant, error) {
				return entry, variant, nil
			}
			model := filepath.Join(localModelsDir(), "test.gguf")
			srv.set("/model", []byte("model fixture"))
			lmDeps.downloadPlan = func(CatalogEntry, QuantVariant) []lmPlanRow { return []lmPlanRow{{srv.url("/model"), model, 13}} }
			calls := &lmCalls{}
			lmDeps.ensureServer = func(context.Context, map[string]any, map[string]any, string, string, string) error {
				calls.add("server")
				return nil
			}
			lmDeps.assignDefault = func(context.Context, map[string]any, string) error {
				calls.add("default")
				return nil
			}
			lmDeps.getSupervisor = func() localModelsSupervisor { return nil }
			flowing, release, once := make(chan struct{}), make(chan struct{}), &sync.Once{}
			realHook := lmDeps.runtimeProgressHook
			lmDeps.runtimeProgressHook = func(job map[string]any) pm.InstallProgress {
				hook := realHook(job)
				var firstMu sync.Mutex
				var firstArchiveBytes int64
				return func(stage string, done, total int64, label string) error {
					if err := hook(stage, done, total, label); err != nil {
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
							case <-time.After(15 * time.Second):
								t.Error("release never came")
							}
						}
					}
					return nil
				}
			}
			ctx := context.Background()
			var resp map[string]any
			if endpoint == "runtime/install" {
				resp, err = LocalModelsRuntimeInstall(ctx, "cpu", "")
			} else {
				resp, err = LocalModelsQuickstart(ctx, "", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			jobID := resp["job_id"].(string)
			func() {
				defer close(release)
				select {
				case <-flowing:
				case <-time.After(5 * time.Second):
					view, _ := LocalModelsJob(ctx, jobID)
					t.Fatalf("runtime did not enter PM's pinned archive transfer: %v", view)
				}
				paused, err := LocalModelsDownloadPause(ctx, jobID)
				if err != nil || paused["paused"] != true {
					t.Fatalf("pause %v %v", paused, err)
				}
			}()
			job := lmPoll(t, jobID, 15*time.Second, "done", "error", "paused")
			if job["status"] != "paused" {
				t.Fatalf("job %v", job)
			}
			factsPath, err := pm.FactsPath(ctx)
			if err != nil {
				t.Fatal(err)
			}
			facts, err := pm.NewFacts(factsPath, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := facts.Get(fixture.Name); ok {
				t.Fatal("a paused install recorded its facts")
			}
			if got := calls.list(); len(got) != 0 || lrPathExists(model) {
				t.Fatalf("calls %v model %v", got, lrPathExists(model))
			}
			if job["can_resume"] != true {
				t.Fatalf("job %v", job)
			}
			if ranges := lmGlob(t, pm.PartialsRoot(), "*.ranges"); len(ranges) == 0 {
				t.Fatal("no durable ranges kept")
			}
			resumed, err := LocalModelsDownloadResume(ctx, jobID)
			if err != nil || resumed["resumed"] != true {
				t.Fatalf("resume %v %v", resumed, err)
			}
			job = lmPoll(t, jobID, 15*time.Second, "done", "error")
			if job["status"] != "done" {
				t.Fatalf("job %v", job)
			}
			selected, err := pm.Installed(ctx, fixture.Name, false)
			if err != nil || selected == nil {
				t.Fatalf("selected %v %v", selected, err)
			}
			for name, body := range files {
				if got, _ := os.ReadFile(filepath.Join(selected.Path, name)); !bytes.Equal(got, body) {
					t.Fatalf("%s %d bytes", name, len(got))
				}
			}
			facts, err = pm.NewFacts(factsPath, false)
			if err != nil {
				t.Fatal(err)
			}
			fact, _ := facts.Get(fixture.Name)
			var artifacts []string
			for _, a := range fact["artifacts"].([]any) {
				artifacts = append(artifacts, a.(string))
			}
			if !reflect.DeepEqual(artifacts, shas) {
				t.Fatalf("artifacts %v", artifacts)
			}
			if endpoint == "quickstart" {
				got, _ := os.ReadFile(model)
				if !reflect.DeepEqual(calls.list(), []any{"server", "default"}) || string(got) != "model fixture" {
					t.Fatalf("calls %v model %q", calls.list(), got)
				}
			}
			if lmRunningOf(jobID) != nil {
				t.Fatal("finished job kept its handles")
			}
		})
	}
}

// Seam restore for a test whose home a fixture of its own sets
func lmRestoreSeams(t *testing.T) {
	t.Helper()
	savedDeps, savedWatcher := lmDeps, loadEnsureWatcher
	loadEnsureWatcher = func() {}
	t.Cleanup(func() { lmDeps, loadEnsureWatcher = savedDeps, savedWatcher })
}

func TestStatusRouteReportsTheMovedEngine(t *testing.T) {
	lr3ForLegacyEnv(t, func(t *testing.T, env lr3LegacyEnv) {
		lmRestoreSeams(t)
		legacy := env.legacyDigests(t)
		env.install(t, env.version, legacy, true)
		lmDeps.runtimeSection = func(context.Context) any { return map[string]any{"enabled": true, "backend": env.backend} }
		lmDeps.stateEndpoint = func() (map[string]any, error) { return nil, nil }
		status, err := LocalModelsStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status["runtime_installed"] != true || status["runtime_backend"] != env.backend {
			t.Fatalf("status %v", status)
		}
		if status["tag"] != "b"+env.version || status["update_available"] != !reflect.DeepEqual(legacy, env.pinned) {
			t.Fatalf("status %v", status)
		}
	})
}

// Profile home under the root's profiles directory with its models dir and an identity marker
func lmLegacyProfile(t *testing.T, root, name string) string {
	t.Helper()
	h := filepath.Join(root, "profiles", name)
	if err := os.MkdirAll(filepath.Join(h, "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestBootAndStatusBothAdoptLegacyModels(t *testing.T) {
	root := t.TempDir()
	t.Setenv(home.EnvHome, root)
	lmRestoreSeams(t)
	savedBoot := bootInstalledEngine
	t.Cleanup(func() { bootInstalledEngine = savedBoot })
	bootInstalledEngine = func(context.Context, string) (*Engine, error) { return nil, nil }
	lmDeps.installedEngine = func(context.Context, string) (*Engine, error) { return nil, nil }
	work := lmLegacyProfile(t, root, "work")
	lmTouch(t, filepath.Join(work, "models", "booted.gguf"))
	if _, err := EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true}}, false); err != nil {
		t.Fatal(err)
	}
	if !lrPathExists(filepath.Join(localModelsDir(), "booted.gguf")) {
		t.Fatal("boot did not adopt the legacy model")
	}
	lmTouch(t, filepath.Join(work, "models", "listed.gguf"))
	lmDeps.runtimeSection = func(context.Context) any { return map[string]any{"enabled": false} }
	lmDeps.stateEndpoint = func() (map[string]any, error) { return nil, nil }
	status, err := LocalModelsStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, raw := range status["models"].([]any) {
		listed = listed || raw.(map[string]any)["id"] == "listed"
	}
	if !listed {
		t.Fatalf("status models %v", status["models"])
	}
}

func TestTheHeadDownloadsWithTheModelFromItsOwnRepoAndIsPricedBeforeDownload(t *testing.T) {
	lmTestHome(t, ".icebath")
	build := QuantVariant{Quant: "UD-IQ4_XS", Files: []AssetFile{
		{Path: "m/Head-Model-00001-of-00002.gguf", SizeBytes: 10 << 20, Revision: "main"},
		{Path: "m/Head-Model-00002-of-00002.gguf", SizeBytes: 40 * lmGiB, Revision: "main"}}}
	entry := CatalogEntry{ID: "head-model", DisplayName: "Head Model", Repo: "org/Head-Model-GGUF", Variants: []QuantVariant{build},
		NCtxTrain: 262144, FullLayers: 12, RecurrentLayers: 36, PerLayerF16: 2048, MoE: true, NVocab: 248320, MTPDraftDepth: 3,
		MTPHead: &AssetFile{Path: "mtp-Head-Model-Q8_0.gguf", SizeBytes: 4 * lmGiB, Repo: "other/Head-Model-GGUF", Revision: "0123abc"}}
	withoutHead := entry
	withoutHead.MTPHead = nil
	var urls []string
	for _, row := range lmDownloadPlan(entry, build) {
		urls = append(urls, row.URL)
	}
	found := false
	for _, u := range urls {
		found = found || u == "https://huggingface.co/other/Head-Model-GGUF/resolve/0123abc/mtp-Head-Model-Q8_0.gguf"
	}
	if !found {
		t.Fatalf("urls %v", urls)
	}
	for _, u := range urls[:2] {
		if !strings.Contains(u, "/org/Head-Model-GGUF/resolve/main/") {
			t.Fatalf("model url %q", u)
		}
	}
	if !entry.MTPCapable() || withoutHead.MTPCapable() {
		t.Fatal("only the head turns MTP on")
	}
	total := float64(128 * lmGiB)
	unified := NewHardwareBudget(int64(total*0.8), 128*lmGiB, 0)
	unified.UMA = true
	with := entry.LaunchPlan(build, unified)
	without := withoutHead.LaunchPlan(build, unified)
	if with.OverheadBytes < without.OverheadBytes+entry.MTPHead.SizeBytes {
		t.Fatalf("overhead %d vs %d", with.OverheadBytes, without.OverheadBytes)
	}
}

// Records an installed llamacpp-cpu engine of version in the store with the given artifact digest
func lmRecordEngine(t *testing.T, store, version, digest string) string {
	t.Helper()
	p, err := pm.GetPackage("llamacpp-cpu")
	if err != nil {
		t.Fatal(err)
	}
	target, err := pm.CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(store, p.StoreEntry(version, target))
	binary := p.Binary(entry, target)
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("installed engine fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	digestOf, err := pm.TreeDigest(entry)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := pm.NewFacts(filepath.Join(store, "facts.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.Record(p.Base().Name, version, filepath.Base(entry), pm.EnvDiff{}, store,
		pm.FactRecord{Target: &target, Artifacts: []string{digest}, Digest: &digestOf}); err != nil {
		t.Fatal(err)
	}
	return binary
}

func TestStatusUsesPmPinAndIgnoresLegacyTag(t *testing.T) {
	dir := t.TempDir()
	h := filepath.Join(dir, "home")
	store := filepath.Join(h, "tools")
	t.Setenv(home.EnvHome, h)
	t.Setenv(pm.EnvRuntimeDir, store)
	lockPath := filepath.Join(dir, "lock.json")
	lmSwapLockfile(t, lockPath)
	target, err := pm.CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	lock := pm.NewLockfile(lockPath)
	lock.SetPin("llamacpp-cpu", "10412", map[string]any{target: map[string]any{"url": "https://example.invalid/cpu.zip", "sha256": strings.Repeat("a", 64)}})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	lmRestoreSeams(t)
	var mu sync.Mutex
	section := map[string]any{"enabled": true, "backend": "cpu", "tag": "b99999"}
	lmDeps.runtimeSection = func(context.Context) any {
		mu.Lock()
		defer mu.Unlock()
		copied := map[string]any{}
		for k, v := range section {
			copied[k] = v
		}
		return copied
	}
	lmDeps.stateEndpoint = func() (map[string]any, error) { return nil, nil }
	lmRecordEngine(t, store, "10362", strings.Repeat("b", 64))
	status, err := LocalModelsStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	version, _ := pm.NewLockfile(lockPath).Version("llamacpp-cpu")
	if status["tag"] != "b10362" || status["configured_tag"] != "b"+version {
		t.Fatalf("status %v", status)
	}
	if status["runtime_installed"] != true || status["update_available"] != true {
		t.Fatalf("status %v", status)
	}
	mu.Lock()
	section["enabled"] = false
	mu.Unlock()
	if status, err := LocalModelsStatus(context.Background()); err != nil || status["update_available"] != false {
		t.Fatalf("disabled status %v %v", status, err)
	}
	mu.Lock()
	section["enabled"] = true
	mu.Unlock()
	lmRecordEngine(t, store, version, strings.Repeat("a", 64))
	if status, err := LocalModelsStatus(context.Background()); err != nil || status["update_available"] != false {
		t.Fatalf("current status %v %v", status, err)
	}
}

func TestMinEngineGate(t *testing.T) {
	lmTestHome(t, ".icebath")
	lmDeps.installedEngine = func(context.Context, string) (*Engine, error) {
		return &Engine{Backend: "cpu", Tag: "b10362", Binary: "unused"}, nil
	}
	for _, c := range []struct {
		minEngine string
		want      bool
	}{{"", false}, {"b10000", false}, {"b10363", true}} {
		if got, err := lmEngineTooOld(context.Background(), c.minEngine); err != nil || got != c.want {
			t.Fatalf("%q: %v %v", c.minEngine, got, err)
		}
	}
}

func TestCatalogAndPresetAgreeOnIdenticalModelFacts(t *testing.T) {
	dir := lmTestHome(t, ".icebath")
	dir = filepath.Dir(dir)
	t.Setenv(home.EnvHome, dir)
	lmDeps.engineTooOld = func(context.Context, string) (bool, error) { return false, nil }
	for _, entry := range Catalog() {
		variant := entry.Variants[0]
		profile := entry.Profile(variant)
		path := filepath.Join(dir, variant.ModelID()+".gguf")
		lr6FakeHeaders(t, func(string) ModelProfile { return profile })
		for _, companion := range []*AssetFile{entry.MMProj, entry.MTPHead} {
			if companion != nil {
				lmTouch(t, filepath.Join(localAssetsDir(), companion.LocalName()))
			}
		}
		for _, vram := range []int64{16, 24, 32, 48} {
			for _, uma := range []bool{false, true} {
				ram := 32 * lmGiB
				if uma {
					ram = 0
				}
				machine := NewHardwareBudget(int64(float64(vram*lmGiB)*0.8), vram*lmGiB, ram)
				machine.UMA = uma
				row, err := lmCatalogRow(context.Background(), entry, machine, nil, nil, map[string]bool{})
				if err != nil {
					t.Fatal(err)
				}
				preset, err := PresetForModel(path, machine, map[string]bool{}, 0, nil, 1, "")
				if err != nil {
					t.Fatal(err)
				}
				if row["fits"] != (preset.Refusal == "") {
					t.Fatalf("%s %d %v: fits %v refusal %q", entry.ID, vram, uma, row["fits"], preset.Refusal)
				}
				if preset.Refusal != "" {
					continue
				}
				if row["start_window"] != preset.Window || row["spilled"] != preset.Spilled {
					t.Fatalf("%s %d %v: row %v preset %+v", entry.ID, vram, uma, row, preset)
				}
				mtp := entry.MTPCapable()
				prefill := preset.Keys["ubatch-size"] == "2048" && mtp
				overhead := RuntimeOverheadBytes + entry.CompanionBytes() + UbLogitsBytes(profile.NVocab, mtp, prefill)
				posture := PostureProfile(profile, mtp, prefill)
				need := profile.WeightsBytes + CtxBytes(posture, preset.Window, true) + overhead
				if preset.Spilled != (need > machine.UsableVRAMBytes) || need > machine.UsableVRAMBytes+machine.RAMAvailableBytes {
					t.Fatalf("%s %d %v: need %d preset %+v", entry.ID, vram, uma, need, preset)
				}
			}
		}
	}
}
