package whatsapp

import (
	"context"
	"maps"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Adapter over the session name and the given extra
func batchingAdapter(t *testing.T, extra map[string]any) *WhatsAppAdapter {
	t.Helper()
	cfg := map[string]any{"session_name": "test"}
	maps.Copy(cfg, extra)
	a, err := NewWhatsAppAdapter(context.Background(), &gateway.PlatformConfig{Enabled: true, Extra: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestBatchDelaysOverriddenViaConfigExtra(t *testing.T) {
	hermeticHome(t)
	delay, split := batchingAdapter(t, map[string]any{"text_batch_delay_seconds": "1.5", "text_batch_split_delay_seconds": 3}).TextBatchDelays()
	if delay != 1.5 || split != 3.0 {
		t.Fatalf("delays = %v %v", delay, split)
	}
}

func TestInvalidConfigValueFallsBackToDefault(t *testing.T) {
	hermeticHome(t)
	delay, split := batchingAdapter(t, map[string]any{"text_batch_delay_seconds": "garbage", "text_batch_split_delay_seconds": -3}).TextBatchDelays()
	defDelay, defSplit := batchingAdapter(t, nil).TextBatchDelays()
	if delay != defDelay || split != defSplit {
		t.Fatalf("delays = %v %v, defaults %v %v", delay, split, defDelay, defSplit)
	}
}
