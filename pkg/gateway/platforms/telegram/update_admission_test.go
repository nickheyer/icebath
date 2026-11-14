package telegram

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type admissionAdapter struct{ name string }

type admissionRig struct {
	adapter *admissionAdapter
	state   *UpdateAdmission
	app     *TelegramApplication
	mu      sync.Mutex
	calls   []int64
}

func newAdmissionRig(t *testing.T, dir string, processor UpdateProcessor) *admissionRig {
	t.Helper()
	r := &admissionRig{adapter: &admissionAdapter{"core"}, state: NewUpdateAdmission(dir)}
	r.app = NewTelegramApplication(111, processor, r.adapter, r.state)
	t.Cleanup(func() {
		if flush := r.state.ReceiptFlush(); flush != nil {
			_ = flush.Wait(context.Background())
		}
	})
	return r
}

func (r *admissionRig) rebuild(processor UpdateProcessor) {
	r.app = NewTelegramApplication(111, processor, r.adapter, r.state)
}

func (r *admissionRig) record(update any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, update.(*Update).UpdateID)
}

func (r *admissionRig) seen() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.calls...)
}

// Core handler accepting the claim at its handoff like the adapter's handle_message
func (r *admissionRig) coreHandler(fail error) *TypeHandler[*Update] {
	return NewTypeHandler[*Update](&Callback{Owner: r.adapter, Fn: func(ctx context.Context, update any, _ *CallbackContext) error {
		if fail != nil {
			return fail
		}
		r.state.CurrentClaim(ctx).Accept()
		r.record(update)
		return nil
	}})
}

func flushed(t *testing.T, s *UpdateAdmission) {
	t.Helper()
	waitTask(t, s.ReceiptFlush())
}

func receiptIDs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := v.(*pyjson.Object).Get("update_ids")
	return append([]string(nil), ids.(*pyjson.Object).Keys...)
}

func TestReplayIsAdmittedOnceBeforeDispatch(t *testing.T) {
	r := newAdmissionRig(t, t.TempDir(), nil)
	r.app.AddHandler(r.coreHandler(nil), 0)
	ctx := context.Background()
	for range 2 {
		if err := r.app.ProcessUpdate(ctx, &Update{UpdateID: 10, Chat: &Chat{ID: 42}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.seen(); !slices.Equal(got, []int64{10}) {
		t.Fatalf("dispatched %v", got)
	}
	if r.state.UpdatesDispatchedTotal() != 2 || len(r.state.InflightUpdateIDs()) != 0 {
		t.Fatalf("dispatched total %d inflight %v", r.state.UpdatesDispatchedTotal(), r.state.InflightUpdateIDs())
	}
	flushed(t, r.state)
	if got := receiptIDs(t, r.state.ReceiptPath(111)); !slices.Equal(got, []string{"10"}) {
		t.Fatalf("receipts %v", got)
	}
}

func TestConcurrentReplayIsDroppedWhileInFlight(t *testing.T) {
	r := newAdmissionRig(t, t.TempDir(), nil)
	entered, release := make(chan struct{}), make(chan struct{})
	r.app.AddHandler(NewTypeHandler[*Update](&Callback{Owner: r.adapter, Fn: func(ctx context.Context, update any, _ *CallbackContext) error {
		r.state.CurrentClaim(ctx).Accept()
		r.record(update)
		close(entered)
		<-release
		return nil
	}}), 0)
	done := make(chan error, 1)
	go func() { done <- r.app.ProcessUpdate(context.Background(), &Update{UpdateID: 10}) }()
	<-entered
	if got := r.state.InflightUpdateIDs(); !slices.Equal(got, []string{"111:10"}) {
		t.Fatalf("inflight %v", got)
	}
	if err := r.app.ProcessUpdate(context.Background(), &Update{UpdateID: 10}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := r.seen(); len(got) != 1 {
		t.Fatalf("dispatched %v", got)
	}
}

func TestOnlyPreHandoffFailureReopensAdmission(t *testing.T) {
	r := newAdmissionRig(t, t.TempDir(), nil)
	fail := errors.New("preparation failed")
	r.app.AddHandler(r.coreHandler(fail), 0)
	var stopped []int64
	r.app.AddHandler(NewTypeHandler[*Update](&Callback{Owner: r.adapter, Fn: func(_ context.Context, update any, _ *CallbackContext) error {
		stopped = append(stopped, update.(*Update).UpdateID)
		return nil
	}}), 99)
	ctx := context.Background()
	if err := r.app.ProcessUpdate(ctx, &Update{UpdateID: 10}); err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 0 {
		t.Fatal("an unaccepted failure must stop the later groups")
	}
	if r.state.SeenUpdateIDs().Has("111:10") {
		t.Fatal("a failed preparation must reopen admission")
	}
	r.rebuild(nil)
	r.app.AddHandler(r.coreHandler(nil), 0)
	if err := r.app.ProcessUpdate(ctx, &Update{UpdateID: 10}); err != nil {
		t.Fatal(err)
	}
	if got := r.seen(); !slices.Equal(got, []int64{10}) || !r.state.SeenUpdateIDs().Has("111:10") {
		t.Fatalf("redelivery dispatched %v", got)
	}
	flushed(t, r.state)
}

func TestFailureAfterHandoffStaysAdmitted(t *testing.T) {
	r := newAdmissionRig(t, t.TempDir(), nil)
	r.app.AddHandler(NewTypeHandler[*Update](&Callback{Owner: r.adapter, Fn: func(ctx context.Context, _ any, _ *CallbackContext) error {
		r.state.CurrentClaim(ctx).Accept()
		return errors.New("after handoff")
	}}), 0)
	var errSeen []error
	r.app.AddErrorHandler(&Callback{Fn: func(_ context.Context, _ any, cbctx *CallbackContext) error {
		errSeen = append(errSeen, cbctx.Error)
		return nil
	}}, BlockDefault)
	if err := r.app.ProcessUpdate(context.Background(), &Update{UpdateID: 7}); err != nil {
		t.Fatal(err)
	}
	if len(errSeen) != 1 || !r.state.SeenUpdateIDs().Has("111:7") {
		t.Fatalf("errors %v seen %v", errSeen, r.state.SeenUpdateIDs().Keys())
	}
	flushed(t, r.state)
}

func TestCaughtPreparationFailureStopsLaterGroups(t *testing.T) {
	r := newAdmissionRig(t, t.TempDir(), nil)
	r.app.AddHandler(NewTypeHandler[*Update](&Callback{Owner: r.adapter, Fn: func(ctx context.Context, _ any, _ *CallbackContext) error {
		r.state.CurrentClaim(ctx).Fail()
		return nil
	}}), 0)
	later := 0
	r.app.AddHandler(NewTypeHandler[*Update](&Callback{Owner: r.adapter, Fn: func(context.Context, any, *CallbackContext) error {
		later++
		return nil
	}}), 1)
	if err := r.app.ProcessUpdate(context.Background(), &Update{UpdateID: 3}); err != nil {
		t.Fatal(err)
	}
	if later != 0 || r.state.SeenUpdateIDs().Has("111:3") {
		t.Fatal("a caught failure must stop blocking groups and stay unrecorded")
	}
}

func TestNativePluginEntryIsTheHandoff(t *testing.T) {
	r := newAdmissionRig(t, t.TempDir(), nil)
	plugin := &Callback{Owner: "plugin", Fn: func(context.Context, any, *CallbackContext) error {
		return errors.New("plugin raised")
	}}
	h := NewTypeHandler[*Update](plugin)
	r.app.AddHandler(h, -1)
	r.rebuild(nil)
	r.app.AddHandler(h, -1)
	if !h.Callback.admitted {
		t.Fatal("handler callback must be admitted once")
	}
	if err := r.app.ProcessUpdate(context.Background(), &Update{UpdateID: 5}); err != nil {
		t.Fatal(err)
	}
	if !r.state.SeenUpdateIDs().Has("111:5") {
		t.Fatal("plugin entry must accept the claim")
	}
	flushed(t, r.state)
}

func TestNonBlockingTaskPinsTheClaim(t *testing.T) {
	r := newAdmissionRig(t, t.TempDir(), nil)
	release := make(chan struct{})
	h := NewTypeHandler[*Update](&Callback{Owner: r.adapter, Fn: func(ctx context.Context, _ any, _ *CallbackContext) error {
		<-release
		return nil
	}})
	h.BlockMode = BlockFalse
	r.app.AddHandler(h, 0)
	if err := r.app.ProcessUpdate(context.Background(), &Update{UpdateID: 9}); err != nil {
		t.Fatal(err)
	}
	if got := r.state.InflightUpdateIDs(); !slices.Equal(got, []string{"111:9"}) {
		t.Fatalf("a scheduled task must pin the claim, inflight %v", got)
	}
	close(release)
	waitFor(t, func() bool { return len(r.state.InflightUpdateIDs()) == 0 })
	waitFor(t, func() bool { return r.state.SeenUpdateIDs().Has("111:9") })
	flushed(t, r.state)
}

func TestCancelledTaskFailsTheClaim(t *testing.T) {
	r := newAdmissionRig(t, t.TempDir(), nil)
	var task *gateway.AdapterTask
	started := make(chan struct{})
	r.app.AddHandler(NewTypeHandler[*Update](&Callback{Owner: r.adapter, Fn: func(ctx context.Context, update any, cbctx *CallbackContext) error {
		task = cbctx.Application.CreateTask(ctx, func(tctx context.Context) error {
			close(started)
			<-tctx.Done()
			return tctx.Err()
		}, update)
		return nil
	}}), 0)
	if err := r.app.ProcessUpdate(context.Background(), &Update{UpdateID: 4}); err != nil {
		t.Fatal(err)
	}
	<-started
	gateway.CancelTask(context.Background(), task)
	waitFor(t, func() bool { return len(r.state.InflightUpdateIDs()) == 0 })
	if r.state.SeenUpdateIDs().Has("111:4") {
		t.Fatal("a cancelled pinned task must fail the claim")
	}
}

func TestErrorCallbackAcceptsTheLiveClaimAndCanStop(t *testing.T) {
	r := newAdmissionRig(t, t.TempDir(), nil)
	r.app.AddHandler(r.coreHandler(errors.New("boom")), 0)
	later := 0
	r.app.AddHandler(NewTypeHandler[*Update](&Callback{Fn: func(context.Context, any, *CallbackContext) error {
		later++
		return nil
	}}), 1)
	observer := &Callback{Fn: func(context.Context, any, *CallbackContext) error { return ErrApplicationHandlerStop }}
	r.app.AddErrorHandler(observer, BlockDefault)
	r.app.AddErrorHandler(observer, BlockDefault)
	if err := r.app.ProcessUpdate(context.Background(), &Update{UpdateID: 6}); err != nil {
		t.Fatal(err)
	}
	if later != 0 || !r.state.SeenUpdateIDs().Has("111:6") {
		t.Fatal("a registered error callback is an extension boundary that accepts the claim and may stop")
	}
	flushed(t, r.state)
	r.app.RemoveErrorHandler(observer)
	if err := r.app.ProcessUpdate(context.Background(), &Update{UpdateID: 8}); err != nil {
		t.Fatal(err)
	}
	if r.state.SeenUpdateIDs().Has("111:8") {
		t.Fatal("without the observer an unaccepted failure stays reopened")
	}
}

func TestRedeliveryToRebuiltAdapterIsDropped(t *testing.T) {
	dir := t.TempDir()
	r := newAdmissionRig(t, dir, nil)
	r.app.AddHandler(r.coreHandler(nil), 0)
	if err := r.app.ProcessUpdate(context.Background(), &Update{UpdateID: 10}); err != nil {
		t.Fatal(err)
	}
	flushed(t, r.state)
	restarted := newAdmissionRig(t, dir, nil)
	restarted.app.AddHandler(restarted.coreHandler(nil), 0)
	if err := restarted.app.ProcessUpdate(context.Background(), &Update{UpdateID: 10}); err != nil {
		t.Fatal(err)
	}
	if len(restarted.seen()) != 0 {
		t.Fatal("a restarted adapter must drop a receipted redelivery")
	}
	path := r.state.ReceiptPath(111)
	stale := nowSeconds() - ReceiptTTLSeconds - 1
	body := fmt.Sprintf(`{"update_ids": {"10": %f, "20": "bad", "x": %f, "-3": %f}}`, stale, nowSeconds(), nowSeconds())
	if err := os.WriteFile(path, []byte("\uFEFF"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := newAdmissionRig(t, dir, nil)
	fresh.app.AddHandler(fresh.coreHandler(nil), 0)
	for _, id := range []int64{10, -3} {
		if err := fresh.app.ProcessUpdate(context.Background(), &Update{UpdateID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if got := fresh.seen(); !slices.Equal(got, []int64{10}) {
		t.Fatalf("dispatched %v", got)
	}
	flushed(t, fresh.state)
	if got := receiptIDs(t, path); !slices.Equal(got, []string{"-3", "10"}) {
		t.Fatalf("receipts %v", got)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := newAdmissionRig(t, dir, nil)
	broken.app.AddHandler(broken.coreHandler(nil), 0)
	if err := broken.app.ProcessUpdate(context.Background(), &Update{UpdateID: 30}); err != nil {
		t.Fatal(err)
	}
	if got := broken.seen(); !slices.Equal(got, []int64{30}) {
		t.Fatalf("an unreadable receipt file must be ignored, dispatched %v", got)
	}
	flushed(t, broken.state)
}

func TestReceiptLoadRaisesOnAnIntPastTheFloatRange(t *testing.T) {
	dir := t.TempDir()
	r := newAdmissionRig(t, dir, nil)
	big := "1" + strings.Repeat("0", 400)
	if err := os.WriteFile(r.state.ReceiptPath(111), []byte(`{"update_ids": {"1": `+big+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.app.ProcessUpdate(context.Background(), &Update{UpdateID: 1}); err == nil {
		t.Fatal("float of an int past the float range must raise OverflowError")
	}
}

func TestBuildUpdateProcessorReadsTheExtraBound(t *testing.T) {
	cases := []struct {
		extra map[string]any
		want  int
	}{
		{map[string]any{}, DefaultMaxConcurrentUpdates},
		{map[string]any{"max_concurrent_updates": 7}, 7},
		{map[string]any{"max_concurrent_updates": "lots"}, DefaultMaxConcurrentUpdates},
		{map[string]any{"max_concurrent_updates": math.Inf(1)}, DefaultMaxConcurrentUpdates},
		{map[string]any{"max_concurrent_updates": 0}, DefaultMaxConcurrentUpdates},
	}
	for _, c := range cases {
		if got := BuildUpdateProcessor(c.extra, "Telegram").MaxConcurrentUpdates(); got != c.want {
			t.Fatalf("%v: %d", c.extra, got)
		}
	}
	if _, err := NewPerChatUpdateProcessor(0); err == nil {
		t.Fatal("a bound below one must raise ValueError")
	}
}

func TestPerChatProcessorKeepsChatOrderAcrossACancelledWaiter(t *testing.T) {
	p := BuildUpdateProcessor(map[string]any{}, "Telegram")
	var mu sync.Mutex
	var order []string
	gate := make(chan struct{})
	handler := func(name string) func(context.Context) error {
		return func(context.Context) error {
			mu.Lock()
			order = append(order, "start "+name)
			mu.Unlock()
			if name == "A" {
				<-gate
			}
			mu.Lock()
			order = append(order, "end "+name)
			mu.Unlock()
			return nil
		}
	}
	chat := &Update{Chat: &Chat{ID: 1}}
	results := make([]chan error, 3)
	cancels := make([]context.CancelFunc, 3)
	for i, name := range []string{"A", "B", "C"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i], results[i] = cancel, make(chan error, 1)
		go func() { results[i] <- p.ProcessUpdate(ctx, chat, handler(name)) }()
		time.Sleep(10 * time.Millisecond)
	}
	cancels[1]()
	if err := <-results[1]; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter returned %v", err)
	}
	close(gate)
	if err := <-results[0]; err != nil {
		t.Fatal(err)
	}
	if err := <-results[2]; err != nil {
		t.Fatal(err)
	}
	cancels[0]()
	cancels[2]()
	if want := []string{"start A", "end A", "start C", "end C"}; !slices.Equal(order, want) {
		t.Fatalf("order %v", order)
	}
}

func TestSlowUpdateDoesNotBlockOtherChats(t *testing.T) {
	r := newAdmissionRig(t, t.TempDir(), BuildUpdateProcessor(map[string]any{}, "Telegram"))
	gate := make(chan struct{})
	r.app.AddHandler(NewTypeHandler[*Update](&Callback{Fn: func(_ context.Context, update any, _ *CallbackContext) error {
		if update.(*Update).UpdateID == 10 {
			<-gate
		}
		return nil
	}}), -1)
	r.app.AddHandler(r.coreHandler(nil), 0)
	ctx := context.Background()
	for _, u := range []*Update{{UpdateID: 10, Chat: &Chat{ID: 42}}, {UpdateID: 11, Chat: &Chat{ID: 43}}, {UpdateID: 12, Chat: &Chat{ID: 42}}} {
		if err := r.app.FetchUpdate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return slices.Equal(r.seen(), []int64{11}) })
	close(gate)
	waitFor(t, func() bool { return slices.Equal(r.seen(), []int64{11, 10, 12}) })
	waitFor(t, func() bool { return len(r.state.InflightUpdateIDs()) == 0 })
	flushed(t, r.state)
	if got := receiptIDs(t, filepath.Join(r.state.ReceiptDir, "telegram_update_receipts_111.json")); len(got) != 3 {
		t.Fatalf("receipts %v", got)
	}
}
