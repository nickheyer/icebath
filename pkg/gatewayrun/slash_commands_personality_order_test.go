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
)

// Personality listing and the unknown name reply both follow the merged dict order, builtins then user entries
func TestPersonalityListBuiltinsThenUserEntriesInFileOrder(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	home.ResetKeyCache()
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	cfg := "personalities:\n  zeta: Zeta prompt\n  alpha: Alpha prompt\nagent:\n  personalities:\n    helpful: Overridden helpful\n    beta: Beta prompt\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{"helpful", "concise", "technical", "creative", "teacher", "kawaii", "catgirl", "pirate",
		"shakespeare", "surfer", "noir", "uwu", "philosopher", "hype", "zeta", "alpha", "beta"}
	r := allocGatewayRunner()
	ctx := context.Background()
	event := gateway.NewMessageEvent("/personality")
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "c1", ChatType: "dm"}
	listing, err := r.handlePersonalityCommand(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	last := -1
	for _, name := range want {
		at := strings.Index(listing, "`"+name+"`")
		if at < 0 {
			at = strings.Index(listing, name)
		}
		if at <= last {
			t.Fatalf("%s listed out of order in\n%s", name, listing)
		}
		last = at
	}
	if !strings.Contains(listing, "Overridden helpful") {
		t.Fatalf("user override of a builtin missing from\n%s", listing)
	}
	unknown := gateway.NewMessageEvent("/personality nope")
	unknown.Source = event.Source
	reply, err := r.handlePersonalityCommand(ctx, unknown)
	if err != nil {
		t.Fatal(err)
	}
	quoted := make([]string, len(want))
	for i, name := range want {
		quoted[i] = "`" + name + "`"
	}
	if !strings.Contains(reply, "`none`, "+strings.Join(quoted, ", ")) {
		t.Fatalf("unknown reply = %q", reply)
	}
}
