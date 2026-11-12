package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/pprof"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Job phases whose byte counter tracks a transfer in flight
var localModelsDownloadPhases = map[string]bool{"starting": true, "installing-runtime": true, "downloading-runtime": true, "downloading": true}

// Provider names whose model.default is a model of the managed llama.cpp runtime
var lmLlamacppProviders = map[string]bool{"llamacpp": true, "llama.cpp": true, "llama-cpp": true}

// Trailing rate window in seconds, the shortest span a rate divides by, and the start failure text
const (
	lmRateWindow        = 8.0
	lmRateMinElapsed    = 0.5
	lmServerStartFailed = "The local server could not start — check the runtime is installed"
)

// File name tail a browsed download's model id drops, a split part suffix optional before the extension
var lmBrowsedModelIDRE = regexp.MustCompile(`(?i)(?:-\p{Nd}{5}-of-\p{Nd}{5})?\.gguf(\n?)\z`)

// Quant reason text of a catalog row by the fit's reason key, the compact text for every other key
var lmQuantReasons = map[string]string{
	"best-large-window": "Recommended build ({quant}) — the quant class this engine is optimized for; " +
		"runs fully on your GPU with a large context window",
	"best-fits": "Recommended build ({quant}) — the quant class this engine is optimized for; " +
		"runs fully on your GPU",
}

const lmQuantReasonCompact = "Compact build sized for this machine ({quant}) — larger than GPU memory, runs slower"

// Router supervisor surface the routes drive, the process supervisor or a stand in
type localModelsSupervisor interface {
	Models(timeout time.Duration) (RouterModels, error)
	UnloadModel(modelID string) error
}

// One transfer a download job runs over its sources
type lmDownloader interface {
	Run(progress pm.ProgressFunc) ([]string, error)
}

// One download plan row, the URL, its destination and the catalog byte count
type lmPlanRow struct {
	URL   string
	Dest  string
	Bytes int64
}

// One live job's pause switch, resume entry and trailing transfer samples
type lmRunningEntry struct {
	pause   *pm.PauseEvent
	resume  func(ctx context.Context) bool
	samples []lmRateSample
}

// One transfer sample, monotonic seconds and the bytes done then
type lmRateSample struct {
	at   float64
	done int64
}

// Python KeyError of a missing key, its text the repr of the key
type lmKeyError struct{ key string }

func (e *lmKeyError) Error() string { return pyrepr.Repr(e.key) }

// Python exception class the error stands for
func (e *lmKeyError) PyTypeName() string { return "KeyError" }

// Job registry in creation order, the running entries and the single flight quickstart lock
var (
	lmJobsMu       sync.Mutex
	lmJobs         = map[string]map[string]any{}
	lmJobOrder     []string
	lmRunning      = map[string]*lmRunningEntry{}
	lmQuickstartMu sync.Mutex
	lmClockStart   = time.Now()
	lmMonotonic    = func() float64 { return time.Since(lmClockStart).Seconds() }
)

// Effective config loads and saves of hermes_cli.config, the functions hermes tests patch
var (
	configLoadFn = func(ctx context.Context) (map[string]any, error) {
		tree, err := config.LoadContext(ctx, home.Dir(ctx), config.EnvRefLookupContext(ctx))
		return map[string]any(tree), err
	}
	configLoadReadonlyFn = func(ctx context.Context) (map[string]any, error) {
		tree, err := config.LoadReadonly(ctx, home.Dir(ctx), config.EnvRefLookupContext(ctx))
		return map[string]any(tree), err
	}
	configSaveFn = func(ctx context.Context, cfg map[string]any) error {
		return config.SaveConfig(ctx, home.Dir(ctx), cfg, config.SaveOptions{})
	}
)

// Module functions the routes reach, each the attribute a hermes test patches
type localModelsDeps struct {
	probeBudget          func(planning bool) (HardwareBudget, error)
	refreshCatalogSoon   func()
	recommendedEntry     func(budget HardwareBudget, entries []CatalogEntry, backend string) (*CatalogEntry, string)
	installedEngine      func(ctx context.Context, backend string) (*Engine, error)
	ensureEngine         func(ctx context.Context, backend string, opts EnsureEngineOptions) (Engine, error)
	stagedModelIDs       func() []string
	stagedModels         func() []string
	modelsDir            func() string
	assetsDir            func() string
	getSupervisor        func() localModelsSupervisor
	servingEngine        func() *Engine
	ensureLocalRuntime   func(ctx context.Context, cfg map[string]any, force bool) (localModelsSupervisor, error)
	refreshLocalRuntime  func(ctx context.Context) (bool, error)
	shutdownLocalRuntime func() error
	adoptLegacyModels    func() ([]string, error)
	stateEndpoint        func() (map[string]any, error)
	routerRequest        func(endpoint map[string]any, path string, timeout time.Duration, payload []byte) (any, error)
	loadConfig           func(ctx context.Context) map[string]any
	runtimeSection       func(ctx context.Context) any
	quickstartTarget     func(ctx context.Context, modelID string, budget HardwareBudget) (CatalogEntry, QuantVariant, error)
	setRuntimeEnabled    func(ctx context.Context, enabled bool) (map[string]any, error)
	runtimeTarget        func(ctx context.Context, requested string) (string, string, error)
	engineTooOld         func(ctx context.Context, minEngine string) (bool, error)
	hfURL                func(repo, path, revision string) string
	downloadPlan         func(entry CatalogEntry, variant QuantVariant) []lmPlanRow
	downloadTarget       func(ctx context.Context, modelID string) (CatalogEntry, QuantVariant, error)
	runDownloadPlan      func(ctx context.Context, job map[string]any, plan []lmPlanRow, label string, entry *CatalogEntry) error
	downloadJob          func(ctx context.Context, job map[string]any, plan []lmPlanRow) error
	newDownload          func(sources []pm.Source, pause *pm.PauseEvent) lmDownloader
	downloadProgressHook func(job map[string]any) pm.ProgressFunc
	runtimeProgressHook  func(job map[string]any) pm.InstallProgress
	installEngineJob     func(ctx context.Context, job map[string]any, backend string) (Engine, error)
	ensureServer         func(ctx context.Context, job map[string]any, cfg map[string]any, modelID, failDetail, skipMsg string) error
	assignDefault        func(ctx context.Context, job map[string]any, modelID string) error
	applyAssignment      func(ctx context.Context, scope, provider, model, task, baseURL, apiKey string) (map[string]any, error)
	refreshRuntime       func(ctx context.Context, refreshLocal func(ctx context.Context) (bool, error), skipMsg string)
	terminateStatePID    func() error
	stopRecordedOrphan   func() (bool, error)
	loadingProgress      func() map[string]LoadProgress
	readPresetDecisions  func() map[string]*PresetEntry
	clearWindowOverride  func(modelID string) error
	searchModels         func(q string, limit int) ([]HFModelHit, error)
	pricedRepoFiles      func(repo string, budget HardwareBudget) ([]HFFileGroup, error)
	installedPackage     func(ctx context.Context, name string) (*pm.InstalledPackage, error)
}

var lmDeps localModelsDeps

func init() { lmDeps = defaultLocalModelsDeps() }

// The real module functions the routes call
func defaultLocalModelsDeps() localModelsDeps {
	return localModelsDeps{
		probeBudget:        ProbeBudget,
		refreshCatalogSoon: RefreshCatalogSoon,
		recommendedEntry:   RecommendedEntry,
		installedEngine:    func(ctx context.Context, backend string) (*Engine, error) { return InstalledEngine(ctx, backend, true) },
		ensureEngine:       EnsureEngine,
		stagedModelIDs:     stagedModelIDs,
		stagedModels:       stagedModels,
		modelsDir:          localModelsDir,
		assetsDir:          localAssetsDir,
		getSupervisor: func() localModelsSupervisor {
			if sup := getLocalSupervisor(); sup != nil {
				return sup
			}
			return nil
		},
		servingEngine: servingEngine,
		ensureLocalRuntime: func(ctx context.Context, cfg map[string]any, force bool) (localModelsSupervisor, error) {
			sup, err := bootEnsureLocalRuntime(ctx, cfg, force)
			if sup == nil {
				return nil, err
			}
			return sup, err
		},
		refreshLocalRuntime:  func(ctx context.Context) (bool, error) { return refreshLocalRuntimeCtx(ctx), nil },
		shutdownLocalRuntime: ShutdownLocalRuntime,
		adoptLegacyModels:    adoptLegacyModels,
		stateEndpoint:        lmStateEndpoint,
		routerRequest:        lmRouterRequest,
		loadConfig:           lmLoadConfig,
		runtimeSection:       lmRuntimeSection,
		quickstartTarget:     lmQuickstartTarget,
		setRuntimeEnabled:    lmSetRuntimeEnabled,
		runtimeTarget:        lmRuntimeTarget,
		engineTooOld:         lmEngineTooOld,
		hfURL:                lmHFURL,
		downloadPlan:         lmDownloadPlan,
		downloadTarget:       lmDownloadTarget,
		runDownloadPlan:      lmRunDownloadPlan,
		downloadJob:          lmDownloadJob,
		newDownload: func(sources []pm.Source, pause *pm.PauseEvent) lmDownloader {
			return pm.NewDownload(sources, pm.DownloadOptions{Pause: pause})
		},
		downloadProgressHook: lmDownloadProgressHook,
		runtimeProgressHook:  lmRuntimeProgressHook,
		installEngineJob:     lmInstallEngineJob,
		ensureServer:         lmEnsureServer,
		assignDefault:        lmAssignDefault,
		applyAssignment: func(ctx context.Context, scope, provider, model, task, baseURL, apiKey string) (map[string]any, error) {
			return ApplyModelAssignmentSync(ctx, scope, provider, model, task, baseURL, apiKey, AuxReasoningEffort{}, nil)
		},
		refreshRuntime:      lmRefreshRuntime,
		terminateStatePID:   lmTerminateStatePID,
		stopRecordedOrphan:  StopRecordedOrphan,
		loadingProgress:     GetLoadingProgress,
		readPresetDecisions: func() map[string]*PresetEntry { return readPresetDecisions("") },
		clearWindowOverride: ClearWindowOverride,
		searchModels:        SearchModels,
		pricedRepoFiles:     PricedRepoFiles,
		installedPackage: func(ctx context.Context, name string) (*pm.InstalledPackage, error) {
			return pm.Installed(ctx, name, false)
		},
	}
}

// LocalModelsHumanGB renders bytes as gibibytes with one decimal
func LocalModelsHumanGB(n float64) string {
	return fmt.Sprintf("%.1f GB", n/float64(1<<30))
}

// Token count as whole thousands of 1024 with a K suffix
func lmKLabel(tokens int64) string {
	q := tokens / 1024
	if tokens%1024 != 0 && tokens < 0 {
		q--
	}
	return fmt.Sprintf("%dK", q)
}

// Route refusal of status carrying prefix and the error text
func lmHTTPError(status int, prefix string, err error) error {
	return httpException(status, prefix+err.Error())
}

// Value of fn or def on failure, warn logging the failure repr after its prefix, debug the failure
func lmQuiet[T any](fn func() (T, error), def T, warn, debug string) T {
	v, err := fn()
	if err == nil {
		return v
	}
	if warn != "" {
		slog.Warn(warn + pyrepr.ExceptionRepr(err))
	}
	if debug != "" {
		slog.Debug(debug, "error", err)
	}
	return def
}

// Pause refusal the downloader raises when its pause switch is set
func lmDownloadPaused(msg string) error {
	return &pm.DownloadError{Kind: pm.DownloadWasPaused, Message: msg}
}

// Whether err is the downloader's pause refusal
func lmIsDownloadPaused(err error) bool { return pm.IsDownloadKind(err, pm.DownloadWasPaused) }

// Whether err stands for a Python RuntimeError or OSError, the downloader failures an optional download tolerates
func lmIsRuntimeOrOSError(err error) bool {
	var de *pm.DownloadError
	var he *pm.HTTPError
	var rd *pm.RemoteDisconnectedError
	var ue *pyurl.URLError
	var ne net.Error
	return errors.As(err, &de) || errors.As(err, &he) || errors.As(err, &rd) || errors.As(err, &ue) ||
		errors.As(err, &ne) || pypath.IsOSError(err)
}

// Job id of a job record
func lmJobID(job map[string]any) string {
	id, _ := job["job_id"].(string)
	return id
}

// Fresh job registered as running in its first phase
func lmNewJob(kind, target string, modelID any) map[string]any {
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	job := map[string]any{
		"job_id": hex.EncodeToString(raw), "kind": kind, "target": target, "model_id": modelID,
		"status": "running", "phase": "starting", "detail": "", "total_bytes": nil, "done_bytes": int64(0),
		"started_at": float64(time.Now().UnixNano()) / 1e9, "error": nil,
	}
	lmJobsMu.Lock()
	id := lmJobID(job)
	if _, seen := lmJobs[id]; !seen {
		lmJobOrder = append(lmJobOrder, id)
	}
	lmJobs[id] = job
	lmJobsMu.Unlock()
	return job
}

// Transfer rate and remaining seconds from the trailing samples, nil for anything not honestly measurable
func lmRateAndETA(samples []lmRateSample, done, total int64) (*float64, *int64) {
	first := samples[0]
	elapsed := samples[len(samples)-1].at - first.at
	if elapsed < lmRateMinElapsed || done <= first.done {
		return nil, nil
	}
	rate := float64(done-first.done) / elapsed
	if total != 0 {
		eta := int64(max(0, math.RoundToEven(float64(max(0, total-done))/rate)))
		return &rate, &eta
	}
	return &rate, nil
}

// Refreshes the job's smoothed rate and ETA from the sample it just reported, the caller holding the registry lock
func lmRecordRate(job map[string]any, done int64) {
	running := lmRunning[lmJobID(job)]
	if running == nil {
		return
	}
	now := lmMonotonic()
	running.samples = append(running.samples, lmRateSample{at: now, done: done})
	for len(running.samples) > 1 && now-running.samples[0].at > lmRateWindow {
		running.samples = running.samples[1:]
	}
	total, _ := job["total_bytes"].(int64)
	rate, eta := lmRateAndETA(running.samples, done, total)
	if rate == nil {
		delete(job, "bytes_per_sec")
		delete(job, "eta_seconds")
		return
	}
	job["bytes_per_sec"] = *rate
	if eta == nil {
		job["eta_seconds"] = nil
	} else {
		job["eta_seconds"] = *eta
	}
}

// Wire view of a job with its pause, resume and percent facts, the caller holding the registry lock
func lmJobView(job map[string]any) map[string]any {
	out := make(map[string]any, len(job)+4)
	for k, v := range job {
		out[k] = v
	}
	running := lmRunning[lmJobID(job)]
	var pause *pm.PauseEvent
	hasResume := false
	if running != nil {
		pause, hasResume = running.pause, running.resume != nil
	}
	status := out["status"]
	pauseRequested := pause != nil && pause.IsSet() && status == "running"
	out["pause_requested"] = pauseRequested
	out["can_pause"] = pause != nil && status == "running" && localModelsDownloadPhases[anyString(out["phase"])] && !pauseRequested
	out["can_resume"] = status == "paused" && hasResume
	if total, ok := out["total_bytes"].(int64); ok && total != 0 {
		done, _ := out["done_bytes"].(int64)
		out["percent"] = min(int64(100), int64(math.RoundToEven(float64(done)/float64(total)*100)))
	}
	if status != "running" {
		delete(out, "bytes_per_sec")
		delete(out, "eta_seconds")
	}
	return out
}

// Pause refusal when the job's pause switch is set, the caller holding the registry lock
func lmCheckJobPause(job map[string]any) error {
	if running := lmRunning[lmJobID(job)]; running != nil && running.pause != nil && running.pause.IsSet() {
		return lmDownloadPaused("download paused")
	}
	return nil
}

// Moves the job to a phase, refusing to leave a download phase once a pause was requested
func lmStep(job map[string]any, phase, detail string) error {
	lmJobsMu.Lock()
	defer lmJobsMu.Unlock()
	if localModelsDownloadPhases[anyString(job["phase"])] && !localModelsDownloadPhases[phase] {
		if err := lmCheckJobPause(job); err != nil {
			return err
		}
	}
	job["phase"] = phase
	job["detail"] = detail
	return nil
}

// Final phase of a job, the worker publishing its terminal status after releasing resources
func lmFinish(job map[string]any, detail string) error { return lmStep(job, "done", detail) }

// Worker options of one job
type lmSpawnOptions struct {
	failMsg       string
	onExit        func()
	downloadLabel *string
	resumable     bool
}

// Starts one worker per job, a pause keeping the job's resources and a terminal outcome releasing them
func lmSpawnJob(ctx context.Context, job map[string]any, name string, body func(ctx context.Context) error, opts lmSpawnOptions) {
	var guard sync.Mutex
	pause := pm.NewPauseEvent()
	jobID := lmJobID(job)
	run := func(ctx context.Context) {
		status := "done"
		defer func() {
			defer func() {
				lmJobsMu.Lock()
				if status != "paused" {
					delete(lmRunning, jobID)
				}
				job["status"] = status
				guard.Unlock()
				lmJobsMu.Unlock()
			}()
			if status != "paused" && opts.onExit != nil {
				opts.onExit()
			}
		}()
		err := body(ctx)
		if err == nil && opts.downloadLabel != nil {
			if err = lmFinish(job, *opts.downloadLabel+" ready"); err == nil {
				lmDeps.refreshRuntime(ctx, lmDeps.refreshLocalRuntime, "post-download runtime refresh skipped")
			}
		}
		switch {
		case err == nil:
		case lmIsDownloadPaused(err):
			status = "paused"
		default:
			if opts.failMsg != "" {
				slog.Warn(fmt.Sprintf(opts.failMsg, err))
			}
			status = "error"
			lmJobsMu.Lock()
			job["error"] = err.Error()
			lmJobsMu.Unlock()
		}
	}
	start := func(ctx context.Context, initial bool) bool {
		if !guard.TryLock() {
			return false
		}
		lmJobsMu.Lock()
		if !initial && job["status"] != "paused" {
			lmJobsMu.Unlock()
			guard.Unlock()
			return false
		}
		pause.Clear()
		if running := lmRunning[jobID]; running != nil {
			running.samples = nil
		}
		job["status"] = "running"
		job["error"] = nil
		lmJobsMu.Unlock()
		go pprof.Do(context.WithoutCancel(ctx), pprof.Labels("thread", name), run)
		return true
	}
	if opts.resumable {
		lmJobsMu.Lock()
		lmRunning[jobID] = &lmRunningEntry{resume: func(ctx context.Context) bool { return start(ctx, false) }, pause: pause}
		lmJobsMu.Unlock()
	}
	start(ctx, true)
}

// Bounces a running router through refreshLocal so it rescans the models directory, a failure only logged under skipMsg
func lmRefreshRuntime(ctx context.Context, refreshLocal func(ctx context.Context) (bool, error), skipMsg string) {
	lmQuiet(func() (bool, error) { return refreshLocal(ctx) }, false, "", skipMsg)
}

// Connection failure urlopen wraps in a URLError
type lmDialError struct{ err error }

func (e *lmDialError) Error() string { return e.err.Error() }

func (e *lmDialError) Unwrap() error { return e.err }

// Connection whose every read and write gets the socket timeout urlopen sets
type lmTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *lmTimeoutConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(p)
}

func (c *lmTimeoutConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(p)
}

// urlopen of one request with the socket timeout, environment proxies and urllib's error classes
func lmURLOpen(method, rawURL string, headers map[string]string, body []byte, timeout time.Duration) ([]byte, error) {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		Proxy:             pyurl.UrlopenProxy,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, &lmDialError{err}
			}
			return &lmTimeoutConn{Conn: conn, timeout: timeout}, nil
		},
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var visited map[string]int
	for {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, rawURL, reader)
		if err != nil {
			return nil, fmt.Errorf("unknown url type: %s", pyrepr.Repr(rawURL))
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, lmURLOpenError(err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return nil, lmURLOpenError(err)
			}
			return raw, nil
		}
		next, err := lmRedirect(resp, method, rawURL, &visited)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		kept := map[string]string{}
		for k, v := range headers {
			if lower := strings.ToLower(k); lower != "content-length" && lower != "content-type" {
				kept[k] = v
			}
		}
		if method != http.MethodHead {
			method = http.MethodGet
		}
		rawURL, headers, body = next, kept, nil
	}
}

// urllib HTTPError of a response urlopen refuses, its text the code and the server's reason phrase
type lmHTTPStatusError struct {
	Code int
	URL  string
	Msg  string
}

func (e *lmHTTPStatusError) Error() string { return fmt.Sprintf("HTTP Error %d: %s", e.Code, e.Msg) }

// Python exception class the error stands for
func (e *lmHTTPStatusError) PyTypeName() string { return "HTTPError" }

// Python module of urllib's HTTPError
func (e *lmHTTPStatusError) PyModule() string { return "urllib.error" }

// Python repr of urllib's HTTPError, the code and the quoted reason
func (e *lmHTTPStatusError) PyRepr() string {
	return fmt.Sprintf("<HTTPError %d: %s>", e.Code, pyrepr.Repr(e.Msg))
}

const lmRedirectLoopMsg = "The HTTP server returned a redirect error that would lead to an infinite loop.\nThe last 30x error message was:\n"

// Next URL urllib's redirect handler follows for a non 2xx response, the HTTPError it raises when it refuses
func lmRedirect(resp *http.Response, method, current string, visited *map[string]int) (string, error) {
	code := resp.StatusCode
	reason := strings.TrimPrefix(strings.TrimPrefix(resp.Status, fmt.Sprint(code)), " ")
	if code != 301 && code != 302 && code != 303 && code != 307 && code != 308 {
		return "", &lmHTTPStatusError{code, current, reason}
	}
	target := resp.Header.Values("Location")
	if len(target) == 0 {
		target = resp.Header.Values("Uri")
	}
	if len(target) == 0 {
		return "", &lmHTTPStatusError{code, current, reason}
	}
	newURL := target[0]
	parts, err := pyurl.Parse(newURL)
	if err != nil {
		return "", err
	}
	if parts.Scheme != "http" && parts.Scheme != "https" && parts.Scheme != "ftp" && parts.Scheme != "" {
		return "", &lmHTTPStatusError{code, newURL, fmt.Sprintf("%s - Redirection to url '%s' is not allowed", reason, newURL)}
	}
	if parts.Path == "" && parts.Netloc != "" {
		parts.Path = "/"
	}
	joined, err := pyurl.Join(current, pyurl.Quote(pyurl.Unparse(parts), "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"))
	if err != nil {
		return "", err
	}
	if method != http.MethodGet && method != http.MethodHead && (method != http.MethodPost || code > 303) {
		return "", &lmHTTPStatusError{code, current, reason}
	}
	joined = strings.ReplaceAll(joined, " ", "%20")
	if *visited == nil {
		*visited = map[string]int{}
	} else if (*visited)[joined] >= 4 || len(*visited) >= 10 {
		return "", &lmHTTPStatusError{code, current, lmRedirectLoopMsg + reason}
	}
	(*visited)[joined]++
	return joined, nil
}

// urllib's exception for a transport failure, a URLError for the connect and the raw socket error after it
func lmURLOpenError(err error) error {
	var ne net.Error
	timedOut := errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
	var de *lmDialError
	if errors.As(err, &de) {
		if timedOut {
			return &pyurl.URLError{Reason: "timed out"}
		}
		return &pyurl.URLError{Reason: pyhttpx.OSErrorText(de.err)}
	}
	if timedOut {
		return &lmTimeoutError{}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &pm.RemoteDisconnectedError{Message: "Remote end closed connection without response"}
	}
	return err
}

// Python TimeoutError of a socket read past its timeout
type lmTimeoutError struct{}

func (*lmTimeoutError) Error() string      { return "timed out" }
func (*lmTimeoutError) Timeout() bool      { return true }
func (*lmTimeoutError) Temporary() bool    { return true }
func (*lmTimeoutError) PyTypeName() string { return "TimeoutError" }

// Calls the local router with its bearer key, a GET decoding JSON and a POST of payload returning nil
func lmRouterRequest(endpoint map[string]any, path string, timeout time.Duration, payload []byte) (any, error) {
	headers := map[string]string{"Authorization": "Bearer " + pyStr(pyGetAny(endpoint, "api_key", ""))}
	method := http.MethodGet
	if payload != nil {
		headers["Content-Type"] = "application/json"
		method = http.MethodPost
	}
	rawBase, present := endpoint["base_url"]
	if !present {
		return nil, &lmKeyError{"base_url"}
	}
	baseURL, ok := rawBase.(string)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'rsplit'", pyTypeName(rawBase))
	}
	if i := strings.LastIndex(baseURL, "/v1"); i >= 0 {
		baseURL = baseURL[:i]
	}
	raw, err := lmURLOpen(method, baseURL+path, headers, payload, timeout)
	if err != nil || payload != nil {
		return nil, err
	}
	return lmJSONLoadsBytes(raw)
}

// json.loads of a response body with its Python decode error
func lmJSONLoadsBytes(raw []byte) (any, error) {
	text, err := hfJSONText(raw)
	if err != nil {
		return nil, err
	}
	if v, ok := moaJSONLoads(text); ok {
		return v, nil
	}
	if _, err := pyjson.Loads(text); err != nil {
		return nil, err
	}
	return nil, errors.New("Expecting value: line 1 column 1 (char 0)")
}

// Python dict .get of a decoded JSON value, an AttributeError for anything but a mapping
func lmDictGet(v any, key string, def any) (any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(v))
	}
	return pyGetAny(m, key, def), nil
}

// Python iteration of a decoded JSON value as list items, a TypeError for a scalar
func lmIterate(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case map[string]any:
		out := make([]any, 0, len(t))
		for _, k := range config.MappingKeys(t) {
			out = append(out, k)
		}
		return out, nil
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyTypeName(v))
}

// Read only config for status paths that render degraded, empty on any failure
func lmLoadConfig(ctx context.Context) map[string]any {
	return lmQuiet(func() (map[string]any, error) { return configLoadReadonlyFn(ctx) }, map[string]any{}, "", "")
}

// The local_runtime section as the config holds it, an empty mapping when falsy
func lmRuntimeSection(ctx context.Context) any {
	cfg := lmDeps.loadConfig(ctx)
	if cfg == nil {
		cfg = map[string]any{}
	}
	if section := cfg["local_runtime"]; pyTruthy(section) {
		return section
	}
	return map[string]any{}
}

// The configured backend of a section as text, auto when the key is absent
func lmSectionBackend(section any) (string, error) {
	v, err := lmDictGet(section, "backend", "auto")
	if err != nil {
		return "", err
	}
	return pyStr(v), nil
}

// Persists local_runtime.enabled under the config mutation lock and returns the config written
func lmSetRuntimeEnabled(ctx context.Context, enabled bool) (map[string]any, error) {
	ctx, unlock := LockConfigMutation(ctx)
	defer unlock()
	cfg, err := configLoadFn(ctx)
	if err != nil {
		return nil, err
	}
	section, present := cfg["local_runtime"]
	if !present {
		section = map[string]any{}
		assignKey(cfg, "local_runtime", section)
	}
	m, ok := section.(map[string]any)
	if _, isList := section.([]any); isList {
		return nil, errors.New("list indices must be integers or slices, not str")
	} else if !ok {
		return nil, fmt.Errorf("'%s' object does not support item assignment", pyTypeName(section))
	}
	assignKey(m, "enabled", enabled)
	if err := configSaveFn(ctx, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Pinned tag and resolved backend of the requested or configured backend, a 400 when PM has no reviewed artifact
func lmRuntimeTarget(ctx context.Context, requested string) (string, string, error) {
	section := lmDeps.runtimeSection(ctx)
	backend := requested
	if backend == "" {
		var err error
		if backend, err = lmSectionBackend(section); err != nil {
			return "", "", err
		}
	}
	resolved, err := ResolveBackend(backend, nil, "")
	if err != nil {
		return "", "", lmHTTPError(400, "", err)
	}
	tag, err := PinnedTag(resolved)
	if err != nil {
		return "", "", lmHTTPError(400, "", err)
	}
	return tag, resolved, nil
}

// Python int of a b prefixed llama.cpp build tag
func lmTagBuild(tag string) (int64, error) {
	text := strings.TrimPrefix(tag, "b")
	n, ok := pythonUnicodeIntText(text)
	if !ok {
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(text))
	}
	return n, nil
}

// Whether the installed or pinned engine predates the build a catalog entry needs
func lmEngineTooOld(ctx context.Context, minEngine string) (bool, error) {
	if minEngine == "" {
		return false, nil
	}
	backend, err := lmSectionBackend(lmDeps.runtimeSection(ctx))
	if err != nil {
		return false, err
	}
	engine, err := lmDeps.installedEngine(ctx, backend)
	if err != nil {
		return false, err
	}
	tag := ""
	if engine != nil {
		tag = engine.Tag
	} else if tag, _, err = lmDeps.runtimeTarget(ctx, ""); err != nil {
		return false, err
	}
	have, err := lmTagBuild(tag)
	if err != nil {
		return false, err
	}
	need, err := lmTagBuild(minEngine)
	if err != nil {
		return false, err
	}
	return have < need, nil
}

// Catalog entries this engine can activate today, never nil so an empty pool stays empty
func lmEligibleEntries(ctx context.Context) ([]CatalogEntry, error) {
	out := []CatalogEntry{}
	for _, e := range Catalog() {
		old, err := lmDeps.engineTooOld(ctx, e.MinEngine)
		if err != nil {
			return nil, err
		}
		if !old {
			out = append(out, e)
		}
	}
	return out, nil
}

// Catalog entry of a family id, a 404 when the catalog lacks it
func lmEntryOr404(modelID string) (CatalogEntry, error) {
	entry, ok := CatalogByID()[modelID]
	if !ok {
		return CatalogEntry{}, httpException(404, "unknown model "+modelID)
	}
	return entry, nil
}

// Force starts the local server, failing with failDetail when neither this nor another process serves
func lmStartLocalServer(ctx context.Context, cfg map[string]any, failDetail string) (localModelsSupervisor, error) {
	sup, err := lmDeps.ensureLocalRuntime(ctx, cfg, true)
	if err != nil {
		return nil, err
	}
	if sup == nil {
		endpoint, err := lmDeps.stateEndpoint()
		if err != nil {
			return nil, err
		}
		if endpoint == nil {
			return nil, errors.New(failDetail)
		}
	}
	return sup, nil
}

// Starts the local server and bounces a router whose spawn time listing lacks modelID
func lmEnsureServer(ctx context.Context, job map[string]any, cfg map[string]any, modelID, failDetail, skipMsg string) error {
	if err := lmStep(job, "starting-server", "Starting the local server"); err != nil {
		return err
	}
	sup, err := lmStartLocalServer(ctx, cfg, failDetail)
	if err != nil {
		return err
	}
	rescanIfUnknown := func(known func(string) bool) error {
		if known(modelID) {
			return nil
		}
		lmJobsMu.Lock()
		job["detail"] = "Refreshing the local server"
		lmJobsMu.Unlock()
		_, err := lmDeps.refreshLocalRuntime(ctx)
		return err
	}
	if sup != nil {
		lmQuiet(func() (struct{}, error) {
			models, err := sup.Models(30 * time.Second)
			if err != nil {
				return struct{}{}, err
			}
			return struct{}{}, rescanIfUnknown(func(id string) bool { _, ok := models.Get(id); return ok })
		}, struct{}{}, "", skipMsg)
		return nil
	}
	endpoint, err := lmDeps.stateEndpoint()
	if err != nil {
		return err
	}
	if endpoint == nil {
		return nil
	}
	lmQuiet(func() (struct{}, error) {
		known, err := lmKnownModels(endpoint)
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, rescanIfUnknown(func(id string) bool { return known[id] })
	}, struct{}{}, "", skipMsg)
	return nil
}

// Model ids the router behind a persisted endpoint lists right now
func lmKnownModels(endpoint map[string]any) (map[any]bool, error) {
	body, err := lmDeps.routerRequest(endpoint, "/models", 10*time.Second, nil)
	if err != nil {
		return nil, err
	}
	if !pyTruthy(body) {
		body = map[string]any{}
	}
	data, err := lmDictGet(body, "data", []any{})
	if err != nil {
		return nil, err
	}
	items, err := lmIterate(data)
	if err != nil {
		return nil, err
	}
	known := map[any]bool{}
	for _, item := range items {
		id, err := lmDictGet(item, "id", nil)
		if err != nil {
			return nil, err
		}
		if err := lmHashable(id); err != nil {
			return nil, err
		}
		known[id] = true
	}
	return known, nil
}

// TypeError Python raises for a JSON container used as a dict key
func lmHashable(v any) error {
	switch v.(type) {
	case []any:
		return errors.New("unhashable type: 'list'")
	case map[string]any:
		return errors.New("unhashable type: 'dict'")
	}
	return nil
}

// JSON object key text of a decoded scalar used as a dict key
func lmJSONKey(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return pyStr(v)
}

// Makes modelID the main model through the same assignment the model set route runs
func lmAssignDefault(ctx context.Context, job map[string]any, modelID string) error {
	if err := lmStep(job, "setting-default", "Making it your default"); err != nil {
		return err
	}
	_, err := lmDeps.applyAssignment(ctx, "main", "llamacpp", modelID, "", "", "")
	return err
}

// Hugging Face resolve URL of a repo file at a revision
func lmHFURL(repo, path, revision string) string {
	return fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", repo, revision, path)
}

// Variant model id of a staged file with split part suffixes stripped
func lmModelIDFor(gguf string) string { return ModelIDFromStem(pyPathStem(gguf)) }

// Every local file of a staged model, its split parts plus the catalog companions on disk
func lmVariantFilesOnDisk(modelID string) []string {
	var files []string
	for _, p := range lrGlobGGUF(lmDeps.modelsDir()) {
		if lmModelIDFor(p) == modelID {
			files = append(files, p)
		}
	}
	if entry, _ := FindEntryForModel(modelID); entry != nil {
		for _, a := range []*AssetFile{entry.MMProj, entry.Draft, entry.MTPHead} {
			if a == nil {
				continue
			}
			if p := filepath.Join(lmDeps.assetsDir(), a.LocalName()); lrPathExists(p) {
				files = append(files, p)
			}
		}
	}
	return files
}

// Everything a variant needs, its files then the projector, draft and MTP head companions
func lmDownloadPlan(entry CatalogEntry, variant QuantVariant) []lmPlanRow {
	var plan []lmPlanRow
	for _, a := range variant.Files {
		plan = append(plan, lmPlanRow{lmDeps.hfURL(entry.Repo, a.Path, a.Revision), filepath.Join(lmDeps.modelsDir(), a.LocalName()), a.SizeBytes})
	}
	for _, a := range []*AssetFile{entry.MMProj, entry.Draft, entry.MTPHead} {
		if a == nil {
			continue
		}
		repo := a.Repo
		if repo == "" {
			repo = entry.Repo
		}
		plan = append(plan, lmPlanRow{lmDeps.hfURL(repo, a.Path, a.Revision), filepath.Join(lmDeps.assetsDir(), a.LocalName()), a.SizeBytes})
	}
	return plan
}

// Plan rows of the DFlash drafts that staged models lack and engineTag can load
func lmMissingDrafts(engineTag string) ([]lmPlanRow, error) {
	staged := map[string]bool{}
	for _, id := range lmDeps.stagedModelIDs() {
		staged[id] = true
	}
	build, err := lmTagBuild(engineTag)
	if err != nil {
		return nil, err
	}
	plan := []lmPlanRow{}
	for _, entry := range Catalog() {
		asset := entry.Draft
		if asset == nil || entry.DraftSpecType != "draft-dflash" {
			continue
		}
		stagedVariant := false
		for _, v := range entry.Variants {
			if staged[v.ModelID()] {
				stagedVariant = true
				break
			}
		}
		if !stagedVariant {
			continue
		}
		if entry.DraftMinEngine != "" {
			need, err := lmTagBuild(entry.DraftMinEngine)
			if err != nil {
				return nil, err
			}
			if build < need {
				continue
			}
		}
		dest := filepath.Join(lmDeps.assetsDir(), asset.LocalName())
		if !lrIsFile(dest) {
			repo := asset.Repo
			if repo == "" {
				repo = entry.Repo
			}
			plan = append(plan, lmPlanRow{lmDeps.hfURL(repo, asset.Path, asset.Revision), dest, asset.SizeBytes})
		}
	}
	return plan, nil
}

// Whether a path names a regular file
func lrIsFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Downloads a plan, a catalog entry's draft last and optional so its failure costs the model nothing
func lmRunDownloadPlan(ctx context.Context, job map[string]any, plan []lmPlanRow, label string, entry *CatalogEntry) error {
	draft := ""
	hasDraft := entry != nil && entry.Draft != nil
	if hasDraft {
		draft = filepath.Join(lmDeps.assetsDir(), entry.Draft.LocalName())
	}
	var required, optional []lmPlanRow
	for _, row := range plan {
		if hasDraft && row.Dest == draft {
			optional = append(optional, row)
		} else {
			required = append(required, row)
		}
	}
	if err := lmStep(job, "downloading", "Downloading "+label); err != nil {
		return err
	}
	if err := lmDeps.downloadJob(ctx, job, required); err != nil {
		return err
	}
	if len(optional) == 0 {
		return nil
	}
	lmJobsMu.Lock()
	job["done_bytes"], job["total_bytes"], job["ranges"] = int64(0), nil, pyjson.NewObject()
	lmJobsMu.Unlock()
	if err := lmStep(job, "downloading", "Downloading the draft model that speeds up generation"); err != nil {
		return err
	}
	_, err := lmDownloadOptional(ctx, job, optional)
	return err
}

// Per destination coverage as the ordered mapping the job carries
func lmRangesObject(ranges []pm.RangeSet) *pyjson.Object {
	out := pyjson.NewObject()
	for _, rs := range ranges {
		rows := make([]any, 0, len(rs.Ranges))
		for _, r := range rs.Ranges {
			rows = append(rows, []any{r.Start, r.End})
		}
		out.Set(rs.Key, rows)
	}
	return out
}

// Progress callback writing the plan's byte counts and coverage onto the job with its rate
func lmDownloadProgressHook(job map[string]any) pm.ProgressFunc {
	return func(done, total int64, ranges []pm.RangeSet) error {
		lmJobsMu.Lock()
		defer lmJobsMu.Unlock()
		job["done_bytes"] = done
		if total != 0 {
			job["total_bytes"] = total
		} else {
			job["total_bytes"] = nil
		}
		job["ranges"] = lmRangesObject(ranges)
		lmRecordRate(job, done)
		return nil
	}
}

// Runs the plan through PM's resumable transfer under the job's pause switch
func lmDownloadJob(ctx context.Context, job map[string]any, plan []lmPlanRow) error {
	lmJobsMu.Lock()
	var pause *pm.PauseEvent
	if running := lmRunning[lmJobID(job)]; running != nil {
		pause = running.pause
	}
	lmJobsMu.Unlock()
	sources := make([]pm.Source, 0, len(plan))
	for _, row := range plan {
		sources = append(sources, pm.Source{URL: row.URL, Dest: row.Dest})
	}
	_, err := lmDeps.newDownload(sources, pause).Run(lmDeps.downloadProgressHook(job))
	return err
}

// Downloads files that only speed generation up, a transfer or disk failure logged and reported false
func lmDownloadOptional(ctx context.Context, job map[string]any, plan []lmPlanRow) (bool, error) {
	err := lmDeps.downloadJob(ctx, job, plan)
	switch {
	case err == nil:
		return true, nil
	case lmIsDownloadPaused(err):
		return false, err
	case lmIsRuntimeOrOSError(err):
		slog.Warn(fmt.Sprintf("optional download failed; continuing without it: %s", err))
		return false, nil
	}
	return false, err
}

// Python int of a decoded n_ctx value
func lmPyInt(v any) (int64, error) {
	if s, ok := v.(string); ok {
		n, ok := pythonUnicodeIntText(s)
		if !ok {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(s))
		}
		return n, nil
	}
	if f, ok := v.(float64); ok {
		if math.IsNaN(f) {
			return 0, errors.New("cannot convert float NaN to integer")
		}
		if math.IsInf(f, 0) {
			return 0, errors.New("cannot convert float infinity to integer")
		}
		return int64(f), nil
	}
	n, ok := pythonInt(v)
	if !ok {
		return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))
	}
	return int64(n), nil
}

// Resident models of the live router with each one's window and spill placement facts
func lmLoadedModels(running map[string]any) (map[string]any, map[string]any, error) {
	body, err := lmDeps.routerRequest(running, "/models", 3*time.Second, nil)
	if err != nil {
		return nil, nil, err
	}
	data, err := lmDictGet(body, "data", []any{})
	if err != nil {
		return nil, nil, err
	}
	items, err := lmIterate(data)
	if err != nil {
		return nil, nil, err
	}
	loaded := map[string]any{}
	named := map[string]any{}
	var order []string
	for _, item := range items {
		status, err := lmDictGet(item, "status", map[string]any{})
		if err != nil {
			return nil, nil, err
		}
		value, err := lmDictGet(status, "value", nil)
		if err != nil {
			return nil, nil, err
		}
		if value != "loaded" && value != "ready" && value != "loading" {
			continue
		}
		id, ok := item.(map[string]any)["id"]
		if !ok {
			return nil, nil, &lmKeyError{"id"}
		}
		if err := lmHashable(id); err != nil {
			return nil, nil, err
		}
		key := lmJSONKey(id)
		if _, seen := loaded[key]; !seen {
			order = append(order, key)
			named[key] = id
		}
		loaded[key] = value
	}
	placement := map[string]any{}
	decisions := lmDeps.readPresetDecisions()
	for _, key := range order {
		modelID, isStr := named[key].(string)
		if !isStr {
			continue
		}
		state := loaded[key]
		facts := map[string]any{}
		if plan := decisions[modelID]; plan != nil {
			facts["window"], facts["window_label"], facts["spilled"] = plan.Window, lmKLabel(int64(plan.Window)), plan.Spilled
		}
		if state == "loaded" || state == "ready" {
			if nCtx := lmQuiet(func() (any, error) { return lmGrantedWindow(running, modelID) }, nil, "", ""); pyTruthy(nCtx) {
				n, err := lmPyInt(nCtx)
				if err != nil {
					return nil, nil, err
				}
				facts["granted_window"], facts["granted_window_label"] = n, lmKLabel(n)
			}
		}
		if len(facts) > 0 {
			placement[modelID] = facts
		}
	}
	return loaded, placement, nil
}

// n_ctx a loaded child was granted by its props
func lmGrantedWindow(running map[string]any, modelID string) (any, error) {
	props, err := lmDeps.routerRequest(running, "/props?"+childQuery(modelID), 3*time.Second, nil)
	if err != nil {
		return nil, err
	}
	settings, err := lmDictGet(props, "default_generation_settings", map[string]any{})
	if err != nil {
		return nil, err
	}
	return lmDictGet(settings, "n_ctx", nil)
}

// Status row of a staged model, a split sized by every part on disk
func lmStagedRow(gguf string) (map[string]any, error) {
	parts := SplitParts(gguf)
	if len(parts) == 0 {
		parts = []string{gguf}
	}
	var size int64
	for _, p := range parts {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		size += st.Size()
	}
	return map[string]any{"id": lmModelIDFor(gguf), "size_bytes": size, "size_label": LocalModelsHumanGB(float64(size))}, nil
}

// The active main model when model.provider names the managed llama.cpp runtime, nil otherwise
func lmActiveLlamacppModelID(ctx context.Context) any {
	read := func() (any, error) {
		cfg := lmDeps.loadConfig(ctx)
		var section any = map[string]any{}
		if raw := cfg["model"]; pyTruthy(raw) {
			section = raw
		}
		provider, err := lmDictGet(section, "provider", "")
		if err != nil {
			return nil, err
		}
		if !lmLlamacppProviders[pystr.Lower(pystr.Strip(pyStr(provider)))] {
			return nil, nil
		}
		m := section.(map[string]any)
		if id := pystr.Strip(pyOrStr(pyOr(m["default"], m["name"], ""))); id != "" {
			return id, nil
		}
		return nil, nil
	}
	return lmQuiet(read, nil, "", "")
}

// The managed server's recorded endpoint as base_url and api_key, nil when no live owner records one
func lmStateEndpoint() (map[string]any, error) {
	baseURL, apiKey, ok, err := stateEndpoint(home.Process())
	if err != nil || !ok {
		return nil, err
	}
	return map[string]any{"base_url": baseURL, "api_key": apiKey}, nil
}

// LocalModelsStatus answers the pane's first paint from config, the installed runtime, staged models and the live router
func LocalModelsStatus(ctx context.Context) (map[string]any, error) {
	section := lmDeps.runtimeSection(ctx)
	backend, err := lmSectionBackend(section)
	if err != nil {
		return nil, err
	}
	engine, err := lmDeps.installedEngine(ctx, backend)
	if err != nil {
		return nil, err
	}
	var runtimeBackend any
	if engine != nil {
		runtimeBackend = engine.Backend
	}
	var configuredTag string
	if engine != nil && engine.Backend != "" {
		if configuredTag, err = PinnedTag(engine.Backend); err != nil {
			return nil, err
		}
	} else if configuredTag, _, err = lmDeps.runtimeTarget(ctx, ""); err != nil {
		return nil, err
	}
	tag := configuredTag
	if engine != nil {
		tag = engine.Tag
	}
	var current *pm.InstalledPackage
	if engine != nil && engine.Backend != "" {
		name, ok := BackendPackages[engine.Backend]
		if !ok {
			return nil, &lmKeyError{engine.Backend}
		}
		if current, err = lmDeps.installedPackage(ctx, name); err != nil {
			return nil, err
		}
	}
	lmQuiet(lmDeps.adoptLegacyModels, nil, "legacy model adoption failed: ", "")
	mdir := lmDeps.modelsDir()
	running, err := lmDeps.stateEndpoint()
	if err != nil {
		return nil, err
	}
	loaded, placement := map[string]any{}, map[string]any{}
	if running != nil {
		read := lmQuiet(func() ([2]map[string]any, error) {
			l, p, err := lmLoadedModels(running)
			return [2]map[string]any{l, p}, err
		}, [2]map[string]any{loaded, placement}, "loaded-models read failed: ", "")
		loaded, placement = read[0], read[1]
	}
	enabled, _ := lmDictGet(section, "enabled", nil)
	var serverBaseURL any
	if running != nil {
		serverBaseURL = pyGetAny(running, "base_url", nil)
	}
	out := map[string]any{
		"enabled": pyTruthy(enabled), "tag": tag, "configured_tag": configuredTag,
		"update_available":  pyTruthy(enabled) && engine != nil && current == nil,
		"runtime_installed": runtimeBackend != nil, "runtime_backend": runtimeBackend,
		"server_running": running != nil, "server_base_url": serverBaseURL,
		"active_model_id": lmActiveLlamacppModelID(ctx), "loaded_models": loaded,
		"loading": lmLoadingPayload(), "placement": placement,
	}
	models := []any{}
	if lrPathExists(mdir) {
		for _, gguf := range lmDeps.stagedModels() {
			row, err := lmStagedRow(gguf)
			if err != nil {
				return nil, err
			}
			models = append(models, row)
		}
	}
	out["models"], out["models_dir"] = models, mdir
	return out, nil
}

// Live load progress per model as the stage, value and percent mapping the pane polls
func lmLoadingPayload() map[string]any {
	out := map[string]any{}
	for model, p := range lmDeps.loadingProgress() {
		out[model] = map[string]any{"stage": p.Stage, "value": p.Value, "percent": p.Percent}
	}
	return out
}

// GPU identity and live utilization from the shared nvidia-smi reading, empty for other vendors
func lmNvidiaSMIFacts() (map[string]any, error) {
	query, err := CachedNvidiaGPUQuery(hwGPUQueryTTLSeconds)
	if err != nil || query == nil {
		return map[string]any{}, err
	}
	return map[string]any{"gpu_name": query.GPUName, "gpu_util_percent": query.GPUUtilPercent, "vram_used_bytes": query.UsedBytes}, nil
}

// LocalModelsHardware reports the machine budget as plain facts with live NVIDIA readings where present
func LocalModelsHardware(ctx context.Context) (map[string]any, error) {
	budget, err := lmDeps.probeBudget(false)
	if err != nil {
		return nil, err
	}
	ramTotal, ramAvail, err := hwRAMBytes()
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"uma": budget.UMA, "vram_total_bytes": budget.TotalDeviceBytes, "vram_usable_bytes": budget.UsableVRAMBytes,
		"ram_total_bytes": ramTotal, "ram_available_bytes": ramAvail, "vram_label": LocalModelsHumanGB(float64(budget.TotalDeviceBytes)),
		"gpu_name": nil, "gpu_util_percent": nil, "vram_used_bytes": nil,
	}
	for k, v := range lmQuiet(lmNvidiaSMIFacts, map[string]any{}, "", "") {
		out[k] = v
	}
	return out, nil
}

// Whether every file a variant needs is on disk, the optional draft excluded
func lmInstalled(entry CatalogEntry, variant QuantVariant) bool {
	for _, f := range entry.InstallFiles(variant) {
		dir := lmDeps.assetsDir()
		if slices.Contains(variant.Files, f) {
			dir = lmDeps.modelsDir()
		}
		if !lrIsFile(filepath.Join(dir, f.LocalName())) {
			return false
		}
	}
	return true
}

// Catalog row priced for this machine, what downloads, whether it fits and the context shape it gets
func lmCatalogRow(ctx context.Context, entry CatalogEntry, budget HardwareBudget, recommended, recommendedReason any, stagedIDs map[string]bool) (map[string]any, error) {
	choice := catalogSelectVariantFn(entry, budget)
	var dl *QuantVariant
	for i, v := range entry.Variants {
		if stagedIDs[v.ModelID()] && lmInstalled(entry, v) {
			dl = &entry.Variants[i]
			break
		}
	}
	needsEngine, err := lmDeps.engineTooOld(ctx, entry.MinEngine)
	if err != nil {
		return nil, err
	}
	isRecommended := recommended != nil && entry.ID == recommended
	var reason any
	if isRecommended {
		reason = recommendedReason
	}
	var downloadedID, downloadedQuant, minEngine any
	if dl != nil {
		downloadedID, downloadedQuant = dl.ModelID(), dl.Quant
	}
	if entry.MinEngine != "" {
		minEngine = entry.MinEngine
	}
	nativeLabel := lmKLabel(int64(entry.NCtxTrain))
	row := map[string]any{
		"id": entry.ID, "display_name": entry.DisplayName, "description": entry.Description,
		"native_context": entry.NCtxTrain, "native_context_label": nativeLabel,
		"recommended": isRecommended, "recommended_reason": reason,
		"downloaded": dl != nil, "downloaded_model_id": downloadedID, "downloaded_quant": downloadedQuant,
		"mtp": entry.MTPCapable(), "vision": entry.MMProj != nil, "needs_engine": needsEngine, "min_engine": minEngine,
	}
	if choice == nil {
		smallest := entry.Variants[0]
		for _, v := range entry.Variants[1:] {
			if v.SizeBytes() < smallest.SizeBytes() {
				smallest = v
			}
		}
		total := entry.DownloadBytes(smallest)
		row["fits"], row["size_bytes"], row["size_label"] = false, total, LocalModelsHumanGB(float64(total))
		row["fit_summary"] = "Needs more memory than this machine has"
		row["fit_detail"] = fmt.Sprintf("even the most compact build (%s, %s) exceeds GPU + system memory", smallest.Quant, LocalModelsHumanGB(float64(total)))
		return row, nil
	}
	variant := choice.Variant
	decision := entry.LaunchPlan(variant, budget).Decision
	total := entry.DownloadBytes(variant)
	reasonText, ok := lmQuantReasons[choice.ReasonKey]
	if !ok {
		reasonText = lmQuantReasonCompact
	}
	quantReason := strings.ReplaceAll(reasonText, "{quant}", variant.Quant)
	row["fits"], row["model_id"], row["quant"] = true, variant.ModelID(), variant.Quant
	row["quant_validated"], row["size_bytes"], row["size_label"] = variant.Validated, total, LocalModelsHumanGB(float64(total))
	row["variant_count"], row["quant_reason"] = len(entry.Variants), quantReason
	window, isWindow := decision.(*WindowDecision)
	if !isWindow {
		row["fit_summary"] = quantReason
		return row, nil
	}
	startLabel := lmKLabel(int64(window.Window))
	row["start_window"], row["start_window_label"], row["spilled"] = window.Window, startLabel, window.Spilled()
	shape := fmt.Sprintf("starts at %s and grows toward %s as you use it", startLabel, nativeLabel)
	if window.Window >= entry.NCtxTrain {
		shape = fmt.Sprintf("runs at its full %s context", nativeLabel)
	}
	if window.Spilled() {
		shape += " (larger than your GPU memory — runs slower)"
	}
	row["fit_summary"] = shape
	return row, nil
}

// LocalModelsCatalog prices every catalog entry for this machine with the recommended pick marked
func LocalModelsCatalog(ctx context.Context) (map[string]any, error) {
	lmDeps.refreshCatalogSoon()
	budget, err := lmDeps.probeBudget(true)
	if err != nil {
		return nil, err
	}
	eligible, err := lmEligibleEntries(ctx)
	if err != nil {
		return nil, err
	}
	backend, err := lmSectionBackend(lmDeps.runtimeSection(ctx))
	if err != nil {
		return nil, err
	}
	var recommendedID, recommendedReason any
	if picked, reason := lmDeps.recommendedEntry(budget, eligible, backend); picked != nil {
		recommendedID, recommendedReason = picked.ID, reason
	}
	stagedIDs := map[string]bool{}
	for _, id := range lmDeps.stagedModelIDs() {
		stagedIDs[id] = true
	}
	rows := []any{}
	for _, entry := range Catalog() {
		row, err := lmCatalogRow(ctx, entry, budget, recommendedID, recommendedReason, stagedIDs)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return map[string]any{"models": rows}, nil
}

// Install stages written onto the job as phases while PM keeps the byte accounting
func lmRuntimeProgressHook(job map[string]any) pm.InstallProgress {
	phases := map[string][2]string{
		"download": {"downloading-runtime", "Downloading the local engine"},
		"unpack":   {"unpacking-runtime", "Unpacking the local engine"},
		"verify":   {"verifying-runtime", "Verifying the local engine"},
	}
	return func(stage string, done, total int64, label string) error {
		phase, ok := phases[stage]
		if !ok {
			return &lmKeyError{stage}
		}
		detail := phase[1]
		if label != "" {
			detail += " (" + label + ")"
		}
		return lmStep(job, phase[0], detail)
	}
}

// Installs the pinned engine through PM under the job's progress hooks and pause switch
func lmInstallEngineJob(ctx context.Context, job map[string]any, backend string) (Engine, error) {
	if err := lmStep(job, "installing-runtime", "Preparing the pinned local engine"); err != nil {
		return Engine{}, err
	}
	progress := lmDeps.runtimeProgressHook(job)
	downloadProgress := lmDeps.downloadProgressHook(job)
	lmJobsMu.Lock()
	jobID := lmJobID(job)
	running := lmRunning[jobID]
	lmJobsMu.Unlock()
	if running == nil {
		return Engine{}, &lmKeyError{jobID}
	}
	return lmDeps.ensureEngine(ctx, backend, EnsureEngineOptions{Progress: progress, DownloadProgress: downloadProgress, PauseEvent: running.pause})
}

// LocalModelsRuntimeInstall starts the job installing the pinned engine, fetching missing drafts and restarting a stale server
func LocalModelsRuntimeInstall(ctx context.Context, backendRequest, profile string) (map[string]any, error) {
	tag, backend, err := lmDeps.runtimeTarget(ctx, backendRequest)
	if err != nil {
		return nil, err
	}
	job := lmNewJob("runtime-install", fmt.Sprintf("llama.cpp %s (%s)", tag, backend), nil)
	jobID := lmJobID(job)
	owedRestart := false
	run := func(ctx context.Context) error {
		sctx, _, err := ConfigProfileScope(ctx, profile)
		if err != nil {
			return err
		}
		previous, err := lmDeps.installedEngine(sctx, backend)
		if err != nil {
			return err
		}
		engine, err := lmDeps.installEngineJob(sctx, job, backend)
		if err != nil {
			return err
		}
		drafts, err := lmMissingDrafts(engine.Tag)
		if err != nil {
			return err
		}
		if len(drafts) > 0 {
			before := owedRestart
			owedRestart = true
			if err := lmStep(job, "downloading", "Downloading draft models that speed up generation"); err != nil {
				return err
			}
			ok, err := lmDownloadOptional(sctx, job, drafts)
			if err != nil {
				return err
			}
			if !ok {
				after, err := lmMissingDrafts(engine.Tag)
				if err != nil {
					return err
				}
				if len(after) == len(drafts) {
					owedRestart = before
				}
			}
		}
		running := lmDeps.getSupervisor()
		serving := lmDeps.servingEngine()
		stale := previous == nil || *previous != engine || (serving != nil && serving.Tag != engine.Tag)
		if running != nil && (stale || owedRestart) {
			detail := "Restarting the local server"
			if stale {
				detail = "Switching the running server to the pinned build"
			}
			if err := lmStep(job, "restarting", detail); err != nil {
				return err
			}
			if _, err := lmDeps.refreshLocalRuntime(sctx); err != nil {
				return err
			}
			owedRestart = false
		}
		return lmFinish(job, fmt.Sprintf("llama.cpp %s ready (%s)", tag, backend))
	}
	lmSpawnJob(ctx, job, "lr-runtime-install", run, lmSpawnOptions{failMsg: "runtime install failed: %s", resumable: true})
	return map[string]any{"job_id": jobID, "backend": backend, "tag": tag}, nil
}

// Entry and variant of a family id priced for this machine, or of an exact variant model id
func lmDownloadTarget(ctx context.Context, modelID string) (CatalogEntry, QuantVariant, error) {
	entry, ok := CatalogByID()[modelID]
	if !ok {
		if e, v := FindEntryForModel(modelID); e != nil {
			return *e, *v, nil
		}
		_, err := lmEntryOr404(modelID)
		return CatalogEntry{}, QuantVariant{}, err
	}
	old, err := lmDeps.engineTooOld(ctx, entry.MinEngine)
	if err != nil {
		return CatalogEntry{}, QuantVariant{}, err
	}
	if old {
		return CatalogEntry{}, QuantVariant{}, httpException(409,
			fmt.Sprintf("%s needs llama.cpp %s or newer — update the engine first", entry.DisplayName, entry.MinEngine))
	}
	budget, err := lmDeps.probeBudget(true)
	if err != nil {
		return CatalogEntry{}, QuantVariant{}, err
	}
	choice := catalogSelectVariantFn(entry, budget)
	if choice == nil {
		return CatalogEntry{}, QuantVariant{}, httpException(409, fmt.Sprintf("no variant of %s fits this machine", entry.ID))
	}
	return entry, choice.Variant, nil
}

// LocalModelsDownload starts the download job of a family's build for this machine or of an exact variant
func LocalModelsDownload(ctx context.Context, modelID string) (map[string]any, error) {
	entry, variant, err := lmDeps.downloadTarget(ctx, modelID)
	if err != nil {
		return nil, err
	}
	plan := lmDeps.downloadPlan(entry, variant)
	if len(plan) > 0 && lmPlanOnDisk(plan) {
		return map[string]any{"job_id": nil, "already_downloaded": true, "model_id": variant.ModelID()}, nil
	}
	job := lmNewJob("model-download", fmt.Sprintf("%s (%s)", entry.DisplayName, variant.Quant), entry.ID)
	jobID := lmJobID(job)
	run := func(ctx context.Context) error {
		if err := lmDeps.runDownloadPlan(ctx, job, plan, entry.DisplayName, &entry); err != nil {
			return err
		}
		if err := lmFinish(job, entry.DisplayName+" ready"); err != nil {
			return err
		}
		lmDeps.refreshRuntime(ctx, lmDeps.refreshLocalRuntime, "post-download runtime refresh skipped")
		return nil
	}
	lmSpawnJob(ctx, job, "lr-model-download", run, lmSpawnOptions{failMsg: "model download failed: %s", resumable: true})
	return map[string]any{"job_id": jobID, "model_id": variant.ModelID()}, nil
}

// Whether every destination of a plan is a file on disk
func lmPlanOnDisk(plan []lmPlanRow) bool {
	for _, row := range plan {
		if !lrIsFile(row.Dest) {
			return false
		}
	}
	return true
}

// LocalModelsDownloadPause pauses the download phase of a model, component or quickstart job
func LocalModelsDownloadPause(ctx context.Context, jobID string) (map[string]any, error) {
	lmJobsMu.Lock()
	defer lmJobsMu.Unlock()
	job, ok := lmJobs[jobID]
	if !ok {
		return nil, httpException(404, "unknown download job")
	}
	if view := lmJobView(job); view["can_pause"] != true {
		return map[string]any{"ok": true, "paused": false}, nil
	}
	lmRunning[jobID].pause.Set()
	return map[string]any{"ok": true, "paused": true}, nil
}

// LocalModelsDownloadResume restarts a paused job, PM reusing verified files and durable ranges
func LocalModelsDownloadResume(ctx context.Context, jobID string) (map[string]any, error) {
	lmJobsMu.Lock()
	_, known := lmJobs[jobID]
	var resume func(ctx context.Context) bool
	if running := lmRunning[jobID]; running != nil {
		resume = running.resume
	}
	lmJobsMu.Unlock()
	if !known {
		return nil, httpException(404, "unknown download job")
	}
	return map[string]any{"ok": true, "resumed": resume != nil && resume(ctx)}, nil
}

// LocalModelsDelete removes every split part and companion of a model, then bounces the router off the request
func LocalModelsDelete(ctx context.Context, modelID string) (map[string]any, error) {
	files := lmVariantFilesOnDisk(modelID)
	if len(files) == 0 {
		return nil, httpException(404, "model not found")
	}
	for _, p := range files {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	lmQuiet(func() (struct{}, error) { return struct{}{}, lmDeps.clearWindowOverride(modelID) }, struct{}{}, "", "window-override clear skipped")
	refresh, refreshLocal := lmDeps.refreshRuntime, lmDeps.refreshLocalRuntime
	go refresh(context.WithoutCancel(ctx), refreshLocal, "post-delete runtime refresh skipped")
	return map[string]any{"ok": true}, nil
}

// Explicit model or the machine's recommendation followed by the other recommendable entries, the first that runs here
func lmQuickstartTarget(ctx context.Context, modelID string, budget HardwareBudget) (CatalogEntry, QuantVariant, error) {
	var candidates []CatalogEntry
	if modelID != "" {
		entry, err := lmEntryOr404(modelID)
		if err != nil {
			return CatalogEntry{}, QuantVariant{}, err
		}
		candidates = []CatalogEntry{entry}
	} else {
		eligible, err := lmEligibleEntries(ctx)
		if err != nil {
			return CatalogEntry{}, QuantVariant{}, err
		}
		backend, err := lmSectionBackend(lmDeps.runtimeSection(ctx))
		if err != nil {
			return CatalogEntry{}, QuantVariant{}, err
		}
		picked, _ := lmDeps.recommendedEntry(budget, eligible, backend)
		if picked == nil {
			return CatalogEntry{}, QuantVariant{}, httpException(409,
				"No automatic recommendation for this machine — open Local Models to browse or choose a model explicitly")
		}
		candidates = []CatalogEntry{*picked}
		for _, e := range Catalog() {
			if e.ID != picked.ID && e.Recommend {
				candidates = append(candidates, e)
			}
		}
	}
	for _, candidate := range candidates {
		choice := catalogSelectVariantFn(candidate, budget)
		if choice == nil {
			continue
		}
		old, err := lmDeps.engineTooOld(ctx, candidate.MinEngine)
		if err != nil {
			return CatalogEntry{}, QuantVariant{}, err
		}
		if !old {
			return candidate, choice.Variant, nil
		}
	}
	return CatalogEntry{}, QuantVariant{}, httpException(409,
		"no catalog model fits this machine — open Local Models to browse for a smaller build")
}

// Re-picks once the engine is installed, its device probe pricing the GPU the preflight could only guess
func lmRepricedQuickstart(ctx context.Context, job map[string]any, modelID string, variant QuantVariant) (CatalogEntry, QuantVariant, []lmPlanRow, error) {
	budget, err := lmDeps.probeBudget(true)
	if err != nil {
		return CatalogEntry{}, QuantVariant{}, nil, err
	}
	entry, picked, err := lmDeps.quickstartTarget(ctx, modelID, budget)
	if err != nil {
		return CatalogEntry{}, QuantVariant{}, nil, err
	}
	if picked.ModelID() != variant.ModelID() {
		lmJobsMu.Lock()
		job["target"], job["model_id"] = entry.DisplayName, entry.ID
		lmJobsMu.Unlock()
	}
	plan := lmDeps.downloadPlan(entry, picked)
	if lmPlanOnDisk(plan) {
		plan = []lmPlanRow{}
	}
	return entry, picked, plan, nil
}

// LocalModelsQuickstart starts the one job installing the runtime, downloading this machine's build and making it the default
func LocalModelsQuickstart(ctx context.Context, modelID, profile string) (map[string]any, error) {
	budget, err := lmDeps.probeBudget(true)
	if err != nil {
		return nil, err
	}
	entry, variant, err := lmDeps.quickstartTarget(ctx, modelID, budget)
	if err != nil {
		return nil, err
	}
	_, backend, err := lmDeps.runtimeTarget(ctx, "")
	if err != nil {
		return nil, err
	}
	installed, err := lmDeps.installedEngine(ctx, backend)
	if err != nil {
		return nil, err
	}
	needRuntime := installed == nil
	downloadPlan := lmDeps.downloadPlan(entry, variant)
	needDownload := !lmPlanOnDisk(downloadPlan)
	if !needDownload {
		downloadPlan = []lmPlanRow{}
	}
	var downloadBytes int64
	for _, row := range downloadPlan {
		downloadBytes += row.Bytes
	}
	if !lmQuickstartMu.TryLock() {
		return nil, httpException(409, "Setup is already running")
	}
	job := lmNewJob("quickstart", entry.DisplayName, entry.ID)
	jobID := lmJobID(job)
	resp := map[string]any{"job_id": jobID, "model_id": entry.ID, "display_name": entry.DisplayName,
		"needs_runtime": needRuntime, "needs_download": needDownload, "download_bytes": downloadBytes}
	run := func(ctx context.Context) error {
		if needRuntime {
			current, err := lmDeps.installedEngine(ctx, backend)
			if err != nil {
				return err
			}
			if current == nil {
				if _, err := lmDeps.installEngineJob(ctx, job, backend); err != nil {
					return err
				}
				if entry, variant, downloadPlan, err = lmRepricedQuickstart(ctx, job, modelID, variant); err != nil {
					return err
				}
			}
		}
		if len(downloadPlan) > 0 {
			lmJobsMu.Lock()
			if job["phase"] != "downloading" {
				job["done_bytes"], job["total_bytes"], job["ranges"] = int64(0), nil, pyjson.NewObject()
			}
			lmJobsMu.Unlock()
			if err := lmDeps.runDownloadPlan(ctx, job, downloadPlan, entry.DisplayName, &entry); err != nil {
				return err
			}
		}
		if err := lmStep(job, "starting-server", "Starting the local server"); err != nil {
			return err
		}
		sctx, _, err := ConfigProfileScope(ctx, profile)
		if err != nil {
			return err
		}
		cfg, err := lmDeps.setRuntimeEnabled(sctx, true)
		if err != nil {
			return err
		}
		if err := lmDeps.ensureServer(sctx, job, cfg, variant.ModelID(), "The local server could not start — open Local Models for details",
			"quickstart rescan check skipped"); err != nil {
			return err
		}
		if err := lmDeps.assignDefault(sctx, job, variant.ModelID()); err != nil {
			return err
		}
		return lmFinish(job, entry.DisplayName+" is ready — new chats use it")
	}
	lmSpawnJob(ctx, job, "lr-quickstart", run, lmSpawnOptions{failMsg: "quickstart failed: %s", onExit: lmQuickstartMu.Unlock, resumable: true})
	return resp, nil
}

// Stops a recorded orphan through explicit recovery, a 409 when a live owner holds the server
func lmTerminateStatePID() error {
	stopped, err := lmDeps.stopRecordedOrphan()
	if err != nil {
		return err
	}
	if !stopped {
		return httpException(409, "Another Icebath process owns this server, or its ownership could not be verified")
	}
	return nil
}

// Tears the server down and persists the runtime as disabled
func lmStopServer(ctx context.Context) error {
	if lmDeps.getSupervisor() != nil {
		if err := lmDeps.shutdownLocalRuntime(); err != nil {
			return err
		}
	} else if err := lmDeps.terminateStatePID(); err != nil {
		return err
	}
	_, err := lmDeps.setRuntimeEnabled(ctx, false)
	return err
}

// Persists the runtime as enabled and force starts the server
func lmStartServer(ctx context.Context) error {
	cfg, err := lmDeps.setRuntimeEnabled(ctx, true)
	if err != nil {
		return err
	}
	_, err = lmStartLocalServer(ctx, cfg, lmServerStartFailed)
	return err
}

// LocalModelsServer turns the local engine off durably or back on, any failure but a refusal answering 502
func LocalModelsServer(ctx context.Context, action string) (map[string]any, error) {
	action = pystr.Lower(pystr.Strip(action))
	actions := map[string]func(context.Context) error{"stop": lmStopServer, "start": lmStartServer}
	fn, ok := actions[action]
	if !ok {
		return nil, httpException(400, "action must be 'stop' or 'start'")
	}
	if err := fn(ctx); err != nil {
		var refusal *HTTPException
		if errors.As(err, &refusal) {
			return nil, err
		}
		return nil, httpException(502, err.Error())
	}
	return map[string]any{"ok": true, "action": action}, nil
}

// LocalModelsEject frees a loaded model's memory now through the supervisor or the persisted endpoint
func LocalModelsEject(ctx context.Context, modelID string) (map[string]any, error) {
	if sup := lmDeps.getSupervisor(); sup != nil {
		if err := sup.UnloadModel(modelID); err != nil {
			return nil, lmHTTPError(502, "", err)
		}
		return map[string]any{"ok": true}, nil
	}
	endpoint, err := lmDeps.stateEndpoint()
	if err != nil {
		return nil, err
	}
	if endpoint == nil {
		return nil, httpException(409, "local server is not running")
	}
	if _, err := lmDeps.routerRequest(endpoint, "/models/unload", 120*time.Second, lrRequestBody("model", modelID)); err != nil {
		return nil, lmHTTPError(502, "", err)
	}
	return map[string]any{"ok": true}, nil
}

// LocalModelsActivate starts the job making a downloaded model the default for new chats with the server up
func LocalModelsActivate(ctx context.Context, modelID, profile string) (map[string]any, error) {
	if !slices.Contains(lmDeps.stagedModelIDs(), modelID) {
		return nil, httpException(404, modelID+" is not downloaded")
	}
	job := lmNewJob("model-activate", modelID, modelID)
	jobID := lmJobID(job)
	run := func(ctx context.Context) error {
		sctx, _, err := ConfigProfileScope(ctx, profile)
		if err != nil {
			return err
		}
		cfg, err := configLoadFn(sctx)
		if err != nil {
			return err
		}
		if err := lmDeps.ensureServer(sctx, job, cfg, modelID, lmServerStartFailed, "activate rescan check skipped"); err != nil {
			return err
		}
		if err := lmStep(job, "setting-default", "Making it your default"); err != nil {
			return err
		}
		if _, err := lmDeps.setRuntimeEnabled(sctx, true); err != nil {
			return err
		}
		if err := lmDeps.assignDefault(sctx, job, modelID); err != nil {
			return err
		}
		return lmFinish(job, modelID+" is the default for new chats")
	}
	lmSpawnJob(ctx, job, "lr-model-activate", run, lmSpawnOptions{failMsg: "model activate failed: %s"})
	return map[string]any{"job_id": jobID}, nil
}

// LocalModelsJobs lists running and paused jobs first, then the twenty newest settled ones
func LocalModelsJobs(ctx context.Context) (map[string]any, error) {
	lmJobsMu.Lock()
	defer lmJobsMu.Unlock()
	jobs := make([]map[string]any, 0, len(lmJobOrder))
	for _, id := range lmJobOrder {
		if job, ok := lmJobs[id]; ok {
			jobs = append(jobs, job)
		}
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		a, _ := jobs[i]["started_at"].(float64)
		b, _ := jobs[j]["started_at"].(float64)
		return -a < -b
	})
	var active, recent []map[string]any
	for _, job := range jobs {
		switch job["status"] {
		case "running", "paused":
			active = append(active, job)
		case "done", "error":
			recent = append(recent, job)
		}
	}
	if len(recent) > 20 {
		recent = recent[:20]
	}
	views := []any{}
	for _, job := range append(active, recent...) {
		views = append(views, lmJobView(job))
	}
	return map[string]any{"jobs": views}, nil
}

// LocalModelsJob is one job's wire view, a 404 for an unknown id
func LocalModelsJob(ctx context.Context, jobID string) (map[string]any, error) {
	lmJobsMu.Lock()
	defer lmJobsMu.Unlock()
	job, ok := lmJobs[jobID]
	if !ok {
		return nil, httpException(404, "job not found")
	}
	return lmJobView(job), nil
}

// LocalModelsJobView is the job view the terminal flow renders, a KeyError for an unknown id
func LocalModelsJobView(jobID string) (map[string]any, error) {
	lmJobsMu.Lock()
	defer lmJobsMu.Unlock()
	job, ok := lmJobs[jobID]
	if !ok {
		return nil, &lmKeyError{jobID}
	}
	return lmJobView(job), nil
}

// LocalModelsPauseJob raises a job's pause switch, false when the job has none
func LocalModelsPauseJob(jobID string) bool {
	lmJobsMu.Lock()
	defer lmJobsMu.Unlock()
	running := lmRunning[jobID]
	if running == nil || running.pause == nil {
		return false
	}
	running.pause.Set()
	return true
}

// LocalModelsSearch is the full text Hugging Face search over GGUF models behind the curated catalog
func LocalModelsSearch(ctx context.Context, q string, limit int) (map[string]any, error) {
	if pystr.Strip(q) == "" {
		return map[string]any{"hits": []any{}}, nil
	}
	hits, err := lmDeps.searchModels(q, limit)
	if err != nil {
		return nil, lmHTTPError(502, "Hugging Face search unavailable: ", err)
	}
	out := []any{}
	for _, h := range hits {
		out = append(out, map[string]any{"repo": h.Repo, "downloads": h.Downloads, "likes": h.Likes, "updated": h.Updated, "gated": h.Gated})
	}
	return map[string]any{"hits": out}, nil
}

// LocalModelsSearchFiles lists the servable GGUFs of one repo with a rough fit verdict per quant
func LocalModelsSearchFiles(ctx context.Context, repo string) (map[string]any, error) {
	prefix := fmt.Sprintf("Could not list %s: ", repo)
	budget, err := lmDeps.probeBudget(true)
	if err != nil {
		return nil, lmHTTPError(502, prefix, err)
	}
	groups, err := lmDeps.pricedRepoFiles(repo, budget)
	if err != nil {
		return nil, lmHTTPError(502, prefix, err)
	}
	out := []any{}
	for _, g := range groups {
		paths := make([]any, len(g.Paths))
		for i, p := range g.Paths {
			paths[i] = p
		}
		out = append(out, map[string]any{"label": g.Label, "paths": paths, "total_bytes": g.TotalBytes, "fit": g.Fit})
	}
	return map[string]any{"files": out}, nil
}

// LocalModelsDownloadBrowsed downloads an arbitrary Hugging Face GGUF, every split part in order, into the models dir
func LocalModelsDownloadBrowsed(ctx context.Context, repo string, paths []string) (map[string]any, error) {
	var ggufs []string
	for _, p := range paths {
		if strings.HasSuffix(pystr.Lower(p), ".gguf") {
			ggufs = append(ggufs, p)
		}
	}
	if len(ggufs) == 0 {
		return nil, httpException(422, "no .gguf files given")
	}
	modelID := lmBrowsedModelIDRE.ReplaceAllString(lmLastSegment(ggufs[0]), "$1")
	if slices.Contains(lmDeps.stagedModelIDs(), modelID) {
		return map[string]any{"job_id": nil, "already_downloaded": true, "model_id": modelID}, nil
	}
	job := lmNewJob("model-download", fmt.Sprintf("%s (from %s)", modelID, repo), modelID)
	jobID := lmJobID(job)
	plan := make([]lmPlanRow, 0, len(ggufs))
	for _, p := range ggufs {
		plan = append(plan, lmPlanRow{lmDeps.hfURL(repo, hfQuote(p), "main"), filepath.Join(lmDeps.modelsDir(), lmLastSegment(p)), 0})
	}
	fetch := func(ctx context.Context) error { return lmDeps.runDownloadPlan(ctx, job, plan, modelID, nil) }
	lmSpawnJob(ctx, job, "lm-download-browsed", fetch, lmSpawnOptions{downloadLabel: &modelID, resumable: true})
	return map[string]any{"job_id": jobID, "model_id": modelID}, nil
}

// Text after the last slash
func lmLastSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// pathlib suffix of a path's final component
func lmPathSuffix(p string) string {
	name := filepath.Base(p)
	if i := strings.LastIndex(name, "."); i > 0 && i < len(name)-1 {
		return name[i:]
	}
	return ""
}

// LocalModelsSideload links a GGUF already on this machine into the models dir, copying only when linking fails
func LocalModelsSideload(ctx context.Context, path string) (map[string]any, error) {
	path = pypath.PathStr(path)
	if !lrIsFile(path) || pystr.Lower(lmPathSuffix(path)) != ".gguf" {
		return nil, httpException(422, "Pick a .gguf model file")
	}
	dest := filepath.Join(lmDeps.modelsDir(), filepath.Base(path))
	if lrPathExists(dest) {
		return map[string]any{"ok": true, "model_id": pyPathStem(dest), "already_present": true}, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return nil, err
	}
	if err := os.Link(path, dest); err != nil {
		if err := os.Symlink(path, dest); err != nil {
			if err := pyshutil.CopyFile(path, dest); err != nil {
				return nil, err
			}
		}
	}
	lmDeps.refreshRuntime(ctx, lmDeps.refreshLocalRuntime, "post-sideload runtime refresh skipped")
	return map[string]any{"ok": true, "model_id": pyPathStem(dest)}, nil
}
