package local

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	TouchPrompt        = "Reply with exactly one word: the capital of France."
	touchExpect        = "paris"
	defaultServerPort  = 18434
	IdleUnloadAfter    = 15 * time.Minute
	healthTimeout      = 120 * time.Second
	healthProbeTimeout = 3 * time.Second
	healthPoll         = time.Second
	helpTimeout        = 15 * time.Second
	requestTimeout     = 30 * time.Second
	loadTimeout        = 600 * time.Second
	reloadTimeout      = 10 * time.Second
	unloadTimeout      = 120 * time.Second
	unloadSettle       = 15 * time.Second
	unloadPoll         = 300 * time.Millisecond
	touchTimeout       = 300 * time.Second
	metricsTimeout     = 10 * time.Second
	minAPIKeyLength    = 16
	statusLoaded       = "loaded"
	statusReady        = "ready"
	statusUnloading    = "unloading"
)

var restartBackoff = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second, 60 * time.Second}

var credentialEnvMarkers = []string{"_API_KEY", "_TOKEN", "_SECRET", "PASSWORD", "_CREDENTIALS"}

var ErrStartupCancelled = errors.New("llama-server startup cancelled")

func resident(status string) bool { return status == statusLoaded || status == statusReady }

func serverChildEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(key)
		drop := false
		for _, marker := range credentialEnvMarkers {
			if strings.Contains(upper, marker) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func stablePort() (int, error) {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", defaultServerPort))
	if err == nil {
		l.Close()
		return defaultServerPort, nil
	}
	slog.Warn(fmt.Sprintf("port %d busy; managed llama-server falling back to an ephemeral port — resumed sessions follow the live endpoint", defaultServerPort))
	return freePort()
}

func (r *Runtime) stableAPIKey(ctx context.Context) (string, error) {
	keyPath := home.LlamaCppAPIKey.In(r.root)
	if existing, err := fileutil.ReadText(keyPath); err == nil {
		if trimmed := strings.TrimSpace(existing); len(trimmed) >= minAPIKeyLength {
			return trimmed, nil
		}
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o777); err == nil {
		err = fileutil.WriteFile(ctx, keyPath, []byte(key))
		if err != nil {
			slog.Warn(fmt.Sprintf("could not persist api key (%v); sessions will need a re-pick after restart", err))
		}
	} else {
		slog.Warn(fmt.Sprintf("could not persist api key (%v); sessions will need a re-pick after restart", err))
	}
	return key, nil
}

func (r *Runtime) directIOArgs(ctx context.Context, binary string) ([]string, error) {
	r.hwMu.Lock()
	cached, ok := r.directIO[binary]
	r.hwMu.Unlock()
	if ok {
		return cached, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, helpTimeout)
	defer cancel()
	cmd := exec.Command(binary, "--help")
	cmd.Dir = filepath.Dir(binary)
	proc.Hide(cmd)
	res, err := proc.Run(runCtx, cmd)
	if err != nil {
		return nil, err
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	help := string(res.Stdout) + string(res.Stderr)
	args := []string{}
	switch {
	case strings.Contains(help, "--load-mode"):
		args = []string{"--load-mode", "dio"}
	case strings.Contains(help, "--direct-io"):
		args = []string{"-dio"}
	}
	r.hwMu.Lock()
	r.directIO[binary] = args
	r.hwMu.Unlock()
	return args, nil
}

type idleState int

const (
	idleUnknown idleState = iota
	idleBusy
	idleConfirmed
)

type Supervisor struct {
	rt         *Runtime
	binary     string
	modelsDir  string
	modelsMax  int
	port       int
	apiKey     string
	logPath    string
	presetPath string
	guard      *proc.Guard

	lifecycle sync.Mutex
	mu        sync.Mutex
	child     *serverChild
	stopping  bool
	stopCh    chan struct{}
	stopOnce  *sync.Once
	restarts  int
	logFile   *os.File
	idleSince map[string]time.Time
	refitUsed *int64
	watchDone chan struct{}
}

func (r *Runtime) newSupervisor(ctx context.Context, binary, modelsDir string, modelsMax, port int, presetPath string) (*Supervisor, error) {
	if port == 0 {
		p, err := stablePort()
		if err != nil {
			return nil, err
		}
		port = p
	}
	key, err := r.stableAPIKey(ctx)
	if err != nil {
		return nil, err
	}
	return &Supervisor{
		rt: r, binary: binary, modelsDir: modelsDir, modelsMax: modelsMax, port: port, apiKey: key,
		logPath: home.LlamaServerLog.In(r.root), presetPath: presetPath, guard: r.guard,
		idleSince: map[string]time.Time{},
	}, nil
}

func (s *Supervisor) BaseURL() string { return fmt.Sprintf("http://127.0.0.1:%d/v1", s.port) }

func (s *Supervisor) String() string { return "llama-server router at " + s.BaseURL() }

func (s *Supervisor) GoString() string {
	return fmt.Sprintf("local.Supervisor{BaseURL: %q}", s.BaseURL())
}

func (s *Supervisor) LogValue() slog.Value { return slog.StringValue(s.BaseURL()) }

func (s *Supervisor) ModelsMax() int { return s.modelsMax }

func (s *Supervisor) url(route string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", s.port, route)
}

func (s *Supervisor) exchange(ctx context.Context, route string, body any, timeout time.Duration, jsonType bool) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	method := http.MethodGet
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		method, reader = http.MethodPost, bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, s.url(route), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	if jsonType {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.rt.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := httpclient.CheckStatus(resp); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

func (s *Supervisor) request(ctx context.Context, route string, body any, timeout time.Duration) (any, error) {
	raw, err := s.exchange(ctx, route, body, timeout, true)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Supervisor) running() *serverChild {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child == nil {
		return nil
	}
	select {
	case <-s.child.done:
		return nil
	default:
		return s.child
	}
}

func (s *Supervisor) spawn(ctx context.Context) error {
	ioArgs, err := s.rt.directIOArgs(ctx, s.binary)
	if err != nil {
		return err
	}
	args := []string{
		"--host", "127.0.0.1", "--port", strconv.Itoa(s.port), "--api-key", s.apiKey,
		"--models-max", strconv.Itoa(s.modelsMax), "--models-autoload", "--metrics", "--slots", "--no-ui", "--jinja",
	}
	args = append(args, ioArgs...)
	if _, statErr := os.Stat(s.presetPath); s.presetPath != "" && statErr == nil {
		args = append(args, "--models-preset", s.presetPath)
	} else {
		args = append(args, "--models-dir", s.modelsDir)
	}
	if err := os.MkdirAll(filepath.Dir(s.logPath), 0o777); err != nil {
		return err
	}
	s.mu.Lock()
	if s.logFile != nil {
		s.logFile.Close()
		s.logFile = nil
	}
	s.mu.Unlock()
	logFile, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	shown := append([]string{s.binary}, args...)
	for i := range shown {
		if i > 0 && shown[i-1] == "--api-key" {
			shown[i] = "***"
		}
	}
	if _, err := fmt.Fprintf(logFile, "\n# spawn: %q\n", shown); err != nil {
		logFile.Close()
		return err
	}
	cmd := exec.Command(s.binary, args...)
	cmd.Dir = filepath.Dir(s.binary)
	cmd.Env = serverChildEnv(os.Environ())
	cmd.Stdout, cmd.Stderr = logFile, logFile
	child, err := startServer(ctx, s.guard, s.rt.ledger, cmd)
	if err != nil {
		logFile.Close()
		return err
	}
	s.mu.Lock()
	s.child, s.logFile = child, logFile
	s.mu.Unlock()
	slog.Info(fmt.Sprintf("llama-server router spawned pid=%d port=%d", child.id.PID, s.port))
	return s.writeState(ctx, child.id.PID)
}

type stateDocument struct {
	BaseURL         string  `json:"base_url"`
	APIKey          string  `json:"api_key"`
	PID             int     `json:"pid"`
	CreateTime      float64 `json:"create_time"`
	StartTime       *int64  `json:"start_time"`
	Executable      string  `json:"executable"`
	OwnerPID        int     `json:"owner_pid"`
	OwnerCreateTime float64 `json:"owner_create_time"`
	OwnerStartTime  *int64  `json:"owner_start_time"`
}

func fingerprint(pid int) *int64 {
	f, err := proc.Fingerprint(pid)
	if err != nil {
		return nil
	}
	return &f
}

func (s *Supervisor) writeState(ctx context.Context, pid int) error {
	created, err := proc.Started(pid)
	if err != nil {
		return err
	}
	exe, err := proc.Exe(pid)
	if err != nil {
		return err
	}
	own := os.Getpid()
	ownCreated, err := proc.Started(own)
	if err != nil {
		return err
	}
	doc := stateDocument{
		BaseURL: s.BaseURL(), APIKey: s.apiKey, PID: pid, CreateTime: created, StartTime: fingerprint(pid),
		Executable: exe, OwnerPID: own, OwnerCreateTime: ownCreated, OwnerStartTime: fingerprint(own),
	}
	path := s.rt.StatePath()
	if err := s.rt.homes.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	return fileutil.WriteJSON(ctx, path, doc, fileutil.JSONStyle{Indent: 2}, fileutil.Private())
}

func (s *Supervisor) start(ctx context.Context) error {
	s.lifecycle.Lock()
	s.mu.Lock()
	s.stopping = false
	s.stopCh, s.stopOnce = make(chan struct{}), &sync.Once{}
	s.mu.Unlock()
	err := s.spawn(ctx)
	s.lifecycle.Unlock()
	if err != nil {
		return err
	}
	if err := s.waitHealth(ctx, healthTimeout); err != nil {
		return err
	}
	s.mu.Lock()
	s.watchDone = make(chan struct{})
	done, stopCh := s.watchDone, s.stopCh
	s.mu.Unlock()
	go s.watch(stopCh, done)
	return nil
}

func exitCode(c *serverChild) int {
	if c.cmd.ProcessState == nil {
		return -1
	}
	ex := proc.ExitOf(c.cmd.ProcessState)
	if ex.Signal != 0 {
		return -int(ex.Signal)
	}
	return ex.Code
}

func (s *Supervisor) waitHealth(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	s.mu.Lock()
	stopCh := s.stopCh
	s.mu.Unlock()
	for time.Now().Before(deadline) {
		select {
		case <-stopCh:
			return ErrStartupCancelled
		default:
		}
		s.mu.Lock()
		child := s.child
		s.mu.Unlock()
		if child != nil {
			select {
			case <-child.done:
				return fmt.Errorf("llama-server exited rc=%d during startup (log: %s)", exitCode(child), s.logPath)
			default:
			}
		}
		if s.healthy(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopCh:
			return ErrStartupCancelled
		case <-time.After(healthPoll):
		}
	}
	return fmt.Errorf("llama-server not healthy after %ds (log: %s)", int(timeout.Seconds()), s.logPath)
}

func (s *Supervisor) healthy(ctx context.Context) bool {
	reqCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, s.url("/health"), nil)
	if err != nil {
		return false
	}
	resp, err := s.rt.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *Supervisor) watch(stopCh, done chan struct{}) {
	defer close(done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		s.mu.Lock()
		child, stopping := s.child, s.stopping
		s.mu.Unlock()
		if stopping || child == nil {
			return
		}
		select {
		case <-stopCh:
			return
		case <-child.done:
		}
		s.mu.Lock()
		stopping = s.stopping
		attempt := s.restarts
		s.mu.Unlock()
		if stopping {
			return
		}
		child.releaseOnceEmpty(ctx)
		backoff := restartBackoff[min(attempt, len(restartBackoff)-1)]
		slog.Warn(fmt.Sprintf("llama-server exited rc=%d; restart #%d in %ds", exitCode(child), attempt+1, int(backoff.Seconds())))
		select {
		case <-stopCh:
			return
		case <-time.After(backoff):
		}
		s.mu.Lock()
		s.restarts++
		s.mu.Unlock()
		s.lifecycle.Lock()
		s.mu.Lock()
		stopping = s.stopping
		s.mu.Unlock()
		if stopping {
			s.lifecycle.Unlock()
			return
		}
		child.stopSurvivors(ctx)
		s.reapOrphanedChildren(child)
		err := s.spawn(ctx)
		s.lifecycle.Unlock()
		if err == nil {
			err = s.waitHealth(ctx, healthTimeout)
		}
		if err != nil {
			slog.Error(fmt.Sprintf("llama-server restart failed: %v", err))
		}
	}
}

func (s *Supervisor) Stop(ctx context.Context) error {
	err := s.stopTree(ctx)
	s.mu.Lock()
	watchDone := s.watchDone
	s.mu.Unlock()
	if watchDone != nil {
		select {
		case <-watchDone:
		case <-ctx.Done():
			err = errors.Join(err, ctx.Err())
		}
	}
	return err
}

func (s *Supervisor) stopTree(ctx context.Context) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	s.stopping = true
	if s.stopOnce != nil {
		s.stopOnce.Do(func() { close(s.stopCh) })
	}
	child := s.child
	s.mu.Unlock()
	var errs []error
	if child != nil {
		select {
		case <-child.done:
		default:
			if err := terminateTree(ctx, child.id, false); err != nil {
				errs = append(errs, err)
			}
			waitCtx, cancel := context.WithTimeout(ctx, stopSettle)
			select {
			case <-child.done:
			case <-waitCtx.Done():
				errs = append(errs, fmt.Errorf("llama-server pid=%d did not exit: %w", child.id.PID, waitCtx.Err()))
			}
			cancel()
		}
		if err := child.closeContainment(); err != nil {
			errs = append(errs, err)
		}
		child.stopSurvivors(ctx)
	}
	s.mu.Lock()
	if s.logFile != nil {
		s.logFile.Close()
		s.logFile = nil
	}
	s.mu.Unlock()
	return errors.Join(errs...)
}

func (s *Supervisor) reapOrphanedChildren(crashed *serverChild) {
	if crashed.contained() {
		if err := crashed.closeContainment(); err != nil {
			slog.Warn(fmt.Sprintf("could not close the crashed llama-server job: %v", err))
		}
		return
	}
	if runtime.GOOS == "windows" {
		return
	}
	table, err := proc.ReadTable()
	if err != nil {
		return
	}
	s.mu.Lock()
	own := 0
	if s.child != nil {
		own = s.child.id.PID
	}
	s.mu.Unlock()
	for _, e := range table {
		exe, err := proc.Exe(e.PID)
		if err != nil || exe != s.binary || e.PID == own {
			continue
		}
		if e.PPID != 0 && proc.Exists(e.PPID) {
			continue
		}
		slog.Warn(fmt.Sprintf("reaping orphaned llama-server child pid=%d", e.PID))
		_ = e.Identity().Signal(syscall.SIGKILL)
	}
}

func (s *Supervisor) Models(ctx context.Context, timeout time.Duration) (map[string]string, error) {
	if timeout <= 0 {
		timeout = requestTimeout
	}
	raw, err := s.exchange(ctx, "/models", nil, timeout, true)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Data []struct {
			ID     *string         `json:"id"`
			Status json.RawMessage `json:"status"`
		} `json:"data"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
	}
	out := map[string]string{}
	for _, m := range doc.Data {
		if m.ID == nil {
			return nil, errors.New("router listed a model without an id")
		}
		status := "unknown"
		if len(m.Status) > 0 && string(m.Status) != "null" {
			var st map[string]any
			if err := json.Unmarshal(m.Status, &st); err != nil {
				return nil, err
			}
			if v, ok := st["value"]; ok {
				status = jsonText(v)
			}
		}
		out[*m.ID] = status
	}
	return out, nil
}

func (s *Supervisor) LoadModel(ctx context.Context, modelID string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = loadTimeout
	}
	_, err := s.request(ctx, "/models/load", map[string]string{"model": modelID}, timeout)
	return err
}

func (s *Supervisor) ReloadPresets(ctx context.Context) error {
	_, err := s.request(ctx, "/models?reload=1", nil, reloadTimeout)
	return err
}

func (s *Supervisor) UnloadModel(ctx context.Context, modelID string) error {
	if _, err := s.request(ctx, "/models/unload", map[string]string{"model": modelID}, unloadTimeout); err != nil {
		return err
	}
	deadline := time.Now().Add(unloadSettle)
	for time.Now().Before(deadline) {
		statuses, err := s.Models(ctx, requestTimeout)
		if err != nil {
			return nil
		}
		if status, ok := statuses[modelID]; !ok || (!resident(status) && status != statusUnloading) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(unloadPoll):
		}
	}
	return nil
}

func (s *Supervisor) SweepIdle(ctx context.Context, now time.Time) []string {
	unloaded := []string{}
	statuses, err := s.Models(ctx, requestTimeout)
	if err != nil {
		return unloaded
	}
	for modelID, status := range statuses {
		if !resident(status) {
			s.mu.Lock()
			delete(s.idleSince, modelID)
			s.mu.Unlock()
			continue
		}
		switch s.probeIdle(ctx, modelID) {
		case idleUnknown:
			s.mu.Lock()
			since, ok := s.idleSince[modelID]
			s.mu.Unlock()
			if !ok {
				since = now
			}
			slog.Info(fmt.Sprintf("idle probe for %s failed; keeping idle clock (idle %ds)", modelID, int(now.Sub(since).Seconds())))
			continue
		case idleBusy:
			s.mu.Lock()
			delete(s.idleSince, modelID)
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		first, ok := s.idleSince[modelID]
		if !ok {
			first = now
			s.idleSince[modelID] = now
		}
		s.mu.Unlock()
		if now.Sub(first) < IdleUnloadAfter {
			continue
		}
		if err := s.UnloadModel(ctx, modelID); err != nil {
			slog.Warn(fmt.Sprintf("idle unload of %s failed: %v", modelID, err))
			continue
		}
		s.mu.Lock()
		delete(s.idleSince, modelID)
		s.mu.Unlock()
		unloaded = append(unloaded, modelID)
		slog.Info(fmt.Sprintf("idle-unloaded %s (idle %ds)", modelID, int(now.Sub(first).Seconds())))
	}
	return unloaded
}

func (s *Supervisor) TouchGenerate(ctx context.Context, modelID string, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = touchTimeout
	}
	resp, err := s.request(ctx, "/v1/chat/completions", map[string]any{
		"model": modelID, "messages": []map[string]string{{"role": "user", "content": TouchPrompt}},
		"max_tokens": 512, "temperature": 0,
	}, timeout)
	if err == nil {
		var msg map[string]any
		if root, ok := resp.(map[string]any); ok {
			if choices, ok := root["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					msg, _ = choice["message"].(map[string]any)
				}
			}
		}
		if msg != nil {
			blob := jsonText(msg["content"]) + " " + jsonText(msg["reasoning_content"])
			return strings.Contains(strings.ToLower(blob), touchExpect)
		}
		err = errors.New("response carries no message")
	}
	slog.Warn(fmt.Sprintf("touch generation failed for %s: %v", modelID, err))
	return false
}

func (s *Supervisor) EnsureModelReady(ctx context.Context, modelID string, timeout time.Duration) (bool, error) {
	statuses, err := s.Models(ctx, requestTimeout)
	if err != nil {
		return false, err
	}
	status, ok := statuses[modelID]
	if !ok {
		return false, fmt.Errorf("model %s not present in models dir", modelID)
	}
	if !resident(status) {
		if err := s.LoadModel(ctx, modelID, timeout); err != nil {
			return false, err
		}
	}
	return s.TouchGenerate(ctx, modelID, touchTimeout), nil
}

func (s *Supervisor) IsIdle(ctx context.Context, modelID string) bool {
	return s.probeIdle(ctx, modelID) == idleConfirmed
}

func (s *Supervisor) probeIdle(ctx context.Context, modelID string) idleState {
	var loaded []string
	if modelID != "" {
		loaded = []string{modelID}
	} else {
		statuses, err := s.Models(ctx, requestTimeout)
		if err != nil {
			return idleUnknown
		}
		for id, status := range statuses {
			if resident(status) {
				loaded = append(loaded, id)
			}
		}
	}
	for _, mid := range loaded {
		slots, err := s.request(ctx, "/slots?"+childQuery(mid), nil, requestTimeout)
		if err != nil {
			var status *httpclient.StatusError
			if errors.As(err, &status) && status.Code == http.StatusBadRequest {
				continue
			}
			return idleUnknown
		}
		list, ok := slots.([]any)
		if !ok {
			return idleUnknown
		}
		for _, slot := range list {
			m, ok := slot.(map[string]any)
			if !ok {
				return idleUnknown
			}
			if jsonTruthy(m["is_processing"]) {
				return idleBusy
			}
		}
		text, err := s.exchange(ctx, "/metrics?"+childQuery(mid), nil, metricsTimeout, false)
		if err != nil {
			return idleUnknown
		}
		scanner := bufio.NewScanner(bytes.NewReader(text))
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "llamacpp:requests_processing") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				return idleUnknown
			}
			value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				return idleUnknown
			}
			if value != 0 {
				return idleBusy
			}
		}
	}
	return idleConfirmed
}
