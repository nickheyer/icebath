package server_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	agentserver "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/server"
	sqliteconfig "github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite/config"
	sqlitesessions "github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite/sessions"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

type specs struct{}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{Name: "acme", BaseURL: "https://acme.test/v1", DefaultModel: "a1", Mode: provider.ModeChatCompletions, Auth: provider.AuthAPIKey}}
}

// Wire that answers every request once as many are waiting as it expects, so the test only passes when sessions reply at the same time
type wire struct {
	expect int
	mu     sync.Mutex
	seen   map[string]provider.Route
	all    chan struct{}
	done   chan string
}

func (w *wire) Factories() map[provider.APIMode]provider.Factory {
	return map[provider.APIMode]provider.Factory{provider.ModeChatCompletions: func(route provider.Route) (provider.Client, error) {
		return client{wire: w, route: route}, nil
	}}
}

type client struct {
	wire  *wire
	route provider.Route
}

func (c client) Route() provider.Route { return c.route }

func (c client) Stream(ctx context.Context, request *provider.Request, _ provider.StreamFunc) (*provider.Response, error) {
	w := c.wire
	w.mu.Lock()
	w.seen[request.SessionID] = c.route
	if len(w.seen) == w.expect {
		close(w.all)
	}
	w.mu.Unlock()
	select {
	case <-w.all:
	case <-time.After(5 * time.Second):
		return nil, context.DeadlineExceeded
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { w.done <- request.SessionID }()
	return &provider.Response{Message: &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: "ok"}, FinishReason: "stop"}, nil
}

func TestSessionsOnProfilesAtOnce(t *testing.T) {
	dir := t.TempDir()
	cfg := config.New(dir)
	host := sessions.New(cfg)
	sqliteconfig.Register(cfg)
	sqlitesessions.Register(&host.Registry, cfg)
	providers := provider.New(cfg)
	providers.Register(specs{})
	creds := credentials.New()
	creds.Register(credentials.Keyed{Provider: "acme"})
	wires := transport.New(cfg)
	toolset := tools.New(cfg)
	agentconfig.Register(cfg, providers.Options(cfg), toolset.Risks)
	agent := agentserver.Register(&server.New(cfg).Registry, cfg, &providers.Registry, &wires.Registry, &creds.Registry, &models.New().Registry, &toolset.Registry,
		&host.Registry, &slash.New().Registry)
	if err := cfg.Use("default"); err != nil {
		t.Fatal(err)
	}

	// Three sessions on two profiles, each profile with its own key and the work profile with its own model
	keys := map[string]string{"default": "key-default", "work": "key-work"}
	for profile, key := range keys {
		if err := os.MkdirAll(cfg.ProfileDir(profile), 0o700); err != nil {
			t.Fatal(err)
		}
		settings := cfg.In(profile)
		if _, err := providers.Add(settings, "acme", "", ""); err != nil {
			t.Fatal(err)
		}
		account, err := providers.AddAccount(settings, "acme", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := settings.Set("providers.acme.accounts."+account+"."+providerconfig.APIKeyField, key); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.In("work").Set(agentconfig.Model, "w1"); err != nil {
		t.Fatal(err)
	}
	store, err := host.Current()
	if err != nil {
		t.Fatal(err)
	}
	profileOf := map[string]string{}
	for _, profile := range []string{"default", "work", "work"} {
		session, err := store.Create(dir, profile)
		if err != nil {
			t.Fatal(err)
		}
		profileOf[session.GetId()] = profile
	}

	w := &wire{expect: len(profileOf), seen: map[string]provider.Route{}, all: make(chan struct{}), done: make(chan string, len(profileOf))}
	wires.Register(w)
	for id := range profileOf {
		if err := agent.Send(context.Background(), id, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	for range profileOf {
		select {
		case <-w.done:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d sessions were sent at once", len(w.seen), len(profileOf))
		}
	}
	for id, profile := range profileOf {
		route := w.seen[id]
		want := map[string]string{"default": "a1", "work": "w1"}[profile]
		if route.APIKey != keys[profile] || route.Model != want {
			t.Errorf("session on %s used key %q and model %q", profile, route.APIKey, route.Model)
		}
	}

	// A session moved to another profile uses it from then on, the profile in use left as it is
	var moved string
	for id, profile := range profileOf {
		if profile == "default" {
			moved = id
		}
	}
	if _, err := agent.Profile(moved, "work"); err != nil {
		t.Fatal(err)
	}
	if session, _ := store.Get(moved); session.GetProfile() != "work" || cfg.Profile() != "default" {
		t.Fatalf("moved session is on %q, profile in use %q", session.GetProfile(), cfg.Profile())
	}
	if _, err := agent.Profile(moved, "nowhere"); err == nil {
		t.Fatal("moved to a profile that isn't there")
	}
	if err := agent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
}
