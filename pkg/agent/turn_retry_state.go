package agent

// One shot recovery guards and restart signals for a single API call attempt, fresh per iteration
type TurnRetryState struct {
	CodexAuthRetryAttempted               bool
	AnthropicAuthRetryAttempted           bool
	NousAuthRetryAttempted                bool
	NousPaidEntitlementRefreshAttempted   bool
	WelcomeModelSwitchAttempted           bool
	WelcomeRouteHealAttempted             bool
	ProviderAuthRefreshAttempted          bool
	ProviderStaleRefreshAttempted         bool
	VertexAuthRetryAttempted              bool
	ThinkingSigRetryAttempted             bool
	InvalidEncryptedContentRetryAttempted bool
	NativeCompactionRejectRetryAttempted  bool
	ImageShrinkRetryAttempted             bool
	MultimodalToolContentRetryAttempted   bool
	ReasoningMandatoryRetryAttempted      bool
	OAuth1MBetaRetryAttempted             bool
	LlamaCppGrammarRetryAttempted         bool
	PrimaryRecoveryAttempted              bool
	HasRetried429                         bool
	Affordable402ClampAttempted           bool
	AuthFailoverAttempted                 bool
	AutoRecoveryCyclesUsed                int
	RestartWithCompressedMessages         bool
	RestartWithLengthContinuation         bool
	RestartWithRebuiltMessages            bool
	RestartWithRedirectedMessages         bool
}
