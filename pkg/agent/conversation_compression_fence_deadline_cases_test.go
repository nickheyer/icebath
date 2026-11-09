package agent

import (
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

func TestCommitFencePublishesItsSharedDeadline(t *testing.T) {
	fence := NewCompressionCommitFence()
	if _, armed := fence.DeadlineMonotonic(); armed {
		t.Fatal("a fresh fence publishes no deadline")
	}
	if err := fence.SetTotalCeilingSeconds(600); err != nil {
		t.Fatal(err)
	}
	published, armed := fence.DeadlineMonotonic()
	if remaining := published - pythread.Monotonic(); !armed || remaining <= 590 || remaining > 600 {
		t.Fatalf("published %v armed %v remaining %v", published, armed, remaining)
	}
	if fence.DeadlineExceeded() {
		t.Fatal("a 600 second ceiling is not exceeded yet")
	}
	if err := fence.SetTotalCeilingSeconds(0.001); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if !fence.DeadlineExceeded() {
		t.Fatal("a millisecond ceiling is exceeded")
	}
	if deadline, _ := fence.DeadlineMonotonic(); deadline > pythread.Monotonic() {
		t.Fatalf("deadline %v lies in the future", deadline)
	}
}
