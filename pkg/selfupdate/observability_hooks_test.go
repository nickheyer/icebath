package selfupdate

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
)

func TestObservabilityResolveChannelHookFollowsPackageThenRecord(t *testing.T) {
	chHome(t)
	root := t.TempDir()
	chStamp(t, root, map[string]any{"payload": "bundled", "source": "channel-build", "updateMechanism": "electron-updater",
		"channelBuild": map[string]any{"channel": "r2-preview"}})
	if got, err := observability.ResolveUpdateChannelHook(chConfigFor(root, "stable"), root); err != nil || got != "r2-preview" {
		t.Fatalf("packaged %q %v", got, err)
	}
	source := t.TempDir()
	if got, err := observability.ResolveUpdateChannelHook(chConfigFor(source, "canary"), source); err != nil || got != "canary" {
		t.Fatalf("recorded %q %v", got, err)
	}
	if _, err := observability.ResolveUpdateChannelHook(chConfigFor(source, "Not A Channel!"), source); err == nil {
		t.Fatal("an invalid recorded channel must refuse")
	}
}

func TestObservabilityPackageChannelHookAnswersTheMatchedStampValue(t *testing.T) {
	cases := []struct {
		stamp map[string]any
		want  string
	}{
		{map[string]any{"payload": "light"}, "light"},
		{map[string]any{"payload": "runtime", "updateMechanism": "app-installer"}, "runtime"},
		{map[string]any{"payload": "source", "updateMechanism": "microsoft-store"}, "microsoft-store"},
		{map[string]any{"updateMechanism": "electron-updater"}, "electron-updater"},
		{map[string]any{"payload": "source", "updateMechanism": "self"}, ""},
		{map[string]any{"payload": 7}, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		root := t.TempDir()
		if tc.stamp != nil {
			chStamp(t, root, tc.stamp)
		}
		got, err := observability.PackageChannelHook(root)
		if err != nil || got != tc.want {
			t.Fatalf("stamp %v answered %q %v, want %q", tc.stamp, got, err, tc.want)
		}
	}
}
