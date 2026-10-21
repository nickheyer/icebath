// Gemini endpoint profile with model listing and thinking knobs
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"

	_ "github.com/nickheyer/icebath/pkg/protocol/google-generative-ai"
)

const (
	Vendor  = "google"
	Name    = "google"
	BaseURL = "https://generativelanguage.googleapis.com"
	KeyEnv  = "GEMINI_API_KEY"
	// Environment variable overriding the base URL
	BaseURLEnv = "GEMINI_BASE_URL"
	// Model used when none is chosen
	DefaultModel = "gemini-2.5-pro"
)

// Profile for the Gemini API authenticated with an API key header
func Profile() profiles.Profile {
	return profiles.Profile{Name: Name, Vendor: Vendor, Protocol: protocol.GoogleGenerativeAI, BaseURL: BaseURL, Auth: profiles.AuthHeader, AuthName: "x-goog-api-key", Key: profiles.Env(KeyEnv), Models: []string{DefaultModel}, BaseURLEnv: BaseURLEnv}
}

func init() {
	profiles.Register(Profile())
	profiles.RegisterVendor(profiles.Vendor{Name: Vendor, Models: Models})
}

type modelPage struct {
	Models []struct {
		Name             string   `json:"name"`
		DisplayName      string   `json:"displayName"`
		InputTokenLimit  int      `json:"inputTokenLimit"`
		OutputTokenLimit int      `json:"outputTokenLimit"`
		Methods          []string `json:"supportedGenerationMethods"`
	} `json:"models"`
	NextPageToken string `json:"nextPageToken"`
}

// Lists the models that support generateContent, following pagination
func Models(ctx context.Context, p *provider.Provider) ([]profiles.Model, error) {
	var out []profiles.Model
	token := ""
	for {
		q := url.Values{"pageSize": {"1000"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page modelPage
		if err := profiles.GetJSON(ctx, p, "/v1beta/models", q, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Models {
			if !supports(m.Methods, "generateContent") {
				continue
			}
			out = append(out, profiles.Model{ID: strings.TrimPrefix(m.Name, "models/"), Name: m.DisplayName, Context: m.InputTokenLimit, MaxOutput: m.OutputTokenLimit})
		}
		if page.NextPageToken == "" || page.NextPageToken == token {
			return out, nil
		}
		token = page.NextPageToken
	}
}

func supports(methods []string, want string) bool {
	for _, m := range methods {
		if m == want {
			return true
		}
	}
	return false
}

// Request extras enabling thinking with a token budget and thought summaries in the response
func Thinking(budget int) map[string]json.RawMessage {
	return map[string]json.RawMessage{"generationConfig": json.RawMessage(fmt.Sprintf(`{"thinkingConfig":{"thinkingBudget":%d,"includeThoughts":true}}`, budget))}
}
