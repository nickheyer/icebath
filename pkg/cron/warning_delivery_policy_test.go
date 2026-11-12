package cron

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Gateway config with telegram enabled, the autouse configured_transport fixture of the Python file
func configuredTransport(t *testing.T) {
	t.Helper()
	prev := loadGatewayConfig
	t.Cleanup(func() { loadGatewayConfig = prev })
	loadGatewayConfig = func(ctx context.Context) (*gateway.GatewayConfig, error) {
		cfg := gateway.DefaultGatewayConfig(ctx)
		cfg.Platforms[gateway.PlatformTelegram] = &gateway.PlatformConfig{Enabled: true}
		return cfg, nil
	}
}

// Counts standalone sends in place of the real lane, the patched _deliver_standalone
func countStandaloneSends(t *testing.T) *int {
	t.Helper()
	calls := new(int)
	prev := deliverStandaloneDoor
	t.Cleanup(func() { deliverStandaloneDoor = prev })
	deliverStandaloneDoor = func(*Store, context.Context, *targetDelivery, string, []gateway.MediaRef, *[]string, *[]string) {
		*calls++
	}
	return calls
}

// Job record decoded from its Python dict form
func jobFromDict(t *testing.T, pairs ...any) *Job {
	t.Helper()
	job, err := objectJob(pyjson.ObjectOf(pairs...))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestFailureQueueSettlesWithoutClaimingASuppressedSend(t *testing.T) {
	for _, suppress := range []bool{false, true} {
		t.Run(fmt.Sprint(suppress), func(t *testing.T) {
			s, _ := newTestStore(t)
			ctx := context.Background()
			configuredTransport(t)
			realDeliverySeams(t)
			deliveryDBOverride = filepath.Join(t.TempDir(), "queue.db")
			t.Cleanup(func() { deliveryDBOverride = "" })
			writePinConfig(t, s, "display: {suppress_warning_notifications: false}\n")
			job := jobFromDict(t, "id", "fixture", "name", "fixture", "deliver", "telegram:chat", "execution_id", "run")
			q := openDeliveryQueue(s)
			mustEnqueue(t, q, ctx, "run", job, "arbitrary diagnostic", true)
			writePinConfig(t, s, fmt.Sprintf("display: {suppress_warning_notifications: %v}\n", suppress))
			sends := countStandaloneSends(t)
			if n, err := s.drainDeliveryQueue(ctx, map[string]any{}, nil); err != nil || n != 1 {
				t.Fatalf("drain = %d, %v", n, err)
			}
			want, wantSends := "delivered", 1
			if suppress {
				want, wantSends = "suppressed", 0
			}
			if row := mustStatus(t, q, ctx, "run"); row["status"] != want || *sends != wantSends {
				t.Fatalf("status %v sends %d", row["status"], *sends)
			}
			if n, err := s.drainDeliveryQueue(ctx, map[string]any{}, nil); err != nil || n != 0 {
				t.Fatalf("second drain = %d, %v", n, err)
			}
		})
	}
}

func TestSuccessContentAndExplicitDestinationOverrideSurvive(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	configuredTransport(t)
	realDeliverySeams(t)
	writePinConfig(t, s, "display:\n  suppress_warning_notifications: true\n  platforms:\n    telegram:\n      suppress_warning_notifications: false\n")
	job := jobFromDict(t, "id", "fixture", "deliver", "telegram:chat")
	sends := countStandaloneSends(t)
	for _, tc := range []struct {
		content    string
		forFailure bool
	}{{"warning quoted in requested result", false}, {"diagnostic", true}} {
		if errText, err := s.deliverResult(ctx, job, tc.content, nil, nil, tc.forFailure); err != nil || errText != nil {
			t.Fatalf("%s = %v, %v", tc.content, errText, err)
		}
	}
	if *sends != 2 {
		t.Fatalf("sends = %d", *sends)
	}
}

func TestMixedTargetQueueIsNotReportedWhollySuppressed(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	configuredTransport(t)
	realDeliverySeams(t)
	deliveryDBOverride = filepath.Join(t.TempDir(), "queue.db")
	t.Cleanup(func() { deliveryDBOverride = "" })
	writePinConfig(t, s, "display:\n  suppress_warning_notifications: true\n  platforms:\n    telegram:\n      suppress_warning_notifications: false\n")
	job := jobFromDict(t, "id", "fixture", "deliver", []any{"slack:muted", "telegram:allowed"})
	q := openDeliveryQueue(s)
	mustEnqueue(t, q, ctx, "mixed", job, "diagnostic", true)
	sends := countStandaloneSends(t)
	if n, err := s.drainDeliveryQueue(ctx, map[string]any{}, nil); err != nil || n != 1 {
		t.Fatalf("drain = %d, %v", n, err)
	}
	if *sends != 1 {
		t.Fatalf("sends = %d", *sends)
	}
	if row := mustStatus(t, q, ctx, "mixed"); row["status"] != "delivered" {
		t.Fatalf("status = %v", row["status"])
	}
}
