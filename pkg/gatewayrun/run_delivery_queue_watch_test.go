package gatewayrun

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
)

func TestTheWatchSettlesAfterTheDrainWroteItsOutcome(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	queue := cron.DeliveryQueueOf(home.Dir(ctx))
	var passes []int
	watch := NewDeliveryQueueWatch(ctx, func() []*string { return []*string{nil} }, func() {
		n, err := queue.Drain(ctx, func(*cron.Job, string, bool) (*string, error) { return nil, nil }, 20)
		if err != nil {
			t.Fatal(err)
		}
		passes = append(passes, n)
	})
	if watch.Changed(ctx) {
		t.Fatal("no queue file, nothing to wake on")
	}
	if _, err := queue.Enqueue(ctx, "exec-1", &cron.Job{ID: "job"}, "hello", false); err != nil {
		t.Fatal(err)
	}
	if !watch.Changed(ctx) {
		t.Fatal("the enqueue moved the queue stamp")
	}
	watch.Drain()
	if !slices.Equal(passes, []int{1}) {
		t.Fatalf("passes = %v", passes)
	}
	status, err := queue.GetStatus(ctx, "exec-1")
	if err != nil || status == nil || status["status"] != "delivered" {
		t.Fatalf("status = %v, %v", status, err)
	}
	if !watch.Changed(ctx) {
		t.Fatal("the delivered status was written to the queue")
	}
	watch.Drain()
	if !slices.Equal(passes, []int{1, 0}) {
		t.Fatalf("passes = %v", passes)
	}
	if watch.Changed(ctx) {
		t.Fatal("an empty pass writes nothing")
	}
	if watch.Changed(ctx) {
		t.Fatal("a stable queue never wakes the watch")
	}
	wal := cron.QueuePath(home.Dir(ctx)) + "-wal"
	if err := os.WriteFile(wal, []byte("frame"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !watch.Changed(ctx) {
		t.Fatal("a write to the wal file moves the stamp")
	}
}
