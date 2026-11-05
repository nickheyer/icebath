package mcp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
	"gopkg.in/yaml.v3"
)

// Messaging chat answering every approval card it is sent with one choice
type scopeChat struct {
	env   *tools.Env
	key   string
	mu    sync.Mutex
	cards []map[string]any
}

func openScopeChat(t *testing.T, home, sessionKey, choice string) *scopeChat {
	t.Helper()
	c := &scopeChat{env: sessionEnv(map[string]any{"platform": "telegram", "key": sessionKey}), key: sessionKey}
	c.env.Home = home
	tools.RegisterGatewayNotify(sessionKey, func(data map[string]any) error {
		c.mu.Lock()
		c.cards = append(c.cards, data)
		c.mu.Unlock()
		tools.ResolveGatewayApproval(sessionKey, choice, false, "", data["request_id"].(string))
		return nil
	})
	t.Cleanup(func() { tools.UnregisterGatewayNotify(sessionKey) })
	return c
}

func (c *scopeChat) gate(t *testing.T, toolName string) string {
	t.Helper()
	var booked []*Charge
	return trustGateCheck(envContext(c.env), c.env, "srv", toolName, &booked, nil, "")
}

func (c *scopeChat) seen() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.cards)
}

// command_allowlist entries persisted to the isolated home config
func configAllowlist(t *testing.T, home string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		CommandAllowlist []string `yaml:"command_allowlist"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.CommandAllowlist
}

func TestTrustScopeSessionAndAlwaysStopNextPromptForThatToolOnly(t *testing.T) {
	for _, choice := range []string{"session", "always"} {
		t.Run(choice, func(t *testing.T) {
			home := isolateHome(t)
			resetHandlerLedgers(t)
			t.Cleanup(func() { tools.ClearSession("tg:chat-a-" + choice); tools.ClearSession("tg:chat-b-" + choice) })
			serverTrustLevels[ServerKey{Name: "srv"}] = TrustUntrusted
			chat := openScopeChat(t, home, "tg:chat-a-"+choice, choice)
			for _, tool := range []string{"write", "write", "other"} {
				if got := chat.gate(t, tool); got != "" {
					t.Fatalf("%s blocked %s", tool, got)
				}
			}
			cards := chat.seen()
			if len(cards) != 2 || cards[0]["allow_session"] != true || cards[0]["allow_permanent"] != true {
				t.Fatalf("cards %v", cards)
			}
			other := openScopeChat(t, home, "tg:chat-b-"+choice, "deny")
			blocked := other.gate(t, "write")
			allowlist := configAllowlist(t, home)
			if choice == "session" {
				if blocked == "" || len(other.seen()) != 1 || slices.Contains(allowlist, "mcp:srv/write") {
					t.Fatalf("blocked %q cards %d allowlist %v", blocked, len(other.seen()), allowlist)
				}
				return
			}
			if blocked != "" || len(other.seen()) != 0 || !slices.Contains(allowlist, "mcp:srv/write") {
				t.Fatalf("blocked %q cards %d allowlist %v", blocked, len(other.seen()), allowlist)
			}
		})
	}
}

func TestTrustScopeConsentWithoutIdentityOffersAndRecordsOnlyOnce(t *testing.T) {
	home := isolateHome(t)
	chat := openScopeChat(t, home, "tg:chat-c", "session")
	if got := requestTrustConsent(context.Background(), chat.env, "Use the camera?", "server asks", tools.ElicitationOptions{}); got != "accept" {
		t.Fatalf("gateway consent %q", got)
	}
	cards := chat.seen()
	if len(cards) != 1 || cards[0]["allow_session"] != false || cards[0]["allow_permanent"] != false {
		t.Fatalf("cards %v", cards)
	}
	if tools.IsApproved(chat.env, "tg:chat-c", "mcp_elicitation") {
		t.Fatal("gateway session answer was remembered")
	}
	env := &tools.Env{Home: home, Values: &sync.Map{}}
	asker := &recordingAsker{answer: "always"}
	env.Ask = asker
	if got := requestTrustConsent(context.Background(), env, "Fill card?", "pay", tools.ElicitationOptions{}); got != "accept" {
		t.Fatalf("cli consent %q", got)
	}
	if len(asker.asked) != 1 || !reflect.DeepEqual(asker.asked[0][0].Choices, []string{"once", "deny"}) {
		t.Fatalf("cli asked %v", asker.asked)
	}
	if tools.IsApproved(env, "default", "mcp_elicitation") || len(tools.LoadPermanentAllowlist(env)) != 0 || len(configAllowlist(t, home)) != 0 {
		t.Fatal("stale broader answer was remembered")
	}
}
