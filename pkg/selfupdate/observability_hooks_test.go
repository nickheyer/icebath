package selfupdate

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
)

func TestObservabilityResolveChannelHookFollowsTheRecord(t *testing.T) {
	chHome(t)
	source := t.TempDir()
	if got, err := observability.ResolveUpdateChannelHook(chConfigFor(source, "canary"), source); err != nil || got != "canary" {
		t.Fatalf("recorded %q %v", got, err)
	}
	if _, err := observability.ResolveUpdateChannelHook(chConfigFor(source, "Not A Channel!"), source); err == nil {
		t.Fatal("an invalid recorded channel must refuse")
	}
	if got, err := observability.ResolveUpdateChannelHook(map[string]any{}, source); err != nil || got != ChannelMain {
		t.Fatalf("unrecorded %q %v", got, err)
	}
}
