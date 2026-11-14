package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Host source whose lookup fails the way a broken Relay binding does
type relayFailingHostSource struct{ asked int }

func (s *relayFailingHostSource) ForProfile(context.Context, string, bool) RelayHost {
	s.asked++
	panic("relay host lookup failed")
}

func TestCompactionNotificationFailureNeverReachesTheCaller(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	config := "gateway:\n  telemetry:\n    session_segments:\n      on_compaction: true\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ResetRelaySegmentsConfigForTests()
	t.Cleanup(ResetRelaySegmentsConfigForTests)
	source := &relayFailingHostSource{}
	coordinator := &RelaySessionCoordinator{Registry: source}
	coordinator.NotifySessionCompacted(context.Background(), "profile", "session-new", "session-old")
	if source.asked != 1 {
		t.Fatalf("the notification asked the host source %d times, want 1", source.asked)
	}
}
