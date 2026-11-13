package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Loopback Discord gateway and REST API serving login, the gateway URL and a READY session
type fakeGateway struct {
	srv        *httptest.Server
	mu         sync.Mutex
	closeWith  int
	identifies int
	conns      []*websocket.Conn
	rest       []string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	f := &fakeGateway{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGateway) serve(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		f.socket(w, r)
		return
	}
	io.Copy(io.Discard, r.Body)
	path := strings.TrimPrefix(r.URL.Path, "/api/v"+discordgo.APIVersion)
	f.mu.Lock()
	f.rest = append(f.rest, r.Method+" "+path)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case path == "/gateway":
		io.WriteString(w, `{"url":"ws://`+r.Host+`"}`)
	case path == "/users/@me":
		io.WriteString(w, `{"id":"999","username":"Hermes","bot":true}`)
	case strings.HasPrefix(path, "/applications/") && r.Method == http.MethodGet:
		io.WriteString(w, `[]`)
	case strings.HasPrefix(path, "/applications/"):
		io.WriteString(w, `{}`)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// Gateway socket that sends HELLO, answers IDENTIFY with READY or the scripted close, and ACKs heartbeats
func (f *fakeGateway) socket(w http.ResponseWriter, r *http.Request) {
	c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conns = append(f.conns, c)
	f.mu.Unlock()
	defer c.Close()
	c.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 45000}})
	seq := 0
	for {
		var frame struct {
			Op int             `json:"op"`
			D  json.RawMessage `json:"d"`
		}
		if err := c.ReadJSON(&frame); err != nil {
			return
		}
		switch frame.Op {
		case 1:
			c.WriteJSON(map[string]any{"op": 11})
		case 2, 6:
			f.mu.Lock()
			f.identifies++
			closeWith := f.closeWith
			f.mu.Unlock()
			if closeWith != 0 {
				c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(closeWith, "scripted"))
				return
			}
			seq++
			c.WriteJSON(map[string]any{"op": 0, "t": "READY", "s": seq, "d": map[string]any{"v": 10, "session_id": "s1",
				"user": map[string]any{"id": "999", "username": "Hermes", "bot": true}, "guilds": []any{},
				"application": map[string]any{"id": "999"}, "resume_gateway_url": "ws://" + r.Host}})
		}
	}
}

// Points the REST client of a link at the fake, keeping the rate limit cap layer
func (f *fakeGateway) attach(l *gatewayLink) {
	target, _ := url.Parse(f.srv.URL)
	l.rateCap.base = rewriteTransport{target: target}
}

// Replaces the bot factory so every connect builds a real link wired to the fake, recording each one
func (f *fakeGateway) installBot(t *testing.T) *[]*gatewayLink {
	var created []*gatewayLink
	var mu sync.Mutex
	orig := commandsBot
	commandsBot = func(token string, intents discordgo.Intent, mentions *discordgo.MessageAllowedMentions, proxy gateway.ProxyKwargs) (*gatewayLink, error) {
		l, err := orig(token, intents, mentions, proxy)
		if err != nil {
			return nil, err
		}
		f.attach(l)
		mu.Lock()
		created = append(created, l)
		mu.Unlock()
		return l, nil
	}
	t.Cleanup(func() { commandsBot = orig })
	return &created
}

// Command registry seams returning nothing, the empty catalog of a fresh home
func emptySlashRegistry(t *testing.T) {
	reg, plug, skills := gateway.GatewayRegistryCommands, gateway.PluginSlashCommandEntries, gateway.DiscordSkillCommandsByCategory
	gateway.GatewayRegistryCommands = func(context.Context) ([]gateway.SlashCommandEntry, error) { return nil, nil }
	gateway.PluginSlashCommandEntries = func(context.Context) ([]gateway.SlashCommandEntry, error) { return nil, nil }
	gateway.DiscordSkillCommandsByCategory = func(context.Context, map[string]bool) ([][]gateway.SkillCommandEntry, []gateway.SkillCommandEntry, int, error) {
		return nil, nil, 0, nil
	}
	t.Cleanup(func() {
		gateway.GatewayRegistryCommands, gateway.PluginSlashCommandEntries, gateway.DiscordSkillCommandsByCategory = reg, plug, skills
	})
}

// Real adapter for a token, the way hermes builds DiscordAdapter(PlatformConfig(...))
func newConnectAdapter(t *testing.T, extra map[string]any) *DiscordAdapter {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(func() { gateway.FlushRuntimeStatus(5 * time.Second) })
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled, cfg.Token = true, "test-token"
	if extra != nil {
		cfg.Extra = extra
	}
	a, err := NewDiscordAdapter(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
