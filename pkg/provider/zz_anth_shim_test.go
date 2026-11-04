package provider

import (
	"context"
	"net/http"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Temporary test only stand ins for every contract other slices still owe, deleted after the run

type CreditsState struct {
	RemainingMicros int64
	PurchasedMicros int64
	PurchasedUSD    string
	Unmetered       bool
	RemainingUSD    string
	PaidAccess      bool
	DenominatorKind string
	SpendSource     string
	DisabledReason  string
}

func (c *CreditsState) UsedFraction() (float64, bool) { return 0, false }
func (c *CreditsState) IsUsageWindow() bool           { return false }
func (c *CreditsState) Depleted() bool                { return false }
func (c *CreditsState) WindowExhausted() bool         { return false }
func (c *CreditsState) TopTierExhausted() bool        { return false }
func (c *CreditsState) AgeSeconds() float64           { return 0 }

type AgentNotice struct{ Key, Text string }

const DevCreditsFixtureEnv = "ICEBATH_DEV_CREDITS_FIXTURE"

func DevFixtureCreditsState() *CreditsState                            { return nil }
func ParseCreditsHeaders(h http.Header, provider string) *CreditsState { return nil }
func NewCreditsLatch() map[string]any                                  { return map[string]any{} }
func SyncPlanSnapshot(state *CreditsState) bool                        { return false }
func RewarmPricingBeforeDepletedNotice(c *CreditsCapture) bool         { return false }
func RestoreWindowNotices(sessionID string, latch map[string]any)      {}
func RememberWindowNotices(sessionID string, latch map[string]any)     {}
func ForgetWindowNotices(sessionID string)                             {}
func RememberShownBand(sessionID string, band any)                     {}
func ResponseIsUsageWindow(h http.Header) bool                         { return false }
func EvaluateCreditsNotices(state *CreditsState, latch map[string]any, modelIsFree, modelIsTopTier bool) ([]AgentNotice, []string) {
	return nil, nil
}
func PrintBillingOrEntitlementGuidance(emit func(...string), capability, provider, baseURL, model string, unverified bool) bool {
	return false
}
func PrintNousEntitlementGuidance(emit func(...string), capability string) bool { return false }
func NegotiateRequestTier(route Route, overrides map[string]any)                {}
func ApplyDaybreak(route Route, daybreak bool, overrides map[string]any)        {}
func newCodexResponsesClient(route Route) (Client, error)                       { return nil, nil }
func reasoningMintedOutsideClaude(message map[string]any, resolveProfileARN func(string) string) bool {
	return false
}
func resolveMoaAggregator(cfg map[string]any, preset string) (string, string, bool) {
	return "", "", false
}
func resolveBedrockRuntimeRegion(cfg map[string]any) string { return "us-east-1" }
func nousInferenceEnvOverride() string                      { return "" }
func leftCoreProviderInstallHint(name string) string        { return "" }
func hasAWSCredentials(instanceMetadata bool) bool          { return false }
func envWriteRefusal(h, key, action string) string          { return "" }
func vertexRuntimeConfig(ctx context.Context, cfg map[string]any, secrets *config.Secrets, h string) (func(context.Context) (string, error), string, bool) {
	return nil, "", false
}
func validateNousInferenceURLFromNetwork(u string) string { return u }
func resolveNousRuntimeCredentialsBackground(ctx context.Context) (string, string, bool) {
	return "", "", false
}
func resolveLlamacppEndpoint(ctx context.Context, cfg map[string]any, h string) (string, string, bool) {
	return "", "", false
}
func resolveBedrockBearerToken() string                                       { return "" }
func resolveAWSAuthEnvVar() string                                            { return "" }
func rememberCodexCatalog(cacheKey string, entries []any)                     {}
func recoverLeftCoreProvider(name string)                                     {}
func probeBedrockContextLength(ctx context.Context, model, region string) int { return 0 }
func pluginAuxiliaryTasks() []map[string]any                                  { return nil }
func nousPolicyPresent(ctx context.Context) (bool, bool)                      { return false, false }
func managedLocalStateEndpoint(h string) (string, string, bool)               { return "", "", false }
func leftCoreProfileEnv() []string                                            { return nil }
func isAnthropicBedrockModel(model string) bool                               { return false }
func homeCredentialSource(h string) CredentialSource                          { return NewStore(h) }
func entraTokenProvider(scope string) (func(context.Context) (string, error), error) {
	return nil, nil
}
func configStructureIssueLines(cfg map[string]any) []string    { return nil }
func bedrockStaticContextLength(model string) int              { return 0 }
func bedrockOpenAIUsesMantle(model string) bool                { return false }
func bedrockOpenAIBaseURL(region string) string                { return "" }
func bedrockGuardrailConfig(cfg map[string]any) map[string]any { return nil }
func resolveZaiBaseURLDetected(ctx context.Context, h, apiKey, defaultURL string) string {
	return defaultURL
}
func refreshXAIOAuthPure(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
	return nil, nil
}
func refreshExpiredCodexProbeToken(env PoolEnv, token, refreshToken string) (map[string]any, bool) {
	return nil, false
}
func probeCodexQuotaRestored(env PoolEnv, token, baseURL string) (bool, bool) { return false, false }
func codexPoolRouteBaseURL(env PoolEnv, entryBaseURL string) string           { return entryBaseURL }
func resolveNousRuntimeCredentials(env PoolEnv, forceRefresh bool, staleAccessToken string) (map[string]any, error) {
	return nil, nil
}
func quarantineNousOAuthState(state map[string]any, err error, reason string) {}
func quarantineNousPoolEntries(data AuthData, err error, reason string)       {}
func resolveQwenRuntimeCredentials(env PoolEnv, refreshIfExpiring bool) (map[string]any, error) {
	return nil, nil
}
func guestWithoutFreeTier(state map[string]any) bool              { return false }
func routeCanServeModel(provider, baseURL, model string) bool     { return true }
func healForkedSingleUseOAuthGrants(env PoolEnv, provider string) {}
func hasExplicitVertexConfig(env PoolEnv) bool                    { return false }
func gatewayBridgedAllowAllUsers() (string, bool)                 { return "", false }
func loadManagedEnv() map[string]string                           { return nil }
func localSubprocessEnv(scrubSecrets, inheritProfileHome bool) map[string]string {
	return nil
}
func workerConfigSnapshotActive() bool { return false }
func configBackendView(h string) ConfigBackendView {
	return ConfigBackendView{}
}
func pmInstalledBinary(name string) (string, bool)        { return "", false }
func pmEnsure(name string, explicit bool) (string, error) { return "", nil }

// Contracts of this slice
func providerAuthStatus(ctx context.Context, provider string) (bool, bool) { return false, false }
func resolveCodexRuntimeCredentialsReadOnly(env PoolEnv) (map[string]any, error) {
	return nil, nil
}
func fetchNousModels(ctx context.Context, apiKey, inferenceBaseURL string) []string { return nil }
func withBackgroundCaller(ctx context.Context) context.Context                      { return ctx }
func nousPortalAccountIsFreeTier(ctx context.Context, forceFresh bool) (bool, error) {
	return false, nil
}
func windowPlanDefault(modelIDs []string, pricing map[string]any) string { return "" }
func modelAliasIdentity(nameLower string) (string, string, bool) {
	switch nameLower {
	case "sonnet":
		return "anthropic", "claude-sonnet", true
	case "opus":
		return "anthropic", "claude-opus", true
	}
	return "", "", false
}
func bedrockModelIDsOrNone(ctx context.Context) ([]string, bool)              { return nil, false }
func ollamaLocalCatalog(ctx context.Context, forceRefresh bool) []string      { return nil }
func fetchOllamaCloudModels(ctx context.Context, force bool) ([]string, bool) { return nil, false }
func ollamaNativeProbeReachable(ctx context.Context) bool                     { return false }
func clearOllamaLocalCaches()                                                 {}

const ollamaLocalModelsCacheTTL = 300 * time.Second

var (
	anthropicCommonBetas    = []string{"interleaved-thinking-2025-05-14", "fine-grained-tool-streaming-2025-05-14"}
	anthropicOAuthOnlyBetas = []string{"claude-code-20250219", "oauth-2025-04-20"}
	anthropicContext1MBeta  = "context-1m-2025-08-07"
)

func formatAuthError(err *AuthError) string           { return err.Message }
func isTerminalCodexOAuthRefreshError(err error) bool { return false }
func isTerminalXAIOAuthRefreshError(err error) bool   { return false }
func isTerminalNousRefreshError(err error) bool       { return false }
func canOpenGraphicalBrowser() bool                   { return false }
