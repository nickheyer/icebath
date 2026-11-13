package photon

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Health states of the merged inbound stream
const (
	streamStarting   = "starting"
	streamHealthy    = "healthy"
	streamRecovering = "recovering"
	streamDegraded   = "degraded"
)

// Number from an env var in milliseconds, the default when unset, empty or not a number
func envMillis(key string, def time.Duration, allowNonPositive bool) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || (!allowNonPositive && v <= 0) {
		return def
	}
	return time.Duration(v * float64(time.Millisecond))
}

func envCount(key string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// Recovering, degraded and silence bookkeeping of the inbound streams
type streamHealth struct {
	mu            sync.Mutex
	state         string
	degradedSince time.Time
	lastIssueAt   time.Time
	lastIssue     string
	issueCount    int
	degradeAfter  int

	lastFrameAt     time.Time
	lastProbeAt     time.Time
	lastProbeResult string
	zombieSuspected bool
}

func newStreamHealth() *streamHealth {
	now := time.Now()
	return &streamHealth{state: streamStarting, degradeAfter: envCount("PHOTON_STREAM_INTERRUPTED_DEGRADE_COUNT", 3),
		lastFrameAt: now}
}

func (h *streamHealth) markHealthy() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state, h.degradedSince, h.issueCount = streamHealthy, time.Time{}, 0
}

func (h *streamHealth) markDegradedLocked(reason string) {
	now := time.Now()
	if h.state != streamDegraded {
		h.degradedSince = now
	}
	h.state, h.lastIssueAt, h.lastIssue = streamDegraded, now, reason
	h.issueCount++
}

func (h *streamHealth) markDegraded(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.markDegradedLocked(reason)
}

// Counts an interruption, a burst of them since the last healthy event degrading the stream
func (h *streamHealth) markRecovering(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == streamDegraded {
		h.lastIssueAt, h.lastIssue = time.Now(), reason
		return
	}
	if h.state != streamRecovering {
		h.issueCount = 0
	}
	h.state, h.lastIssueAt, h.lastIssue = streamRecovering, time.Now(), reason
	h.issueCount++
	if h.issueCount >= h.degradeAfter {
		h.markDegradedLocked(reason)
	}
}

// Any frame, heartbeats included, proves the stream is not half open
func (h *streamHealth) noteFrame() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastFrameAt, h.zombieSuspected = time.Now(), false
}

// Silence and last probe outcome while a half open stream is suspected
func (h *streamHealth) zombieReport() (bool, time.Duration, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.zombieSuspected, time.Since(h.lastFrameAt), h.lastProbeResult
}

// State, degraded duration and last issue of the streams
func (h *streamHealth) degraded() (bool, time.Duration, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != streamDegraded {
		return false, 0, ""
	}
	return true, time.Since(h.degradedSince), h.lastIssue
}

// Outcome of an upstream liveness probe
type probeOutcome int

const (
	probeAlive probeOutcome = iota
	probeInconclusive
	probeHung
)

// Cheap authenticated read of a synthetic message id over the line, a not found answer proving the wire works
func probeLine(ctx context.Context, l *line, timeout time.Duration) (probeOutcome, string) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := l.getMessage(pctx, uuid.NewString())
	switch {
	case err == nil:
		return probeAlive, "round-trip completed"
	case pctx.Err() != nil && ctx.Err() == nil:
		return probeHung, "probe timed out"
	}
	if re := asRPCError(err); re != nil && (re.notFound() || re.ErrorCode == "messageNotFound") {
		return probeAlive, "not-found round-trip"
	}
	return probeInconclusive, err.Error()
}

// Half open stream detector, silence past the threshold plus a probe proven live channel degrading the stream
type zombieWatchdog struct {
	health    *streamHealth
	sess      func() *session
	threshold time.Duration
	cooldown  time.Duration
	timeout   time.Duration
	tick      time.Duration
}

func newZombieWatchdog(health *streamHealth, sess func() *session) *zombieWatchdog {
	return &zombieWatchdog{health: health, sess: sess,
		threshold: envMillis("PHOTON_STREAM_SILENCE_PROBE_MS", 10*time.Minute, true),
		cooldown:  envMillis("PHOTON_STREAM_PROBE_COOLDOWN_MS", 2*time.Minute, false),
		timeout:   envMillis("PHOTON_STREAM_PROBE_TIMEOUT_MS", 10*time.Second, false),
		tick:      30 * time.Second}
}

func (w *zombieWatchdog) run(ctx context.Context) {
	if w.threshold <= 0 {
		return
	}
	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.check(ctx)
		}
	}
}

func (w *zombieWatchdog) check(ctx context.Context) {
	h := w.health
	h.mu.Lock()
	silent := time.Since(h.lastFrameAt)
	sinceProbe := time.Since(h.lastProbeAt)
	h.mu.Unlock()
	if silent < w.threshold || sinceProbe < w.cooldown {
		return
	}
	s := w.sess()
	if s == nil {
		return
	}
	lines := s.currentLines()
	if len(lines) == 0 {
		return
	}
	outcome, reason := probeLine(ctx, lines[0], w.timeout)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastProbeAt = time.Now()
	h.lastProbeResult = map[probeOutcome]string{probeAlive: "alive", probeInconclusive: "inconclusive", probeHung: "inconclusive"}[outcome]
	silent = time.Since(h.lastFrameAt)
	if outcome != probeAlive || silent < w.threshold {
		return
	}
	h.zombieSuspected = true
	issue := "inbound stream silent for " + strconv.FormatInt(silent.Milliseconds(), 10) +
		"ms while an upstream probe succeeded (" + reason + ") — half-open (zombie) gRPC stream suspected"
	s.tele.log(logError, "spectrum.stream", issue, nil)
	h.markDegradedLocked(issue)
}
