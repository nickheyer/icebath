package providers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type ReasoningCaps struct {
	Supports        bool
	Mandatory       bool
	Efforts         []string
	EffortsDeclared bool
}

type RouteFacts struct {
	Provider          string
	Model             string
	BaseURL           string
	SessionID         string
	CacheScope        string
	DataUse           []string
	Reasoning         *wire.Reasoning
	SupportsReasoning bool
	OllamaNumCtx      int
	LMStudioOptions   []string
	LMStudioProbed    bool
	QwenSession       json.RawMessage
	ProviderPrefs     json.RawMessage
	MinCodingScore    json.RawMessage
	Caps              *ReasoningCaps
}

type Extras struct {
	Body     map[string]json.RawMessage
	TopLevel map[string]json.RawMessage
	Headers  map[string]string
	Model    string
}

type ClientExtras struct {
	CACert string
}

type ClientOptions struct {
	APIKey  auth.Secret
	BaseURL string
	Headers map[string]string
	Timeout time.Duration
	Command string
	Args    []string
}

type ListInput struct {
	APIKey  auth.Secret
	BaseURL string
	Timeout time.Duration
	ChatIDs func(items []wire.Value) []string
}

type Probe struct {
	Models  []string
	Listed  bool
	URL     string
	BaseURL string
}

type SetupStatus struct {
	Available    bool
	LoggedIn     bool
	Plan         string
	Detail       string
	LoginCommand string
}

type DiscoveredModel struct {
	ID    string
	Label string
	Note  string
}

type ErrorInput struct {
	Err     error
	Status  int
	Code    string
	Message string
	Body    json.RawMessage
	Model   string
}

type Verdict struct {
	Reason    string
	Retryable *bool
	Compress  *bool
	Rotate    *bool
	Fallback  *bool
	Context   *VerdictContext
}

type VerdictContext struct {
	ResetAt               time.Time
	BillingUnverified     bool
	PossibleContentFilter bool
	UpstreamProvider      string
	WelcomeRefusal        *portal.Refusal
	WelcomeRoute          string
	UsageWindow           bool
	DisplayMessage        string
}

type AuthArgs struct {
	Provider  string
	Type      string
	Label     string
	NoBrowser bool
	Console   auth.Console
}

type DeviceLogin struct {
	UserCode        string
	VerificationURL string
	ExpiresIn       time.Duration
	Interval        time.Duration
	State           json.RawMessage
}

type DevicePoll struct {
	Status     string
	Interval   time.Duration
	Credential json.RawMessage
	Message    string
}

type UsageWindow struct {
	Label       string
	UsedPercent *float64
	ResetAt     time.Time
	Detail      string
	Scope       string
}

type AccountUsage struct {
	Provider          string
	Source            string
	FetchedAt         time.Time
	Title             string
	Plan              string
	Windows           []UsageWindow
	Details           []string
	UnavailableReason string
	Identity          string
	Raw               json.RawMessage
	UsageWindow       bool
}

type Cost struct {
	AmountUSD      string
	Status         string
	Source         string
	Label          string
	FetchedAt      time.Time
	PricingVersion string
	Notes          []string
}

type PoolReader interface {
	PoolProviders(ctx context.Context) ([]string, error)
}

type MessagePreparer interface {
	PrepareMessages(ctx context.Context, msgs []wire.Message) ([]wire.Message, error)
}

type BodyExtender interface {
	ExtraBody(ctx context.Context, f RouteFacts) (map[string]json.RawMessage, error)
}

type ArgsExtender interface {
	APIExtras(ctx context.Context, f RouteFacts) (Extras, error)
}

type ReasoningDefaulter interface {
	DefaultReasoning(model string) *wire.Reasoning
}

type MaxTokensCapper interface {
	MaxTokens(model string) (int, bool)
}

type EffortDeclarer interface {
	Efforts(ctx context.Context, model string) ([]string, bool)
}

type ClientConfigurer interface {
	ClientExtras(ctx context.Context, baseURL string) ClientExtras
}

type ClientMaker interface {
	Client(ctx context.Context, o ClientOptions) (wire.Caller, error)
}

type ModeChooser interface {
	ModeForModel(ctx context.Context, model string, apiKey *auth.Secret) (wire.Mode, bool, error)
}

type ClientHeaderer interface {
	ClientHeaders(ctx context.Context, baseURL string, apiKey auth.Secret, images bool) (map[string]string, bool, error)
}

type RequestHeaderer interface {
	RequestHeaders(ctx context.Context, baseURL, model string, userInitiated bool) (map[string]string, error)
}

type MaxTokensFielder interface {
	MaxTokensField(ctx context.Context, baseURL, model string) (wire.MaxTokensField, bool, error)
}

type SecretResolver interface {
	ResolveSecret(ctx context.Context) (key auth.Secret, source string, ok bool, err error)
}

type BaseURLDeriver interface {
	RuntimeBaseURL(ctx context.Context, apiKey auth.Secret, def, envURL string) (string, bool)
}

type PoolEntryPreparer interface {
	RuntimeKey(e *auth.Entry) (auth.Secret, bool)
}

type PoolSeeder interface {
	SeedPool(ctx context.Context, suppressed []string, explicit bool) ([]auth.Seed, error)
}

type SourceRemover interface {
	RemoveSource(source string) (sources, hints []string, ok bool)
}

type RuntimeRefresher interface {
	RefreshRuntime(ctx context.Context, reason string) (key auth.Secret, baseURL string, ok bool, err error)
}

type StaleDetector interface {
	IsStale(status int, message string) (bool, error)
}

type RefreshNoticer interface {
	RefreshNotice(reason string) (string, error)
}

type RejectionHinter interface {
	RejectionHint(status int, baseURL string) ([]string, error)
}

type AuthEvidencer interface {
	ExternalAuthEvidence() (verified bool, source string, ok bool)
}

type CredentialsHinter interface {
	MissingCredentialsHint() (string, bool)
}

type StatusReporter interface {
	AuthStatus(ctx context.Context) (auth.Status, bool)
}

type EntitlementExplainer interface {
	EntitlementMessage(ctx context.Context) string
}

type StateSeeder interface {
	StateSeedable(state json.RawMessage) bool
}

type ReasoningPayloader interface {
	ReasoningPayload(ctx context.Context, model string, r *wire.Reasoning, baseURL string) (json.RawMessage, error)
}

type CarrierReplayer interface {
	ReplayCarriers(ctx context.Context, values map[string]string, model, baseURL string) (map[string]string, error)
}

type SignatureReplayer interface {
	ReplaysFunctionSignature(ctx context.Context, model, baseURL string) (bool, error)
}

type ContextLimiter interface {
	ContextLength(model string) (int, bool)
}

type LiveContexter interface {
	LiveContextLength(ctx context.Context, model string, apiKey auth.Secret, baseURL string) (int, bool)
}

type ModelNormalizer interface {
	NormalizeModel(ctx context.Context, model string, apiKey auth.Secret) (string, bool)
}

type UsageCoster interface {
	UsageCost(model string, u *wire.Usage) (*Cost, bool)
}

type ModelLister interface {
	ListModels(ctx context.Context, in ListInput) ([]string, bool)
}

type PickerCataloger interface {
	PickerCatalog(ctx context.Context, force bool) ([]string, bool, error)
}

type ModelProber interface {
	ProbeModels(ctx context.Context, baseURL string, apiKey auth.Secret, timeout time.Duration) (Probe, bool)
}

type CatalogInvalidator interface {
	InvalidateCatalog()
}

type SetupReporter interface {
	SetupStatus(ctx context.Context) (SetupStatus, bool)
}

type ModelDiscoverer interface {
	DiscoverModels(ctx context.Context) ([]DiscoveredModel, bool)
}

type AuxResolver interface {
	AuxModel(ctx context.Context, vision bool) string
}

type VisionDefaulter interface {
	VisionModel(ctx context.Context) (string, bool)
}

type UsageFetcher interface {
	AccountUsage(ctx context.Context, baseURL string, apiKey auth.Secret) (*AccountUsage, error)
}

type AuthHandler interface {
	HandleAuth(ctx context.Context, action string, args AuthArgs) (bool, error)
}

type CredentialRefresher interface {
	CredentialRefresher() auth.Refresher
}

type ErrorClassifier interface {
	ClassifyError(ctx context.Context, in ErrorInput) (Verdict, bool)
}

type AccountStatuser interface {
	AccountStatus(ctx context.Context) (json.RawMessage, error)
}

type DeviceLoginer interface {
	StartDeviceLogin(ctx context.Context) (DeviceLogin, error)
	PollDeviceLogin(ctx context.Context, state json.RawMessage) (DevicePoll, error)
	SaveDeviceLogin(ctx context.Context, credential json.RawMessage) error
}

type ModelSetupFlow interface {
	ModelSetup(ctx context.Context, cfg *config.Config, currentModel string, args AuthArgs) error
}

type SetupEffortDeclarer interface {
	SetupEfforts(model string) ([]string, bool)
}

type ListingGate interface {
	Listed(ctx context.Context, pools PoolReader) (bool, error)
}

type Hook string

const (
	HookClientHeaders    Hook = "client_headers"
	HookRequestHeaders   Hook = "request_headers"
	HookMaxTokensParam   Hook = "max_tokens_param"
	HookReasoningPayload Hook = "reasoning_payload"
	HookReplayCarriers   Hook = "replay_message_carriers"
	HookReplaysSignature Hook = "replays_function_thought_signature"
	HookModeForModel     Hook = "api_mode_for_model"
	HookRefreshRuntime   Hook = "refresh_runtime_credentials"
	HookIsStale          Hook = "is_stale_credential_error"
	HookRefreshNotice    Hook = "credential_refresh_notice"
	HookRejectionHint    Hook = "request_rejection_hint"
)

type HookSet interface {
	Overrides(h Hook) bool
}

func Overrides(p Profile, h Hook) bool {
	if p == nil {
		return false
	}
	if set, ok := p.(HookSet); ok {
		return set.Overrides(h)
	}
	switch h {
	case HookClientHeaders:
		_, ok := p.(ClientHeaderer)
		return ok
	case HookRequestHeaders:
		_, ok := p.(RequestHeaderer)
		return ok
	case HookMaxTokensParam:
		_, ok := p.(MaxTokensFielder)
		return ok
	case HookReasoningPayload:
		_, ok := p.(ReasoningPayloader)
		return ok
	case HookReplayCarriers:
		_, ok := p.(CarrierReplayer)
		return ok
	case HookReplaysSignature:
		_, ok := p.(SignatureReplayer)
		return ok
	case HookModeForModel:
		_, ok := p.(ModeChooser)
		return ok
	case HookRefreshRuntime:
		_, ok := p.(RuntimeRefresher)
		return ok
	case HookIsStale:
		_, ok := p.(StaleDetector)
		return ok
	case HookRefreshNotice:
		_, ok := p.(RefreshNoticer)
		return ok
	case HookRejectionHint:
		_, ok := p.(RejectionHinter)
		return ok
	}
	return false
}
