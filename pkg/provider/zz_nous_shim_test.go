package provider

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Temporary test only stand ins for every contract other slices still owe to the auth flows test build, deleted after the run

type CreditsState struct {
	RemainingMicros int64
	RemainingUSD    string
	PaidAccess      bool
	DisabledReason  string
	DenominatorKind string
	Unmetered       bool
	SpendSource     string
	PurchasedUSD    string
	PurchasedMicros int64
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
func leftCoreProviderInstallHint(name string) string        { return "" }
func hasAWSCredentials(instanceMetadata bool) bool          { return false }
func envWriteRefusal(h, key, action string) string          { return "" }
func vertexRuntimeConfig(ctx context.Context, cfg map[string]any, secrets *config.Secrets, h string) (func(context.Context) (string, error), string, bool) {
	return nil, "", false
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
func findClaudeCodeCLI(command string) string { return "" }
func refreshXAIOAuthPure(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
	return nil, nil
}
func resolveQwenRuntimeCredentials(env PoolEnv, refreshIfExpiring bool) (map[string]any, error) {
	return nil, nil
}
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

func providerAuthStatus(ctx context.Context, provider string) (bool, bool) { return false, false }
func windowPlanDefault(modelIDs []string, pricing map[string]any) string   { return "" }
func modelAliasIdentity(nameLower string) (string, string, bool) {
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

func heldConnectorsToken(state map[string]any, skew time.Duration) string { return "" }
func freeTierNotSignedIn() string                                         { return "You're not signed in." }
func resolveXAIOAuthRuntimeCredentials(env PoolEnv, forceRefresh, refreshIfExpiring bool, refreshSkew time.Duration) (map[string]any, error) {
	return nil, xaiErr("no xai", "xai_auth_missing", true)
}
func getQwenAuthStatus(env PoolEnv) map[string]any { return map[string]any{"logged_in": false} }
func resolveMinimaxOAuthRuntimeCredentials(env PoolEnv) (map[string]any, error) {
	return nil, nil
}
func getPluginOAuthAuthStatus(env PoolEnv, provider string) map[string]any {
	return map[string]any{"logged_in": false}
}
func noteSignInFailure(err error)             {}
func leftCoreLaunchEnvNames() map[string]bool { return map[string]bool{} }

// Contracts of the nous slice, steerable from the nous tests
var (
	testNousEntitlementMessage   = ""
	testNousEntitlementCalls     = 0
	testBillingInvalidateCalls   = 0
	testSettleAfterUpgradeCalls  = 0
	testSaveModelChoiceCalls     []string
	testGuestHandoffDrainResult  []map[string]any
	testPromptModelSelection     func(p ModelSelectionPrompt) (string, bool)
	testNousPolicyPresent        = func(ctx context.Context) (bool, bool) { return false, false }
	testNousPortalAccountIsFree  = func(ctx context.Context, forceFresh bool) (bool, error) { return false, nil }
	testGuestWithoutFreeTier     = func(state map[string]any) bool { return false }
	testWithBackgroundCallerSeen = 0
)

func nousPortalEntitlementMessage(env PoolEnv, capability string) (string, error) {
	testNousEntitlementCalls++
	return testNousEntitlementMessage, nil
}
func nousPolicyNotice(env PoolEnv, removed bool) string                       { return "" }
func nousBillingInvalidateCachedToken()                                       { testBillingInvalidateCalls++ }
func runWithChallenge(env PoolEnv, fn func() (string, error)) (string, error) { return fn() }
func refreshGuestState(env PoolEnv, state map[string]any, client *http.Client, purpose string) (string, error) {
	return "", nousErr("guest exchange is not part of this build", "anon_exchange_unavailable", false)
}
func guestWithoutFreeTier(state map[string]any) bool       { return testGuestWithoutFreeTier(state) }
func anonCredentialDeadCode(err error) (string, bool)      { return "", false }
func clearDeadGuest(env PoolEnv, code, deadToken string)   {}
func settleAfterUpgrade(env PoolEnv, state map[string]any) { testSettleAfterUpgradeCalls++ }
func withBackgroundCaller(ctx context.Context) context.Context {
	testWithBackgroundCallerSeen++
	return ctx
}
func nousPolicyPresent(ctx context.Context) (bool, bool) { return testNousPolicyPresent(ctx) }
func nousPortalAccountIsFreeTier(ctx context.Context, forceFresh bool) (bool, error) {
	return testNousPortalAccountIsFree(ctx, forceFresh)
}
func guestHandoffNoteReplacement(env PoolEnv, previous, next map[string]any) {}
func guestHandoffDrain(env PoolEnv) []map[string]any                         { return testGuestHandoffDrainResult }
func guestHandoffResultLines(view map[string]any) []string {
	return []string{anyString(view["line"])}
}
func guestHandoffStartWorker(env PoolEnv)                          {}
func guestHandoffStopWorker(timeout time.Duration)                 {}
func nousPickerUsage(env PoolEnv, warm bool) (any, []string, bool) { return nil, nil, false }
func featuredFirst(ids, featured []string) []string                { return ids }
func largeModelNotice(env PoolEnv, model, provider string) string  { return "" }

type ModelSelectionPrompt struct {
	ModelIDs           []string
	Pricing            PricingTable
	UnavailableModels  []string
	PortalURL          string
	UnavailableMessage string
	ConfirmProvider    string
	ConfirmBaseURL     string
	ConfirmAPIKey      string
	Usage              any
}

func promptModelSelection(env PoolEnv, out io.Writer, p ModelSelectionPrompt) (string, bool) {
	if testPromptModelSelection == nil {
		return "", false
	}
	return testPromptModelSelection(p)
}
func saveModelChoice(env PoolEnv, modelID string) error {
	testSaveModelChoiceCalls = append(testSaveModelChoiceCalls, modelID)
	return nil
}

// Binds the launch profile scope when multiplexing is on and the call carries none
func withLaunchProfileScopeIfMultiplexed(ctx context.Context) context.Context {
	if !IsMultiplexActive(ctx) || boundScope(ctx) != nil {
		return ctx
	}
	h := home.Process()
	return WithSecretScope(ctx, BuildProfileSecretScope(h), h)
}
