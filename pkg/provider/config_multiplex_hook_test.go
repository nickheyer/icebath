package provider

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestConfigIsMultiplexActiveFollowsTheProviderGate(t *testing.T) {
	ctx := context.Background()
	if config.IsMultiplexActive == nil {
		t.Fatal("pkg/config has no multiplex gate installed")
	}
	SetMultiplexActive(false)
	if config.IsMultiplexActive(ctx) {
		t.Fatal("gate reads active outside a multiplex process")
	}
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	if !config.IsMultiplexActive(ctx) {
		t.Fatal("gate misses the multiplex process")
	}
}
