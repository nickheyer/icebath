package process

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tool/redact"
)

const (
	EventCompletion       = "completion"
	EventWatchMatch       = "watch_match"
	EventWatchDisabled    = "watch_disabled"
	EventOverflowTripped  = "watch_overflow_tripped"
	EventOverflowReleased = "watch_overflow_released"
	EventHeartbeat        = "heartbeat"
)

const (
	watchMinIntervalSeconds   = 15
	watchStrikeLimit          = 3
	watchLifetimeMaxHits      = 8
	watchGlobalMaxPerWindow   = 15
	watchGlobalWindowSeconds  = 10
	watchGlobalCooldownSecond = 30
	watchOutputLines          = 20
	watchOutputChars          = 2000
	heartbeatOutputChars      = 2000
)

type Event struct {
	Type              string
	EventID           string
	SessionID         string
	SessionKey        string
	TaskID            string
	OwnerTaskID       string
	ParentSessionID   string
	Command           string
	StartedAt         float64
	Route             Route
	Pattern           string
	Output            string
	OutputCut         int
	Omitted           int
	Suppressed        int
	Message           string
	Seq               int
	Interval          int
	Elapsed           int
	HandoffNote       string
	ExitCode          *int
	CompletionReason  string
	TerminationSource string
	OriginUISessionID string
	Restored          bool
	Delegation        *DelegationEvent
}

type eventQueue struct {
	mu       sync.Mutex
	items    []Event
	ready    chan struct{}
	inFlight map[string]int
}

func (q *eventQueue) put(e Event) {
	q.mu.Lock()
	q.items = append(q.items, e)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

type watchBreaker struct {
	mu                   sync.Mutex
	windowStart          float64
	windowHits           int
	trippedUntil         float64
	suppressedDuringTrip int
}

func newEventID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

func (r *Registry) eventBaseLocked(s *Session) Event {
	return Event{
		EventID:         newEventID(),
		StartedAt:       s.startedAt,
		ParentSessionID: s.parentSessionID,
		SessionID:       s.id,
		SessionKey:      s.sessionKey,
		TaskID:          s.taskID,
		OwnerTaskID:     s.ownerTaskID,
		Command:         s.command,
		Route:           s.route,
	}
}

func (r *Registry) redactEvent(ctx context.Context, ev *Event) {
	command := ev.Command
	if ev.Output != "" {
		output := ev.Output
		if replaced, ok := r.transform.TransformTerminalOutput(ctx, command, output, ev.ExitCode, ev.TaskID, ""); ok {
			output = replaced
		}
		ev.Output = r.redactor.Terminal(ctx, output, command, false)
	}
	if command != "" {
		ev.Command = r.redactor.Text(ctx, command, redact.Mode{CodeFile: true})
	}
}

func trimTrailingSpace(s string) string { return strings.TrimRightFunc(s, outputSpace) }

func (r *Registry) checkWatch(s *Session, text string) {
	s.mu.Lock()
	patterns, disabled, exited := s.watchPatterns, s.watchDisabled, s.exited
	s.mu.Unlock()
	if len(patterns) == 0 || disabled || exited {
		return
	}
	var pattern string
	var lines []string
	for _, line := range outputLines(text) {
		for _, p := range patterns {
			if strings.Contains(line, p) {
				if len(lines) == 0 {
					pattern = p
				}
				lines = append(lines, trimTrailingSpace(line))
				break
			}
		}
	}
	if len(lines) == 0 {
		return
	}
	t := now()
	s.mu.Lock()
	if s.watchCooldownUntil != 0 && t < s.watchCooldownUntil {
		s.watchSuppressed += len(lines)
		if s.watchStrikePending {
			s.mu.Unlock()
			return
		}
		s.watchStrikePending = true
		s.watchStrikes++
		if s.watchStrikes < watchStrikeLimit {
			s.mu.Unlock()
			return
		}
		s.watchDisabled = true
		s.notify = true
		ev := r.watchDisabledLocked(s, s.watchSuppressed, strconv.Itoa(watchStrikeLimit)+
			" consecutive rate-limit windows triggered (min spacing "+strconv.Itoa(watchMinIntervalSeconds)+"s). ")
		s.mu.Unlock()
		r.redactEvent(s.ctx, &ev)
		r.events.put(ev)
		return
	}
	if s.watchCooldownUntil != 0 && !s.watchStrikePending {
		s.watchStrikes = 0
	}
	s.watchStrikePending = false
	s.watchCooldownUntil = t + watchMinIntervalSeconds
	s.watchHits++
	suppressed := s.watchSuppressed
	s.watchSuppressed = 0
	exhausted := s.watchHits >= watchLifetimeMaxHits
	if exhausted {
		s.watchDisabled = true
		s.notify = true
	}
	base := r.eventBaseLocked(s)
	s.mu.Unlock()
	output := strings.Join(lines[:min(len(lines), watchOutputLines)], "\n")
	if utf8.RuneCountInString(output) > watchOutputChars {
		output = runeHead(output, watchOutputChars) + "\n...(truncated)"
	}
	if r.admitWatch(t) {
		ev := base
		ev.Type = EventWatchMatch
		ev.Pattern = pattern
		ev.Output = output
		ev.Suppressed = suppressed
		r.redactEvent(s.ctx, &ev)
		r.events.put(ev)
	}
	if exhausted {
		s.mu.Lock()
		ev := r.watchDisabledLocked(s, 0, "reached the lifetime cap of "+strconv.Itoa(watchLifetimeMaxHits)+" delivered matches. ")
		s.mu.Unlock()
		r.redactEvent(s.ctx, &ev)
		r.events.put(ev)
	}
}

func (r *Registry) watchDisabledLocked(s *Session, suppressed int, why string) Event {
	ev := r.eventBaseLocked(s)
	ev.Type = EventWatchDisabled
	ev.Suppressed = suppressed
	ev.Message = "Watch patterns disabled for process " + s.id + " — " + why +
		"Falling back to notify_on_complete semantics; you'll get " +
		"exactly one notification when the process exits."
	return ev
}

func (r *Registry) admitWatch(t float64) bool {
	var events []Event
	b := &r.breaker
	b.mu.Lock()
	if b.trippedUntil != 0 && t >= b.trippedUntil {
		suppressed := b.suppressedDuringTrip
		b.trippedUntil = 0
		b.suppressedDuringTrip = 0
		b.windowStart, b.windowHits = t, 0
		if suppressed > 0 {
			events = append(events, Event{
				Type:       EventOverflowReleased,
				Suppressed: suppressed,
				Message: "Watch-pattern notifications resumed. " + strconv.Itoa(suppressed) +
					" match event(s) were suppressed during the flood.",
			})
		}
	}
	var admit bool
	if b.trippedUntil != 0 && t < b.trippedUntil {
		b.suppressedDuringTrip++
	} else {
		if t-b.windowStart >= watchGlobalWindowSeconds {
			b.windowStart, b.windowHits = t, 0
		}
		admit = b.windowHits < watchGlobalMaxPerWindow
		if admit {
			b.windowHits++
		} else {
			b.trippedUntil = t + watchGlobalCooldownSecond
			b.suppressedDuringTrip++
			events = append(events, Event{
				Type: EventOverflowTripped,
				Message: "Watch-pattern overflow: >" + strconv.Itoa(watchGlobalMaxPerWindow) +
					" notifications in " + strconv.Itoa(watchGlobalWindowSeconds) + "s across all processes. " +
					"Suppressing further watch_match events for " + strconv.Itoa(watchGlobalCooldownSecond) + "s.",
			})
		}
	}
	b.mu.Unlock()
	for _, ev := range events {
		r.events.put(ev)
	}
	return admit
}

func (r *Registry) ArmHeartbeat(s *Session, seconds int) int {
	seconds = max(seconds, heartbeatMinSeconds)
	s.mu.Lock()
	s.heartbeatSeconds = seconds
	s.heartbeatLast = now()
	s.mu.Unlock()
	r.mu.Lock()
	if !r.heartbeatRunning && !r.closed {
		r.heartbeatRunning = true
		r.wg.Add(1)
		go r.heartbeatLoop()
	}
	r.mu.Unlock()
	return seconds
}

func (r *Registry) heartbeatLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(heartbeatTick)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		}
		t := now()
		var due []*Session
		r.mu.Lock()
		for _, s := range r.running.list() {
			s.mu.Lock()
			if s.heartbeatSeconds > 0 && !s.exited && t-s.heartbeatLast >= float64(s.heartbeatSeconds) {
				due = append(due, s)
			}
			s.mu.Unlock()
		}
		r.mu.Unlock()
		for _, s := range due {
			r.emitHeartbeat(s, t)
		}
	}
}

func heartbeatTail(output string) (string, int) {
	if n := utf8.RuneCountInString(output); n > heartbeatOutputChars {
		return runeTail(output, heartbeatOutputChars), n - heartbeatOutputChars
	}
	return output, 0
}

func (r *Registry) emitHeartbeat(s *Session, t float64) {
	s.mu.Lock()
	s.heartbeatLast = t
	delta := s.out.total - s.heartbeatTotal
	output := ""
	if delta > 0 {
		output = runeTail(s.out.String(), delta)
	}
	s.heartbeatTotal = s.out.total
	if output == "" {
		s.mu.Unlock()
		return
	}
	output, omitted := heartbeatTail(output)
	s.heartbeatSeq++
	ev := r.eventBaseLocked(s)
	ev.Type = EventHeartbeat
	ev.Seq = s.heartbeatSeq
	ev.Interval = s.heartbeatSeconds
	if s.startedAt != 0 {
		ev.Elapsed = int(t - s.startedAt)
	}
	ev.Output = output
	ev.Omitted = omitted
	s.mu.Unlock()
	r.redactEvent(s.ctx, &ev)
	r.events.put(ev)
}
