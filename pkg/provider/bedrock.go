package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

var bedrockLog = slog.Default()

// ---- routed profile credentials ----

// Session variables a routed profile may set for its own AWS identity
var awsScopedCredentialVars = []struct{ field, env string }{
	{"aws_access_key_id", "AWS_ACCESS_KEY_ID"}, {"aws_secret_access_key", "AWS_SECRET_ACCESS_KEY"},
	{"aws_session_token", "AWS_SESSION_TOKEN"}, {"profile_name", "AWS_PROFILE"},
}

// BedrockScopedCredentialError refuses a multiplexed profile that would borrow the launch identity
type BedrockScopedCredentialError struct{}

func (BedrockScopedCredentialError) Error() string {
	return "Bedrock auth is refused for this profile: it sets no complete AWS credential of its " +
		"own, and under multiplexed profiles the ambient default chain (process env, ~/.aws, " +
		"instance metadata) would sign this profile's calls with the LAUNCH context's identity. " +
		"Set AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY in this profile's own config, or " +
		"AWS_PROFILE to name a shared AWS config profile."
}

// Session options from the routed profile's secret scope, empty when no home override is active
func scopedAWSSessionOptions(ctx context.Context) (awsSessionOptions, error) {
	if home.Override(ctx) == "" {
		return awsSessionOptions{}, nil
	}
	scope := CurrentSecretScope(ctx)
	got := map[string]string{}
	for _, v := range awsScopedCredentialVars {
		if val := strings.TrimSpace(scope[v.env]); val != "" {
			got[v.field] = val
		}
	}
	complete := (got["aws_access_key_id"] != "" && got["aws_secret_access_key"] != "") || got["profile_name"] != ""
	if !complete && IsMultiplexActive(ctx) {
		return awsSessionOptions{}, BedrockScopedCredentialError{}
	}
	return awsSessionOptions{AccessKeyID: got["aws_access_key_id"], SecretAccessKey: got["aws_secret_access_key"],
		SessionToken: got["aws_session_token"], Profile: got["profile_name"]}, nil
}

// ---- client cache ----

// Cache slot of one AWS client, home empty for the unscoped launch context
type bedrockClientKey struct {
	home        string
	service     string
	region      string
	readTimeout time.Duration
}

var (
	bedrockClientsMu sync.Mutex
	bedrockClients   = map[bedrockClientKey]*awsServiceClient{}
)

// Configured request timeout for a Bedrock model, model override over the provider value
func bedrockRequestTimeout(ctx context.Context, model string) (time.Duration, bool) {
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		return 0, false
	}
	providers, _ := map[string]any(tree)["providers"].(map[string]any)
	return configuredProviderTimeout(providers, "bedrock", model, "timeout_seconds", "request_timeout_seconds")
}

// Cached client per home, service, region and read timeout, built from the routed profile's own credentials
func bedrockCachedClient(ctx context.Context, service, region string, readTimeout time.Duration) (*awsServiceClient, error) {
	key := bedrockClientKey{service: service, region: region, readTimeout: readTimeout}
	if o := home.Override(ctx); o != "" {
		key.home = home.Key(o)
	}
	bedrockClientsMu.Lock()
	defer bedrockClientsMu.Unlock()
	if c, ok := bedrockClients[key]; ok {
		return c, nil
	}
	opts, err := scopedAWSSessionOptions(ctx)
	if err != nil {
		return nil, err
	}
	attempts := 0
	if service == "bedrock-runtime" {
		attempts = 1
	}
	c := newAWSServiceClient(awsClientConfig{service: service, signingName: "bedrock", region: region, session: opts,
		bearer: resolveBedrockBearerTokenCtx(ctx), readTimeout: readTimeout, totalMaxAttempts: attempts})
	bedrockClients[key] = c
	return c, nil
}

// Runtime client whose read timeout is the model's configured request timeout
func bedrockRuntimeClient(ctx context.Context, region, model string) (*awsServiceClient, error) {
	timeout, _ := bedrockRequestTimeout(ctx, model)
	return bedrockCachedClient(ctx, "bedrock-runtime", region, timeout)
}

// Control plane client with botocore default timeouts and retries
func bedrockControlClient(ctx context.Context, region string) (*awsServiceClient, error) {
	return bedrockCachedClient(ctx, "bedrock", region, 0)
}

// ResetBedrockClientCache drops every cached client, profile resolution and fallback warning
func ResetBedrockClientCache() {
	bedrockClientsMu.Lock()
	for k, c := range bedrockClients {
		c.closeIdle()
		delete(bedrockClients, k)
	}
	bedrockClientsMu.Unlock()
	bedrockProfileMu.Lock()
	bedrockProfileCache = map[string]string{}
	bedrockProfileMu.Unlock()
	bedrockWarnedMu.Lock()
	bedrockWarnedDefault = map[string]bool{}
	bedrockWarnedMu.Unlock()
}

// InvalidateBedrockRuntimeClient evicts every read timeout slot of one region's runtime clients
func InvalidateBedrockRuntimeClient(ctx context.Context, region string) bool {
	homeKey := ""
	if o := home.Override(ctx); o != "" {
		homeKey = home.Key(o)
	}
	bedrockClientsMu.Lock()
	defer bedrockClientsMu.Unlock()
	evicted := false
	for k, c := range bedrockClients {
		if k.home == homeKey && k.service == "bedrock-runtime" && k.region == region {
			c.closeIdle()
			delete(bedrockClients, k)
			evicted = true
		}
	}
	return evicted
}

// ---- Bedrock Mantle OpenAI Responses ----

// Geo and global cross region inference profile prefixes
var bedrockProfilePrefixes = []string{"global", "us", "eu", "apac", "ap", "au", "jp", "ca", "sa", "me", "af"}

// OpenAI profile prefixes, the GPT 5.6 cards also document India
var bedrockOpenAIProfilePrefixes = append(append([]string(nil), bedrockProfilePrefixes...), "in")

// BedrockOpenAIResponsesModelIDs are the bare in region OpenAI ids Mantle serves
var BedrockOpenAIResponsesModelIDs = []string{
	"openai.gpt-6-astra", "openai.gpt-6.1-sol", "openai.gpt-6-sol", "openai.gpt-6-luna",
	"openai.gpt-5.5", "openai.gpt-5.6-sol", "openai.gpt-5.6-terra", "openai.gpt-5.6-luna",
}

var bedrockOpenAIHostRe = regexp.MustCompile(`(?i)^bedrock-mantle\.([a-z0-9-]+)\.api\.aws$`)

// Bare id and profile prefix of a Bedrock hosted OpenAI model, false for any other model
func parseBedrockOpenAIModelID(model string) (string, string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(model))
	prefix, rest, sep := strings.Cut(normalized, ".")
	if sep && containsString(bedrockOpenAIProfilePrefixes, prefix) && strings.HasPrefix(rest, "openai.") {
		normalized = rest
	} else {
		prefix = ""
	}
	for _, id := range BedrockOpenAIResponsesModelIDs {
		if strings.ToLower(id) == normalized {
			return normalized, prefix, true
		}
	}
	return "", "", false
}

// Reports a Bedrock hosted OpenAI GPT model in any endpoint form
func isOpenAIBedrockModel(model string) bool {
	_, _, ok := parseBedrockOpenAIModelID(model)
	return ok
}

// Reports a bare in region OpenAI id that Mantle's Responses endpoint serves
func bedrockOpenAIUsesMantle(model string) bool {
	_, prefix, ok := parseBedrockOpenAIModelID(model)
	return ok && prefix == ""
}

// Appends the Mantle only OpenAI models control plane discovery never lists
func mergeBedrockOpenAIModelIDs(ids []string) []string {
	merged := append([]string(nil), ids...)
	seen := map[string]bool{}
	for _, id := range merged {
		seen[strings.ToLower(id)] = true
	}
	for _, id := range BedrockOpenAIResponsesModelIDs {
		if !seen[strings.ToLower(id)] {
			merged = append(merged, id)
		}
	}
	return merged
}

// Mantle OpenAI compatible base URL for the region, the runtime region when empty
func bedrockOpenAIBaseURL(region string) string {
	resolved := strings.TrimSpace(region)
	if resolved == "" {
		resolved = resolveBedrockRuntimeRegion(nil)
	}
	return "https://bedrock-mantle." + resolved + ".api.aws/openai/v1"
}

// Region of a Mantle host and the lowered path without its trailing slash
func bedrockMantleURLParts(baseURL string) (string, bool, string) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", false, ""
	}
	path := strings.ToLower(strings.TrimRight(u.Path, "/"))
	m := bedrockOpenAIHostRe.FindStringSubmatch(u.Hostname())
	if m == nil {
		return "", false, path
	}
	return m[1], true, path
}

// Region of a Mantle OpenAI base URL, false for other hosts
func bedrockOpenAIRegionFromBaseURL(baseURL string) (string, bool) {
	region, ok, _ := bedrockMantleURLParts(baseURL)
	return region, ok
}

// Reports a Mantle endpoint, bare host or the openai and openai v1 paths
func isBedrockOpenAIBaseURL(baseURL string) bool {
	_, ok, path := bedrockMantleURLParts(baseURL)
	return ok && (path == "" || path == "/openai" || path == "/openai/v1")
}

// Bedrock API key from the process environment
func resolveBedrockBearerToken() string {
	return strings.TrimSpace(os.Getenv("AWS_BEARER_TOKEN_BEDROCK"))
}

// Bedrock API key read through the routed profile's secret scope under a home override
func resolveBedrockBearerTokenCtx(ctx context.Context) string {
	if home.Override(ctx) != "" {
		return strings.TrimSpace(GetSecretStr(ctx, "AWS_BEARER_TOKEN_BEDROCK", ""))
	}
	return resolveBedrockBearerToken()
}

// Bedrock API key from an explicit environment mapping
func resolveBedrockBearerTokenFrom(env map[string]string) string {
	return strings.TrimSpace(env["AWS_BEARER_TOKEN_BEDROCK"])
}

// BedrockOpenAINoCredentialsError is a Mantle call with neither a key nor resolvable AWS credentials
type BedrockOpenAINoCredentialsError struct{ Err error }

func (e *BedrockOpenAINoCredentialsError) Error() string {
	return "No AWS credentials available for Bedrock OpenAI Responses. " +
		"Configure AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, AWS_PROFILE, SSO, or an instance/task role."
}

func (e *BedrockOpenAINoCredentialsError) Unwrap() error { return e.Err }

// Credential provider that reports a missing chain as the Mantle no credentials error
type bedrockMantleCredentials struct{ inner awsCredentialProvider }

func (m bedrockMantleCredentials) Retrieve(ctx context.Context) (awsCredentials, error) {
	creds, err := m.inner.Retrieve(ctx)
	if errors.Is(err, ErrNoAWSCredentials) {
		return awsCredentials{}, &BedrockOpenAINoCredentialsError{Err: err}
	}
	return creds, err
}

// Round tripper signing Mantle requests with SigV4 that owns Authorization over the aws-sdk sentinel bearer
func NewBedrockOpenAISigV4Auth(ctx context.Context, region string, base http.RoundTripper) (http.RoundTripper, error) {
	resolved := strings.TrimSpace(region)
	if resolved == "" {
		resolved = resolveBedrockRuntimeRegionCtx(ctx)
	}
	opts, err := scopedAWSSessionOptions(ctx)
	if err != nil {
		return nil, err
	}
	return &sigV4RoundTripper{base: base, creds: bedrockMantleCredentials{newAWSChainCredentials(opts)}, service: "bedrock", region: resolved,
		drop: []string{"Authorization", "X-Amz-Date", "X-Amz-Security-Token"}}, nil
}

// Round tripper bounding each request and its body by a total timeout like an httpx client timeout
type awsTotalTimeoutTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

type awsCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b awsCancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func (t awsTotalTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), t.timeout)
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = awsCancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// HTTP client that SigV4 signs Mantle requests, bounded by the timeout when positive
func buildBedrockOpenAIHTTPClient(ctx context.Context, region string, timeout time.Duration) (*http.Client, error) {
	rt, err := NewBedrockOpenAISigV4Auth(ctx, region, http.DefaultTransport)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: rt}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client, nil
}

// ConfigureBedrockOpenAIRoute installs SigV4 on a Mantle route unless it carries a real Bedrock API key
func ConfigureBedrockOpenAIRoute(ctx context.Context, route Route, timeout time.Duration) (Route, error) {
	key := strings.TrimSpace(route.APIKey)
	if !isBedrockOpenAIBaseURL(route.BaseURL) || (key != "" && route.APIKey != "aws-sdk" && route.APIKey != "no-key-required") {
		return route, nil
	}
	region, ok := bedrockOpenAIRegionFromBaseURL(route.BaseURL)
	if !ok {
		region = resolveBedrockRuntimeRegionCtx(ctx)
	}
	base := route.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	rt, err := NewBedrockOpenAISigV4Auth(ctx, region, base)
	if err != nil {
		return route, err
	}
	if timeout > 0 {
		rt = awsTotalTimeoutTransport{base: rt, timeout: timeout}
	}
	route.APIKey = "aws-sdk"
	route.Transport = rt
	return route, nil
}

// ---- error classification ----

// Reports a dead pooled connection or transport failure, the signal to evict the cached client
func isBedrockStaleConnectionError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var svc *AWSServiceError
	if errors.As(err, &svc) {
		return false
	}
	var rt *AWSReadTimeoutError
	var ue *url.Error
	var ne net.Error
	var op *net.OpError
	switch {
	case errors.As(err, &rt), errors.As(err, &ue), errors.As(err, &op), errors.As(err, &ne):
		return true
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNABORTED), errors.Is(err, net.ErrClosed):
		return true
	}
	return false
}

// Reports IAM denying InvokeModelWithResponseStream, the permanent signal to stop streaming
func isBedrockStreamingAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	var svc *AWSServiceError
	var he *ccHTTPError
	if errors.As(err, &svc) {
		msg += " " + strings.ToLower(svc.Message+" "+svc.Body)
	}
	if errors.As(err, &he) {
		msg += " " + strings.ToLower(he.Body)
	}
	if !strings.Contains(msg, "invokemodelwithresponsestream") {
		return false
	}
	if svc != nil {
		return svc.Code == "AccessDeniedException" || svc.Code == "UnauthorizedException"
	}
	if he != nil {
		if code := awsErrorTypeHeader(he.Headers); code != "" {
			return code == "AccessDeniedException" || code == "UnauthorizedException"
		}
	}
	return strings.Contains(msg, "not authorized") || strings.Contains(msg, "accessdenied")
}

// ---- credential detection ----

// Auth source groups in priority order, the first group whose variables are all set names the source
var awsAuthEnvChain = [][]string{
	{"AWS_BEARER_TOKEN_BEDROCK"},
	{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"},
	{"AWS_PROFILE"},
	{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"},
	{"AWS_WEB_IDENTITY_TOKEN_FILE"},
}

// Default chain probe, replaced by tests counting walks
var awsChainHasCredentialsFn = awsChainHasCredentials

// Active AWS auth source from an environment mapping, then iam-role through the default chain, else empty
func resolveAWSAuthEnvVarFrom(env func(string) string, instanceMetadata bool) string {
	for _, group := range awsAuthEnvChain {
		all := true
		for _, name := range group {
			if strings.TrimSpace(env(name)) == "" {
				all = false
				break
			}
		}
		if all {
			return group[0]
		}
	}
	if awsChainHasCredentialsFn(instanceMetadata) {
		return "iam-role"
	}
	return ""
}

// Active AWS auth source name of the process environment, empty when none resolves
func resolveAWSAuthEnvVar() string {
	return resolveAWSAuthEnvVarFrom(os.Getenv, true)
}

// Reports any AWS credential source, env fast path then the default chain with IMDS when asked
func hasAWSCredentials(instanceMetadata bool) bool {
	return resolveAWSAuthEnvVarFrom(os.Getenv, instanceMetadata) != ""
}

// HasAWSCredentials reports whether any AWS credential source resolves for the process
func HasAWSCredentials(instanceMetadata bool) bool { return hasAWSCredentials(instanceMetadata) }

// ResolveAWSAuthEnvVar names the active AWS auth source of the process, empty when none
func ResolveAWSAuthEnvVar() string { return resolveAWSAuthEnvVar() }

// ---- region and guardrail ----

// Shared config region lookup, replaced by tests
var resolveBedrockRegionFn = resolveBedrockRegionLive

// AWS_REGION then AWS_DEFAULT_REGION then the shared config profile region then us-east-1
func resolveBedrockRegionLive() string {
	for _, name := range []string{"AWS_REGION", "AWS_DEFAULT_REGION"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	if r := newAWSSession(awsSessionOptions{}).region(); r != "" {
		return r
	}
	return "us-east-1"
}

// ResolveBedrockRegion is the AWS region the environment and shared config select
func ResolveBedrockRegion() string { return resolveBedrockRegionFn() }

// Process home config document, empty on any load failure
func bedrockProcessConfig(ctx context.Context) map[string]any {
	tree, err := config.Load(home.Dir(ctx), nil)
	if err != nil {
		return map[string]any{}
	}
	return map[string]any(tree)
}

// Configured bedrock.region, else the environment and shared config region, the process config when nil
func resolveBedrockRuntimeRegion(cfg map[string]any) string {
	if cfg == nil {
		cfg = bedrockProcessConfig(context.Background())
	}
	section, _ := cfg["bedrock"].(map[string]any)
	if r := strings.TrimSpace(stringOf(section["region"])); r != "" && section["region"] != nil {
		return r
	}
	return resolveBedrockRegionFn()
}

// Runtime region read from the config of the home the context serves
func resolveBedrockRuntimeRegionCtx(ctx context.Context) string {
	return resolveBedrockRuntimeRegion(bedrockProcessConfig(ctx))
}

// ResolveBedrockRuntimeRegion is the region every Bedrock endpoint of the home must use
func ResolveBedrockRuntimeRegion(ctx context.Context) string {
	return resolveBedrockRuntimeRegionCtx(ctx)
}

var bedrockRuntimeRegionRe = regexp.MustCompile(`bedrock-runtime\.([a-z0-9-]+)\.`)

// Region of a bedrock-runtime URL, us-east-1 when the host names none
func bedrockRegionFromRuntimeURL(baseURL string) string {
	if m := bedrockRuntimeRegionRe.FindStringSubmatch(baseURL); m != nil {
		return m[1]
	}
	return "us-east-1"
}

// Converse guardrailConfig from bedrock.guardrail, nil when identifier or version is unset
func bedrockGuardrailConfig(cfg map[string]any) map[string]any {
	if cfg == nil {
		cfg = bedrockProcessConfig(context.Background())
	}
	section, _ := cfg["bedrock"].(map[string]any)
	gr, _ := section["guardrail"].(map[string]any)
	if !truthy(gr["guardrail_identifier"]) || !truthy(gr["guardrail_version"]) {
		return nil
	}
	out := map[string]any{"guardrailIdentifier": gr["guardrail_identifier"], "guardrailVersion": gr["guardrail_version"]}
	for _, pair := range [][2]string{{"stream_processing_mode", "streamProcessingMode"}, {"trace", "trace"}} {
		if truthy(gr[pair[0]]) {
			out[pair[1]] = gr[pair[0]]
		}
	}
	return out
}

// BedrockGuardrailConfig is the configured Converse guardrail of the home the context serves
func BedrockGuardrailConfig(ctx context.Context) map[string]any {
	return bedrockGuardrailConfig(bedrockProcessConfig(ctx))
}

// InvokeModel header form of the configured guardrail, the only channel the Messages wire has
func bedrockGuardrailHeaders(cfg map[string]any) map[string]string {
	gr := bedrockGuardrailConfig(cfg)
	if gr == nil {
		return map[string]string{}
	}
	headers := map[string]string{
		"X-Amzn-Bedrock-GuardrailIdentifier": stringOf(gr["guardrailIdentifier"]),
		"X-Amzn-Bedrock-GuardrailVersion":    stringOf(gr["guardrailVersion"]),
	}
	switch strings.ToLower(stringOf(gr["trace"])) {
	case "enabled", "enabled_full", "true":
		headers["X-Amzn-Bedrock-Trace"] = "ENABLED"
	}
	return headers
}

// BedrockGuardrailActionField is the InvokeModel body field flagging a guardrail substitution
const BedrockGuardrailActionField = "amazon-bedrock-guardrailAction"

// Reports an InvokeModel reply Bedrock replaced with guardrail messaging
func anthropicResponseGuardrailIntervened(message map[string]any) bool {
	return strings.ToUpper(stringOf(message[BedrockGuardrailActionField])) == "INTERVENED"
}

// BedrockBinding is the agent state one Bedrock wire binds, the aws-sdk sentinel never a credential
type BedrockBinding struct {
	Region          string
	GuardrailConfig map[string]any
	APIKey          string
	BaseURL         string
	IsOAuth         bool
	Client          Client
}

// BindBedrockRuntime points an agent at a non Mantle Bedrock wire, building the Messages client for Claude
func BindBedrockRuntime(ctx context.Context, baseURL string, mode APIMode, model string) (*BedrockBinding, error) {
	b := &BedrockBinding{Region: bedrockRegionFromRuntimeURL(baseURL), GuardrailConfig: BedrockGuardrailConfig(ctx), APIKey: "aws-sdk", BaseURL: baseURL}
	if mode != ModeAnthropicMessages {
		return b, nil
	}
	route := Route{Provider: "bedrock", Model: model, BaseURL: baseURL, APIKey: "aws-sdk", Mode: ModeAnthropicMessages, Auth: AuthAWSSigV4, Region: b.Region}
	if timeout, ok := bedrockRequestTimeout(ctx, model); ok {
		route.Timeout = timeout
	}
	opts, err := scopedAWSSessionOptions(ctx)
	if err != nil {
		return nil, err
	}
	route.AccessKeyID, route.SecretAccessKey, route.SessionToken = opts.AccessKeyID, opts.SecretAccessKey, opts.SessionToken
	client, err := NewClient(route)
	if err != nil {
		return nil, err
	}
	b.Client = client
	return b, nil
}

// ---- model ids ----

var bedrockAnthropicModelRe = regexp.MustCompile(`(?i)^(?:(?:` + strings.Join(bedrockProfilePrefixes, "|") + `)\.)?anthropic\.claude`)

// Reports Claude on Bedrock in any regional or global profile form
func isAnthropicBedrockModel(model string) bool {
	return bedrockAnthropicModelRe.MatchString(model)
}

// IsAnthropicBedrockModel reports Claude on Bedrock, the Messages wire path
func IsAnthropicBedrockModel(model string) bool { return isAnthropicBedrockModel(model) }

// Live discovered model ids of the runtime region merged with the Mantle ids, false on failure or empty
func bedrockModelIDsOrNone(ctx context.Context) ([]string, bool) {
	discovered := discoverBedrockModelsFn(ctx, resolveBedrockRuntimeRegionCtx(ctx), nil)
	if len(discovered) == 0 {
		return nil, false
	}
	ids := make([]string, 0, len(discovered))
	for _, m := range discovered {
		ids = append(ids, m.ID)
	}
	return mergeBedrockOpenAIModelIDs(ids), true
}

// ---- converse reasoning issuer ----

// BedrockConverseIssuerType names the reasoning_details record stamping the model that minted a turn
const BedrockConverseIssuerType = "bedrock.native_assistant"

var (
	bedrockReplayStripPatterns  = []string{"deepseek.r1", "deepseek-r1", "moonshotai.kimi-k3"}
	bedrockReplayIssuerPatterns = []string{"openai.gpt-6"}
)

// Model stamped on the turn's issuer record, false when unstamped
func bedrockIssuer(msg map[string]any) (string, bool) {
	details, _ := msg["reasoning_details"].([]any)
	for _, d := range details {
		m, ok := d.(map[string]any)
		if !ok || m["type"] != BedrockConverseIssuerType {
			continue
		}
		if model, ok := m["model"].(string); ok {
			return model, true
		}
	}
	if typed, ok := msg["reasoning_details"].([]map[string]any); ok {
		for _, m := range typed {
			if m["type"] == BedrockConverseIssuerType {
				if model, ok := m["model"].(string); ok {
					return model, true
				}
			}
		}
	}
	return "", false
}

// Resolves an application profile ARN through resolve, the ARN itself when unresolved
func bedrockResolveIssuerARN(model string, resolve func(string) string) string {
	if resolve == nil {
		return resolveInferenceProfileModelID(context.Background(), model, "")
	}
	if out := resolve(model); out != "" {
		return out
	}
	return model
}

// Converse replay policy of a model, signed for Claude, issuer for GPT 6, strip for R1 and Kimi K3, else unsigned
func bedrockConverseReplayPolicy(model string, resolve func(string) string) string {
	if bedrockApplicationProfileRe.MatchString(model) {
		model = bedrockResolveIssuerARN(model, resolve)
	}
	lowered := strings.ToLower(model)
	if strings.Contains(lowered, "claude") {
		return "signed"
	}
	for _, p := range bedrockReplayIssuerPatterns {
		if strings.Contains(lowered, p) {
			return "issuer"
		}
	}
	for _, p := range bedrockReplayStripPatterns {
		if strings.Contains(lowered, p) {
			return "strip"
		}
	}
	return "unsigned"
}

// Reports a stamped issuer outside the Claude family, an unresolved application profile counting as unknown
func bedrockMintedOutsideClaude(issuer string, ok bool, resolve func(string) string) bool {
	if !ok {
		return false
	}
	if bedrockApplicationProfileRe.MatchString(issuer) && bedrockResolveIssuerARN(issuer, resolve) == issuer {
		return false
	}
	return bedrockConverseReplayPolicy(issuer, resolve) != "signed"
}

// Reports a turn whose reasoning a non Claude Converse model minted, so Claude gets none of it
func reasoningMintedOutsideClaude(msg map[string]any, resolve func(string) string) bool {
	issuer, ok := bedrockIssuer(msg)
	return bedrockMintedOutsideClaude(issuer, ok, resolve)
}

// ---- discovery ----

// BedrockModel is one discovered foundation model or inference profile
type BedrockModel struct {
	ID               string
	Name             string
	Provider         string
	InputModalities  []string
	OutputModalities []string
	Streaming        bool
}

const bedrockDiscoveryTTL = 3600 * time.Second

type bedrockDiscoveryEntry struct {
	at     time.Time
	models []BedrockModel
}

var (
	bedrockDiscoveryMu    sync.Mutex
	bedrockDiscoveryCache = map[string]bedrockDiscoveryEntry{}
)

// Discovery entry point, replaced by tests
var discoverBedrockModelsFn = DiscoverBedrockModels

// ResetBedrockDiscoveryCache clears the model discovery cache
func ResetBedrockDiscoveryCache() {
	bedrockDiscoveryMu.Lock()
	bedrockDiscoveryCache = map[string]bedrockDiscoveryEntry{}
	bedrockDiscoveryMu.Unlock()
}

func bedrockModelEntry(id, name, provider string, input, output []string) BedrockModel {
	n := strings.TrimSpace(name)
	if n == "" {
		n = id
	}
	return BedrockModel{ID: id, Name: n, Provider: provider, InputModalities: input, OutputModalities: output, Streaming: true}
}

type bedrockFoundationSummary struct {
	ModelID                    string   `json:"modelId"`
	ModelName                  string   `json:"modelName"`
	ProviderName               string   `json:"providerName"`
	InputModalities            []string `json:"inputModalities"`
	OutputModalities           []string `json:"outputModalities"`
	ResponseStreamingSupported bool     `json:"responseStreamingSupported"`
	ModelLifecycle             struct {
		Status string `json:"status"`
	} `json:"modelLifecycle"`
}

// Active, streaming capable, text output foundation models passing the provider filter
func bedrockListFoundationModels(ctx context.Context, c *awsServiceClient, filter map[string]bool, models *[]BedrockModel) error {
	var out struct {
		ModelSummaries []bedrockFoundationSummary `json:"modelSummaries"`
	}
	if err := c.doJSON(ctx, "ListFoundationModels", http.MethodGet, "/foundation-models", nil, nil, &out); err != nil {
		return err
	}
	for _, s := range out.ModelSummaries {
		id := strings.TrimSpace(s.ModelID)
		if id == "" {
			continue
		}
		prefix := ""
		if strings.Contains(id, ".") {
			prefix = strings.ToLower(strings.SplitN(id, ".", 2)[0])
		}
		if len(filter) > 0 && !filter[strings.ToLower(s.ProviderName)] && !filter[prefix] {
			continue
		}
		if strings.ToUpper(s.ModelLifecycle.Status) != "ACTIVE" || !s.ResponseStreamingSupported || !containsString(s.OutputModalities, "TEXT") {
			continue
		}
		input, output := s.InputModalities, s.OutputModalities
		if input == nil {
			input = []string{}
		}
		*models = append(*models, bedrockModelEntry(id, s.ModelName, strings.TrimSpace(s.ProviderName), input, output))
	}
	return nil
}

// Active inference profiles not already listed, paginated through nextToken
func bedrockListInferenceProfiles(ctx context.Context, c *awsServiceClient, filter map[string]bool, models *[]BedrockModel) error {
	type summary struct {
		InferenceProfileID   string `json:"inferenceProfileId"`
		InferenceProfileName string `json:"inferenceProfileName"`
		Status               string `json:"status"`
		Models               []struct {
			ModelArn string `json:"modelArn"`
		} `json:"models"`
	}
	var profiles []summary
	next := ""
	for {
		var q url.Values
		if next != "" {
			q = url.Values{"nextToken": {next}}
		}
		var out struct {
			Summaries []summary `json:"inferenceProfileSummaries"`
			NextToken string    `json:"nextToken"`
		}
		if err := c.doJSON(ctx, "ListInferenceProfiles", http.MethodGet, "/inference-profiles", q, nil, &out); err != nil {
			return err
		}
		profiles = append(profiles, out.Summaries...)
		if next = out.NextToken; next == "" {
			break
		}
	}
	seen := map[string]bool{}
	for _, m := range *models {
		seen[strings.ToLower(m.ID)] = true
	}
	for _, p := range profiles {
		id := strings.TrimSpace(p.InferenceProfileID)
		if id == "" || p.Status != "ACTIVE" || seen[strings.ToLower(id)] {
			continue
		}
		if len(filter) > 0 {
			match := false
			for _, m := range p.Models {
				if filter[strings.ToLower(extractBedrockProviderFromARN(m.ModelArn))] {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		*models = append(*models, bedrockModelEntry(id, p.InferenceProfileName, "inference-profile", []string{"TEXT"}, []string{"TEXT"}))
		seen[strings.ToLower(id)] = true
	}
	return nil
}

// DiscoverBedrockModels lists foundation models and inference profiles, cached an hour per region and filter
func DiscoverBedrockModels(ctx context.Context, region string, providerFilter []string) []BedrockModel {
	sorted := append([]string(nil), providerFilter...)
	sort.Strings(sorted)
	key := region + ":" + strings.Join(sorted, ",")
	if o := home.Override(ctx); o != "" {
		key = home.Key(o) + "|" + key
	}
	bedrockDiscoveryMu.Lock()
	cached, ok := bedrockDiscoveryCache[key]
	bedrockDiscoveryMu.Unlock()
	if ok && len(cached.models) > 0 && awsNow().Sub(cached.at) < bedrockDiscoveryTTL {
		return cached.models
	}
	client, err := bedrockControlClient(ctx, region)
	if err != nil {
		bedrockLog.Warn(fmt.Sprintf("Failed to create Bedrock client for model discovery: %v", err))
		return []BedrockModel{}
	}
	filter := map[string]bool{}
	for _, f := range providerFilter {
		filter[strings.ToLower(f)] = true
	}
	models := []BedrockModel{}
	if err := bedrockListFoundationModels(ctx, client, filter, &models); err != nil {
		bedrockLog.Warn(fmt.Sprintf("Failed to list Bedrock foundation models: %v", err))
	}
	if err := bedrockListInferenceProfiles(ctx, client, filter, &models); err != nil {
		bedrockLog.Debug(fmt.Sprintf("Skipping inference profile discovery: %v", err))
	}
	sort.SliceStable(models, func(i, j int) bool {
		gi, gj := strings.HasPrefix(models[i].ID, "global."), strings.HasPrefix(models[j].ID, "global.")
		if gi != gj {
			return gi
		}
		return strings.ToLower(models[i].Name) < strings.ToLower(models[j].Name)
	})
	bedrockDiscoveryMu.Lock()
	bedrockDiscoveryCache[key] = bedrockDiscoveryEntry{at: awsNow(), models: models}
	bedrockDiscoveryMu.Unlock()
	return models
}

var bedrockFoundationARNRe = regexp.MustCompile(`foundation-model/([^.]+)`)

// Provider segment of a foundation model ARN, empty when absent
func extractBedrockProviderFromARN(arn string) string {
	if m := bedrockFoundationARNRe.FindStringSubmatch(arn); m != nil {
		return m[1]
	}
	return ""
}

// ---- context lengths ----

// Static Bedrock windows, longest substring match wins, ties go to the earlier row
var bedrockContextLengths = []struct {
	key    string
	length int
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

// BedrockDefaultContextLength is the window of a Bedrock model no source knows
const BedrockDefaultContextLength = 128_000

// Probe padding tiers in tokens and the conservative words per token ratio
var bedrockProbeTiers = []int{1_300_000, 2_200_000}

const bedrockWordsPerToken = 0.9

var (
	bedrockWarnedMu      sync.Mutex
	bedrockWarnedDefault = map[string]bool{}
)

// Live context probe, replaced by tests
var bedrockProbeFn = probeBedrockContextLengthLive

// Probed window of a model in a region, zero when the probe could not determine one
func probeBedrockContextLength(ctx context.Context, model, region string) int {
	return bedrockProbeFn(ctx, model, region)
}

// Provokes a length error with oversized Converse prompts, the only authoritative window source
func probeBedrockContextLengthLive(ctx context.Context, model, region string) int {
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := bedrockRuntimeClient(ctx, region, model)
	if err != nil {
		bedrockLog.Debug(fmt.Sprintf("Bedrock context probe skipped for %s: %v", model, err))
		return 0
	}
	lastError := ""
	for _, tier := range bedrockProbeTiers {
		oversized := strings.Repeat("data ", int(float64(tier)/bedrockWordsPerToken))
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"text": oversized}}}},
			"inferenceConfig": map[string]any{"maxTokens": 8}}
		err := client.doJSON(ctx, "Converse", http.MethodPost, bedrockModelPath(model, "converse"), nil, body, nil)
		if err == nil {
			bedrockLog.Debug(fmt.Sprintf("Bedrock context probe for %s accepted ~%s-token prompt; window is at least that", model, commaInt(tier)))
			return tier
		}
		lastError = err.Error()
		if limit, ok := ParseContextLimitFromError(lastError); ok && limit >= 1024 {
			bedrockLog.Info(fmt.Sprintf("Probed Bedrock context window for %s: %s tokens", model, commaInt(limit)))
			return limit
		}
	}
	if len(lastError) > 200 {
		lastError = lastError[:200]
	}
	bedrockLog.Debug(fmt.Sprintf("Bedrock context probe for %s returned no parseable limit: %s", model, lastError))
	return 0
}

// Escaped runtime path of an operation on a model id, slashes in ARNs encoded and colons kept
func bedrockModelPath(model, operation string) string {
	return "/model/" + bedrockEscapeModelID(model) + "/" + operation
}

// Percent encodes a model id the way the SDKs do, every reserved byte except the colon
func bedrockEscapeModelID(model string) string {
	var b strings.Builder
	for i := 0; i < len(model); i++ {
		c := model[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~', c == ':':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Window of a model, live probe when asked and a region is known, then the static table, then the default
func bedrockContextLength(ctx context.Context, model, region string, probe bool) int {
	if ctx == nil {
		ctx = context.Background()
	}
	profileARN := ""
	if bedrockApplicationProfileRe.MatchString(model) {
		profileARN = model
		model = resolveInferenceProfileModelID(ctx, profileARN, region)
	}
	if probe && region != "" {
		if probed := probeBedrockContextLength(ctx, model, region); probed > 0 {
			return probed
		}
	}
	lower := strings.ToLower(model)
	best, bestLen := -1, -1
	for i, row := range bedrockContextLengths {
		if strings.Contains(lower, row.key) && len(row.key) > bestLen {
			best, bestLen = i, len(row.key)
		}
	}
	if best >= 0 {
		return bedrockContextLengths[best].length
	}
	if profileARN != "" {
		bedrockLog.Warn(fmt.Sprintf("Bedrock inference profile %s resolved no known model window; using the %s default. "+
			"Grant bedrock:GetInferenceProfile or set model.context_length explicitly if the wrapped model has a larger window.",
			profileARN, commaInt(BedrockDefaultContextLength)))
	} else {
		bedrockWarnedMu.Lock()
		first := !bedrockWarnedDefault[model]
		bedrockWarnedDefault[model] = true
		bedrockWarnedMu.Unlock()
		if first {
			bedrockLog.Warn(fmt.Sprintf("Bedrock model %s has no known context window; using the static %s-token fallback "+
				"(not a detected limit). Set model.context_length in config.yaml if the model's window is larger.", model, commaInt(BedrockDefaultContextLength)))
		}
	}
	return BedrockDefaultContextLength
}

// Static table window of a model with no probe, the metadata fallback
func bedrockStaticContextLength(model string) int {
	return bedrockContextLength(context.Background(), model, "", false)
}

// BedrockContextLength is the window of a model, probing the region first when probe is set
func BedrockContextLength(ctx context.Context, model, region string, probe bool) int {
	return bedrockContextLength(ctx, model, region, probe)
}

// ---- application inference profiles ----

var (
	bedrockApplicationProfileRe = regexp.MustCompile(`:application-inference-profile/`)
	bedrockARNRegionRe          = regexp.MustCompile(`(?i)^arn:[^:]+:bedrock:([a-z0-9-]+):`)
	bedrockProfileMu            sync.Mutex
	bedrockProfileCache         = map[string]string{}
)

// Wrapped model ARN of an application profile, the profile ARN itself when GetInferenceProfile fails, cached per process
func resolveInferenceProfileModelID(ctx context.Context, profileARN, region string) string {
	bedrockProfileMu.Lock()
	if v, ok := bedrockProfileCache[profileARN]; ok {
		bedrockProfileMu.Unlock()
		return v
	}
	bedrockProfileMu.Unlock()
	if m := bedrockARNRegionRe.FindStringSubmatch(profileARN); m != nil && m[1] != "" {
		region = m[1]
	}
	if region == "" {
		region = resolveBedrockRegionFn()
	}
	resolved := profileARN
	client, err := bedrockControlClient(ctx, region)
	if err == nil {
		var out struct {
			Models []struct {
				ModelArn string `json:"modelArn"`
			} `json:"models"`
		}
		err = client.doJSON(ctx, "GetInferenceProfile", http.MethodGet, "/inference-profiles/"+bedrockEscapeModelID(profileARN), nil, nil, &out)
		if err == nil {
			for _, m := range out.Models {
				if m.ModelArn != "" {
					resolved = m.ModelArn
					break
				}
			}
		}
	}
	if err != nil {
		bedrockLog.Debug(fmt.Sprintf("Inference profile resolution skipped for %s: %v", profileARN, err))
	}
	bedrockProfileMu.Lock()
	bedrockProfileCache[profileARN] = resolved
	bedrockProfileMu.Unlock()
	return resolved
}
