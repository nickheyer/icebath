package remote

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// A process whose recorded poller is gone re-arms a live poller on its next config read
func TestForkedChildRearmsItsPoller(t *testing.T) {
	p := setupPlane(t)
	ctx := context.Background()
	if _, err := config.LoadContext(ctx, p.Home, os.LookupEnv); err != nil {
		t.Fatal(err)
	}
	b := testBackend()
	b.mu.Lock()
	first := b.pollerDone
	gone := make(chan struct{})
	close(gone)
	b.pollerDone = gone
	b.mu.Unlock()
	if _, err := config.LoadContext(ctx, p.Home, os.LookupEnv); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	rearmed := b.pollerDone
	b.mu.Unlock()
	if rearmed == nil || rearmed == gone || rearmed == first || !b.pollerAlive() {
		t.Fatal("the poller was not re-armed")
	}
}
