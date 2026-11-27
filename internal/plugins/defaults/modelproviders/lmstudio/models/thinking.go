package models

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	lmstudioconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// How long options found empty are kept before LM Studio is asked again; options it published are kept
const optionsTTL = time.Minute

type options struct {
	values []string
	at     time.Time
}

// Reasoning options LM Studio publishes for its models, by server and model
type reasoning struct {
	mu   sync.Mutex
	held map[string]options
}

// Answered by the listing registered beside it
func (r *reasoning) Models(context.Context, provider.Route) ([]string, bool) {
	return nil, false
}

// Options the model publishes for reasoning, and whether it takes reasoning at all: an option other than off
func (r *reasoning) Thinking(ctx context.Context, env *models.Env) (provider.Thinking, bool) {
	if env.Route.Spec != lmstudioconfig.Name {
		return provider.Thinking{}, false
	}
	values := r.options(ctx, env.Route)
	thinking := provider.Thinking{Efforts: values, Known: true}
	for _, option := range values {
		thinking.Takes = thinking.Takes || option != "" && option != "off"
	}
	return thinking, true
}

// Reasoning options of the route's model from LM Studio's listing, empty when it publishes none or can't be read
func (r *reasoning) options(ctx context.Context, route provider.Route) []string {
	key := route.Model + "\x00" + route.BaseURL
	r.mu.Lock()
	held, ok := r.held[key]
	r.mu.Unlock()
	if ok && (len(held.values) > 0 || time.Since(held.at) < optionsTTL) {
		return held.values
	}
	values := []string{}
	if root := models.LMStudioRoot(route.BaseURL); root != "" {
		headers := map[string]string{"Accept": "application/json", "User-Agent": provider.UserAgent("icebath-cli")}
		if token := strings.TrimSpace(route.APIKey); token != "" {
			headers["Authorization"] = "Bearer " + token
		}
		if data, err := models.GetJSON(ctx, root+"/api/v1/models", 5*time.Second, headers); err == nil {
			values = published(data, route.Model)
		}
	}
	r.mu.Lock()
	r.held[key] = options{values, time.Now()}
	r.mu.Unlock()
	return values
}

// Reasoning options a listing gives the model, in lower case
func published(data any, model string) []string {
	document, _ := data.(map[string]any)
	entries, ok := document["data"].([]any)
	if !ok {
		entries, _ = document["models"].([]any)
	}
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if key, _ := entry["key"].(string); key != model {
			if id, _ := entry["id"].(string); id != model {
				continue
			}
		}
		capabilities, _ := entry["capabilities"].(map[string]any)
		reasoning, _ := capabilities["reasoning"].(map[string]any)
		allowed, _ := reasoning["allowed_options"].([]any)
		values := []string{}
		for _, option := range allowed {
			if text, ok := option.(string); ok {
				values = append(values, strings.ToLower(strings.TrimSpace(text)))
			}
		}
		return values
	}
	return []string{}
}
