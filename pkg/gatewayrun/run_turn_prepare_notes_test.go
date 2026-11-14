package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	"gopkg.in/yaml.v3"
)

func TestFirstDmGetsTheProfileBuildOfferOnce(t *testing.T) {
	ctx := context.Background()
	h := t.TempDir()
	t.Setenv(home.EnvHome, h)
	t.Setenv("TELEGRAM_HOME_CHANNEL", "home")
	home.ResetKeyCache()
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	cfgPath := filepath.Join(h, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("model: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := allocGatewayRunner()
	r.config = &gateway.GatewayConfig{}
	r.sessionStore = gateway.NewSessionStore(ctx, filepath.Join(h, "sessions"), r.config, nil)
	t.Cleanup(r.sessionStore.CloseAllDBHandles)
	user := "u"
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "dm", UserID: &user, ChatType: "dm"}
	var first, second []string
	if err := r.hmwaFirstContactNotes(ctx, source, nil, &first, false); err != nil {
		t.Fatal(err)
	}
	if err := r.hmwaFirstContactNotes(ctx, source, nil, &second, false); err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0] != strings.TrimSpace(tools.ProfileBuildDirective()) {
		t.Fatalf("first %q", first)
	}
	if len(second) != 1 || second[0] == first[0] {
		t.Fatalf("second %q", second)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	seen, _ := doc["onboarding"].(map[string]any)["seen"].(map[string]any)
	if seen[tools.OnboardingProfileBuildFlag] != true {
		t.Fatalf("seen %v", seen)
	}
}
