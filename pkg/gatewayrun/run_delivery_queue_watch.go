package gatewayrun

import (
	"context"
	"os"
	"slices"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Seconds between looks at the queue file inside one housekeeping interval
const deliveryQueueWatchSeconds = 1.0

// Chore label of the drain the watch triggers
const deliveryQueueDrainLabel = "Cron durable delivery queue drain"

// Path, modification time in nanoseconds and size of one queue file
type queueStamp struct {
	path    string
	mtimeNS int64
	size    int64
}

// Stamp comparison of every served profile's delivery queue file, draining when a stamp moved
type DeliveryQueueWatch struct {
	homes  func() []*string
	Drain  func()
	served []*string
	seen   []queueStamp
}

// Watch over the served homes, a nil home standing for the active one, stamped once at creation
func NewDeliveryQueueWatch(ctx context.Context, homes func() []*string, drain func()) *DeliveryQueueWatch {
	w := &DeliveryQueueWatch{homes: homes, Drain: drain, served: slices.Clone(homes())}
	w.seen = w.stamp(ctx)
	return w
}

// Re-resolves the served profiles once per tick rather than on every poll
func (w *DeliveryQueueWatch) refreshHomes() {
	w.served = slices.Clone(w.homes())
}

// Stamps of every served queue file and its write ahead log that exist now
func (w *DeliveryQueueWatch) stamp(ctx context.Context) []queueStamp {
	var stamps []queueStamp
	for _, served := range w.served {
		root := home.Dir(ctx)
		if served != nil {
			root = *served
		}
		path := cron.QueuePath(root)
		for _, candidate := range []string{path, path + "-wal"} {
			info, err := os.Stat(candidate)
			if err != nil {
				continue
			}
			stamps = append(stamps, queueStamp{candidate, info.ModTime().UnixNano(), info.Size()})
		}
	}
	return stamps
}

// Whether any queue stamp moved since the last look, remembering the new stamps
func (w *DeliveryQueueWatch) Changed(ctx context.Context) bool {
	current := w.stamp(ctx)
	if slices.Equal(current, w.seen) {
		return false
	}
	w.seen = current
	return true
}

// Sleeps one housekeeping interval, draining the worker queue the moment it changes
func waitForNextTick(ctx context.Context, stopEvent *pythread.Event, interval float64, watch *DeliveryQueueWatch, chore func(label string, fn func())) {
	if watch == nil {
		stopEvent.Wait(interval)
		return
	}
	watch.refreshHomes()
	deadline := pythread.Monotonic() + interval
	for {
		remaining := max(0.0, deadline-pythread.Monotonic())
		if stopEvent.Wait(min(remaining, deliveryQueueWatchSeconds)) {
			return
		}
		if watch.Changed(ctx) {
			chore(deliveryQueueDrainLabel, watch.Drain)
		}
		if pythread.Monotonic() >= deadline {
			return
		}
	}
}
