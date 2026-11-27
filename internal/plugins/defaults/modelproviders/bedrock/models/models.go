package models

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/credentials/aws"
	"github.com/nickheyer/icebath/internal/models"
	bedrockconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/config"
	bedrockprovider "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/provider"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Windows of the model families Bedrock serves, the longest name the model holds winning
var windows = []struct {
	key    string
	window int
}{
	{"xai.grok-4.6", 500_000},
	{"anthropic.claude-fable-5", 1_000_000}, {"anthropic.claude-fable", 1_000_000}, {"anthropic.claude-sonnet-5", 1_000_000},
	{"anthropic.claude-opus-5", 1_000_000}, {"anthropic.claude-opus-4-8", 1_000_000}, {"anthropic.claude-opus-4-7", 1_000_000},
	{"anthropic.claude-opus-4-6", 1_000_000}, {"anthropic.claude-sonnet-4-6", 1_000_000},
	{"anthropic.claude-sonnet-4-5", 200_000}, {"anthropic.claude-haiku-4-5", 200_000}, {"anthropic.claude-opus-4", 200_000},
	{"anthropic.claude-sonnet-4", 200_000}, {"anthropic.claude-3-5-sonnet", 200_000}, {"anthropic.claude-3-5-haiku", 200_000},
	{"anthropic.claude-3-opus", 200_000}, {"anthropic.claude-3-sonnet", 200_000}, {"anthropic.claude-3-haiku", 200_000},
	{"amazon.nova-pro", 300_000}, {"amazon.nova-lite", 300_000}, {"amazon.nova-micro", 128_000},
	{"meta.llama4-maverick", 128_000}, {"meta.llama4-scout", 128_000}, {"meta.llama3-3-70b-instruct", 128_000},
	{"mistral.mistral-large", 128_000}, {"deepseek.v3", 128_000},
	{"openai.gpt-6.1-sol", 1_000_000},
	{"openai.gpt-6-astra", 1_050_000}, {"openai.gpt-6-sol", 1_050_000}, {"openai.gpt-6-luna", 1_050_000},
	{"openai.gpt-5.5", 1_050_000}, {"openai.gpt-5.6-sol", 1_050_000}, {"openai.gpt-5.6-terra", 1_050_000}, {"openai.gpt-5.6-luna", 1_050_000},
}

// Window of a Bedrock model no source knows
const fallbackWindow = 128_000

// Prompt sizes the live probe sends, in tokens, and the words a token is taken to hold
var probeTiers = []int{1_300_000, 2_200_000}

const wordsPerToken = 0.9

// How long a probe that found no window is not tried again
const probeCooldown = 5 * time.Minute

// Bedrock's models, live and by their windows: the foundation models and inference profiles its control plane lists with the models only
// Mantle serves, and each model's window as a live probe finds it, else its family's
type catalog struct {
	profiles aws.Profiles
	mu       sync.Mutex
	failed   map[string]time.Time
	warned   map[string]bool
}

func Register(registry *models.Registry) {
	registry.Register(&catalog{failed: map[string]time.Time{}, warned: map[string]bool{}})
}

// Region a route goes to: its credential's, else the one its address names
func region(route provider.Route) string {
	return cmp.Or(route.Region, aws.EndpointRegion(route.BaseURL), "us-east-1")
}

func bearer(route provider.Route) string {
	if route.Sign != nil {
		return ""
	}
	return strings.TrimSpace(route.APIKey)
}

var client = &http.Client{Timeout: 30 * time.Second}

type foundation struct {
	ModelID                    string   `json:"modelId"`
	ModelName                  string   `json:"modelName"`
	ProviderName               string   `json:"providerName"`
	OutputModalities           []string `json:"outputModalities"`
	ResponseStreamingSupported bool     `json:"responseStreamingSupported"`
	ModelLifecycle             struct {
		Status string `json:"status"`
	} `json:"modelLifecycle"`
}

type profile struct {
	InferenceProfileID   string `json:"inferenceProfileId"`
	InferenceProfileName string `json:"inferenceProfileName"`
	Status               string `json:"status"`
}

// Models the region serves: active foundation models that stream text, then the active inference profiles not among them, global ones
// first and the rest by name, with the OpenAI models Mantle serves that the control plane never lists
func (c *catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != bedrockconfig.Name || route.Sign == nil && bearer(route) == "" {
		return nil, false
	}
	base := aws.ControlURL(region(route))
	type entry struct{ id, name string }
	var found []entry
	var listing struct {
		ModelSummaries []foundation `json:"modelSummaries"`
	}
	if err := aws.Call(ctx, client, "ListFoundationModels", http.MethodGet, base+"/foundation-models", nil, route.Sign, bearer(route), &listing); err != nil {
		slog.Warn(fmt.Sprintf("Failed to list Bedrock foundation models: %v", err))
	}
	seen := map[string]bool{}
	for _, s := range listing.ModelSummaries {
		id := strings.TrimSpace(s.ModelID)
		if id == "" || !strings.EqualFold(s.ModelLifecycle.Status, "ACTIVE") || !s.ResponseStreamingSupported || !slices.Contains(s.OutputModalities, "TEXT") {
			continue
		}
		found = append(found, entry{id, strs.Filled(s.ModelName, id)})
		seen[strings.ToLower(id)] = true
	}
	next := ""
	for {
		address := base + "/inference-profiles"
		if next != "" {
			address += "?" + url.Values{"nextToken": {next}}.Encode()
		}
		var page struct {
			Summaries []profile `json:"inferenceProfileSummaries"`
			NextToken string    `json:"nextToken"`
		}
		if err := aws.Call(ctx, client, "ListInferenceProfiles", http.MethodGet, address, nil, route.Sign, bearer(route), &page); err != nil {
			slog.Debug(fmt.Sprintf("Skipping inference profile discovery: %v", err))
			break
		}
		for _, p := range page.Summaries {
			id := strings.TrimSpace(p.InferenceProfileID)
			if id == "" || p.Status != "ACTIVE" || seen[strings.ToLower(id)] {
				continue
			}
			found = append(found, entry{id, strs.Filled(p.InferenceProfileName, id)})
			seen[strings.ToLower(id)] = true
		}
		if next = page.NextToken; next == "" {
			break
		}
	}
	if len(found) == 0 {
		return nil, false
	}
	slices.SortStableFunc(found, func(a, b entry) int {
		ga, gb := strings.HasPrefix(a.id, "global."), strings.HasPrefix(b.id, "global.")
		switch {
		case ga && !gb:
			return -1
		case gb && !ga:
			return 1
		}
		return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
	})
	ids := make([]string, 0, len(found)+len(bedrockprovider.MantleModels))
	for _, e := range found {
		ids = append(ids, e.id)
	}
	for _, id := range bedrockprovider.MantleModels {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	return ids, true
}

var profileModel = regexp.MustCompile(`:application-inference-profile/`)

// Window of the route's model: the one a probe found before, else a live probe of the region, which only an oversized prompt makes Bedrock
// state, once every five minutes at most for a model it gave none for; else its family's, the fallback with a warning for one of none
func (c *catalog) ContextLength(ctx context.Context, env *models.Env) (int, bool) {
	route := env.Route
	if route.Spec != bedrockconfig.Name || route.Mode == provider.ModeCodexResponses {
		return 0, false
	}
	model, key := route.Model, strings.TrimRight(route.BaseURL, "/")
	if key == "" {
		key = "bedrock://"
	}
	if n, ok := env.Host.CachedContext(env.Config, model, key); ok && n > 0 && !env.Refresh {
		return n, true
	}
	r := region(route)
	resolved := c.profiles.Resolve(ctx, client, model, r, route.Sign, bearer(route))
	memo := key + "\x00" + model + "\x00" + r
	c.mu.Lock()
	for k, at := range c.failed {
		if time.Since(at) >= probeCooldown {
			delete(c.failed, k)
		}
	}
	_, cooling := c.failed[memo]
	c.mu.Unlock()
	if !cooling && route.BaseURL != "" && (route.Sign != nil || bearer(route) != "") {
		if n := c.probe(ctx, route, resolved); n > 0 {
			env.Host.SaveContext(env.Config, model, key, n)
			return n, true
		}
		c.mu.Lock()
		c.failed[memo] = time.Now()
		c.mu.Unlock()
	}
	return c.static(model, resolved), true
}

// Window Bedrock states when it refuses an oversized prompt, or the size of a prompt it took; 0 when it said nothing that tells
func (c *catalog) probe(ctx context.Context, route provider.Route, model string) int {
	last := ""
	for _, tier := range probeTiers {
		prompt := strings.Repeat("data ", int(float64(tier)/wordsPerToken))
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"text": prompt}}}},
			"inferenceConfig": map[string]any{"maxTokens": 8}}
		err := aws.Call(ctx, &http.Client{Timeout: 5 * time.Minute}, "Converse", http.MethodPost, strings.TrimRight(route.BaseURL, "/")+
			aws.ModelPath(model, "converse"), body, route.Sign, bearer(route), nil)
		if err == nil {
			slog.Debug(fmt.Sprintf("Bedrock context probe for %s accepted ~%s-token prompt; window is at least that", model, strs.Thousands(tier)))
			return tier
		}
		last = err.Error()
		if limit, ok := models.ContextLimit(last); ok && limit >= 1024 {
			slog.Info(fmt.Sprintf("Probed Bedrock context window for %s: %s tokens", model, strs.Thousands(limit)))
			return limit
		}
	}
	slog.Debug(fmt.Sprintf("Bedrock context probe for %s returned no parseable limit: %s", model, strs.Head(last, 200)))
	return 0
}

// Window of the model's family, the fallback with a warning once for a model of none
func (c *catalog) static(model, resolved string) int {
	lower := strings.ToLower(resolved)
	best, length := 0, -1
	for _, row := range windows {
		if strings.Contains(lower, row.key) && len(row.key) > length {
			best, length = row.window, len(row.key)
		}
	}
	if length >= 0 {
		return best
	}
	if profileModel.MatchString(model) {
		slog.Warn(fmt.Sprintf("Bedrock inference profile %s resolved no known model window; using the %s default. Grant bedrock:GetInferenceProfile "+
			"or set the provider's context_length if the wrapped model has a larger window.", model, strs.Thousands(fallbackWindow)))
		return fallbackWindow
	}
	c.mu.Lock()
	first := !c.warned[model]
	c.warned[model] = true
	c.mu.Unlock()
	if first {
		slog.Warn(fmt.Sprintf("Bedrock model %s has no known context window; using the static %s-token fallback (not a detected limit). Set the "+
			"provider's context_length if the model's window is larger.", model, strs.Thousands(fallbackWindow)))
	}
	return fallbackWindow
}
