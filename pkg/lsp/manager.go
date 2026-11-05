package lsp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Service lifetime defaults in seconds
const (
	DefaultIdleTimeout = 600.0
	minIdleTimeout     = 30.0
	deltaBaselineCap   = 256
)

const managerLogger = "agent.lsp.manager"

// Server id and root a client or broken pair is keyed by
type pairKey struct{ serverID, root string }

// Retry deadline of a broken pair, forever meaning until restart
type brokenEntry struct {
	deadline time.Time
	forever  bool
}

// Options configure a service the way the lsp config section does
type Options struct {
	Enabled            bool
	WaitMode           string
	WaitTimeout        float64
	InstallStrategy    string
	BinaryOverrides    map[string][]string
	EnvOverrides       map[string]map[string]string
	InitOverrides      map[string]map[string]any
	DisabledServers    []string
	IdleTimeout        float64
	ExtraServers       []*ServerDef
	BrokenRetrySeconds float64
	WarmupTimeout      float64
	ExcludeRoots       any
	TrustedWorkspaces  any
	OperatorRoots      map[string]bool
}

// Service owns the language server clients of one profile and the post write checks that use them
type Service struct {
	rt                *Runtime
	enabled           bool
	waitMode          string
	waitTimeout       float64
	installStrategy   string
	binaryOverrides   map[string][]string
	envOverrides      map[string]map[string]string
	initOverrides     map[string]map[string]any
	disabledServers   map[string]bool
	idleTimeout       float64
	extraServers      []*ServerDef
	brokenRetry       float64
	warmupTimeout     float64
	excludeRoots      []string
	excludeInvalid    bool
	trustedWorkspaces []string
	untrustedSkipped  map[pairKey]bool
	operatorRoots     atomic.Pointer[map[string]bool]

	l             *loop
	stateMu       sync.Mutex
	clients       *omap[pairKey, *Client]
	broken        *omap[pairKey, brokenEntry]
	spawning      map[pairKey]*future[*Client]
	lastUsed      map[pairKey]time.Time
	reaperTask    *task
	deltaBaseline *omap[string, []map[string]any]
	checks        *editChecks
	reapOnce      func(ctx context.Context) error
}

// Python float() of a config value
func pyFloat(v any) (float64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case float64:
		return t, nil
	case string:
		s := strings.ToLower(pystr.Strip(t))
		switch strings.TrimLeft(s, "+-") {
		case "inf", "infinity":
			if strings.HasPrefix(s, "-") {
				return math.Inf(-1), nil
			}
			return math.Inf(1), nil
		case "nan":
			return math.NaN(), nil
		}
		if !strings.Contains(s, "__") && !strings.HasPrefix(strings.TrimLeft(s, "+-"), "_") && !strings.HasSuffix(s, "_") {
			if f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64); err == nil || errors.Is(err, strconv.ErrRange) {
				return f, nil
			}
		}
		return 0, &pyExc{typ: "ValueError", text: "could not convert string to float: " + pyrepr.StrRepr(t)}
	}
	return 0, &pyExc{typ: "TypeError", text: fmt.Sprintf("float() argument must be a string or a real number, not '%s'", pyrepr.TypeName(v))}
}

// Float of a config value with a default for None and unconvertible values
func floatOr(v any, fallback float64) float64 {
	if v == nil {
		return fallback
	}
	f, err := pyFloat(v)
	if err != nil {
		return fallback
	}
	return f
}

// Config list of paths with the tilde expanded, false when not a list of strings
func pathList(v any) ([]string, bool) {
	if v == nil {
		return []string{}, true
	}
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := []string{}
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		if s != "" {
			out = append(out, pypath.Expanduser(s))
		}
	}
	return out, true
}

// ParseTrustedWorkspaces reads lsp.trusted_workspaces, a malformed value trusting nothing extra
func ParseTrustedWorkspaces(rt *Runtime, value any) []string {
	roots, ok := pathList(value)
	if !ok {
		rt.logf(slog.LevelWarn, eventLogger, "lsp.trusted_workspaces must be a list of directories, e.g. ['~/code/my-app'] (got %s); only the "+
			"workspaces you launched Icebath or opened a session in are trusted until the key is fixed", pyrepr.TypeName(value))
		return []string{}
	}
	return roots
}

// Exclude globs, false failing closed on a malformed value
func parseExcludeRoots(rt *Runtime, value any) ([]string, bool) {
	roots, ok := pathList(value)
	if !ok {
		rt.logf(slog.LevelWarn, eventLogger, "lsp.exclude_roots must be a list of glob patterns, e.g. ['~/big-monorepo', '/srv/*/vendor'] "+
			"(got %s); LSP is skipped for every workspace until the key is fixed", pyrepr.TypeName(value))
	}
	return roots, ok
}

// Client key of a root, multi root servers sharing one process across trusted roots
func clientKeyFor(srv *ServerDef, root string, trusted bool) pairKey {
	if srv.MultiRoot && trusted {
		return pairKey{srv.ServerID, ""}
	}
	return pairKey{srv.ServerID, root}
}

// Python max(0.0, f), NaN losing to zero
func pyMax0(f float64) float64 {
	if f > 0 {
		return f
	}
	return 0
}

// Seconds as a duration, NaN treated as zero
func seconds(f float64) time.Duration {
	if math.IsNaN(f) {
		return 0
	}
	if f > float64(math.MaxInt64)/1e9 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(f * float64(time.Second))
}

// NewService builds a service and starts its loop and idle reaper when enabled
func NewService(rt *Runtime, o Options) *Service {
	s := &Service{
		rt: rt, enabled: o.Enabled, waitMode: o.WaitMode, waitTimeout: o.WaitTimeout, installStrategy: o.InstallStrategy,
		binaryOverrides: o.BinaryOverrides, envOverrides: o.EnvOverrides, initOverrides: o.InitOverrides,
		disabledServers: map[string]bool{}, idleTimeout: o.IdleTimeout, extraServers: append([]*ServerDef{}, o.ExtraServers...),
		brokenRetry: pyMax0(o.BrokenRetrySeconds), warmupTimeout: pyMax0(o.WarmupTimeout),
		untrustedSkipped: map[pairKey]bool{},
		l:                newLoop(), clients: newOmap[pairKey, *Client](), broken: newOmap[pairKey, brokenEntry](),
		spawning: map[pairKey]*future[*Client]{}, lastUsed: map[pairKey]time.Time{},
		deltaBaseline: newOmap[string, []map[string]any](), checks: newEditChecks(),
	}
	if s.waitMode != "document" && s.waitMode != "full" {
		s.waitMode = "document"
	}
	if s.binaryOverrides == nil {
		s.binaryOverrides = map[string][]string{}
	}
	if s.envOverrides == nil {
		s.envOverrides = map[string]map[string]string{}
	}
	if s.initOverrides == nil {
		s.initOverrides = map[string]map[string]any{}
	}
	for _, d := range o.DisabledServers {
		s.disabledServers[d] = true
	}
	roots, valid := parseExcludeRoots(rt, o.ExcludeRoots)
	s.excludeRoots, s.excludeInvalid = roots, !valid
	s.trustedWorkspaces = ParseTrustedWorkspaces(rt, o.TrustedWorkspaces)
	opRoots := maps.Clone(o.OperatorRoots)
	if opRoots == nil {
		opRoots = map[string]bool{}
	}
	s.operatorRoots.Store(&opRoots)
	s.reapOnce = s.reapIdleOnce
	if s.enabled {
		s.l.start()
		if s.idleTimeout > 0 {
			s.reaperTask = s.l.spawn(context.Background(), s.idleReaperLoop)
		}
	}
	return s
}

// ServiceFromConfig builds a service from the lsp config section the way create_from_config does
func ServiceFromConfig(rt *Runtime, lspCfg any, operatorRoots map[string]bool) (*Service, error) {
	cfg, ok := lspCfg.(map[string]any)
	if !ok {
		cfg = map[string]any{}
	}
	idle, err := pyFloat(dgetOr(cfg, "idle_timeout", DefaultIdleTimeout))
	if err != nil {
		idle = DefaultIdleTimeout
	}
	if idle > 0 && idle < minIdleTimeout {
		idle = minIdleTimeout
	}
	servers := map[string]map[string]any{}
	if sc, ok := pyOr(cfg["servers"], map[string]any{}).(map[string]any); ok {
		for n, c := range sc {
			if m, ok := c.(map[string]any); ok {
				servers[n] = m
			}
		}
	}
	waitMode := ""
	switch wm := dgetOr(cfg, "wait_mode", "document").(type) {
	case string:
		waitMode = wm
	case []any, map[string]any:
		return nil, &pyExc{typ: "TypeError", text: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(wm))}
	}
	waitTimeout, err := pyFloat(dgetOr(cfg, "wait_timeout", DiagnosticsDocumentWait.Seconds()))
	if err != nil {
		return nil, err
	}
	installStrategy := "auto"
	if v, ok := cfg["install_strategy"]; ok {
		installStrategy = pyrepr.Str(v)
		if s, isStr := v.(string); isStr {
			installStrategy = s
		}
	}
	o := Options{
		Enabled: pyrepr.Truthy(dgetOr(cfg, "enabled", true)), WaitMode: waitMode, WaitTimeout: waitTimeout,
		InstallStrategy: installStrategy, BinaryOverrides: map[string][]string{}, EnvOverrides: map[string]map[string]string{},
		InitOverrides: map[string]map[string]any{}, IdleTimeout: idle,
		BrokenRetrySeconds: floatOr(cfg["broken_retry_seconds"], 0), WarmupTimeout: floatOr(cfg["warmup_timeout"], 0),
		ExcludeRoots: cfg["exclude_roots"], TrustedWorkspaces: cfg["trusted_workspaces"], OperatorRoots: operatorRoots,
	}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	serversAny := map[string]any{}
	for _, n := range names {
		c := servers[n]
		serversAny[n] = c
		if cmd, ok := c["command"].([]any); ok && len(cmd) > 0 {
			parts := make([]string, len(cmd))
			for i, p := range cmd {
				parts[i] = pyrepr.Str(p)
			}
			o.BinaryOverrides[n] = parts
		}
		if env, ok := c["env"].(map[string]any); ok {
			m := map[string]string{}
			for k, v := range env {
				m[k] = pyrepr.Str(v)
			}
			o.EnvOverrides[n] = m
		}
		if init, ok := c["initialization_options"].(map[string]any); ok {
			o.InitOverrides[n] = init
		}
		if pyrepr.Truthy(c["disabled"]) {
			o.DisabledServers = append(o.DisabledServers, n)
		}
	}
	o.ExtraServers = CustomServers(rt, serversAny)
	return NewService(rt, o), nil
}

// Config declared servers first, then the built in registry
func (s *Service) serverFor(filePath string) *ServerDef {
	if len(s.extraServers) > 0 {
		if srv := FindServerForFile(filePath, s.extraServers); srv != nil {
			return srv
		}
	}
	return FindServerForFile(filePath, nil)
}

// HandlesExtension reports whether a config declared or built in server claims the extension
func (s *Service) HandlesExtension(ext string) bool {
	ext = strings.ToLower(ext)
	for _, srv := range s.extraServers {
		if srv.Handles(ext) {
			return true
		}
	}
	return StaticRegistryHandles(ext)
}

// IsActive reports whether the service should be consulted at all
func (s *Service) IsActive() bool { return s.enabled }

// Broken set key of a file, false when the file is not gated into a workspace
func (s *Service) brokenKey(srv *ServerDef, filePath string) (pairKey, bool) {
	wsRoot, gated := ResolveWorkspaceForFile(filePath, "")
	if wsRoot == "" || !gated {
		return pairKey{}, false
	}
	root := wsRoot
	func() {
		defer func() {
			if recover() != nil {
				root = wsRoot
			}
		}()
		if r := srv.ResolveRoot(filePath, wsRoot); r != "" {
			root = r
		}
	}()
	return pairKey{srv.ServerID, root}, true
}

// EnabledFor reports whether LSP runs for the file, remembering the calling session's operator roots
func (s *Service) EnabledFor(filePath string, operatorRoots map[string]bool) bool {
	var srv *ServerDef
	if s.enabled {
		srv = s.serverFor(filePath)
	}
	if srv == nil || s.disabledServers[srv.ServerID] {
		return false
	}
	key, ok := s.brokenKey(srv, filePath)
	if !ok {
		return false
	}
	if s.rootExcluded(key.root) {
		logRootExcluded(s.rt, srv.ServerID, key.root, filePath, s.excludeInvalid)
		return false
	}
	s.noteOperatorRoots(operatorRoots)
	if s.untrustedDenied(srv, key.root, filePath, s.trusted(key.root)) {
		return false
	}
	if s.isBroken(key) {
		retryIn, has := s.brokenRetryIn(key)
		logSkippedBroken(s.rt, srv.ServerID, key.root, filePath, retryIn, has)
		return false
	}
	return true
}

// Remembers a session's operator roots so trust spans every session of the process
func (s *Service) noteOperatorRoots(roots map[string]bool) {
	cur := *s.operatorRoots.Load()
	subset := true
	for r := range roots {
		if !cur[r] {
			subset = false
			break
		}
	}
	if subset {
		return
	}
	s.stateMu.Lock()
	merged := maps.Clone(*s.operatorRoots.Load())
	maps.Copy(merged, roots)
	s.operatorRoots.Store(&merged)
	s.stateMu.Unlock()
}

func (s *Service) trusted(root string) bool {
	return IsTrustedWorkspace(root, s.trustedWorkspaces, *s.operatorRoots.Load())
}

// Client key of a root with the state lock held, an untrusted multi root client keeping its own key
func (s *Service) liveKey(srv *ServerDef, root string) pairKey {
	own := pairKey{srv.ServerID, root}
	if _, ok := s.clients.get(own); ok {
		return own
	}
	if _, ok := s.spawning[own]; ok {
		return own
	}
	return clientKeyFor(srv, root, s.trusted(root))
}

// Whether a server that may run project code is denied in an untrusted root
func (s *Service) untrustedDenied(srv *ServerDef, root, filePath string, trusted bool) bool {
	if trusted || UntrustedSafeServers[srv.ServerID] {
		return false
	}
	s.stateMu.Lock()
	s.untrustedSkipped[pairKey{srv.ServerID, root}] = true
	s.stateMu.Unlock()
	logUntrustedSkipped(s.rt, srv.ServerID, root, filePath)
	return true
}

// Whether the root matches an exclude glob or the key is malformed
func (s *Service) rootExcluded(root string) bool {
	if s.excludeInvalid {
		return true
	}
	sep := string(filepath.Separator)
	for _, pat := range s.excludeRoots {
		if fnmatch.FnmatchCase(root, pat) || fnmatch.FnmatchCase(root, strings.TrimRight(pat, sep)+sep+"*") {
			return true
		}
	}
	return false
}

// Broken set membership, an entry past its retry deadline dropped for one more try
func (s *Service) isBroken(key pairKey) bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	entry, ok := s.broken.get(key)
	if !ok {
		return false
	}
	if !entry.forever && !time.Now().Before(entry.deadline) {
		s.broken.pop(key)
		return false
	}
	return true
}

func (s *Service) brokenRetryIn(key pairKey) (float64, bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	entry, ok := s.broken.get(key)
	if !ok || entry.forever {
		return 0, false
	}
	return max(0, time.Until(entry.deadline).Seconds()), true
}

func (s *Service) markBroken(key pairKey) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.brokenRetry > 0 {
		s.broken.set(key, brokenEntry{deadline: time.Now().Add(seconds(s.brokenRetry))})
	} else {
		s.broken.set(key, brokenEntry{forever: true})
	}
}

// Wait budget of a file with the loop held, the warm up budget for a cold root
func (s *Service) waitBudgetOnLoop(filePath string) time.Duration {
	if s.warmupTimeout <= s.waitTimeout {
		return seconds(s.waitTimeout)
	}
	srv := s.serverFor(filePath)
	if srv == nil {
		return seconds(s.waitTimeout)
	}
	key, ok := s.brokenKey(srv, filePath)
	if !ok {
		return seconds(s.waitTimeout)
	}
	s.stateMu.Lock()
	client, _ := s.clients.get(s.liveKey(srv, key.root))
	s.stateMu.Unlock()
	if client != nil && client.isRunning() {
		return seconds(s.waitTimeout)
	}
	return seconds(s.warmupTimeout)
}

// Wait budget of a file from outside the loop
func (s *Service) waitBudget(filePath string) time.Duration {
	var d time.Duration
	s.l.with(func() { d = s.waitBudgetOnLoop(filePath) })
	return d
}

// SnapshotBaseline records the current diagnostics of a file as the delta baseline before a write
func (s *Service) SnapshotBaseline(filePath string, operatorRoots map[string]bool) error {
	if !s.EnabledFor(filePath, operatorRoots) {
		return nil
	}
	budget := s.waitBudget(filePath)
	t := max(DiagnosticsDocumentWait+3*time.Second, budget+3*time.Second)
	diags, err := runOnLoop(s.l, t, func(ctx context.Context) ([]map[string]any, error) {
		return s.snapshotAsync(ctx, filePath, budget)
	})
	if err != nil {
		s.rt.logf(slog.LevelDebug, managerLogger, "baseline snapshot failed for %s: %s", filePath, errText(err))
		s.markBrokenForFile(filePath, err)
		diags = nil
	}
	if diags == nil {
		diags = []map[string]any{}
	}
	absPath, err := pypath.Abspath(filePath)
	if err != nil {
		return err
	}
	s.setDeltaBaseline(absPath, diags)
	return nil
}

// StartEditCheck schedules the non blocking post write check, nil when LSP does not handle the file
func (s *Service) StartEditCheck(filePath string, preText, postText *string, owner string, carry []Note, operatorRoots map[string]bool) (*EditFuture, error) {
	if !s.EnabledFor(filePath, operatorRoots) {
		return nil, nil
	}
	return startEditCheck(s, filePath, preText, postText, owner, carry)
}

// Stores a baseline as the newest entry so eviction follows write order
func (s *Service) setDeltaBaseline(absPath string, diags []map[string]any) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.deltaBaseline.pop(absPath)
	s.deltaBaseline.set(absPath, diags)
	for s.deltaBaseline.len() > deltaBaselineCap {
		oldest, _ := s.deltaBaseline.first()
		s.deltaBaseline.pop(oldest)
	}
}

// GetDiagnosticsSync opens the file, waits for fresh diagnostics and returns those not in the baseline
func (s *Service) GetDiagnosticsSync(filePath string, delta bool, timeout time.Duration, shift LineShift, operatorRoots map[string]bool) ([]map[string]any, error) {
	if !s.EnabledFor(filePath, operatorRoots) {
		return []map[string]any{}, nil
	}
	serverID := s.serverFor(filePath).ServerID
	budget := s.waitBudget(filePath)
	t := timeout
	if t <= 0 {
		t = budget + 2*time.Second
	}
	type result struct {
		diags []map[string]any
		ok    bool
	}
	r, err := runOnLoop(s.l, t, func(ctx context.Context) (result, error) {
		d, ok, err := s.openAndWaitAsync(ctx, filePath, false, budget)
		return result{d, ok}, err
	})
	if err != nil {
		var te *TimeoutError
		if errors.As(err, &te) {
			logTimeout(s.rt, serverID, filePath, "diagnostics")
			s.rt.logf(slog.LevelDebug, managerLogger, "LSP diagnostics timeout for %s: %s", filePath, errText(err))
		} else {
			logServerError(s.rt, serverID, filePath, err)
			s.rt.logf(slog.LevelDebug, managerLogger, "LSP diagnostics fetch failed for %s: %s", filePath, errText(err))
		}
		s.markBrokenForFile(filePath, err)
		return []map[string]any{}, nil
	}
	if !r.ok {
		logTimeout(s.rt, serverID, filePath, "fresh diagnostics")
		return []map[string]any{}, nil
	}
	diags := r.diags
	if delta {
		if diags, err = s.applyDelta(filePath, diags, shift); err != nil {
			return nil, err
		}
	}
	if len(diags) > 0 {
		logDiagnostics(s.rt, serverID, filePath, len(diags))
	} else {
		logClean(s.rt, serverID, filePath)
	}
	return diags, nil
}

// Drops diagnostics present in the shifted baseline, then rolls the baseline forward
func (s *Service) applyDelta(filePath string, diags []map[string]any, shift LineShift) ([]map[string]any, error) {
	absPath, err := pypath.Abspath(filePath)
	if err != nil {
		return nil, err
	}
	s.stateMu.Lock()
	baseline, _ := s.deltaBaseline.get(absPath)
	s.stateMu.Unlock()
	if len(baseline) > 0 {
		if shift != nil {
			shifted, err := ShiftBaseline(baseline, shift)
			if err != nil {
				return nil, err
			}
			baseline = shifted
		}
		seen := map[string]bool{}
		for _, d := range baseline {
			k, err := DiagnosticKey(d)
			if err != nil {
				return nil, err
			}
			seen[k] = true
		}
		kept := []map[string]any{}
		for _, d := range diags {
			k, err := DiagnosticKey(d)
			if err != nil {
				return nil, err
			}
			if !seen[k] {
				kept = append(kept, d)
			}
		}
		diags = kept
	}
	fresh, err := runOnLoop(s.l, 2*time.Second, s.currentDiagsAsync(filePath))
	if err == nil && len(fresh) > 0 {
		s.setDeltaBaseline(absPath, fresh)
	}
	return diags, nil
}

// Marks the file's pair broken after a failure, drops its client and logs the failure once
func (s *Service) detachBroken(filePath string, cause error) *Client {
	srv := s.serverFor(filePath)
	if srv == nil {
		return nil
	}
	key, ok := s.brokenKey(srv, filePath)
	if !ok {
		return nil
	}
	alreadyBroken := s.isBroken(key)
	s.markBroken(key)
	s.stateMu.Lock()
	ckey := s.liveKey(srv, key.root)
	client, _ := s.clients.pop(ckey)
	delete(s.lastUsed, ckey)
	s.stateMu.Unlock()
	if !alreadyBroken {
		logSpawnFailed(s.rt, key.serverID, key.root, cause)
	}
	return client
}

// Tool side detach that shuts the dropped client down through the loop
func (s *Service) markBrokenForFile(filePath string, cause error) {
	if client := s.detachBroken(filePath, cause); client != nil {
		runOnLoop(s.l, time.Second, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, client.shutdown(ctx)
		})
	}
}

// Loop side detach awaiting the dropped client's shutdown within a second
func (s *Service) markBrokenForFileAsync(ctx context.Context, filePath string, cause error) error {
	client := s.detachBroken(filePath, cause)
	if client == nil {
		return nil
	}
	_, err := waitFor(ctx, time.Second, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, client.shutdown(ctx)
	})
	if err != nil {
		if isCancellation(err) {
			return err
		}
		s.rt.logf(slog.LevelDebug, managerLogger, "shutdown of broken LSP client failed")
	}
	return nil
}

// Shutdown tears down every client and stops the loop
func (s *Service) Shutdown() {
	if !s.enabled {
		return
	}
	if _, err := runOnLoop(s.l, 10*time.Second, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, s.shutdownAsync(ctx)
	}); err != nil {
		s.rt.logf(slog.LevelDebug, managerLogger, "LSP shutdown error: %s", errText(err))
	}
	s.l.stop()
	ClearWorkspaceCache()
}

// ClientStatus is one client row of the service status
type ClientStatus struct {
	ServerID         string
	WorkspaceRoot    string
	WorkspaceFolders []string
	State            string
	Running          bool
}

// Status is the service snapshot icebath lsp status renders
type Status struct {
	Enabled             bool
	WaitMode            string
	WaitTimeout         float64
	InstallStrategy     string
	Clients             []ClientStatus
	Broken              [][2]string
	DisabledServers     []string
	BrokenRetrySeconds  float64
	WarmupTimeout       float64
	ExcludeRoots        []string
	ExcludeRootsInvalid bool
	TrustedWorkspaces   []string
	UntrustedSkipped    [][2]string
}

// GetStatus snapshots the service for the status command
func (s *Service) GetStatus() Status {
	st := Status{Enabled: s.enabled, WaitMode: s.waitMode, WaitTimeout: s.waitTimeout, InstallStrategy: s.installStrategy,
		BrokenRetrySeconds: s.brokenRetry, WarmupTimeout: s.warmupTimeout, ExcludeRootsInvalid: s.excludeInvalid,
		Clients: []ClientStatus{}, Broken: [][2]string{}, UntrustedSkipped: [][2]string{}}
	s.l.with(func() {
		s.stateMu.Lock()
		defer s.stateMu.Unlock()
		for _, k := range s.clients.keys() {
			c, _ := s.clients.get(k)
			st.Clients = append(st.Clients, ClientStatus{ServerID: c.ServerID, WorkspaceRoot: c.WorkspaceRoot,
				WorkspaceFolders: append([]string{}, c.workspaceFolders...), State: c.state, Running: c.isRunning()})
		}
		for _, k := range s.broken.keys() {
			if e, _ := s.broken.get(k); e.forever || time.Now().Before(e.deadline) {
				st.Broken = append(st.Broken, [2]string{k.serverID, k.root})
			}
		}
		for k := range s.untrustedSkipped {
			if !s.trusted(k.root) {
				st.UntrustedSkipped = append(st.UntrustedSkipped, [2]string{k.serverID, k.root})
			}
		}
	})
	sort.Slice(st.UntrustedSkipped, func(i, j int) bool {
		a, b := st.UntrustedSkipped[i], st.UntrustedSkipped[j]
		return a[0] < b[0] || (a[0] == b[0] && a[1] < b[1])
	})
	for d := range s.disabledServers {
		st.DisabledServers = append(st.DisabledServers, d)
	}
	sort.Strings(st.DisabledServers)
	if st.DisabledServers == nil {
		st.DisabledServers = []string{}
	}
	st.ExcludeRoots = append([]string{}, s.excludeRoots...)
	st.TrustedWorkspaces = append([]string{}, s.trustedWorkspaces...)
	return st
}

// Pre write baseline, empty when no fresh data arrived
func (s *Service) snapshotAsync(ctx context.Context, filePath string, budget time.Duration) ([]map[string]any, error) {
	diags, ok, err := s.openAndWaitAsync(ctx, filePath, true, budget)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []map[string]any{}, nil
	}
	return diags, nil
}

// Opens the file and waits for fresh diagnostics, false meaning no verdict in budget
func (s *Service) openAndWaitAsync(ctx context.Context, filePath string, snapshot bool, budget time.Duration) ([]map[string]any, bool, error) {
	client, err := s.getOrSpawn(ctx, filePath)
	if err != nil || client == nil {
		return nil, false, err
	}
	fresh, err := func() (bool, error) {
		srv := s.serverFor(filePath)
		version, err := client.openFile(ctx, filePath, LanguageIDFor(filePath, srv), nil)
		if err != nil {
			return false, err
		}
		if !snapshot {
			if err := client.saveFile(ctx, filePath); err != nil {
				return false, err
			}
		}
		if budget <= 0 {
			budget = seconds(s.waitTimeout)
		}
		return clientWaitForDiagnostics(client, ctx, filePath, version, s.waitMode, budget)
	}()
	if err != nil {
		if isCancellation(err) {
			return nil, false, err
		}
		if snapshot {
			s.rt.logf(slog.LevelDebug, managerLogger, "snapshot open/wait failed: %s", errText(err))
		} else {
			s.rt.logf(slog.LevelDebug, managerLogger, "open/wait failed for %s: %s", filePath, errText(err))
		}
		return nil, false, nil
	}
	s.touch(client)
	if !fresh {
		return nil, false, nil
	}
	diags, err := client.diagnosticsFor(filePath, true)
	if err != nil {
		return nil, false, err
	}
	return diags, true, nil
}

// Fresh diagnostics the live client of a file holds, without syncing anything
func (s *Service) currentDiagsAsync(filePath string) func(ctx context.Context) ([]map[string]any, error) {
	return func(ctx context.Context) ([]map[string]any, error) {
		ws, gated := ResolveWorkspaceForFile(filePath, "")
		srv := s.serverFor(filePath)
		if ws == "" || !gated || srv == nil {
			return []map[string]any{}, nil
		}
		root := srv.ResolveRoot(filePath, ws)
		if root == "" {
			return []map[string]any{}, nil
		}
		s.stateMu.Lock()
		client, _ := s.clients.get(s.liveKey(srv, root))
		s.stateMu.Unlock()
		if client == nil {
			return []map[string]any{}, nil
		}
		return client.diagnosticsFor(filePath, true)
	}
}

// Running client for a file's root, spawning it once while concurrent callers share the spawn
func (s *Service) getOrSpawn(ctx context.Context, filePath string) (*Client, error) {
	srv := s.serverFor(filePath)
	if srv == nil {
		return nil, nil
	}
	if s.disabledServers[srv.ServerID] {
		logDisabled(s.rt, srv.ServerID, filePath, "disabled in config")
		return nil, nil
	}
	wsRoot, gated := ResolveWorkspaceForFile(filePath, "")
	if wsRoot == "" || !gated {
		logNoProjectRoot(s.rt, srv.ServerID, filePath)
		return nil, nil
	}
	root := srv.ResolveRoot(filePath, wsRoot)
	if root == "" {
		logDisabled(s.rt, srv.ServerID, filePath, "exclude marker hit (server gated off)")
		return nil, nil
	}
	if s.untrustedDenied(srv, root, filePath, s.trusted(root)) {
		return nil, nil
	}
	if s.isBroken(pairKey{srv.ServerID, root}) {
		return nil, nil
	}
	s.stateMu.Lock()
	key := s.liveKey(srv, root)
	trusted := key.root == "" || s.trusted(root)
	client, _ := s.clients.get(key)
	var spawning *future[*Client]
	owner := false
	if client != nil && client.isRunning() {
		s.lastUsed[key] = time.Now()
	} else {
		client = nil
		spawning = s.spawning[key]
		owner = spawning == nil
		if owner {
			spawning = newFuture[*Client]()
			s.spawning[key] = spawning
		}
	}
	s.stateMu.Unlock()
	if client != nil {
		logActive(s.rt, srv.ServerID, root)
		return s.attachRoot(ctx, srv, client, root)
	}
	if !owner {
		if err := s.l.await(ctx, spawning.done); err != nil {
			return nil, err
		}
		if spawning.cancelled || spawning.err != nil || spawning.value == nil {
			return nil, nil
		}
		return s.attachRoot(ctx, srv, spawning.value, root)
	}
	defer func() {
		s.stateMu.Lock()
		delete(s.spawning, key)
		s.stateMu.Unlock()
		spawning.setCancelled()
	}()
	client, err := s.spawnClient(ctx, srv, root, trusted)
	if err != nil {
		return nil, err
	}
	if client == nil {
		s.markBroken(pairKey{srv.ServerID, root})
	} else {
		s.stateMu.Lock()
		s.clients.set(key, client)
		s.lastUsed[key] = time.Now()
		s.stateMu.Unlock()
		logActive(s.rt, srv.ServerID, root)
	}
	spawning.setResult(client)
	return client, nil
}

// Announces a root to a shared multi root process instead of spawning another
func (s *Service) attachRoot(ctx context.Context, srv *ServerDef, client *Client, root string) (*Client, error) {
	if srv.MultiRoot {
		if err := client.addWorkspaceFolder(ctx, root); err != nil {
			return nil, err
		}
	}
	return client, nil
}

// Resolves the binary and starts a client, nil after logging when either fails
func (s *Service) spawnClient(ctx context.Context, srv *ServerDef, root string, trusted bool) (*Client, error) {
	sctx := ServerContext{WorkspaceRoot: root, InstallStrategy: s.installStrategy, BinaryOverrides: s.binaryOverrides,
		EnvOverrides: s.envOverrides, InitOverrides: s.initOverrides, Trusted: trusted, Runtime: s.rt}
	var spec *SpawnSpec
	var err error
	s.l.unlocked(func() { spec, err = srv.BuildSpawn(root, sctx) })
	if err != nil {
		return nil, err
	}
	if spec == nil {
		logServerUnavailable(s.rt, srv.ServerID, srv.ServerID)
		return nil, nil
	}
	client := newClient(s.l, s.rt, ClientOptions{ServerID: srv.ServerID, WorkspaceRoot: spec.WorkspaceRoot, Command: spec.Command,
		Env: spec.Env, Cwd: spec.Cwd, InitializationOptions: spec.InitializationOptions,
		SeedDiagnosticsOnFirstPush: spec.SeedDiagnosticsOnFirstPush || srv.SeedFirstPush})
	if err := clientStart(client, ctx); err != nil {
		if isCancellation(err) {
			return nil, err
		}
		logSpawnFailed(s.rt, srv.ServerID, root, err)
		return nil, nil
	}
	return client, nil
}

// Starts a freshly built client, swapped by tests that record what a spawn would hand the server
var clientStart = (*Client).start

// Refreshes last use of a client still in the live map
func (s *Service) touch(client *Client) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	for _, k := range s.clients.keys() {
		if c, _ := s.clients.get(k); c == client {
			s.lastUsed[k] = time.Now()
		}
	}
}

// Sweeps idle clients every interval, a failing sweep never ending the reaper
func (s *Service) idleReaperLoop(ctx context.Context) {
	interval := seconds(min(60, s.idleTimeout))
	for {
		if err := s.l.sleep(ctx, interval); err != nil {
			return
		}
		if err := s.reapOnce(ctx); err != nil {
			if isCancellation(err) {
				return
			}
			s.rt.logf(slog.LevelDebug, managerLogger, "LSP idle reaper sweep error: %s", errText(err))
		}
	}
}

// Shuts down clients idle past the timeout and those whose every root was deleted
func (s *Service) reapIdleOnce(ctx context.Context) error {
	cutoff := time.Now().Add(-seconds(s.idleTimeout))
	s.stateMu.Lock()
	var clients []*Client
	for _, k := range s.clients.keys() {
		if last, ok := s.lastUsed[k]; !ok || last.Before(cutoff) {
			c, _ := s.clients.pop(k)
			clients = append(clients, c)
			delete(s.lastUsed, k)
		}
	}
	s.stateMu.Unlock()
	if len(clients) > 0 {
		keys := make([]clientKey, len(clients))
		for i, c := range clients {
			keys[i] = clientKey{c.ServerID, c.WorkspaceRoot}
		}
		logReaped(s.rt, keys, s.idleTimeout)
		if err := s.shutdownAll(ctx, clients); err != nil {
			return err
		}
	}
	_, err := s.detachRoots(ctx, func(folder string) bool {
		st, err := os.Stat(folder)
		return err != nil || !st.IsDir()
	}, "workspace root deleted")
	return err
}

// Shuts clients down concurrently, their own errors ignored
func (s *Service) shutdownAll(ctx context.Context, clients []*Client) error {
	tasks := make([]*task, len(clients))
	for i, c := range clients {
		c := c
		tasks[i] = s.l.spawn(ctx, func(ctx context.Context) { c.shutdown(ctx) })
	}
	for _, t := range tasks {
		if err := s.l.await(ctx, t.done); err != nil {
			for _, t := range tasks {
				t.cancel()
			}
			return err
		}
	}
	return nil
}

// ReleaseWorkspace shuts down the clients serving a removed worktree or any root beneath it
func (s *Service) ReleaseWorkspace(workspaceRoot string) (int, error) {
	if !s.enabled {
		return 0, nil
	}
	root, err := pypath.Abspath(workspaceRoot)
	if err != nil {
		return 0, err
	}
	n, err := runOnLoop(s.l, 15*time.Second, func(ctx context.Context) (int, error) { return s.releaseAsync(ctx, root) })
	if err != nil {
		s.rt.logf(slog.LevelDebug, managerLogger, "LSP release of %s failed: %s", root, errText(err))
		return 0, nil
	}
	return n, nil
}

func (s *Service) releaseAsync(ctx context.Context, root string) (int, error) {
	under := func(path string) bool {
		return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
	}
	s.stateMu.Lock()
	var pending []<-chan struct{}
	for _, f := range s.spawning {
		if !f.isDone() {
			pending = append(pending, f.done)
		}
	}
	for _, k := range s.broken.keys() {
		if under(k.root) {
			s.broken.pop(k)
		}
	}
	for _, p := range s.deltaBaseline.keys() {
		if under(p) {
			s.deltaBaseline.pop(p)
		}
	}
	s.stateMu.Unlock()
	if len(pending) > 0 {
		deadline := time.Now().Add(10 * time.Second)
		for _, ch := range pending {
			if _, err := s.l.awaitAny(ctx, max(0, time.Until(deadline)), ch); err != nil {
				return 0, err
			}
		}
	}
	released, err := s.detachRoots(ctx, under, "workspace released")
	if err != nil {
		return 0, err
	}
	ClearWorkspaceCache()
	return released, nil
}

// Detaches clients whose every folder is gone and trims gone folders off multi root clients
func (s *Service) detachRoots(ctx context.Context, isGone func(string) bool, reason string) (int, error) {
	type trim struct {
		client  *Client
		folders []string
	}
	s.stateMu.Lock()
	var dead []*Client
	for _, k := range s.clients.keys() {
		c, _ := s.clients.get(k)
		all := true
		for _, f := range c.workspaceFolders {
			if !isGone(f) {
				all = false
				break
			}
		}
		if all {
			s.clients.pop(k)
			delete(s.lastUsed, k)
			dead = append(dead, c)
		}
	}
	var trims []trim
	for _, k := range s.clients.keys() {
		c, _ := s.clients.get(k)
		var gone []string
		for _, f := range c.workspaceFolders {
			if isGone(f) {
				gone = append(gone, f)
			}
		}
		trims = append(trims, trim{c, gone})
	}
	s.stateMu.Unlock()
	for _, t := range trims {
		for _, f := range t.folders {
			if err := t.client.removeWorkspaceFolder(ctx, f); err != nil {
				if isCancellation(err) {
					return 0, err
				}
				s.rt.logf(slog.LevelDebug, managerLogger, "LSP folder removal for %s failed: %s", f, errText(err))
			}
		}
	}
	if len(dead) > 0 {
		keys := make([]clientKey, len(dead))
		for i, c := range dead {
			keys[i] = clientKey{c.ServerID, c.WorkspaceRoot}
		}
		logReleased(s.rt, keys, reason)
		if err := s.shutdownAll(ctx, dead); err != nil {
			return 0, err
		}
	}
	return len(dead), nil
}

// Stops the reaper and shuts every client down
func (s *Service) shutdownAsync(ctx context.Context) error {
	if r := s.reaperTask; r != nil {
		s.reaperTask = nil
		r.cancel()
		if err := s.l.await(ctx, r.done); err != nil {
			return err
		}
	}
	s.stateMu.Lock()
	var clients []*Client
	for _, k := range s.clients.keys() {
		c, _ := s.clients.get(k)
		clients = append(clients, c)
	}
	s.clients.clear()
	s.broken.clear()
	s.lastUsed = map[pairKey]time.Time{}
	s.stateMu.Unlock()
	return s.shutdownAll(ctx, clients)
}
