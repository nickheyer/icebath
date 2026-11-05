package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
)

// Process wide router singleton, the engine it serves and whether the exit hook is armed
var (
	bootMu             sync.Mutex
	localSupervisor    *LlamaServerSupervisor
	localServingEngine *Engine
	localExitHooked    bool
)

// Router statuses that hold GPU memory, presets change only while every model is out of them
var bootHoldsMemory = map[string]bool{"loaded": true, "loading": true, "sleeping": true, "ready": true}

// Free memory change below which a re-plan cannot move a window by a ladder rung
const bootRefitStepBytes = 256 << 20

// Idle sweep cadence, launch window re-plan cadence and the seams tests replace
var (
	bootSweepEvery        = 120.0
	bootRefitEvery        = 30 * time.Second
	bootSweeperSleep      = time.Sleep
	bootNvidiaQuery       = func() (*NvidiaGPUQuery, error) { return CachedNvidiaGPUQuery(hwGPUQueryTTLSeconds) }
	bootGPUClass          = hostinfo.GPUClass
	bootInstalledEngine   = func(ctx context.Context, backend string) (*Engine, error) { return InstalledEngine(ctx, backend, true) }
	bootStartSupervisor   = func(s *LlamaServerSupervisor) error { return s.Start(120 * time.Second) }
	bootStateEndpoint     = func(ctx context.Context) (bool, error) { _, _, ok, err := stateEndpoint(home.Dir(ctx)); return ok, err }
	bootPresetsStaleFn    = presetsStale
	bootAdmittedModelsMax = admittedModelsMax
	bootGeneratePresets   = generateBootPresets
	bootLockTimeout       = 130 * time.Second
	bootProbeBudget       = ProbeBudget
	bootStopStateServer   = stopStateServer
)

// GPU vendor for backend selection, the detailed NVIDIA name first, then the host GPU class, "" when unknown
func detectGPUVendor() (string, error) {
	query, err := bootNvidiaQuery()
	if err != nil {
		return "", err
	}
	if query != nil && query.GPUName != "" {
		return "nvidia " + query.GPUName, nil
	}
	switch detected := bootGPUClass(); detected {
	case "nvidia", "amd", "intel":
		return detected, nil
	}
	return "", nil
}

// Machine scoped models directory every profile's managed server shares
func localModelsDir() string {
	return filepath.Join(home.Root(), "models")
}

// Companion files directory that the router listing never mistakes for a servable model
func localAssetsDir() string {
	return filepath.Join(localModelsDir(), "assets")
}

// Companion files directory under the name the capabilities slice calls
func localRuntimeAssetsDir() string {
	return localAssetsDir()
}

// Entries of dir matching *.gguf in Python path order, nothing when the directory cannot be read
func lrGlobGGUF(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		name := e.Name()
		match := strings.HasSuffix(name, ".gguf")
		if runtime.GOOS == "windows" {
			match = strings.HasSuffix(strings.ToLower(name), ".gguf")
		}
		if match {
			out = append(out, filepath.Join(dir, name))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if runtime.GOOS == "windows" {
			return strings.ToLower(out[i]) < strings.ToLower(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// Servable GGUFs in a directory, a split once by its first part and only with every part present when required
func stagedIn(modelsDir string, requireComplete bool) []string {
	files := lrGlobGGUF(modelsDir)
	names := map[string]bool{}
	for _, p := range files {
		names[filepath.Base(p)] = true
	}
	var out []string
	for _, p := range files {
		name := filepath.Base(p)
		m := GGUFSplitPartRE.FindStringSubmatchIndex(name)
		if m == nil {
			out = append(out, p)
			continue
		}
		if name[m[2]:m[3]] != "00001" {
			continue
		}
		stem, totalText := name[:m[0]], name[m[4]:m[5]]
		total, _ := pythonUnicodeIntText(totalText)
		complete := true
		if requireComplete {
			for i := int64(2); i <= total; i++ {
				if !names[fmt.Sprintf("%s-%05d-of-%s.gguf", stem, i, totalText)] {
					complete = false
					break
				}
			}
		}
		if complete {
			out = append(out, p)
		}
	}
	return out
}

// Moves GGUFs from the old per profile models layout into the machine dirs by rename only, returning the new paths
func adoptLegacyModels() ([]string, error) {
	profilesRoot := filepath.Join(home.Root(), "profiles")
	if st, err := os.Stat(profilesRoot); err != nil || !st.IsDir() {
		return nil, nil
	}
	ents, err := os.ReadDir(profilesRoot)
	if err != nil {
		return nil, err
	}
	var moved []string
	for _, e := range ents {
		profile := filepath.Join(profilesRoot, e.Name())
		old := filepath.Join(profile, "models")
		if strings.HasPrefix(e.Name(), ".") || !lrIsDir(old) || !home.ProfileHasIdentity(profile) {
			continue
		}
		for _, pair := range [][2]string{{old, localModelsDir()}, {filepath.Join(old, "assets"), localAssetsDir()}} {
			srcDir, destDir := pair[0], pair[1]
			for _, src := range lrGlobGGUF(srcDir) {
				dest := filepath.Join(destDir, filepath.Base(src))
				if lrPathExists(dest) {
					slog.Warn(fmt.Sprintf("legacy model %s not moved: %s already exists", src, dest))
					continue
				}
				err := os.MkdirAll(destDir, 0o777)
				if err == nil {
					err = os.Rename(src, dest)
				}
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					slog.Warn(fmt.Sprintf("legacy model %s not moved to %s: %s", src, destDir, err))
					continue
				}
				moved = append(moved, dest)
			}
		}
		for _, emptied := range []string{filepath.Join(old, "assets"), old} {
			_ = lrRmdir(emptied)
		}
	}
	if len(moved) > 0 {
		slog.Info(fmt.Sprintf("moved %d legacy model file(s) into %s", len(moved), localModelsDir()))
	}
	return moved, nil
}

func lrIsDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Removes an empty directory, never its contents
func lrRmdir(p string) error {
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("not a directory: %s", p)
	}
	if ents, err := os.ReadDir(p); err != nil || len(ents) > 0 {
		return fmt.Errorf("directory not empty: %s", p)
	}
	return os.Remove(p)
}

// Servable staged models, continuation parts, incomplete splits and assets never count
func stagedModels() []string {
	return stagedIn(localModelsDir(), true)
}

// Router model ids of the staged models
func stagedModelIDs() []string {
	var out []string
	for _, p := range stagedModels() {
		out = append(out, ModelIDFromStem(pyPathStem(p)))
	}
	return out
}

// Boot entry point a refresh restarts through
var bootEnsureLocalRuntime func(ctx context.Context, cfg map[string]any, force bool) (*LlamaServerSupervisor, error)

func init() { bootEnsureLocalRuntime = EnsureLocalRuntime }

// Whether a staged model lacks a policy section in the preset file and would autoload with stock fit
func presetsStale() bool {
	known := readPresetDecisions("")
	for _, mid := range stagedModelIDs() {
		entry, ok := known[mid]
		if !ok || (entry.Refusal == "" && entry.Keys["model"] == "") {
			return true
		}
	}
	return false
}

// Stops the server the state file records before a replacement boots, but only an orphan whose owner is gone
func stopStateServer() error {
	state := readLocalRuntimeState()
	var proc localRuntimeProc
	if isModernLocalRuntimeState(state) {
		if !lrOwnerIsDead(state) {
			slog.Info(fmt.Sprintf("llama-server pid=%s belongs to a live icebath process; leaving it", pyStr(pyGetAny(state, "pid", nil))))
			return nil
		}
		proc = lrRecordedProcess(state)
	} else {
		legacy, err := lrLegacyRecordedProcess(state)
		if err != nil {
			return err
		}
		proc = legacy
	}
	if proc == nil {
		return nil
	}
	if err := lrTerminateTree(proc, true); err != nil {
		slog.Warn(fmt.Sprintf("could not stop the incumbent llama-server (pid=%d): %s", proc.Pid(), err))
	}
	return nil
}

// Restarts the managed server so the router rescans the models directory, false when nothing restarted
func refreshLocalRuntime() bool {
	ok, err := func() (bool, error) {
		ctx := context.Background()
		if getLocalSupervisor() == nil {
			live, err := bootStateEndpoint(ctx)
			if err != nil {
				return false, err
			}
			if !live {
				return false, nil
			}
			if err := bootStopStateServer(); err != nil {
				return false, err
			}
		} else if err := ShutdownLocalRuntime(); err != nil {
			return false, err
		}
		sup, err := bootEnsureLocalRuntime(ctx, loadConfigTree(home.Process()), true)
		return sup != nil, err
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("local runtime refresh failed: %s", err))
		return false
	}
	return ok
}

// Residency cap derived from the hardware budget with models_max as the ceiling, configured on a probe miss
func admittedModelsMax(mdir string, configured int, engineTag string) int {
	capacity, err := bootProbeBudget(true)
	if err != nil {
		slog.Warn(fmt.Sprintf("residency cap probe failed (%s); using models_max=%d", err, configured))
		return configured
	}
	var live *HardwareBudget
	if capacity.UMA {
		live = bootstrapLaunchBudget(capacity, 0)
	}
	capped, err := AdmittedResidencyCount(mdir, capacity, configured, live, engineTag)
	if err != nil {
		slog.Warn(fmt.Sprintf("residency cap probe failed (%s); using models_max=%d", err, configured))
		return configured
	}
	if capped != configured {
		slog.Info(fmt.Sprintf("residency cap: %d resident model(s) on this card (models_max=%d)", capped, configured))
	}
	return capped
}

// Writes the launch policy INI against capacity, keeping a previous INI on failure, "" when the router runs stock fit
func generateBootPresets(mdir, presetPath string, residents int, engineTag string) string {
	capacity, err := bootProbeBudget(true)
	var entries []*PresetEntry
	if err == nil {
		entries, err = GeneratePresets(mdir, capacity, presetPath, nil, bootstrapLaunchBudget(capacity, 0), residents, engineTag)
	}
	if err == nil {
		for _, entry := range entries {
			if entry.Refusal != "" {
				slog.Warn(fmt.Sprintf("model refused by physics check: %s", entry.Refusal))
			}
		}
		return presetPath
	}
	if lrPathExists(presetPath) {
		slog.Error(fmt.Sprintf("preset generation failed (%s); serving with the PREVIOUS launch policies — models staged since the last successful generation run unpoliced until this is fixed", err))
		return presetPath
	}
	slog.Error(fmt.Sprintf("preset generation failed (%s) and no previous policy file exists; router runs stock fit", err))
	return ""
}

// Launch budget beside other programs' GPU memory, nil when the probe fails so a boot never waits on it
func bootstrapLaunchBudget(capacity HardwareBudget, ownBytes int64) *HardwareBudget {
	live, err := LaunchBudget(capacity, ownBytes)
	if err != nil {
		slog.Warn(fmt.Sprintf("free GPU memory probe failed (%s); launch windows use capacity", err))
		return nil
	}
	return live
}

// Re-plans launch windows against free GPU memory while no model is loaded, true when the router got new presets
func refitIdlePresets(sup *LlamaServerSupervisor) (bool, error) {
	path := sup.PresetPath
	if path == "" || !lrPathExists(path) {
		return false, nil
	}
	if holds, err := anyHoldsMemory(sup); err != nil || holds {
		return false, err
	}
	capacity, err := bootProbeBudget(true)
	if err != nil {
		return false, err
	}
	live := bootstrapLaunchBudget(capacity, 0)
	if live == nil {
		return false, nil
	}
	sup.mu.Lock()
	last := sup.refitUsable
	sup.mu.Unlock()
	if last != nil && lrAbs64(live.UsableVRAMBytes-*last) < bootRefitStepBytes {
		return false, nil
	}
	engineTag := ""
	if engine := servingEngine(); engine != nil {
		engineTag = engine.Tag
	}
	entries, err := PlanPresets(localModelsDir(), capacity, nil, live, sup.ModelsMax, engineTag)
	if err != nil {
		return false, err
	}
	text := RenderPresets(entries)
	sup.lifecycle.Lock()
	defer sup.lifecycle.Unlock()
	proc := sup.currentProc()
	if proc == nil {
		return false, nil
	}
	if _, exited := proc.Poll(); exited {
		return false, nil
	}
	if holds, err := anyHoldsMemory(sup); err != nil || holds {
		return false, err
	}
	usable := live.UsableVRAMBytes
	sup.mu.Lock()
	sup.refitUsable = &usable
	sup.mu.Unlock()
	current, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	current = bytes.TrimPrefix(current, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(current) {
		return false, fmt.Errorf("'utf-8' codec can't decode %s", path)
	}
	if text == string(current) {
		return false, nil
	}
	if err := config.AtomicWrite(path, []byte(text), 0o600); err != nil {
		return false, err
	}
	if err := sup.ReloadPresets(); err != nil {
		return false, err
	}
	slog.Info(fmt.Sprintf("launch windows re-planned for %.1f GiB of free GPU memory", float64(live.UsableVRAMBytes)/float64(int64(1)<<30)))
	return true, nil
}

func lrAbs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Whether any router model sits in a status that holds GPU memory
func anyHoldsMemory(sup *LlamaServerSupervisor) (bool, error) {
	models, err := sup.Models(5 * time.Second)
	if err != nil {
		return false, err
	}
	for _, id := range models.IDs {
		if s, ok := models.Status[id].(string); ok && bootHoldsMemory[s] {
			return true, nil
		}
	}
	return false, nil
}

// Serializes the state check then spawn across every process on this machine, bounded and never failing a boot
func crossProcessBootLock(timeout time.Duration) func() {
	path := filepath.Join(RuntimesRoot(), "boot.lock")
	err := os.MkdirAll(filepath.Dir(path), 0o777)
	var f *os.File
	if err == nil {
		f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("boot lock unavailable (%s); proceeding without it", err))
		return func() {}
	}
	deadline := time.Now().Add(timeout)
	for !lrTryLockBootFile(f) {
		if !time.Now().Before(deadline) {
			slog.Warn(fmt.Sprintf("boot lock contended past %.0fs; proceeding without it", timeout.Seconds()))
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return func() {
		_ = lrUnlockBootFile(f)
		_ = f.Close()
	}
}

// local_runtime config section, an error where Python's .get on a non mapping raises
func bootSection(cfg map[string]any) (map[string]any, error) {
	raw := cfg["local_runtime"]
	if !pyTruthy(raw) {
		return map[string]any{}, nil
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(raw))
	}
	return section, nil
}

// Idempotent boot of the managed runtime, nil when disabled or unavailable, err only where hermes raises
func EnsureLocalRuntime(ctx context.Context, cfg map[string]any, force bool) (*LlamaServerSupervisor, error) {
	section, err := bootSection(cfg)
	if err != nil {
		return nil, err
	}
	if !force && !pyTruthy(section["enabled"]) {
		return nil, nil
	}
	if sup := getLocalSupervisor(); sup != nil {
		return sup, nil
	}
	if _, err := adoptLegacyModels(); err != nil {
		slog.Warn(fmt.Sprintf("legacy model adoption failed: %s", err))
	}
	if !force && len(stagedModels()) == 0 {
		slog.Info("local runtime enabled but no models staged; not booting")
		return nil, nil
	}
	unlock := crossProcessBootLock(bootLockTimeout)
	defer unlock()
	live, err := bootStateEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	if live {
		if !bootPresetsStaleFn() {
			slog.Info("managed llama-server already running (another process)")
			return nil, nil
		}
		slog.Info("running server's presets predate the staged models; replacing it so every model launches with a policy")
		if err := bootStopStateServer(); err != nil {
			return nil, err
		}
	}
	sup, err := bootServe(ctx, section)
	if err != nil {
		bootMu.Lock()
		localServingEngine = nil
		bootMu.Unlock()
		slog.Warn(fmt.Sprintf("managed local runtime unavailable: %s", err))
		return nil, nil
	}
	return sup, nil
}

// Integer config value with Python int() coercion
func bootIntSetting(section map[string]any, key string, def int) (int, error) {
	raw, ok := section[key]
	if !ok {
		return def, nil
	}
	v, ok := pythonInt(raw)
	if !ok {
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyRepr(raw))
	}
	return v, nil
}

// Serves the installed engine under a fresh supervisor, nil without error when no engine is installed
func bootServe(ctx context.Context, section map[string]any) (*LlamaServerSupervisor, error) {
	backend := "auto"
	if raw, ok := section["backend"]; ok {
		s, isStr := raw.(string)
		if !isStr {
			return nil, errors.New(pyRepr(raw))
		}
		backend = s
	}
	engine, err := bootInstalledEngine(ctx, backend)
	if err != nil {
		return nil, err
	}
	if engine == nil {
		slog.Info("local runtime enabled but no PM engine installed; use the Local Models pane")
		return nil, nil
	}
	bootMu.Lock()
	localServingEngine = engine
	bootMu.Unlock()
	mdir := localModelsDir()
	if err := os.MkdirAll(mdir, 0o777); err != nil {
		return nil, err
	}
	configured, err := bootIntSetting(section, "models_max", 4)
	if err != nil {
		return nil, err
	}
	modelsMax := bootAdmittedModelsMax(mdir, configured, engine.Tag)
	presetPath := bootGeneratePresets(mdir, filepath.Join(RuntimesRoot(), "presets.ini"), modelsMax, engine.Tag)
	port, err := bootIntSetting(section, "port", 0)
	if err != nil {
		return nil, err
	}
	sup, err := NewLlamaServerSupervisor(engine.Binary, mdir, modelsMax, port, nil, "", presetPath)
	if err != nil {
		return nil, err
	}
	if err := bootStartSupervisor(sup); err != nil {
		_ = sup.Stop()
		return nil, err
	}
	bootMu.Lock()
	localSupervisor = sup
	bootMu.Unlock()
	stopAtExit()
	slog.Info(fmt.Sprintf("managed llama-server up at %s (backend=%s tag=%s)", sup.BaseURL(), engine.Backend, engine.Tag))
	startIdleSweeper(sup)
	return sup, nil
}

// Arms the exit hook so the process that boots the server stops it on a clean exit
func stopAtExit() {
	bootMu.Lock()
	defer bootMu.Unlock()
	localExitHooked = true
}

// Clean exit hook of the process, stopping the server this process booted and nothing it only adopted
func LocalRuntimeAtExit() error {
	bootMu.Lock()
	hooked := localExitHooked
	bootMu.Unlock()
	if !hooked {
		return nil
	}
	return ShutdownLocalRuntime()
}

// Stops the process local server and forgets the engine it served
func ShutdownLocalRuntime() error {
	bootMu.Lock()
	sup := localSupervisor
	bootMu.Unlock()
	if sup != nil {
		if err := sup.Stop(); err != nil {
			return err
		}
	}
	bootMu.Lock()
	defer bootMu.Unlock()
	if sup != nil && localSupervisor == sup {
		localSupervisor = nil
	}
	localServingEngine = nil
	return nil
}

// Process local supervisor, nil when this process supervises no server
func getLocalSupervisor() *LlamaServerSupervisor {
	bootMu.Lock()
	defer bootMu.Unlock()
	return localSupervisor
}

// Engine the process local server runs or is booting, nil when there is none
func servingEngine() *Engine {
	bootMu.Lock()
	defer bootMu.Unlock()
	return localServingEngine
}

// Unloads idle models every couple of minutes and re-plans windows every 30 s while the router lives
func startIdleSweeper(sup *LlamaServerSupervisor) {
	go func() {
		lastSweep := supMonotonic()
		for {
			proc := sup.currentProc()
			if proc == nil {
				return
			}
			if _, exited := proc.Poll(); exited {
				return
			}
			bootSweeperSleep(bootRefitEvery)
			if supMonotonic()-lastSweep >= bootSweepEvery {
				lastSweep = supMonotonic()
				sup.SweepIdle(nil)
			}
			if _, err := refitIdlePresets(sup); err != nil {
				slog.Debug(fmt.Sprintf("launch window re-plan skipped: %s", err))
			}
		}
	}()
}
