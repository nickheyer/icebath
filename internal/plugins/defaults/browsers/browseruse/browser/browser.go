// Browser Use's cloud browsers, on the user's own key
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/config"
	buconfig "github.com/nickheyer/icebath/internal/plugins/defaults/browsers/browseruse/config"
)

const (
	name  = "browser-use"
	title = "Browser Use"
	base  = "https://api.browser-use.com/api/v3"
)

func Register(registry *browser.Registry) {
	registry.Register(browser.Declared{Services: []browser.Provider{{
		Name: name, Title: title, About: "Cloud browsers run by Browser Use", Section: buconfig.Key,
		Keys:      []browser.Key{{Env: buconfig.APIKeyEnv, Prompt: "Browser Use API key", URL: "https://browser-use.com", Setting: buconfig.APIKey}},
		Detect:    1,
		Available: func(cfg *config.Config) bool { return key(cfg) != "" },
		Create:    create,
		Close:     closeSession,
	}}})
}

func key(cfg *config.Config) string { return strings.TrimSpace(cfg.GetString(buconfig.APIKey)) }

func headers(apiKey string) map[string]string {
	return map[string]string{"Content-Type": "application/json", "X-Browser-Use-API-Key": apiKey}
}

// Opens a browser in Browser Use's cloud
func create(ctx context.Context, cfg *config.Config, task string) (*browser.Session, error) {
	apiKey := key(cfg)
	if apiKey == "" {
		if chosen, ok := browser.Chosen(cfg); ok && chosen == name {
			return nil, errors.New("browser is configured to use 'browser-use', but BROWSER_USE_API_KEY is not set. Pick another one in /settings → " +
				"Browser.")
		}
		return nil, errors.New("Browser Use requires a direct BROWSER_USE_API_KEY credential.")
	}
	reply, err := browser.Call(ctx, http.MethodPost, base+"/browsers", headers(apiKey), map[string]any{}, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%s API connection failed: %w", title, err)
	}
	if !reply.OK() {
		return nil, fmt.Errorf("Failed to create %s session: %d %s", title, reply.Status, reply.Body)
	}
	var data struct {
		ID         string `json:"id"`
		CDPURL     string `json:"cdpUrl"`
		ConnectURL string `json:"connectUrl"`
		TimeoutAt  string `json:"timeoutAt"`
	}
	if err := json.Unmarshal(reply.Body, &data); err != nil {
		return nil, fmt.Errorf("%s answered what is no session: %w", title, err)
	}
	if data.ID == "" {
		return nil, errors.New(title + " answered a session without its 'id'")
	}
	s := &browser.Session{ID: data.ID, Name: browser.SessionName(task), CDP: data.CDPURL, Features: map[string]bool{"browser_use": true}}
	if s.CDP == "" {
		s.CDP = data.ConnectURL
	}
	if data.TimeoutAt != "" {
		if at, err := time.Parse(time.RFC3339Nano, strings.Replace(data.TimeoutAt, "z", "Z", 1)); err == nil {
			s.Expires = at
		} else {
			slog.Warn("Ignoring invalid cloud browser session expiry timestamp")
		}
	}
	slog.Info("Created " + title + " session " + s.Name)
	return s, nil
}

func closeSession(ctx context.Context, cfg *config.Config, id string) bool {
	apiKey := key(cfg)
	if apiKey == "" {
		slog.Warn(fmt.Sprintf("Cannot close %s session %s — missing credentials", title, id))
		return false
	}
	return browser.Release(ctx, title, http.MethodPatch, base+"/browsers/"+id, id, headers(apiKey), map[string]any{"action": "stop"})
}
