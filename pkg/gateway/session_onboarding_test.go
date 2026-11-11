package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestOnboardingStartFromAClientThatIsNotTheLocalDesktopChangesNothing(t *testing.T) {
	ctx, db := newTestStateDB(t)
	previous := tools.AllSkillNames
	tools.AllSkillNames = skills.AllSkillNames
	t.Cleanup(func() { tools.AllSkillNames = previous })
	profileHome := filepath.Dir(db.Path())
	if err := os.WriteFile(filepath.Join(profileHome, "config.yaml"),
		[]byte("model:\n  provider: openrouter\n  default: anthropic/claude-sonnet-4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(ctx, filepath.Join(profileHome, "sessions"), DefaultGatewayConfig(ctx), nil)
	store.PinDB(db)
	runner := &recoverTestRunner{store: store, adapters: pydict.New[Platform, PlatformAdapter](),
		profiles: pydict.New[string, *pydict.Dict[Platform, PlatformAdapter]]()}
	a, err := InitializeSessionAuthority(ctx, runner, profileHome, "owner", db, false)
	if err != nil {
		t.Fatal(err)
	}
	runner.launch = a
	connection, err := NewAuthorityConnection(a, wireContractTransport{}, map[string]any{"user_id": "uid:1", "provider": "local",
		"profile_id": profileHome, "instance_id": "owner", "native_bootstrap": true}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(ctx)
	reply, err := connection.Dispatch(ctx, pyjson.ObjectOf("jsonrpc", "2.0", "id", int64(1), "method", "onboarding.start",
		"params", pyjson.ObjectOf("facts_source", "local", "locale", "en")))
	if err != nil {
		t.Fatal(err)
	}
	refused := wireReply(t, reply)
	if _, ok := refused["error"]; !ok {
		t.Fatalf("start answered %v", refused)
	}
	raw, err := os.ReadFile(filepath.Join(profileHome, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "onboarding") {
		t.Fatalf("the install flag was written: %s", raw)
	}
	a.mu.Lock()
	live := len(a.Sessions)
	a.mu.Unlock()
	if live != 0 {
		t.Fatalf("%d live sessions", live)
	}
}
