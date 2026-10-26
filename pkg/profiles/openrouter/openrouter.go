// OpenRouter endpoint profile with model listing, attribution headers and provider routing
package openrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

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
	// Environment variable overriding the base URL
	BaseURLEnv = "OPENROUTER_BASE_URL"
	// Model used when none is chosen
	DefaultModel = "anthropic/claude-sonnet-4.5"
)

// Profile for the Chat Completions compatible API
func Profile() profiles.Profile {
	return profiles.Profile{Name: Name, Vendor: Vendor, Protocol: protocol.OpenAICompletions, BaseURL: BaseURL, Auth: profiles.AuthBearer, Key: profiles.Env(KeyEnv), Models: []string{DefaultModel}, BaseURLEnv: BaseURLEnv, Ghost: Unretained()}
}

// Provider routing that only reaches endpoints keeping no prompts and training on nothing
func Unretained() *provider.Directive {
	return &provider.Directive{Extra: map[string]json.RawMessage{"provider": json.RawMessage(`{"zdr":true,"data_collection":"deny"}`)}}
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
		Pricing *struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
			CacheRead  string `json:"input_cache_read"`
			CacheWrite string `json:"input_cache_write"`
		} `json:"pricing"`
		Architecture *struct {
			InputModalities []string `json:"input_modalities"`
		} `json:"architecture"`
		SupportedParameters []string `json:"supported_parameters"`
	} `json:"data"`
}

// Dollars per million tokens from a per token rate string the router reports
func perMillion(rate string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(rate), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f * 1e6
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Lists every model the router offers with its pricing and what it accepts
func Models(ctx context.Context, p *provider.Provider) ([]profiles.Model, error) {
	var list modelList
	if err := profiles.GetJSON(ctx, p, "/models", nil, &list); err != nil {
		return nil, err
	}
	out := make([]profiles.Model, 0, len(list.Data))
	for _, m := range list.Data {
		model := profiles.Model{ID: m.ID, Name: m.Name, Vendor: Vendor, Context: m.ContextLength, Created: m.Created}
		if m.TopProvider != nil {
			model.MaxOutput = m.TopProvider.MaxCompletionTokens
		}
		if m.Pricing != nil {
			pr := profiles.Pricing{Input: perMillion(m.Pricing.Prompt), Output: perMillion(m.Pricing.Completion), CacheRead: perMillion(m.Pricing.CacheRead), CacheWrite: perMillion(m.Pricing.CacheWrite)}
			model.Pricing = &pr
		}
		if m.Architecture != nil || len(m.SupportedParameters) > 0 {
			caps := profiles.Capabilities{Tools: has(m.SupportedParameters, "tools"), Thinking: has(m.SupportedParameters, "reasoning") || has(m.SupportedParameters, "include_reasoning")}
			if m.Architecture != nil {
				caps.Vision = has(m.Architecture.InputModalities, "image")
				caps.Documents = has(m.Architecture.InputModalities, "file")
			}
			model.Capabilities = &caps
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
