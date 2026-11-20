package gatewayrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Host multiplexer with the launch profile on Discord and the served profile coder on Telegram, a restart notice owed
func multiplexRestartRunner(t *testing.T) (*GatewayRunner, string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	r, _ := makeRestartRunner(t)
	r.config = homeOrderConfig(gateway.PlatformDiscord, "launch-home")
	r.config.SessionsDir = filepath.Join(tmp, "sessions")
	r.adapters = adapterDict()
	r.profileConfigs = pydict.New[string, *gateway.GatewayConfig]()
	r.profileConfigs.Set("coder", homeOrderConfig(gateway.PlatformTelegram, "coder-home"))
	r.profileAdapters = pydict.New[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]]()
	r.profileAdapters.Set("coder", adapterDict())
	marker := filepath.Join(tmp, ".restart_pending.json")
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return r, marker
}

func TestPlannedRestartNotifiesEveryServedProfile(t *testing.T) {
	r, marker := multiplexRestartRunner(t)
	launch, coder := newRestartTestAdapter(), newRestartTestAdapter()
	r.adapters.Set(gateway.PlatformDiscord, launch)
	coderAdapters, _ := r.profileAdapters.Get("coder")
	coderAdapters.Set(gateway.PlatformTelegram, coder)
	r.replayPendingPlannedRestartNotification(context.Background())
	if len(launch.calls()) != 1 {
		t.Fatalf("launch home got %d notices", len(launch.calls()))
	}
	sent := coder.calls()
	if len(sent) != 1 {
		t.Fatalf("a served profile's home channel is owed the restart notice, got %d", len(sent))
	}
	if sent[0].chatID != "coder-home" || sent[0].content != "♻️ Gateway online — Icebath is back and ready." {
		t.Fatalf("coder notice %+v", sent[0])
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("every owed target was notified, the marker must be gone: %v", err)
	}
}

func TestMarkerSurvivesUntilAServedProfileIsReachable(t *testing.T) {
	r, marker := multiplexRestartRunner(t)
	launch := newRestartTestAdapter()
	r.adapters.Set(gateway.PlatformDiscord, launch)
	r.replayPendingPlannedRestartNotification(context.Background())
	if len(launch.calls()) != 1 {
		t.Fatalf("launch home got %d notices", len(launch.calls()))
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("coder's channel was never notified, the marker must stay: %v", err)
	}
	var pending struct {
		Delivered [][]any `json:"delivered_targets"`
	}
	if err := json.Unmarshal(raw, &pending); err != nil {
		t.Fatal(err)
	}
	reached := false
	for _, target := range pending.Delivered {
		reached = reached || (len(target) > 0 && target[0] == "discord")
	}
	if !reached {
		t.Fatalf("the reached target is not recorded: %s", raw)
	}
	coder := newRestartTestAdapter()
	coderAdapters, _ := r.profileAdapters.Get("coder")
	coderAdapters.Set(gateway.PlatformTelegram, coder)
	r.replayPendingPlannedRestartNotification(context.Background())
	if len(coder.calls()) != 1 {
		t.Fatalf("coder got %d notices", len(coder.calls()))
	}
	if len(launch.calls()) != 1 {
		t.Fatal("a reached home was notified twice")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the marker outlived its last owed target: %v", err)
	}
}
