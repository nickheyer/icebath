// OpenRouter endpoint profile with model listing, attribution headers and provider routing
package openrouter

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"

	_ "github.com/nickheyer/icebath/pkg/protocol/openai-completions"
)

const (
	Vendor  = "openrouter"
	Name    = "openrouter"
	BaseURL = "https://openrouter.ai/api/v1"
	KeyEnv  = "OPENROUTER_API_KEY"
)

// Profile for the Chat Completions compatible API
func Profile() profiles.Profile {
	return profiles.Profile{Name: Name, Vendor: Vendor, Protocol: protocol.OpenAICompletions, BaseURL: BaseURL, Auth: profiles.AuthBearer, Key: profiles.Env(KeyEnv)}
}

func init() {
	profiles.Register(Profile())
	profiles.RegisterVendor(profiles.Vendor{Name: Vendor, Models: Models})
}

type modelList struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Created       int64  `json:"created"`
		ContextLength int    `json:"context_length"`
		TopProvider   *struct {
			MaxCompletionTokens int `json:"max_completion_tokens"`
		} `json:"top_provider"`
	} `json:"data"`
}

// Lists every model the router offers
func Models(ctx context.Context, p *provider.Provider) ([]profiles.Model, error) {
	var list modelList
	if err := profiles.GetJSON(ctx, p, "/models", nil, &list); err != nil {
		return nil, err
	}
	out := make([]profiles.Model, 0, len(list.Data))
	for _, m := range list.Data {
		model := profiles.Model{ID: m.ID, Name: m.Name, Context: m.ContextLength, Created: m.Created}
		if m.TopProvider != nil {
			model.MaxOutput = m.TopProvider.MaxCompletionTokens
		}
		out = append(out, model)
	}
	return out, nil
}

// Headers crediting the calling application on the router leaderboard
func Attribution(referer, title string) http.Header {
	h := http.Header{}
	if referer != "" {
		h.Set("HTTP-Referer", referer)
	}
	if title != "" {
		h.Set("X-Title", title)
	}
	return h
}

// Request extras pinning the upstream providers tried in order
func Providers(order ...string) map[string]json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"order": order, "allow_fallbacks": false})
	return map[string]json.RawMessage{"provider": raw}
}
