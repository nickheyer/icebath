package observability

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

var (
	reachedOutcomes = Set("completed", "interrupted", "failed", "steered", "steering")
	waitThresholds  = []threshold{{5, "lt_5s"}, {30, "5s_to_30s"}, {120, "30s_to_2m"}, {600, "2m_to_10m"}}
	// Monotonic clock in seconds of the busy arrival seams, replaced by tests
	busyMonotonic = func() float64 { return float64(time.Since(busyEpoch)) / float64(time.Second) }
	busyEpoch     = time.Now()
)

// RuntimeHomeOfSourceHook resolves the runtime profile home of a message source's routing identity, set by pkg/gateway
var RuntimeHomeOfSourceHook func(source any) (string, bool)

// Wait bucket of a busy arrival, never for one that never ran
func WaitBucket(seconds *float64) string {
	if seconds == nil {
		return "never"
	}
	return bucket(max(0, *seconds), waitThresholds, "gte_10m")
}

// Bounded dimensions of one busy arrival row
func BusyArrivalFields(ctx context.Context, platformValue any, disposition string, seconds *float64) map[string]string {
	if !BusyArrivalDispositions.Has(disposition) {
		disposition = "other"
	}
	return map[string]string{"disposition": disposition, "platform": platform(ctx, platformValue), "wait_bucket": WaitBucket(seconds)}
}

// Arrival is one human message that reached its chat while the chat's turn was running
type Arrival struct {
	arrived     float64
	platform    any
	home        string
	disposition string
	admitting   bool
	admissionID string
	merged      []*Arrival
	done        bool
	parked      bool
}

// BusyEvent is the gateway message event the busy seams read and stamp, implemented by pkg/gateway
type BusyEvent interface {
	BusyArrival() *Arrival
	SetBusyArrival(*Arrival)
	Internal() bool
	Command() string
	Source() any
	ChatID() any
}

// BusySteerOutcome is the busy resolution of a follow up message
type BusySteerOutcome struct {
	Steered, Redirected bool
	EffectiveMode       string
}

const queuedMax = 4096

var (
	busyMu      sync.Mutex
	queued      = map[string]*Arrival{}
	queuedOrder []string
)

// Open arrival stamped on an event, nil when absent or already resolved
func busyTag(event BusyEvent) *Arrival {
	if event == nil {
		return nil
	}
	if tag := event.BusyArrival(); tag != nil && !tag.done {
		return tag
	}
	return nil
}

// Stamps a message that reached its busy chat once, with the routing identity's runtime home
func StampBusyArrival(adapter any, event BusyEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Busy arrival not stamped", "error", r)
		}
	}()
	if event.BusyArrival() != nil || event.Internal() || event.Command() != "" {
		return
	}
	if RuntimeHomeOfSourceHook == nil {
		panic("cannot import name 'identity_of'")
	}
	homeDir, _ := RuntimeHomeOfSourceHook(event.Source())
	busyMu.Lock()
	defer busyMu.Unlock()
	event.SetBusyArrival(&Arrival{arrived: busyMonotonic(), platform: sourcePlatform(adapter, event.ChatID()),
		home: homeDir, disposition: "queued"})
}

// Folds an event into the held burst head so its fate is the head's
func NoteBusyMerged(head, event BusyEvent) {
	busyMu.Lock()
	defer busyMu.Unlock()
	tag, headTag := busyTag(event), busyTag(head)
	if tag != nil && headTag != nil {
		tag.done = true
		headTag.merged = append(headTag.merged, tag)
	}
}

// Applies the busy resolution, a steer reaching the agent now and a hard interrupt running next
func NoteBusyOutcome(ctx context.Context, event BusyEvent, outcome BusySteerOutcome) {
	busyMu.Lock()
	defer busyMu.Unlock()
	tag := busyTag(event)
	if tag == nil {
		return
	}
	tag.home = home.Dir(ctx)
	if outcome.Steered || outcome.Redirected {
		resolveArrival(ctx, tag, "steered", ptrFloat(busyMonotonic()))
	} else if outcome.EffectiveMode == "interrupt" {
		tag.disposition = "interrupted"
	}
}

// Marks the message as reaching the routed profile's FIFO admission in that profile's scope
func NoteBusyAdmitting(ctx context.Context, event BusyEvent) {
	busyMu.Lock()
	defer busyMu.Unlock()
	if tag := busyTag(event); tag != nil {
		tag.admitting = true
		tag.home = home.Dir(ctx)
	}
}

func ptrFloat(v float64) *float64 { return &v }

// Settles a committed row by its status, nil outcome meaning the row was deleted with its chat
func NoteBusyAdmitted(ctx context.Context, event BusyEvent, admissionID, status string, outcome any) {
	busyMu.Lock()
	defer busyMu.Unlock()
	tag := busyTag(event)
	if tag == nil {
		return
	}
	outcomeText, isText := outcome.(string)
	switch {
	case status == "queued":
		tag.admissionID = admissionID
		if _, seen := queued[admissionID]; !seen {
			queuedOrder = append(queuedOrder, admissionID)
		}
		queued[admissionID] = tag
		for len(queued) > queuedMax {
			oldest := queuedOrder[0]
			queuedOrder = queuedOrder[1:]
			if t, ok := queued[oldest]; ok {
				t.done = true
				delete(queued, oldest)
			}
		}
	case status == "started" || status == "unknown" || (status == "terminal" && isText && reachedOutcomes.Has(outcomeText)):
		resolveArrival(ctx, tag, tag.disposition, ptrFloat(busyMonotonic()))
	case status == "terminal":
		resolveArrival(ctx, tag, "dropped", nil)
	}
}

// Removes and returns the queued arrival of an admission id
func popQueued(admissionID string) *Arrival {
	tag, ok := queued[admissionID]
	if !ok {
		return nil
	}
	delete(queued, admissionID)
	for i, id := range queuedOrder {
		if id == admissionID {
			queuedOrder = append(queuedOrder[:i:i], queuedOrder[i+1:]...)
			break
		}
	}
	return tag
}

// Settles a queued row that left the FIFO without its own turn, steered reaching the agent
func BusyRowSettled(ctx context.Context, admissionID any, outcome string) {
	busyMu.Lock()
	defer busyMu.Unlock()
	tag := popQueued(pyText(admissionID))
	if tag == nil || tag.done {
		return
	}
	if outcome == "steered" {
		resolveArrival(ctx, tag, "steered", ptrFloat(busyMonotonic()))
	} else {
		resolveArrival(ctx, tag, "dropped", nil)
	}
}

// Keeps an arrival owed while its row waits behind a paused FIFO
func NoteBusyParked(event BusyEvent) {
	busyMu.Lock()
	defer busyMu.Unlock()
	if tag := busyTag(event); tag != nil {
		tag.parked = true
	}
}

// Resolves a queued arrival whose row the FIFO claimed and is running
func BusyTurnStarted(ctx context.Context, admissionID any) {
	busyMu.Lock()
	defer busyMu.Unlock()
	if tag := popQueued(pyText(admissionID)); tag != nil && !tag.done {
		resolveArrival(ctx, tag, tag.disposition, ptrFloat(busyMonotonic()))
	}
}

// Settles an arrival when its busy handler returns, admitted and never run counting as dropped
func SettleBusyArrival(ctx context.Context, event BusyEvent, discarded bool) {
	busyMu.Lock()
	defer busyMu.Unlock()
	tag := busyTag(event)
	if tag == nil || (tag.parked && !discarded) {
		return
	}
	if tag.admissionID != "" {
		popQueued(tag.admissionID)
	}
	switch {
	case !discarded && cancelling(ctx):
		tag.done = true
	case discarded || tag.admitting:
		resolveArrival(ctx, tag, "dropped", nil)
	default:
		tag.done = true
	}
}

// Reports whether the handler's context is being cancelled, a restart rather than a drop
func cancelling(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil
}

// Emits the arrival and every arrival merged into it in the home that runs the chat
func resolveArrival(ctx context.Context, tag *Arrival, disposition string, reached *float64) {
	rows := []struct {
		arrival *Arrival
		kind    string
	}{{tag, disposition}}
	for _, m := range tag.merged {
		rows = append(rows, struct {
			arrival *Arrival
			kind    string
		}{m, "merged"})
	}
	for _, r := range rows {
		r.arrival.done = true
		var seconds *float64
		if reached != nil {
			seconds = ptrFloat(*reached - r.arrival.arrived)
		}
		platformValue, kind, homeDir := r.arrival.platform, r.kind, tag.home
		submit(context.WithoutCancel(ctx), func(ctx context.Context) {
			emitBusyArrival(inHome(ctx, homeDir), platformValue, kind, seconds)
		})
	}
}

// Records one busy arrival row
func emitBusyArrival(ctx context.Context, platformValue any, disposition string, seconds *float64) {
	emit(ctx, BusyArrivalMetric, func() map[string]string {
		return BusyArrivalFields(ctx, platformValue, disposition, seconds)
	})
}
