package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/provider/sigv4"

	_ "github.com/nickheyer/icebath/pkg/protocol/anthropic-messages"
	_ "github.com/nickheyer/icebath/pkg/protocol/bedrock-converse-stream"
	_ "github.com/nickheyer/icebath/pkg/protocol/google-generative-ai"
	_ "github.com/nickheyer/icebath/pkg/protocol/mistral-conversations"
	_ "github.com/nickheyer/icebath/pkg/protocol/ollama-chat"
	_ "github.com/nickheyer/icebath/pkg/protocol/openai-completions"
	_ "github.com/nickheyer/icebath/pkg/protocol/openai-responses"
)

// Auth kind signing requests with AWS credentials
const AuthSigV4 AuthKind = "aws-sigv4"

// Registers the built in endpoint profiles and the vendors that list their models
func init() {
	Register(Profile{Name: "anthropic", Vendor: "anthropic", Protocol: protocol.AnthropicMessages, BaseURL: "https://api.anthropic.com", Auth: AuthHeader, AuthName: "x-api-key", Key: Env("ANTHROPIC_API_KEY"), Models: []string{"claude-opus-5"}, BaseURLEnv: "ANTHROPIC_BASE_URL"})
	RegisterVendor(Vendor{Name: "anthropic", Models: anthropicModels})
	RegisterAuth(AuthSigV4, bedrockAuth)
	Register(bedrockProfile(sigv4.Region()))
	RegisterVendor(Vendor{Name: "bedrock", Models: bedrockModels})
	Register(Profile{Name: "google", Vendor: "google", Protocol: protocol.GoogleGenerativeAI, BaseURL: "https://generativelanguage.googleapis.com", Auth: AuthHeader, AuthName: "x-goog-api-key", Key: Env("GEMINI_API_KEY"), Models: []string{"gemini-2.5-pro"}, BaseURLEnv: "GEMINI_BASE_URL"})
	RegisterVendor(Vendor{Name: "google", Models: googleModels})
	Register(Profile{Name: "nebu", Vendor: "openai", Protocol: protocol.OpenAICompletions, BaseURL: "http://127.0.0.1:8484/v1", Auth: AuthBearer, Key: Env("NEBU_AUTH_TOKEN"), BaseURLEnv: "NEBU_BASE_URL", GhostDirective: headerDirective("X-Nebu-Ghost"), BareDirective: headerDirective("X-Nebu-Bare")})
	Register(Profile{Name: "mistral", Vendor: "mistral", Protocol: protocol.MistralConversations, BaseURL: "https://api.mistral.ai", Auth: AuthBearer, Key: Env("MISTRAL_API_KEY"), Models: []string{"mistral-large-latest"}, BaseURLEnv: "MISTRAL_BASE_URL"})
	RegisterVendor(Vendor{Name: "mistral", Models: mistralModels})
	Register(ollamaProfile(os.Getenv("OLLAMA_HOST")))
	RegisterVendor(Vendor{Name: "ollama", Models: ollamaModels})
	Register(Profile{Name: "openai", Vendor: "openai", Protocol: protocol.OpenAIResponses, BaseURL: "https://api.openai.com/v1", Auth: AuthBearer, Key: Env("OPENAI_API_KEY"), Models: []string{"gpt-5"}, BaseURLEnv: "OPENAI_BASE_URL", GhostDirective: unstored()})
	Register(Profile{Name: "openai-chat", Vendor: "openai", Protocol: protocol.OpenAICompletions, BaseURL: "https://api.openai.com/v1", Auth: AuthBearer, Key: Env("OPENAI_API_KEY"), Models: []string{"gpt-5"}, BaseURLEnv: "OPENAI_BASE_URL", GhostDirective: unstored()})
	RegisterVendor(Vendor{Name: "openai", Models: openaiModels})
	Register(Profile{Name: "openrouter", Vendor: "openrouter", Protocol: protocol.OpenAICompletions, BaseURL: "https://openrouter.ai/api/v1", Auth: AuthBearer, Key: Env("OPENROUTER_API_KEY"), Models: []string{"anthropic/claude-sonnet-4.5"}, BaseURLEnv: "OPENROUTER_BASE_URL", GhostDirective: unretained()})
	RegisterVendor(Vendor{Name: "openrouter", Models: openrouterModels})
}

// Directive heard as the header set to 1
func headerDirective(name string) *provider.Directive {
	return &provider.Directive{Header: http.Header{name: {"1"}}}
}

// Body field asking OpenAI to keep no stored copy of the response
func unstored() *provider.Directive {
	return &provider.Directive{Extra: map[string]json.RawMessage{"store": json.RawMessage("false")}}
}

// Provider routing that only reaches OpenRouter endpoints keeping no prompts and training on nothing
func unretained() *provider.Directive {
	return &provider.Directive{Extra: map[string]json.RawMessage{"provider": json.RawMessage(`{"zdr":true,"data_collection":"deny"}`)}}
}

type anthropicModelPage struct {
	Data []struct {
		ID          string    `json:"id"`
		DisplayName string    `json:"display_name"`
		CreatedAt   time.Time `json:"created_at"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
}

// Lists every model the Anthropic endpoint serves, following pagination
func anthropicModels(ctx context.Context, p *provider.Provider) ([]Model, error) {
	var out []Model
	after := ""
	for {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		var page anthropicModelPage
		header := http.Header{"Accept": {"application/json"}, "anthropic-version": {"2023-06-01"}}
		if err := DoJSON(ctx, p, protocol.Call{Method: http.MethodGet, Path: "/v1/models", Query: q, Header: header}, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Data {
			model := Model{ID: m.ID, Name: m.DisplayName}
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

// Profile for the Bedrock runtime API in a region, the default region when the value does not form a usable host
func bedrockProfile(region string) Profile {
	if region == "" {
		region = "us-east-1"
	}
	p := Profile{Name: "bedrock", Vendor: "bedrock", Protocol: protocol.BedrockConverseStream, BaseURL: "https://bedrock-runtime." + region + ".amazonaws.com", Auth: AuthSigV4, Options: map[string]string{"region": region}, Models: []string{"us.anthropic.claude-sonnet-4-5-20250929-v1:0"}, BaseURLEnv: "BEDROCK_BASE_URL"}
	if err := p.Validate(); err != nil || bedrockRegionOf(p.BaseURL) != region {
		return bedrockProfile("us-east-1")
	}
	return p
}

// Builds the signer from the profile options, the base URL and a key holding access, secret and optional token joined by colons
func bedrockAuth(p Profile, key string) (provider.Auth, error) {
	region := p.Options["region"]
	if region == "" {
		region = bedrockRegionOf(p.BaseURL)
	}
	if region == "" {
		region = sigv4.Region()
	}
	if region == "" {
		return nil, fmt.Errorf("profile %s: region is required, set options.region or AWS_REGION", p.Name)
	}
	service := p.Options["service"]
	if service == "" {
		service = "bedrock"
	}
	var source sigv4.Source
	switch {
	case p.Options["profile"] != "":
		source = sigv4.Profile{Name: p.Options["profile"]}
	case key != "":
		parts := strings.SplitN(key, ":", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("profile %s: key must look like ACCESS:SECRET or ACCESS:SECRET:TOKEN", p.Name)
		}
		creds := sigv4.Credentials{AccessKey: parts[0], SecretKey: parts[1]}
		if len(parts) == 3 {
			creds.SessionToken = parts[2]
		}
		source = sigv4.Static(creds)
	default:
		source = sigv4.Default()
	}
	return &sigv4.Auth{Region: region, Service: service, Source: source}, nil
}

// Reads the region out of a regional Bedrock host name
func bedrockRegionOf(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(u.Hostname(), ".")
	if len(parts) >= 4 && (parts[0] == "bedrock-runtime" || parts[0] == "bedrock") && strings.HasSuffix(u.Hostname(), ".amazonaws.com") {
		return parts[1]
	}
	return ""
}

// Control plane provider for the signing region on AWS hosts, the same host elsewhere
func bedrockControlPlane(p *provider.Provider) (*provider.Provider, error) {
	if p == nil || p.Protocol == nil {
		return nil, errors.New("bedrock: provider is required")
	}
	control := *p
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return nil, err
	}
	if host := strings.ToLower(u.Hostname()); host == "amazonaws.com" || strings.HasSuffix(host, ".amazonaws.com") {
		region := bedrockRegionOf(p.BaseURL)
		if a, ok := p.Auth.(*sigv4.Auth); ok && a.Region != "" {
			region = a.Region
		}
		if region == "" {
			region = sigv4.Region()
		}
		if region == "" {
			return nil, fmt.Errorf("bedrock: no region known for %s, set options.region or AWS_REGION", p.BaseURL)
		}
		control.BaseURL = "https://bedrock." + region + ".amazonaws.com"
	}
	return &control, nil
}

type bedrockSummaries struct {
	ModelSummaries []struct {
		ModelID                    string   `json:"modelId"`
		ModelName                  string   `json:"modelName"`
		ProviderName               string   `json:"providerName"`
		OutputModalities           []string `json:"outputModalities"`
		ResponseStreamingSupported bool     `json:"responseStreamingSupported"`
	} `json:"modelSummaries"`
}

type bedrockProfilePage struct {
	Summaries []struct {
		ID     string `json:"inferenceProfileId"`
		Name   string `json:"inferenceProfileName"`
		Status string `json:"status"`
	} `json:"inferenceProfileSummaries"`
	NextToken string `json:"nextToken"`
}

// Lists streaming text models and active cross region inference profiles from the Bedrock control plane
func bedrockModels(ctx context.Context, p *provider.Provider) ([]Model, error) {
	control, err := bedrockControlPlane(p)
	if err != nil {
		return nil, err
	}
	var list bedrockSummaries
	if err := GetJSON(ctx, control, "/foundation-models", nil, &list); err != nil {
		return nil, err
	}
	var out []Model
	for _, m := range list.ModelSummaries {
		if !m.ResponseStreamingSupported || !slices.Contains(m.OutputModalities, "TEXT") {
			continue
		}
		out = append(out, Model{ID: m.ModelID, Name: strings.TrimSpace(m.ProviderName + " " + m.ModelName)})
	}
	token := ""
	for {
		q := url.Values{"maxResults": {"1000"}}
		if token != "" {
			q.Set("nextToken", token)
		}
		var page bedrockProfilePage
		if err := GetJSON(ctx, control, "/inference-profiles", q, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Summaries {
			if s.Status == "" || s.Status == "ACTIVE" {
				out = append(out, Model{ID: s.ID, Name: strings.TrimSpace(s.Name)})
			}
		}
		if page.NextToken == "" || page.NextToken == token {
			return out, nil
		}
		token = page.NextToken
	}
}

type googleModelPage struct {
	Models []struct {
		Name             string   `json:"name"`
		DisplayName      string   `json:"displayName"`
		InputTokenLimit  int      `json:"inputTokenLimit"`
		OutputTokenLimit int      `json:"outputTokenLimit"`
		Methods          []string `json:"supportedGenerationMethods"`
	} `json:"models"`
	NextPageToken string `json:"nextPageToken"`
}

// Lists the Gemini models that support generateContent, following pagination
func googleModels(ctx context.Context, p *provider.Provider) ([]Model, error) {
	var out []Model
	token := ""
	for {
		q := url.Values{"pageSize": {"1000"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page googleModelPage
		if err := GetJSON(ctx, p, "/v1beta/models", q, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Models {
			if slices.Contains(m.Methods, "generateContent") {
				out = append(out, Model{ID: strings.TrimPrefix(m.Name, "models/"), Name: m.DisplayName, Context: m.InputTokenLimit, MaxOutput: m.OutputTokenLimit})
			}
		}
		if page.NextPageToken == "" || page.NextPageToken == token {
			return out, nil
		}
		token = page.NextPageToken
	}
}

type mistralModelList struct {
	Data []struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Created          int64  `json:"created"`
		MaxContextLength int    `json:"max_context_length"`
		Capabilities     *struct {
			CompletionChat bool `json:"completion_chat"`
		} `json:"capabilities"`
	} `json:"data"`
}

// Lists the Mistral models that can chat
func mistralModels(ctx context.Context, p *provider.Provider) ([]Model, error) {
	var list mistralModelList
	if err := GetJSON(ctx, p, "/v1/models", nil, &list); err != nil {
		return nil, err
	}
	var out []Model
	for _, m := range list.Data {
		if m.Capabilities != nil && !m.Capabilities.CompletionChat {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		out = append(out, Model{ID: m.ID, Name: name, Context: m.MaxContextLength, Created: m.Created})
	}
	return out, nil
}

// Profile for an Ollama server at an OLLAMA_HOST style value, the local default when it is empty or unusable
func ollamaProfile(host string) Profile {
	host = strings.TrimSuffix(strings.TrimSpace(host), "/")
	if host == "" {
		host = "http://localhost:11434"
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	p := Profile{Name: "ollama", Vendor: "ollama", Protocol: protocol.OllamaChat, BaseURL: host, Auth: AuthNone, Models: []string{"llama3.2"}}
	if err := p.Validate(); err != nil {
		return ollamaProfile("")
	}
	return p
}

type ollamaTagList struct {
	Models []struct {
		Name       string    `json:"name"`
		ModifiedAt time.Time `json:"modified_at"`
		Details    *struct {
			Family        string `json:"family"`
			ParameterSize string `json:"parameter_size"`
		} `json:"details"`
	} `json:"models"`
}

// Lists the models pulled onto the Ollama server
func ollamaModels(ctx context.Context, p *provider.Provider) ([]Model, error) {
	var list ollamaTagList
	if err := GetJSON(ctx, p, "/api/tags", nil, &list); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(list.Models))
	for _, m := range list.Models {
		model := Model{ID: m.Name, Name: m.Name}
		if m.Details != nil && m.Details.Family != "" {
			model.Name = strings.TrimSpace(m.Details.Family + " " + m.Details.ParameterSize)
		}
		if !m.ModifiedAt.IsZero() {
			model.Created = m.ModifiedAt.Unix()
		}
		out = append(out, model)
	}
	return out, nil
}

type openaiModelList struct {
	Data []struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
	} `json:"data"`
}

// Lists every model the OpenAI endpoint serves
func openaiModels(ctx context.Context, p *provider.Provider) ([]Model, error) {
	var list openaiModelList
	if err := GetJSON(ctx, p, "/models", nil, &list); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(list.Data))
	for _, m := range list.Data {
		out = append(out, Model{ID: m.ID, Name: m.ID, Created: m.Created})
	}
	return out, nil
}

type openrouterModelList struct {
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

// Lists every model OpenRouter offers with its pricing and what it accepts
func openrouterModels(ctx context.Context, p *provider.Provider) ([]Model, error) {
	var list openrouterModelList
	if err := GetJSON(ctx, p, "/models", nil, &list); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(list.Data))
	for _, m := range list.Data {
		model := Model{ID: m.ID, Name: m.Name, Vendor: "openrouter", Context: m.ContextLength, Created: m.Created}
		if m.TopProvider != nil {
			model.MaxOutput = m.TopProvider.MaxCompletionTokens
		}
		if m.Pricing != nil {
			pr := Pricing{Input: perMillion(m.Pricing.Prompt), Output: perMillion(m.Pricing.Completion), CacheRead: perMillion(m.Pricing.CacheRead), CacheWrite: perMillion(m.Pricing.CacheWrite)}
			model.Pricing = &pr
		}
		if m.Architecture != nil || len(m.SupportedParameters) > 0 {
			caps := Capabilities{Tools: slices.Contains(m.SupportedParameters, "tools"), Thinking: slices.Contains(m.SupportedParameters, "reasoning") || slices.Contains(m.SupportedParameters, "include_reasoning")}
			if m.Architecture != nil {
				caps.Vision = slices.Contains(m.Architecture.InputModalities, "image")
				caps.Documents = slices.Contains(m.Architecture.InputModalities, "file")
			}
			model.Capabilities = &caps
		}
		out = append(out, model)
	}
	return out, nil
}
