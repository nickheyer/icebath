package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	defaultTurnLivenessTimeoutS = 600.0
	defaultTurnLivenessPollS    = 15.0
	minTurnLivenessPollS        = 0.01
	turnLivenessTimeoutKey      = "agent.turn_liveness.timeout_s"
	turnLivenessPollKey         = "agent.turn_liveness.poll_s"
	turnStallDumpMaxThreads     = 40
)

func turnLivenessLog() *slog.Logger { return slog.Default().With("logger", "agent.turn_liveness") }

// Builds the awake idle meter of each new watchdog, swapped by tests that drive both clocks
var newTurnLivenessIdleMeter = state.NewAwakeIdleMeter

// One activity clock observation, activityTS zero when the clock was never stamped
type activitySnapshot struct {
	generation  int64
	activityTS  float64
	idleSeconds float64
}

func warnInvalidTurnLivenessValue(key string, raw any, def float64) {
	turnLivenessLog().Warn(fmt.Sprintf("Invalid %s in config.yaml: %s — falling back to default %.1f.", key, pyrepr.Repr(raw), def))
}

// Coerces one duration knob, warning and defaulting on typos, NaN and Inf, erroring only on overflow
func resolveFiniteSeconds(raw any, def float64, key string) (float64, error) {
	value, err := pyint.Float(raw)
	if err != nil {
		if pe, ok := err.(*pyint.Error); ok && pe.Kind == "OverflowError" {
			return 0, err
		}
		value = math.NaN()
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		warnInvalidTurnLivenessValue(key, raw, def)
		return def, nil
	}
	return value, nil
}

// Dict view of a config node, false for anything that is not a mapping
func livenessConfigDict(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case config.Tree:
		return m, true
	}
	return nil, false
}

// Timeout and poll seconds from agent.turn_liveness, nil timeout when non positive opts out
func resolveTurnLivenessSettings(cfg map[string]any) (*float64, float64, error) {
	var rawSection any
	if agentCfg, ok := livenessConfigDict(cfg["agent"]); ok {
		rawSection = agentCfg["turn_liveness"]
	}
	section, isDict := livenessConfigDict(rawSection)
	if rawSection != nil && !isDict {
		warnInvalidTurnLivenessValue("agent.turn_liveness", rawSection, defaultTurnLivenessTimeoutS)
	}
	rawTimeout, ok := section["timeout_s"]
	if !ok {
		rawTimeout = defaultTurnLivenessTimeoutS
	}
	timeoutS, err := resolveFiniteSeconds(rawTimeout, defaultTurnLivenessTimeoutS, turnLivenessTimeoutKey)
	if err != nil {
		return nil, 0, err
	}
	rawPoll, ok := section["poll_s"]
	if !ok {
		rawPoll = defaultTurnLivenessPollS
	}
	pollS, err := resolveFiniteSeconds(rawPoll, defaultTurnLivenessPollS, turnLivenessPollKey)
	if err != nil {
		return nil, 0, err
	}
	if pollS <= 0 {
		warnInvalidTurnLivenessValue(turnLivenessPollKey, pollS, defaultTurnLivenessPollS)
		pollS = defaultTurnLivenessPollS
	}
	if timeoutS <= 0 {
		return nil, pollS, nil
	}
	return &timeoutS, pollS, nil
}

// Callbacks and bounds binding one watchdog to the lease that owns the turn
type turnLivenessWatchdogConfig struct {
	sessionID      string
	timeoutS       float64
	pollS          float64
	stopEvent      *pythread.Event
	activityLock   sync.Locker
	isTurnActive   func() bool
	commitAbort    func(snapshot activitySnapshot, message string) bool
	deactivateTurn func()
}

// Sampled idle watchdog that force aborts one silently stalled turn
type turnLivenessWatchdog struct {
	agent                  *AIAgent
	cfg                    turnLivenessWatchdogConfig
	awakeIdle              *state.AwakeIdleMeter
	sample                 func() *activitySnapshot
	lastSurfacedGeneration *int64
}

// Watchdog over the agent's activity clock with the poll clamped to its floor
func newTurnLivenessWatchdog(a *AIAgent, cfg turnLivenessWatchdogConfig) *turnLivenessWatchdog {
	cfg.pollS = math.Max(minTurnLivenessPollS, cfg.pollS)
	w := &turnLivenessWatchdog{agent: a, cfg: cfg, awakeIdle: newTurnLivenessIdleMeter()}
	w.sample = w.sampleActivity
	return w
}

// Starts polling on the shared periodic scheduler and returns its cancel handle
func (w *turnLivenessWatchdog) schedule(ctx context.Context) *cron.ScheduledHandle {
	return cron.SchedulePeriodic(ctx, w.tick, time.Duration(w.cfg.pollS*float64(time.Second)))
}

// One poll that aborts a turn idle past the bound, false once the watchdog is finished
func (w *turnLivenessWatchdog) tick(ctx context.Context) bool {
	if w.cfg.stopEvent.IsSet() {
		return false
	}
	snapshot := w.sample()
	if snapshot == nil {
		return false
	}
	if w.awakeIdle.Measure(snapshot.idleSeconds) < w.cfg.timeoutS {
		return true
	}
	w.surfaceStall(*snapshot)
	message := fmt.Sprintf("Turn made no progress for %ds; aborting to release the session.", int64(snapshot.idleSeconds))
	if !w.cfg.commitAbort(*snapshot, message) {
		return true
	}
	w.cfg.deactivateTurn()
	w.surfaceCommittedAbort(*snapshot)
	observability.RecordWatchdogTurnAbort(ctx, &w.agent.turnExit)
	return false
}

// Generation and timestamp of the activity clock read under its lock, nil once the turn is inactive
func (w *turnLivenessWatchdog) sampleActivity() *activitySnapshot {
	w.cfg.activityLock.Lock()
	if !w.cfg.isTurnActive() {
		w.cfg.activityLock.Unlock()
		return nil
	}
	generation, activityTS := w.agent.turnLivenessActivityGeneration, w.agent.lastActivityTS
	w.cfg.activityLock.Unlock()
	idle := 0.0
	if activityTS != 0 {
		idle = math.Max(0.0, activityWallNow()-activityTS)
	}
	return &activitySnapshot{generation: generation, activityTS: activityTS, idleSeconds: idle}
}

var goroutineHeaderRE = regexp.MustCompile(`^goroutine (\d+) \[[^\]]*\](?: \{(.*)\})?:$`)

// Goroutine id and its thread or name label from a traceback header, question mark when unlabeled
func parseGoroutineHeader(header string) (uint64, string) {
	m := goroutineHeaderRE.FindStringSubmatch(header)
	if m == nil {
		return 0, "?"
	}
	id, _ := strconv.ParseUint(m[1], 10, 64)
	labels := map[string]string{}
	for _, kv := range strings.Split(m[2], ", ") {
		if k, v, ok := strings.Cut(kv, ": "); ok {
			labels[strings.Trim(k, `"`)] = strings.Trim(v, `"`)
		}
	}
	for _, key := range []string{"thread", "name"} {
		if name := labels[key]; name != "" {
			return id, name
		}
	}
	return id, "?"
}

// Every goroutine's traceback, growing the buffer until the whole dump fits
func allGoroutineStacks() string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// Moves the turn goroutine into the last dumped slot when the thread cap would otherwise omit it
func keepTurnBlockInDump(blocks []string, turnID uint64) []string {
	last := turnStallDumpMaxThreads - 1
	if turnID == 0 || len(blocks) <= turnStallDumpMaxThreads {
		return blocks
	}
	for i := last + 1; i < len(blocks); i++ {
		header, _, _ := strings.Cut(blocks[i], "\n")
		if id, _ := parseGoroutineHeader(header); id == turnID {
			out := make([]string, 0, len(blocks))
			out = append(out, blocks[:last]...)
			out = append(out, blocks[i])
			out = append(out, blocks[last:i]...)
			return append(out, blocks[i+1:]...)
		}
	}
	return blocks
}

// Logs every goroutine stack with the turn goroutine marked, failures logged at debug and never raised
func (w *turnLivenessWatchdog) dumpTurnThreadStack() {
	defer func() {
		if r := recover(); r != nil {
			turnLivenessLog().Debug("watchdog stack dump failed", "error", r)
		}
	}()
	turnID := w.agent.executionGoroutineID.Load()
	blocks := keepTurnBlockInDump(strings.Split(strings.TrimRight(allGoroutineStacks(), "\n"), "\n\n"), turnID)
	lines := []string{"## All thread stacks at turn stall (watchdog dump)"}
	dumped := 0
	for _, block := range blocks {
		if dumped >= turnStallDumpMaxThreads {
			lines = append(lines, fmt.Sprintf("  <%d more threads omitted>", len(blocks)-dumped))
			break
		}
		dumped++
		header, body, _ := strings.Cut(block, "\n")
		id, name := parseGoroutineHeader(header)
		marker := ""
		if id != 0 && id == turnID {
			marker = " <- turn thread"
		}
		lines = append(lines, fmt.Sprintf("### thread %s tid=%d%s", name, id, marker))
		for _, sub := range strings.Split(strings.TrimRight(body, " \t\n"), "\n") {
			lines = append(lines, "  "+sub)
		}
	}
	turnLivenessLog().Error(strings.Join(lines, "\n"))
}

// Shows a warning through the agent, a failing emitter logged at debug
func (w *turnLivenessWatchdog) emitWarning(text, debugMsg string) {
	defer func() {
		if r := recover(); r != nil {
			turnLivenessLog().Debug(debugMsg, "error", r)
		}
	}()
	w.agent.emitWarning(text)
}

// Live session id of the agent, the admission id when the agent has none
func (w *turnLivenessWatchdog) currentSessionID() string {
	if sid := w.agent.sessionID; sid != "" {
		return sid
	}
	return w.cfg.sessionID
}

// Logs and warns that recovery is beginning, once per activity generation
func (w *turnLivenessWatchdog) surfaceStall(snapshot activitySnapshot) {
	if w.lastSurfacedGeneration != nil && *w.lastSurfacedGeneration == snapshot.generation {
		return
	}
	generation := snapshot.generation
	w.lastSurfacedGeneration = &generation
	w.dumpTurnThreadStack()
	w.cfg.activityLock.Lock()
	desc := w.agent.lastActivityDesc
	w.cfg.activityLock.Unlock()
	turnLivenessLog().Error(fmt.Sprintf("Turn liveness watchdog fired for session %s: "+
		"no progress for %.1fs (last activity: %s). "+
		"Attempting recovery: force-interrupting the turn and "+
		"stopping lease renewal if it cannot resume (#95548).",
		w.currentSessionID(), snapshot.idleSeconds, pyrepr.Repr(desc)))
	w.emitWarning("⚠️ This turn stopped making progress "+
		fmt.Sprintf("(%ds without activity); ", int64(snapshot.idleSeconds))+
		"attempting recovery so the session can continue.",
		"Failed to emit turn liveness warning")
}

// Logs and warns the definitive abort once the interrupt committed and renewal stopped
func (w *turnLivenessWatchdog) surfaceCommittedAbort(snapshot activitySnapshot) {
	turnLivenessLog().Error(fmt.Sprintf("Turn liveness watchdog aborted turn for session %s: "+
		"no progress for %.1fs; turn interrupted and lease renewal "+
		"stopped (#95548).", w.currentSessionID(), snapshot.idleSeconds))
	w.emitWarning("⚠️ Turn aborted by the liveness watchdog "+
		fmt.Sprintf("(%ds without activity); ", int64(snapshot.idleSeconds))+
		"lease renewal stopped so the session can be reclaimed. "+
		"You can retry your message.",
		"Failed to emit committed-abort warning")
}
