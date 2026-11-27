// Firecrawl's cloud browsers on /v2/browser, chosen by name only
package browser

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/config"
	fcconfig "github.com/nickheyer/icebath/internal/plugins/defaults/browsers/firecrawl/config"
)

const title = "Firecrawl"

func Register(registry *browser.Registry) {
	registry.Register(browser.Declared{Services: []browser.Provider{{
		Name: "firecrawl", Title: title, About: "Cloud browser with remote execution.",
		Keys:      []browser.Key{{Env: fcconfig.APIKeyEnv, Prompt: "Firecrawl API key", URL: "https://firecrawl.dev"}},
		Available: func(cfg *config.Config) bool { return key(cfg) != "" },
		Create:    create,
		Close:     closeSession,
	}}})
}

func key(cfg *config.Config) string { return strings.TrimSpace(cfg.Variable(fcconfig.APIKeyEnv)) }

func base(cfg *config.Config) string {
	return strings.TrimRight(cmp.Or(strings.TrimSpace(cfg.Variable(fcconfig.APIURLEnv)), "https://api.firecrawl.dev"), "/")
}

func headers(cfg *config.Config) (map[string]string, error) {
	k := key(cfg)
	if k == "" {
		return nil, errors.New("FIRECRAWL_API_KEY environment variable is required. Get your key at https://firecrawl.dev")
	}
	return map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + k}, nil
}

// Opens a browser that lives for browser.firecrawl.ttl seconds
func create(ctx context.Context, cfg *config.Config, task string) (*browser.Session, error) {
	ttl, err := strconv.ParseFloat(strings.TrimSpace(cfg.GetString(fcconfig.TTL)), 64)
	if err != nil {
		ttl = 300
	}
	head, err := headers(cfg)
	if err != nil {
		return nil, err
	}
	reply, err := browser.Call(ctx, http.MethodPost, base(cfg)+"/v2/browser", head, map[string]any{"ttl": max(int(ttl), 1)}, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%s API connection failed: %w", title, err)
	}
	if !reply.OK() {
		return nil, fmt.Errorf("Failed to create %s browser session: %d %s", title, reply.Status, reply.Body)
	}
	var data struct {
		ID     string `json:"id"`
		CDPURL string `json:"cdpUrl"`
	}
	if err := json.Unmarshal(reply.Body, &data); err != nil {
		return nil, fmt.Errorf("%s answered what is no session: %w", title, err)
	}
	if data.ID == "" || data.CDPURL == "" {
		return nil, errors.New(title + " answered a session without its 'id' or 'cdpUrl'")
	}
	s := &browser.Session{ID: data.ID, Name: browser.SessionName(task), CDP: data.CDPURL, Features: map[string]bool{"firecrawl": true}}
	slog.Info("Created Firecrawl browser session " + s.Name)
	return s, nil
}

func closeSession(ctx context.Context, cfg *config.Config, id string) bool {
	head, err := headers(cfg)
	if err != nil {
		slog.Warn(fmt.Sprintf("Cannot close %s session %s — missing credentials", title, id))
		return false
	}
	return browser.Release(ctx, title, http.MethodDelete, base(cfg)+"/v2/browser/"+id, id, head, nil)
}
