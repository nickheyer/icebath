// Anthropic endpoint profile with model listing, token counting and thinking options
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"

	_ "github.com/nickheyer/icebath/pkg/protocol/anthropic-messages"
)

const (
	Vendor  = "anthropic"
	Name    = "anthropic"
	BaseURL = "https://api.anthropic.com"
	KeyEnv  = "ANTHROPIC_API_KEY"
	// Environment variable overriding the base URL
	BaseURLEnv = "ANTHROPIC_BASE_URL"
	// Model used when none is chosen
	DefaultModel = "claude-opus-5"
	version      = "2023-06-01"
)

// Profile for the public API authenticated with an API key header
func Profile() profiles.Profile {
	return profiles.Profile{Name: Name, Vendor: Vendor, Protocol: protocol.AnthropicMessages, BaseURL: BaseURL, Auth: profiles.AuthHeader, AuthName: "x-api-key", Key: profiles.Env(KeyEnv), Models: []string{DefaultModel}, BaseURLEnv: BaseURLEnv}
}

func init() {
	profiles.Register(Profile())
	profiles.RegisterVendor(profiles.Vendor{Name: Vendor, Models: Models})
}

type modelPage struct {
	Data []struct {
		ID          string    `json:"id"`
		DisplayName string    `json:"display_name"`
		CreatedAt   time.Time `json:"created_at"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
}

func header() http.Header {
	return http.Header{"Accept": {"application/json"}, "anthropic-version": {version}}
}

// Lists every model the endpoint serves, following pagination
func Models(ctx context.Context, p *provider.Provider) ([]profiles.Model, error) {
	var out []profiles.Model
	after := ""
	for {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		var page modelPage
		if err := profiles.DoJSON(ctx, p, protocol.Call{Method: http.MethodGet, Path: "/v1/models", Query: q, Header: header()}, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Data {
			model := profiles.Model{ID: m.ID, Name: m.DisplayName}
			if !m.CreatedAt.IsZero() {
				model.Created = m.CreatedAt.Unix()
			}
			out = append(out, model)
		}
		if !page.HasMore || page.LastID == "" || page.LastID == after {
			return out, nil
		}
		after = page.LastID
	}
}

// Request extras enabling extended thinking with a token budget
func Thinking(budget int) map[string]json.RawMessage {
	return map[string]json.RawMessage{"thinking": json.RawMessage(fmt.Sprintf(`{"type":"enabled","budget_tokens":%d}`, budget))}
}

// Header opting into the named beta features
func Beta(features ...string) http.Header {
	h := http.Header{}
	h.Set("anthropic-beta", strings.Join(features, ","))
	return h
}

// Fields the token counting endpoint does not accept
var countStripped = []string{"max_tokens", "stream", "temperature", "stop_sequences", "top_p", "top_k", "metadata"}

// Counts the input tokens the request would cost
func CountTokens(ctx context.Context, p *provider.Provider, req protocol.Request) (int, error) {
	req.Stream = false
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1
	}
	call, err := p.Protocol.Encode(req)
	if err != nil {
		return 0, err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(call.Body, &body); err != nil {
		return 0, err
	}
	for _, k := range countStripped {
		delete(body, k)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	call.Path = "/v1/messages/count_tokens"
	call.Body = raw
	var res struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := profiles.DoJSON(ctx, p, call, &res); err != nil {
		return 0, err
	}
	return res.InputTokens, nil
}
