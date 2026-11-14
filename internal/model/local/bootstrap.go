package local

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/toolchain"
)

const (
	bootLockTimeout = 130 * time.Second
	sweepEvery      = 120 * time.Second
	refitEvery      = 30 * time.Second
	refitStepBytes  = int64(256 << 20)
	stopSettle      = 10 * time.Second
	defaultBackend  = "auto"
)

var holdsMemory = map[string]bool{"loaded": true, "loading": true, "sleeping": true, "ready": true}

var ErrClosed = errors.New("local runtime closed")

type Options struct {
	Root    string
	Homes   *home.Resolver
	Host    *hostinfo.Host
	Tools   *toolchain.Store
	HTTP    *httpclient.Transport
	Config  func(ctx context.Context) (*config.Config, error)
	Secrets *secret.Scope
}

type Runtime struct {
	root    string
	homes   *home.Resolver
	host    *hostinfo.Host
	tools   *toolchain.Store
	client  *http.Client
	config  func(ctx context.Context) (*config.Config, error)
	secrets *secret.Scope
	guard   *proc.Guard
	ledger  *proc.Ledger

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	bootMu  sync.Mutex
	mu      sync.Mutex
	closed  bool
	sup     *Supervisor
	serving *Engine
	kicking atomic.Bool

	hwMu     sync.Mutex
	pool     *poolProbe
	accel    map[string]accelProbe
	directIO map[string][]string

	adoptMu     sync.Mutex
	leftInPlace map[string]bool

	catMu    sync.Mutex
	catalog  []CatalogEntry
	catTried time.Time

	hfMu    sync.Mutex
	hfCache map[string]hfCached

	progMu   sync.Mutex
	loading  map[string]*loadEntry
	watching bool

	ollama ollamaCaches
}

func New(o Options) (*Runtime, error) {
	if o.Root == "" || o.Homes == nil || o.Host == nil || o.Tools == nil || o.HTTP == nil || o.Config == nil || o.Secrets == nil {
		return nil, errors.New("local runtime: root, home resolver, host facts, toolchain store, transport, config and secret scope are required")
	}
	entries, err := parseCatalog(packagedCatalog)
	if err != nil {
		return nil, fmt.Errorf("local runtime: packaged catalog: %w", err)
	}
	installRoot, err := home.InstallRoot()
	if err != nil {
		return nil, fmt.Errorf("local runtime: install directory: %w", err)
	}
	return &Runtime{
		root: o.Root, homes: o.Homes, host: o.Host, tools: o.Tools, client: o.HTTP.Client(), config: o.Config,
		secrets: o.Secrets, guard: proc.NewGuard(), done: make(chan struct{}),
		ledger: proc.NewLedger(home.SpawnLedger.In(o.Root), home.SpawnLedgerLock.In(o.Root), installRoot),
		accel:  map[string]accelProbe{}, directIO: map[string][]string{}, leftInPlace: map[string]bool{},
		catalog: entries, hfCache: map[string]hfCached{}, loading: map[string]*loadEntry{},
		ollama: newOllamaCaches(),
	}, nil
}

func (r *Runtime) Close(ctx context.Context) error {
	err := r.Shutdown(ctx)
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.closeOnce.Do(func() { close(r.done) })
	finished := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-ctx.Done():
		err = errors.Join(err, ctx.Err())
	}
	return err
}

func (r *Runtime) detach(ctx context.Context) (context.Context, context.CancelFunc) {
	out, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		select {
		case <-r.done:
			cancel()
		case <-out.Done():
		}
	}()
	return out, cancel
}

func (r *Runtime) goBackground(fn func(ctx context.Context)) {
	r.goDetached(context.Background(), fn)
}

func (r *Runtime) goDetached(parent context.Context, fn func(ctx context.Context)) bool {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		ctx, cancel := r.detach(parent)
		defer cancel()
		fn(ctx)
	}()
	return true
}

func (r *Runtime) ModelsDir() string { return home.Models.In(r.root) }

func (r *Runtime) AssetsDir() string { return home.ModelAssets.In(r.root) }

func StagedIn(modelsDir string, requireComplete bool) []string {
	files, _ := filepath.Glob(filepath.Join(modelsDir, "*.gguf"))
	sort.Strings(files)
	names := map[string]bool{}
	for _, f := range files {
		names[filepath.Base(f)] = true
	}
	var out []string
	for _, f := range files {
		name := filepath.Base(f)
		m := splitPartPattern.FindStringSubmatchIndex(name)
		if m == nil {
			out = append(out, f)
			continue
		}
		if name[m[2]:m[3]] != "00001" {
			continue
		}
		stem, totalText := name[:m[0]], name[m[4]:m[5]]
		total, _ := strconv.Atoi(totalText)
		complete := true
		for i := 2; requireComplete && i <= total; i++ {
			if !names[fmt.Sprintf("%s-%05d-of-%s.gguf", stem, i, totalText)] {
				complete = false
				break
			}
		}
		if complete {
			out = append(out, f)
		}
	}
	return out
}

func (r *Runtime) StagedModels() []string { return StagedIn(r.ModelsDir(), true) }

func (r *Runtime) StagedModelIDs() []string {
	staged := r.StagedModels()
	out := make([]string, len(staged))
	for i, p := range staged {
		out[i] = ModelIDFromStem(strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)))
	}
	return out
}

func (r *Runtime) AdoptLegacyModels() ([]string, error) {
	profilesRoot := home.Profiles.In(r.root)
	if fi, err := os.Stat(profilesRoot); err != nil || !fi.IsDir() {
		return nil, nil
	}
	homes, err := os.ReadDir(profilesRoot)
	if err != nil {
		return nil, err
	}
	var moved []string
	for _, h := range homes {
		profileHome := filepath.Join(profilesRoot, h.Name())
		old := filepath.Join(profileHome, "models")
		if strings.HasPrefix(h.Name(), ".") {
			continue
		}
		if fi, err := os.Stat(old); err != nil || !fi.IsDir() || !home.ProfileHasIdentity(profileHome) {
			continue
		}
		for _, pair := range [][2]string{{old, r.ModelsDir()}, {filepath.Join(old, "assets"), r.AssetsDir()}} {
			sources, _ := filepath.Glob(filepath.Join(pair[0], "*.gguf"))
			sort.Strings(sources)
			for _, src := range sources {
				dest := filepath.Join(pair[1], filepath.Base(src))
				if _, err := os.Lstat(dest); err == nil {
					slog.Warn(fmt.Sprintf("legacy model %s not moved: %s already exists", src, dest))
					continue
				}
				err := os.MkdirAll(pair[1], 0o777)
				if err == nil {
					err = os.Rename(src, dest)
				}
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					slog.Warn(fmt.Sprintf("legacy model %s not moved to %s: %v", src, pair[1], err))
					continue
				}
				moved = append(moved, dest)
			}
		}
		_ = os.Remove(filepath.Join(old, "assets"))
		_ = os.Remove(old)
	}
	if len(moved) > 0 {
		slog.Info(fmt.Sprintf("moved %d legacy model file(s) into %s", len(moved), r.ModelsDir()))
	}
	return moved, nil
}

func (r *Runtime) presetsStale() bool {
	known := r.ReadPresetDecisions("")
	for _, id := range r.StagedModelIDs() {
		e, ok := known[id]
		if !ok {
			return true
		}
		if model, _ := e.Key(presetKeyModel); e.Refusal == "" && model == "" {
			return true
		}
	}
	return false
}

func (r *Runtime) stopStateServer(ctx context.Context) {
	state := r.readState()
	var id proc.Identity
	var ok bool
	if state.modern() {
		if !ownerIsDead(state) {
			slog.Info(fmt.Sprintf("llama-server pid=%v belongs to a live icebath process; leaving it", state["pid"]))
			return
		}
		id, ok = recordedProcess(state)
	} else {
		id, _, ok = r.legacyRecordedProcess(state)
	}
	if !ok {
		return
	}
	if err := terminateTree(ctx, id, true); err != nil {
		slog.Warn(fmt.Sprintf("could not stop the incumbent llama-server (pid=%d): %v", id.PID, err))
	}
}

func (r *Runtime) Refresh(ctx context.Context) bool {
	r.mu.Lock()
	sup := r.sup
	r.mu.Unlock()
	if sup == nil {
		if r.stateEndpoint() == nil {
			return false
		}
		r.stopStateServer(ctx)
	} else if err := r.Shutdown(ctx); err != nil {
		slog.Warn(fmt.Sprintf("local runtime refresh failed: %v", err))
		return false
	}
	return r.Ensure(ctx, true) != nil
}

func (r *Runtime) admittedModelsMax(ctx context.Context, modelsDir string, configured int, engineTag string) int {
	capacity := r.ProbeBudget(ctx, true)
	var live *HardwareBudget
	if capacity.UMA {
		live = r.LaunchBudget(ctx, capacity, 0)
	}
	limit := r.admittedResidencyCount(modelsDir, capacity, configured, live, engineTag)
	if limit != configured {
		slog.Info(fmt.Sprintf("residency cap: %d resident model(s) on this card (models_max=%d)", limit, configured))
	}
	return limit
}

func (r *Runtime) writePolicy(ctx context.Context, modelsDir, presetPath string, residents int, engineTag string) string {
	capacity := r.ProbeBudget(ctx, true)
	entries, err := r.generatePresets(ctx, modelsDir, capacity, presetPath, r.LaunchBudget(ctx, capacity, 0), residents, engineTag)
	if err == nil {
		for _, e := range entries {
			if e.Refusal != "" {
				slog.Warn(fmt.Sprintf("model refused by physics check: %s", e.Refusal))
			}
		}
		return presetPath
	}
	if _, statErr := os.Stat(presetPath); statErr == nil {
		slog.Error(fmt.Sprintf("preset generation failed (%v); serving with the PREVIOUS launch policies — models staged since the last successful generation run unpoliced until this is fixed", err))
		return presetPath
	}
	slog.Error(fmt.Sprintf("preset generation failed (%v) and no previous policy file exists; router runs stock fit", err))
	return ""
}

func (r *Runtime) anyHoldsMemory(ctx context.Context, sup *Supervisor) (bool, error) {
	statuses, err := sup.Models(ctx, 5*time.Second)
	if err != nil {
		return false, err
	}
	for _, status := range statuses {
		if holdsMemory[status] {
			return true, nil
		}
	}
	return false, nil
}

func (r *Runtime) refitIdlePresets(ctx context.Context, sup *Supervisor) (bool, error) {
	path := sup.presetPath
	if path == "" {
		return false, nil
	}
	if _, err := os.Stat(path); err != nil {
		return false, nil
	}
	if held, err := r.anyHoldsMemory(ctx, sup); err != nil || held {
		return false, err
	}
	capacity := r.ProbeBudget(ctx, true)
	live := r.LaunchBudget(ctx, capacity, 0)
	if live == nil {
		return false, nil
	}
	sup.mu.Lock()
	last := sup.refitUsed
	sup.mu.Unlock()
	if last != nil {
		if diff := live.UsableVRAMBytes - *last; diff < refitStepBytes && diff > -refitStepBytes {
			return false, nil
		}
	}
	tag := ""
	if engine := r.ServingEngine(); engine != nil {
		tag = engine.Tag
	}
	text, err := renderPresets(r.planPresets(r.ModelsDir(), capacity, live, sup.modelsMax, tag))
	if err != nil {
		return false, err
	}
	sup.lifecycle.Lock()
	defer sup.lifecycle.Unlock()
	if sup.running() == nil {
		return false, nil
	}
	if held, err := r.anyHoldsMemory(ctx, sup); err != nil || held {
		return false, err
	}
	usable := live.UsableVRAMBytes
	sup.mu.Lock()
	sup.refitUsed = &usable
	sup.mu.Unlock()
	current, err := fileutil.ReadText(path)
	if err != nil {
		return false, err
	}
	if text == current {
		return false, nil
	}
	if err := fileutil.WriteFile(ctx, path, []byte(text), fileutil.Perm(0o600), fileutil.TempPrefix("."+filepath.Base(path)+"_")); err != nil {
		return false, err
	}
	if err := sup.ReloadPresets(ctx); err != nil {
		return false, err
	}
	slog.Info(fmt.Sprintf("launch windows re-planned for %.1f GiB of free GPU memory", float64(usable)/gibFloat))
	return true, nil
}

func (r *Runtime) bootLock(ctx context.Context) func() {
	path := home.LlamaCppBootLock.In(r.root)
	wait, cancel := context.WithTimeout(ctx, bootLockTimeout)
	defer cancel()
	l, err := fileutil.OpenLock(wait, path, fileutil.LockPerm(0o600), fileutil.Range(0, 1))
	if err == nil {
		return func() { _ = l.Release() }
	}
	var timeout *fileutil.LockTimeoutError
	if errors.As(err, &timeout) || errors.Is(err, fileutil.ErrLocked) || errors.Is(err, context.DeadlineExceeded) {
		slog.Warn(fmt.Sprintf("boot lock contended past %.0fs; proceeding without it", bootLockTimeout.Seconds()))
	} else {
		slog.Warn(fmt.Sprintf("boot lock unavailable (%v); proceeding without it", err))
	}
	return func() {}
}

func (r *Runtime) Ensure(ctx context.Context, force bool) *Supervisor {
	cfg, err := r.config(ctx)
	if err != nil {
		slog.Warn(fmt.Sprintf("managed local runtime unavailable: %v", err))
		return nil
	}
	section := cfg.LocalRuntime
	if !force && !section.Enabled {
		return nil
	}
	r.bootMu.Lock()
	defer r.bootMu.Unlock()
	if sup := r.Supervisor(); sup != nil {
		return sup
	}
	if _, err := r.AdoptLegacyModels(); err != nil {
		slog.Warn(fmt.Sprintf("legacy model adoption failed: %v", err))
	}
	if !force && len(r.StagedModels()) == 0 {
		slog.Info("local runtime enabled but no models staged; not booting")
		return nil
	}
	release := r.bootLock(ctx)
	defer release()
	if r.stateEndpoint() != nil {
		if !r.presetsStale() {
			slog.Info("managed llama-server already running (another process)")
			return nil
		}
		slog.Info("running server's presets predate the staged models; replacing it so every model launches with a policy")
		r.stopStateServer(ctx)
	}
	backend := string(section.Backend)
	if backend == "" {
		backend = defaultBackend
	}
	sup, err := r.boot(ctx, backend, section)
	if err != nil {
		r.mu.Lock()
		r.serving = nil
		r.mu.Unlock()
		slog.Warn(fmt.Sprintf("managed local runtime unavailable: %v", err))
		return nil
	}
	return sup
}

func (r *Runtime) boot(ctx context.Context, backend string, section config.LocalRuntimeConfig) (*Supervisor, error) {
	engine, err := r.InstalledEngine(ctx, backend, true)
	if err != nil {
		return nil, err
	}
	if engine == nil {
		slog.Info("local runtime enabled but no toolchain engine installed; use the Local Models pane")
		return nil, nil
	}
	r.mu.Lock()
	r.serving = engine
	r.mu.Unlock()
	modelsDir := r.ModelsDir()
	if err := os.MkdirAll(modelsDir, 0o777); err != nil {
		return nil, err
	}
	modelsMax := r.admittedModelsMax(ctx, modelsDir, section.ModelsMax, engine.Tag)
	presetPath := r.writePolicy(ctx, modelsDir, r.PresetsPath(), modelsMax, engine.Tag)
	sup, err := r.newSupervisor(ctx, engine.Binary, modelsDir, modelsMax, section.Port, presetPath)
	if err != nil {
		return nil, err
	}
	if err := sup.start(ctx); err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminateGrace+stopSettle)
		_ = sup.Stop(stopCtx)
		cancel()
		return nil, err
	}
	select {
	case <-r.done:
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminateGrace+stopSettle)
		defer cancel()
		return nil, errors.Join(ErrClosed, sup.Stop(stopCtx))
	default:
	}
	r.mu.Lock()
	r.sup = sup
	r.mu.Unlock()
	slog.Info(fmt.Sprintf("managed llama-server up at %s (backend=%s tag=%s)", sup.BaseURL(), engine.Backend, engine.Tag))
	r.startIdleSweeper(sup)
	return sup, nil
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	sup := r.sup
	r.sup, r.serving = nil, nil
	r.mu.Unlock()
	if sup == nil {
		return nil
	}
	return sup.Stop(ctx)
}

func (r *Runtime) Supervisor() *Supervisor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sup
}

func (r *Runtime) ServingEngine() *Engine {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.serving
}

func (r *Runtime) startIdleSweeper(sup *Supervisor) {
	r.goBackground(func(ctx context.Context) {
		lastSweep := time.Now()
		for sup.running() != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(refitEvery):
			}
			if time.Since(lastSweep) >= sweepEvery {
				lastSweep = time.Now()
				sup.SweepIdle(ctx, time.Now())
			}
			if _, err := r.refitIdlePresets(ctx, sup); err != nil {
				slog.Debug(fmt.Sprintf("launch window re-plan skipped: %v", err))
			}
		}
	})
}
