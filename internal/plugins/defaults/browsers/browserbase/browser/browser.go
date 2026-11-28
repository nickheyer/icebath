// Browserbase's cloud browsers, with stealth, proxies and keep-alive sessions the plan allows
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
	bbconfig "github.com/nickheyer/icebath/internal/plugins/defaults/browsers/browserbase/config"
)

const title = "Browserbase"

func Register(registry *browser.Registry) {
	registry.Register(browser.Declared{Services: []browser.Provider{{
		Name: "browserbase", Title: title, About: "Cloud browsers that sites find hard to spot, with proxies", Section: bbconfig.Key,
		Keys: []browser.Key{{Env: bbconfig.APIKeyEnv, Prompt: "Browserbase API key", URL: "https://browserbase.com", Setting: bbconfig.APIKey},
			{Env: bbconfig.ProjectIDEnv, Prompt: "Browserbase project ID", Setting: bbconfig.ProjectID}},
		Detect: 2,
		Available: func(cfg *config.Config) bool {
			_, _, _, ok := account(cfg)
			return ok
		},
		Create: create,
		Close:  closeSession,
	}}})
}

// Key, project and API address of the profile, false without the key and the project
func account(cfg *config.Config) (key, project, base string, ok bool) {
	key, project = strings.TrimSpace(cfg.GetString(bbconfig.APIKey)), strings.TrimSpace(cfg.GetString(bbconfig.ProjectID))
	base = strings.TrimRight(cmp.Or(strings.TrimSpace(cfg.GetString(bbconfig.BaseURL)), "https://api.browserbase.com"), "/")
	return key, project, base, key != "" && project != ""
}

func headers(key string) map[string]string {
	return map[string]string{"Content-Type": "application/json", "X-BB-API-Key": key}
}

// Paid features dropped in turn on a 402, each with the warning it logs
var paid = [][2]string{
	{"keepAlive", "keepAlive may require paid plan (402), retrying without it. Sessions may timeout during long operations."},
	{"proxies", "Proxies unavailable (402), retrying without proxies. Bot detection may be less effective."},
}

// Opens a session with the features the profile asks for, again without each paid one a 402 refuses
func create(ctx context.Context, cfg *config.Config, task string) (*browser.Session, error) {
	key, project, base, ok := account(cfg)
	if !ok {
		return nil, errors.New("Browserbase requires BROWSERBASE_API_KEY and BROWSERBASE_PROJECT_ID environment variables.")
	}
	proxies, stealth, keepAlive := cfg.GetBool(bbconfig.Proxies), cfg.GetBool(bbconfig.AdvancedStealth), cfg.GetBool(bbconfig.KeepAlive)
	body := map[string]any{"projectId": project}
	if keepAlive {
		body["keepAlive"] = true
	}
	limit, _ := strconv.ParseFloat(strings.TrimSpace(cfg.GetString(bbconfig.SessionTimeout)), 64)
	if int(limit) > 0 {
		body["timeout"] = int(limit)
	}
	if proxies {
		body["proxies"] = true
	}
	if stealth {
		body["browserSettings"] = map[string]any{"advancedStealth": true}
	}
	post := func() (*browser.Reply, error) {
		reply, err := browser.Call(ctx, http.MethodPost, base+"/v1/sessions", headers(key), body, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("%s API connection failed: %w", title, err)
		}
		return reply, nil
	}
	reply, err := post()
	if err != nil {
		return nil, err
	}
	dropped := map[string]bool{}
	for _, feature := range paid {
		if _, asked := body[feature[0]]; reply.Status == 402 && asked {
			dropped[feature[0]] = true
			slog.Warn(feature[1])
			delete(body, feature[0])
			if reply, err = post(); err != nil {
				return nil, err
			}
		}
	}
	if !reply.OK() {
		return nil, fmt.Errorf("Failed to create %s session: %d %s", title, reply.Status, reply.Body)
	}
	var data struct {
		ID         string `json:"id"`
		ConnectURL string `json:"connectUrl"`
	}
	if err := json.Unmarshal(reply.Body, &data); err != nil {
		return nil, fmt.Errorf("%s answered what is no session: %w", title, err)
	}
	if data.ID == "" || data.ConnectURL == "" {
		return nil, errors.New(title + " answered a session without its 'id' or 'connectUrl'")
	}
	_, timed := body["timeout"]
	features := map[string]bool{"basic_stealth": true, "proxies": proxies && !dropped["proxies"], "advanced_stealth": stealth,
		"keep_alive": keepAlive && !dropped["keepAlive"], "custom_timeout": timed}
	var on []string
	for _, k := range []string{"basic_stealth", "proxies", "advanced_stealth", "keep_alive", "custom_timeout"} {
		if features[k] {
			on = append(on, k)
		}
	}
	s := &browser.Session{ID: data.ID, Name: browser.SessionName(task), CDP: data.ConnectURL, Features: features}
	slog.Info(fmt.Sprintf("Created Browserbase session %s with features: %s", s.Name, strings.Join(on, ", ")))
	return s, nil
}

func closeSession(ctx context.Context, cfg *config.Config, id string) bool {
	key, project, base, ok := account(cfg)
	if !ok {
		slog.Warn(fmt.Sprintf("Cannot close %s session %s — missing credentials", title, id))
		return false
	}
	return browser.Release(ctx, title, http.MethodPost, base+"/v1/sessions/"+id, id, headers(key), map[string]any{"projectId": project,
		"status": "REQUEST_RELEASE"})
}
