package models

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	githubcopilotconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/config"
	copilotcredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/credentials"
	copilotprovider "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/provider"
	"github.com/nickheyer/icebath/internal/provider"
)

const (
	// Catalog of the account's models
	catalogURL = githubcopilotconfig.CopilotBaseURL + "/models"
	// How long the catalog read for one token is reused
	catalogTTL = 5 * time.Minute
	timeout    = 5 * time.Second
)

// Endpoints of a model that answers chat
var chatEndpoints = map[string]bool{"/chat/completions": true, "/responses": true, "/v1/messages": true}

// The account's live Copilot catalog: its chat models in the order it lists them, and each one's prompt window
type catalog struct {
	mu    sync.Mutex
	rows  []row
	token string
	at    time.Time
}

// One chat model of the catalog: its id, its prompt window, the reasoning efforts its capabilities list, and whether they are listed in the
// structured form or as the older list that may only name reasoning
type row struct {
	id         string
	window     int
	efforts    []string
	structured bool
	reasons    bool
}

func Register(registry *models.Registry) {
	registry.Register(&catalog{})
}

func (c *catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != githubcopilotconfig.CopilotName {
		return nil, false
	}
	rows := c.read(ctx, route.APIKey)
	if len(rows) == 0 {
		return nil, false
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.id
	}
	return ids, true
}

// The account's chat models as its catalog lists them, the curated list left to stand in when the catalog can't be read
func (c *catalog) Catalog(ctx context.Context, env *models.Env) (models.Catalog, bool) {
	ids, ok := c.Models(ctx, env.Route)
	if !ok {
		return models.Catalog{}, false
	}
	return models.Catalog{IDs: ids}, true
}

// Prompt window the catalog states for the route's model
func (c *catalog) ContextLength(ctx context.Context, env *models.Env) (int, bool) {
	if env.Route.Spec != githubcopilotconfig.CopilotName {
		return 0, false
	}
	for _, r := range c.read(ctx, env.Route.APIKey) {
		if r.id == env.Route.Model && r.window > 0 {
			return r.window, true
		}
	}
	return 0, false
}

// Chat models of the catalog read with the token, kept five minutes; asked without the token when it is refused
func (c *catalog) read(ctx context.Context, token string) []row {
	c.mu.Lock()
	if c.rows != nil && c.token == token && time.Since(c.at) < catalogTTL {
		rows := c.rows
		c.mu.Unlock()
		return rows
	}
	c.mu.Unlock()
	var attempts []map[string]string
	if token != "" {
		authorized := copilotcredentials.Headers()
		authorized["Authorization"] = "Bearer " + token
		attempts = append(attempts, authorized)
	}
	attempts = append(attempts, copilotcredentials.Headers())
	for _, headers := range attempts {
		payload, err := models.GetJSON(ctx, catalogURL, timeout, headers)
		if err != nil {
			continue
		}
		items := listed(payload)
		rows := chat(items, false)
		if len(rows) == 0 && len(items) > 0 {
			rows = chat(items, true)
		}
		if len(rows) > 0 {
			c.mu.Lock()
			c.rows, c.token, c.at = rows, token, time.Now()
			c.mu.Unlock()
			return rows
		}
	}
	return nil
}

// Object rows of a catalog payload: its data list, or the payload when it is the list
func listed(payload any) []map[string]any {
	data := payload
	if object, ok := payload.(map[string]any); ok {
		data = object["data"]
	}
	list, _ := data.([]any)
	var items []map[string]any
	for _, item := range list {
		if object, ok := item.(map[string]any); ok {
			items = append(items, object)
		}
	}
	return items
}

// Chat models of the catalog, each once in its order; the picker flag left out when asked
func chat(items []map[string]any, ignorePicker bool) []row {
	var rows []row
	seen := map[string]bool{}
	for _, item := range items {
		id, _ := item["id"].(string)
		if id = strings.TrimSpace(id); id == "" || seen[id] || !answers(item, ignorePicker) {
			continue
		}
		seen[id] = true
		capabilities, structured := item["capabilities"].(map[string]any)
		limits, _ := capabilities["limits"].(map[string]any)
		window, _ := limits["max_prompt_tokens"].(float64)
		r := row{id: id, window: int(window), structured: structured}
		if structured {
			supports, _ := capabilities["supports"].(map[string]any)
			efforts, _ := supports["reasoning_effort"].([]any)
			for _, effort := range efforts {
				if level := strings.ToLower(strings.TrimSpace(models.Scalar(effort))); level != "" && !slices.Contains(r.efforts, level) {
					r.efforts = append(r.efforts, level)
				}
			}
		} else if legacy, ok := item["capabilities"].([]any); ok {
			for _, capability := range legacy {
				r.reasons = r.reasons || strings.EqualFold(strings.TrimSpace(models.Scalar(capability)), "reasoning")
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// Whether a catalog row is a model that answers chat
func answers(item map[string]any, ignorePicker bool) bool {
	if enabled, ok := item["model_picker_enabled"].(bool); !ignorePicker && ok && !enabled {
		return false
	}
	if capabilities, ok := item["capabilities"].(map[string]any); ok {
		if kind, _ := capabilities["type"].(string); kind != "" && strings.ToLower(strings.TrimSpace(kind)) != "chat" {
			return false
		}
	}
	if endpoints, ok := item["supported_endpoints"].([]any); ok {
		named, served := false, false
		for _, endpoint := range endpoints {
			if text, _ := endpoint.(string); strings.TrimSpace(text) != "" {
				named = true
				served = served || chatEndpoints[strings.TrimSpace(text)]
			}
		}
		if named && !served {
			return false
		}
	}
	return true
}

// Reasoning efforts of the route's model: those its catalog row lists, else none for a row whose older list names no reasoning, else the
// efforts its family takes
func (c *catalog) Thinking(ctx context.Context, env *models.Env) (provider.Thinking, bool) {
	if env.Route.Spec != githubcopilotconfig.CopilotName || env.Route.Model == "" {
		return provider.Thinking{}, false
	}
	rows := c.read(ctx, env.Route.APIKey)
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.id
	}
	normalized := copilotprovider.Native(env.Route.Model, ids)
	efforts := copilotprovider.Efforts(env.Route.Model)
	if at := slices.IndexFunc(rows, func(r row) bool { return r.id == normalized }); at >= 0 {
		switch r := rows[at]; {
		case r.structured:
			efforts = r.efforts
		case !r.reasons:
			efforts = nil
		}
	}
	return provider.Thinking{Takes: len(efforts) > 0, Efforts: efforts, Known: true}, true
}
