package catalog

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth/aws"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	BedrockDefaultContextLength = 128_000
	bedrockDiscoveryTTL         = time.Hour
	bedrockWordsPerToken        = 0.9
	bedrockControlAttempts      = 5
	bedrockDefaultReadTimeout   = 60 * time.Second
	bedrockConnectTimeout       = 60 * time.Second
	bedrockProbeMaxTokens       = 8
)

var bedrockProbeTiers = []int{1_300_000, 2_200_000}

var bedrockContextLengths = []windowRow{
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

var (
	applicationProfileARN = regexp.MustCompile(`:application-inference-profile/`)
	arnRegion             = regexp.MustCompile(`(?i)^arn:[^:]+:bedrock:([a-z0-9-]+):`)
	foundationModelARN    = regexp.MustCompile(`foundation-model/([^.]+)`)
)

type BedrockModel struct {
	ID               string
	Name             string
	Provider         string
	InputModalities  []string
	OutputModalities []string
	Streaming        bool
}

type bedrockDiscovery struct {
	at     time.Time
	models []BedrockModel
}

type bedrockClient struct {
	c        *Catalog
	endpoint string
	service  string
	sign     wire.Authorizer
}

func (c *Catalog) awsEnv(ctx context.Context) aws.Env {
	return func(name string) string {
		value, err := c.secrets.Get(ctx, name, "")
		if err != nil {
			return ""
		}
		return value
	}
}

func (c *Catalog) bedrockRegion(ctx context.Context) string {
	return aws.Region(c.awsEnv(ctx))
}

func (c *Catalog) bedrockClient(ctx context.Context, service, region string) (*bedrockClient, error) {
	env := c.awsEnv(ctx)
	scope, err := aws.Scoped(env, c.secrets.Multiplexed(ctx))
	if err != nil {
		return nil, err
	}
	chain, err := aws.NewChain(aws.Options{Scope: scope, Region: region, HTTP: c.http})
	if err != nil {
		return nil, err
	}
	endpoint, err := chain.Endpoint(service, region)
	if err != nil {
		return nil, err
	}
	return &bedrockClient{c: c, endpoint: strings.TrimRight(endpoint, "/"), service: service, sign: chain.Signer(region, env)}, nil
}

func bedrockFault(service, operation string, resp *reply) error {
	fault := &aws.ServiceError{Service: service, Operation: operation, Status: resp.status, Code: resp.header.Get("X-Amzn-Errortype")}
	if i := strings.IndexByte(fault.Code, ':'); i >= 0 {
		fault.Code = fault.Code[:i]
	}
	if body, err := resp.value(); err == nil {
		fault.Message = body.Get("message").Text()
		if fault.Message == "" {
			fault.Message = body.Get("Message").Text()
		}
		if fault.Code == "" {
			fault.Code = body.Get("__type").Text()
		}
	}
	if fault.Message == "" {
		fault.Message = strings.TrimSpace(string(resp.body))
	}
	return fault
}

func (b *bedrockClient) call(ctx context.Context, operation, method, path string, body []byte, p phases, attempts int) (wire.Value, error) {
	cl, err := b.c.client(ctx, "", p)
	if err != nil {
		return wire.Value{}, err
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(httpclient.Backoff(attempt, time.Second, 20*time.Second, 0.5))
			select {
			case <-ctx.Done():
				timer.Stop()
				return wire.Value{}, ctx.Err()
			case <-timer.C:
			}
		}
		bounded, cancel := context.WithTimeout(ctx, p.total)
		resp, err := send(bounded, cl, method, b.endpoint+path, http.Header{"Accept": {"application/json"}}, body, b.sign)
		cancel()
		if err != nil {
			last = err
			continue
		}
		if resp.success() {
			return resp.value()
		}
		last = bedrockFault(b.service, operation, resp)
		if resp.status != http.StatusTooManyRequests && resp.status < 500 {
			return wire.Value{}, last
		}
	}
	return wire.Value{}, last
}

func bedrockModelEntry(id, name, provider string, input, output []string) BedrockModel {
	if name == "" {
		name = id
	}
	return BedrockModel{ID: id, Name: strings.TrimSpace(name), Provider: provider, InputModalities: input, OutputModalities: output, Streaming: true}
}

func (b *bedrockClient) listFoundationModels(ctx context.Context, filter []string, models []BedrockModel) ([]BedrockModel, error) {
	listing, err := b.call(ctx, "ListFoundationModels", http.MethodGet, "/foundation-models", nil, controlPhases, bedrockControlAttempts)
	if err != nil {
		return models, err
	}
	for _, summary := range listing.Get("modelSummaries").Elems() {
		id := strings.TrimSpace(summary.Get("modelId").Text())
		if id == "" {
			continue
		}
		providerName := summary.Get("providerName").Text()
		prefix := ""
		if head, _, ok := strings.Cut(id, "."); ok {
			prefix = strings.ToLower(head)
		}
		if len(filter) > 0 && !slices.Contains(filter, strings.ToLower(providerName)) && !slices.Contains(filter, prefix) {
			continue
		}
		output := stringList(summary.Get("outputModalities"))
		if strings.ToUpper(summary.Path("modelLifecycle", "status").Text()) != "ACTIVE" ||
			!summary.Get("responseStreamingSupported").Truthy() || !slices.Contains(output, "TEXT") {
			continue
		}
		models = append(models, bedrockModelEntry(id, summary.Get("modelName").Text(), strings.TrimSpace(providerName), stringList(summary.Get("inputModalities")), output))
	}
	return models, nil
}

func (b *bedrockClient) listInferenceProfiles(ctx context.Context, filter []string, models []BedrockModel) ([]BedrockModel, error) {
	var profiles []wire.Value
	for token := ""; ; {
		path := "/inference-profiles"
		if token != "" {
			path += "?" + url.Values{"nextToken": {token}}.Encode()
		}
		page, err := b.call(ctx, "ListInferenceProfiles", http.MethodGet, path, nil, controlPhases, bedrockControlAttempts)
		if err != nil {
			return models, err
		}
		profiles = append(profiles, page.Get("inferenceProfileSummaries").Elems()...)
		token = page.Get("nextToken").Text()
		if token == "" {
			break
		}
	}
	seen := make([]string, 0, len(models))
	for _, m := range models {
		seen = append(seen, strings.ToLower(m.ID))
	}
	for _, profile := range profiles {
		id := strings.TrimSpace(profile.Get("inferenceProfileId").Text())
		if id == "" || profile.Get("status").Text() != "ACTIVE" || slices.Contains(seen, strings.ToLower(id)) {
			continue
		}
		if len(filter) > 0 && !slices.ContainsFunc(profile.Get("models").Elems(), func(m wire.Value) bool {
			return slices.Contains(filter, strings.ToLower(providerFromARN(m.Get("modelArn").Text())))
		}) {
			continue
		}
		models = append(models, bedrockModelEntry(id, profile.Get("inferenceProfileName").Text(), "inference-profile", []string{"TEXT"}, []string{"TEXT"}))
		seen = append(seen, strings.ToLower(id))
	}
	return models, nil
}

func providerFromARN(arn string) string {
	if m := foundationModelARN.FindStringSubmatch(arn); m != nil {
		return m[1]
	}
	return ""
}

func (c *Catalog) DiscoverBedrockModels(ctx context.Context, region string, providerFilter []string) []BedrockModel {
	ctx = c.scoped(ctx)
	sorted := slices.Sorted(slices.Values(providerFilter))
	cacheKey := region + ":" + strings.Join(sorted, ",")
	c.mu.Lock()
	cached, ok := c.discovery[cacheKey]
	c.mu.Unlock()
	if ok && time.Since(cached.at) < bedrockDiscoveryTTL {
		return slices.Clone(cached.models)
	}
	client, err := c.bedrockClient(ctx, "bedrock", region)
	if err != nil {
		slog.Warn("failed to create the Bedrock client for model discovery", "error", err)
		return []BedrockModel{}
	}
	filter := make([]string, 0, len(providerFilter))
	for _, f := range providerFilter {
		filter = append(filter, strings.ToLower(f))
	}
	models := []BedrockModel{}
	if models, err = client.listFoundationModels(ctx, filter, models); err != nil {
		slog.Warn("failed to list Bedrock foundation models", "error", httpclient.Message(err))
	}
	if models, err = client.listInferenceProfiles(ctx, filter, models); err != nil {
		slog.Debug("skipping inference profile discovery", "error", httpclient.Message(err))
	}
	rank := func(m BedrockModel) int {
		if strings.HasPrefix(m.ID, "global.") {
			return 0
		}
		return 1
	}
	slices.SortStableFunc(models, func(a, b BedrockModel) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	c.mu.Lock()
	c.discovery[cacheKey] = bedrockDiscovery{at: time.Now(), models: models}
	c.mu.Unlock()
	return slices.Clone(models)
}

func (c *Catalog) probeBedrockContextLength(ctx context.Context, modelID, region string) (int, bool) {
	client, err := c.bedrockClient(ctx, "bedrock-runtime", region)
	if err != nil {
		slog.Debug("Bedrock context probe skipped", "model", modelID, "error", err)
		return 0, false
	}
	read := bedrockDefaultReadTimeout
	if seconds, ok := c.cfg.ProviderRequestTimeout(ctx, "bedrock", modelID); ok && seconds > 0 {
		read = time.Duration(seconds * float64(time.Second))
	}
	p := phases{connect: bedrockConnectTimeout, read: read, total: read + 2*bedrockConnectTimeout}
	path := "/model/" + strings.ReplaceAll(url.PathEscape(modelID), ":", "%3A") + "/converse"
	lastError := ""
	for _, tier := range bedrockProbeTiers {
		oversized := strings.Repeat("data ", int(float64(tier)/bedrockWordsPerToken))
		body, err := json.Marshal(map[string]any{
			"messages":        []map[string]any{{"role": "user", "content": []map[string]string{{"text": oversized}}}},
			"inferenceConfig": map[string]int{"maxTokens": bedrockProbeMaxTokens},
		})
		if err != nil {
			lastError = err.Error()
			continue
		}
		if _, err := client.call(ctx, "Converse", http.MethodPost, path, body, p, 1); err != nil {
			lastError = httpclient.Message(err)
			if limit, ok := ParseContextLimitFromError(lastError); ok && limit >= 1024 {
				slog.Info("probed the Bedrock context window", "model", modelID, "tokens", groupedInt(limit))
				return limit, true
			}
			continue
		}
		slog.Debug("Bedrock context probe accepted the padded prompt, the window is at least that large", "model", modelID, "tokens", groupedInt(tier))
		return tier, true
	}
	if len(lastError) > 200 {
		lastError = lastError[:200]
	}
	slog.Debug("Bedrock context probe returned no parseable limit", "model", modelID, "error", lastError)
	return 0, false
}

func (c *Catalog) resolveInferenceProfileModelID(ctx context.Context, profileARN, region string) string {
	c.mu.Lock()
	resolved, ok := c.profileARNs[profileARN]
	c.mu.Unlock()
	if ok {
		return resolved
	}
	if m := arnRegion.FindStringSubmatch(profileARN); m != nil && m[1] != "" {
		region = m[1]
	}
	if region == "" {
		region = c.bedrockRegion(ctx)
	}
	resolved = profileARN
	client, err := c.bedrockClient(ctx, "bedrock", region)
	if err == nil {
		var profile wire.Value
		profile, err = client.call(ctx, "GetInferenceProfile", http.MethodGet,
			"/inference-profiles/"+strings.ReplaceAll(url.PathEscape(profileARN), ":", "%3A"), nil, controlPhases, bedrockControlAttempts)
		if err == nil {
			for _, m := range profile.Get("models").Elems() {
				if arn := m.Get("modelArn").Text(); arn != "" {
					resolved = arn
					break
				}
			}
		}
	}
	if err != nil {
		slog.Debug("inference profile resolution skipped", "profile", profileARN, "error", httpclient.Message(err))
	}
	c.mu.Lock()
	c.profileARNs[profileARN] = resolved
	c.mu.Unlock()
	return resolved
}

func (c *Catalog) bedrockContextLength(ctx context.Context, modelID, region string, probe bool) int {
	profileARN := ""
	if applicationProfileARN.MatchString(modelID) {
		profileARN = modelID
		modelID = c.resolveInferenceProfileModelID(ctx, profileARN, region)
	}
	if probe && region != "" {
		if probed, ok := c.probeBedrockContextLength(ctx, modelID, region); ok && probed != 0 {
			return probed
		}
	}
	lower := strings.ToLower(modelID)
	best, found := windowRow{}, false
	for _, row := range bedrockContextLengths {
		if strings.Contains(lower, row.key) && (!found || len(row.key) > len(best.key)) {
			best, found = row, true
		}
	}
	if found {
		return best.window
	}
	if profileARN != "" {
		slog.Warn("Bedrock inference profile resolved no known model window, using the default. Grant bedrock:GetInferenceProfile or set model.context_length explicitly if the wrapped model has a larger window.",
			"profile", profileARN, "tokens", groupedInt(BedrockDefaultContextLength))
	} else if c.once("bedrock-window\x00" + modelID) {
		slog.Warn("Bedrock model has no known context window, using the static fallback (not a detected limit). Set model.context_length in config.yaml if the model's window is larger.",
			"model", modelID, "tokens", groupedInt(BedrockDefaultContextLength))
	}
	return BedrockDefaultContextLength
}
