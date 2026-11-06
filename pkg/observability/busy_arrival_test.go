package observability

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"
)

// busyMessage is a gateway message event carrying its busy arrival stamp
type busyMessage struct {
	text    string
	arrival *Arrival
}

func (m *busyMessage) BusyArrival() *Arrival     { return m.arrival }
func (m *busyMessage) SetBusyArrival(a *Arrival) { m.arrival = a }
func (m *busyMessage) Internal() bool            { return false }
func (m *busyMessage) Source() any               { return "telegram:42" }
func (m *busyMessage) ChatID() any               { return "42" }
func (m *busyMessage) Command() string {
	if len(m.text) > 1 && m.text[0] == '/' {
		return m.text[1:]
	}
	return ""
}

// Captured busy arrival rows read after draining the gateway submit queue
func busyRows(t *testing.T) (*markCapture, func() []Mark) {
	t.Helper()
	m := captureMarks(t)
	savedHook := RuntimeHomeOfSourceHook
	busyMu.Lock()
	queued, queuedOrder = map[string]*Arrival{}, nil
	busyMu.Unlock()
	t.Cleanup(func() { RuntimeHomeOfSourceHook = savedHook })
	RuntimeHomeOfSourceHook = func(any) (string, bool) { return m.home, true }
	return m, func() []Mark {
		if err := Drain(5 * time.Second); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		out := m.rows
		m.rows = nil
		return out
	}
}

func TestBusyDispositionsFollowTheBusyResolutionAndCollectionOffRecordsNothing(t *testing.T) {
	m, read := busyRows(t)
	ctx := context.Background()
	adapter := &fakeAdapter{platform: "telegram"}
	steer, interrupt, cleared := &busyMessage{text: "s"}, &busyMessage{text: "i"}, &busyMessage{text: "c"}
	for _, e := range []*busyMessage{steer, interrupt, cleared} {
		StampBusyArrival(adapter, e)
	}
	NoteBusyOutcome(ctx, steer, BusySteerOutcome{Redirected: true, EffectiveMode: "interrupt"})
	NoteBusyOutcome(ctx, interrupt, BusySteerOutcome{EffectiveMode: "interrupt"})
	NoteBusyAdmitting(ctx, interrupt)
	NoteBusyAdmitted(ctx, interrupt, "adm-2", "queued", nil)
	busyMu.Lock()
	interrupt.arrival.arrived -= 45
	busyMu.Unlock()
	BusyTurnStarted(ctx, "adm-2")
	NoteBusyAdmitting(ctx, cleared)
	NoteBusyAdmitted(ctx, cleared, "adm-3", "queued", nil)
	SettleBusyArrival(ctx, cleared, false)
	for _, e := range []*busyMessage{steer, interrupt, cleared} {
		SettleBusyArrival(ctx, e, false)
	}
	got := read()
	var pairs [][2]string
	for _, r := range got {
		pairs = append(pairs, [2]string{r.Data["disposition"], r.Data["wait_bucket"]})
		if r.Name != BusyArrivalMetric || r.Data["platform"] != "telegram" {
			t.Fatalf("row %v", r)
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	if want := [][2]string{{"dropped", "never"}, {"interrupted", "30s_to_2m"}, {"steered", "lt_5s"}}; !reflect.DeepEqual(pairs, want) {
		t.Fatalf("pairs %v", pairs)
	}
	assertValid(t, got)
	m.setPolicy(false)
	late := &busyMessage{text: "late"}
	StampBusyArrival(adapter, late)
	NoteBusyOutcome(ctx, late, BusySteerOutcome{Steered: true, EffectiveMode: "steer"})
	if rows := read(); len(rows) != 0 {
		t.Fatalf("collection off recorded %v", rows)
	}
}

func TestBusyCommandsAndInternalEventsAreNeverStamped(t *testing.T) {
	busyRows(t)
	cmd := &busyMessage{text: "/status"}
	StampBusyArrival(&fakeAdapter{platform: "telegram"}, cmd)
	if cmd.arrival != nil {
		t.Fatal("a command was stamped")
	}
	if WaitBucket(nil) != "never" || WaitBucket(ptrFloat(-3)) != "lt_5s" || WaitBucket(ptrFloat(600)) != "gte_10m" {
		t.Fatal("wait buckets")
	}
}
