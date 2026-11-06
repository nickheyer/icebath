package selfupdate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunChannelPinnedVersionAlwaysWins(t *testing.T) {
	stable, canary := "stable", "canary"
	for _, transient := range []*string{&stable, &canary, nil} {
		if got, err := SourceUpdateChannel(context.Background(), true, transient, t.TempDir()); err != nil || got != ChannelMain {
			t.Fatalf("%v %v", got, err)
		}
	}
}

func TestRunChannelTransientFlagWinsWithoutReadingConfig(t *testing.T) {
	h := chHome(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("update: [broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"stable", "main", "canary"} {
		if got, err := SourceUpdateChannel(context.Background(), false, &channel, t.TempDir()); err != nil || got != channel {
			t.Fatalf("%v %v", got, err)
		}
	}
}

func TestRunChannelPerInstallRecordActivates(t *testing.T) {
	h := chHome(t)
	root := filepath.Join(filepath.Dir(h), "install")
	chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": "self"})
	data, _ := json.Marshal(chConfigFor(root, "stable"))
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := SourceUpdateChannel(context.Background(), false, nil, root); err != nil || got != ChannelStable {
		t.Fatalf("%v %v", got, err)
	}
}

func TestRunChannelNoRecordStaysMain(t *testing.T) {
	h := chHome(t)
	root := filepath.Join(filepath.Dir(h), "install")
	chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": "self"})
	if got, err := SourceUpdateChannel(context.Background(), false, nil, root); err != nil || got != ChannelMain {
		t.Fatalf("%v %v", got, err)
	}
}

func TestRunChannelConfigFailureNeverChangesTheSubscription(t *testing.T) {
	h := chHome(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("update: [broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SourceUpdateChannel(context.Background(), false, nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "formatting error") {
		t.Fatalf("got %v", err)
	}
}
