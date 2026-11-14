package model

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type execState struct {
	mu              sync.Mutex
	streak          int
	noStream        bool
	noStreamOptions bool
	probeAt         time.Time
	notices         map[string]bool
}

func (e *execState) beginTurn() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notices = nil
}

func (e *execState) resetStreak() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.streak = 0
}

func (e *execState) bumpStreak() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.streak++
	return e.streak
}

func (e *execState) streakValue() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.streak
}

func (e *execState) setStreak(n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.streak = n
}

func (e *execState) streamOff() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.noStream
}

func (e *execState) disableStream() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.noStream = true
}

func (e *execState) streamOptionsOff() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.noStreamOptions
}

func (e *execState) disableStreamOptions() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.noStreamOptions = true
}

func (e *execState) claimProbe(now time.Time, window time.Duration) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.probeAt.IsZero() && now.Sub(e.probeAt) < window {
		return false
	}
	e.probeAt = now
	return true
}

func (e *execState) firstNotice(text string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.notices[text] {
		return false
	}
	if e.notices == nil {
		e.notices = map[string]bool{}
	}
	e.notices[text] = true
	return true
}

func (s *Session) StreamingDisabled() bool { return s.exec.streamOff() }

func (s *Session) DisableStreaming() { s.exec.disableStream() }

func (s *Session) StaleStreak() int { return s.exec.streakValue() }

type envNum struct {
	value   float64
	parsed  bool
	present bool
}

func (n envNum) or(def float64) float64 {
	if n.parsed {
		return n.value
	}
	return def
}

func (n envNum) orInt(def int) int {
	if n.parsed {
		return int(n.value)
	}
	return def
}

type knobs struct {
	streamStale      envNum
	localStreamStale envNum
	streamRead       envNum
	apiTimeout       envNum
	streamRetries    envNum
	staleGiveup      envNum
	codexHard        envNum
	codexTTFB        envNum
	codexTTFBAbove   envNum
	codexTTFBStrict  bool
	codexTTFBMax     envNum
	codexIdle        envNum
	callStale        envNum
	dumpRequests     bool
	dumpStdout       bool
	payloadMax       envNum
	localStaleCfg    float64
	lifetimeCfg      float64
	drainCfg         float64
	providerRequest  float64
	requestSet       bool
	providerStale    float64
	staleSet         bool
}

func (s *Session) envRaw(ctx context.Context, name string) (string, bool, error) {
	v, ok, err := s.r.c.o.Secrets.Lookup(ctx, name)
	if err != nil {
		return "", false, err
	}
	return v, ok, nil
}

func (s *Session) envFloat(ctx context.Context, name string) (envNum, error) {
	raw, ok, err := s.envRaw(ctx, name)
	if err != nil || !ok {
		return envNum{}, err
	}
	n := envNum{present: true}
	if text := strings.TrimSpace(raw); text != "" {
		if v, perr := strconv.ParseFloat(text, 64); perr == nil {
			n.value, n.parsed = v, true
		}
	}
	return n, nil
}

func (s *Session) envInt(ctx context.Context, name string) (envNum, error) {
	raw, ok, err := s.envRaw(ctx, name)
	if err != nil || !ok {
		return envNum{}, err
	}
	n := envNum{present: true}
	if text := strings.TrimSpace(raw); text != "" {
		if v, perr := strconv.Atoi(text); perr == nil {
			n.value, n.parsed = float64(v), true
		}
	}
	return n, nil
}

func (s *Session) envEnabled(ctx context.Context, name string) (bool, error) {
	raw, _, err := s.envRaw(ctx, name)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	}
	return false, nil
}

func (s *Session) loadKnobs(ctx context.Context, rt *Route) (*knobs, error) {
	k := &knobs{}
	floats := []struct {
		name string
		dst  *envNum
	}{
		{"ICEBATH_STREAM_STALE_TIMEOUT", &k.streamStale},
		{"ICEBATH_LOCAL_STREAM_STALE_TIMEOUT", &k.localStreamStale},
		{"ICEBATH_STREAM_READ_TIMEOUT", &k.streamRead},
		{"ICEBATH_API_TIMEOUT", &k.apiTimeout},
		{"ICEBATH_CODEX_HARD_TIMEOUT_SECONDS", &k.codexHard},
		{"ICEBATH_CODEX_TTFB_TIMEOUT_SECONDS", &k.codexTTFB},
		{"ICEBATH_CODEX_TTFB_DISABLE_ABOVE_TOKENS", &k.codexTTFBAbove},
		{"ICEBATH_CODEX_TTFB_MAX_SECONDS", &k.codexTTFBMax},
		{"ICEBATH_CODEX_EVENT_STALE_TIMEOUT_SECONDS", &k.codexIdle},
	}
	for _, f := range floats {
		n, err := s.envFloat(ctx, f.name)
		if err != nil {
			return nil, err
		}
		*f.dst = n
	}
	ints := []struct {
		name string
		dst  *envNum
	}{
		{"ICEBATH_STREAM_RETRIES", &k.streamRetries},
		{"ICEBATH_STREAM_STALE_GIVEUP", &k.staleGiveup},
		{"ICEBATH_PLUGIN_PAYLOAD_MAX_CHARS", &k.payloadMax},
	}
	for _, f := range ints {
		n, err := s.envInt(ctx, f.name)
		if err != nil {
			return nil, err
		}
		*f.dst = n
	}
	raw, ok, err := s.envRaw(ctx, "ICEBATH_API_CALL_STALE_TIMEOUT")
	if err != nil {
		return nil, err
	}
	if ok {
		v, perr := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if perr != nil {
			return nil, fmt.Errorf("model: could not convert ICEBATH_API_CALL_STALE_TIMEOUT to a number of seconds: %q", raw)
		}
		k.callStale = envNum{value: v, parsed: true, present: true}
	}
	if k.codexTTFBStrict, err = s.envEnabled(ctx, "ICEBATH_CODEX_TTFB_STRICT"); err != nil {
		return nil, err
	}
	if k.dumpRequests, err = s.envEnabled(ctx, "ICEBATH_DUMP_REQUESTS"); err != nil {
		return nil, err
	}
	if k.dumpStdout, err = s.envEnabled(ctx, "ICEBATH_DUMP_REQUEST_STDOUT"); err != nil {
		return nil, err
	}
	cfg, err := s.r.c.o.Config.LoadReadonly(ctx)
	if err != nil {
		return nil, err
	}
	k.localStaleCfg, k.lifetimeCfg, k.drainCfg = cfg.Agent.LocalStreamStaleTimeout, cfg.Agent.StreamMaxLifetime, math.Max(0, cfg.Agent.StreamDrainTimeout)
	k.providerRequest, k.requestSet = s.r.c.o.Config.ProviderRequestTimeout(ctx, rt.Provider, rt.Model)
	k.providerStale, k.staleSet = s.r.c.o.Config.ProviderStaleTimeout(ctx, rt.Provider, rt.Model)
	return k, nil
}

func (k *knobs) giveup() int { return k.staleGiveup.orInt(5) }

func (k *knobs) localStaleDefault() float64 {
	return k.localStreamStale.or(k.localStaleCfg)
}

func (k *knobs) streamEnvStaleBase() (float64, bool) {
	return k.streamStale.or(180), k.streamStale.present
}

func (k *knobs) configuredStaleBase() float64 {
	if k.staleSet {
		return k.providerStale
	}
	base, _ := k.streamEnvStaleBase()
	return base
}

func (s *Session) streamsLlamaCpp(rt *Route) bool {
	if rt.Mode != wire.ModeChat && rt.Mode != "" {
		return false
	}
	return s.r.c.o.Local.IsManagedProvider(rt.Provider, rt.BaseURL)
}

func (s *Session) localServerAnswers(ctx context.Context, baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return false
	}
	host := u.Host
	if u.Hostname() == "localhost" {
		host = strings.Replace(host, "localhost", "127.0.0.1", 1)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.Scheme+"://"+host+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := s.r.c.o.HTTP.Client().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *Session) RetryRecoveredLocalServer(ctx context.Context) (bool, error) {
	ctx = s.r.bind(ctx)
	rt := s.Route()
	k, err := s.loadKnobs(ctx, rt)
	if err != nil {
		return false, err
	}
	giveup, streak := k.giveup(), s.exec.streakValue()
	if giveup <= 0 || streak < giveup || !s.streamsLlamaCpp(rt) || !s.localServerAnswers(ctx, rt.BaseURL) {
		return false, nil
	}
	slog.Info(fmt.Sprintf("Local model server answers its health check after %d stale attempts; trying once more.", streak))
	s.exec.setStreak(giveup - 1)
	return true, nil
}

func (s *Session) checkStaleGiveup(k *knobs) error {
	giveup, streak := k.giveup(), s.exec.streakValue()
	if giveup > 0 && streak >= giveup {
		return &StaleCallError{Attempts: streak}
	}
	return nil
}
