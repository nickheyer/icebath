// Inference provider profiles, their registry and the identity tables the model layer resolves providers with
package providers

import (
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type Profile interface {
	Spec() *Spec
}

type Streaming uint8

const (
	StreamingDefault Streaming = iota
	StreamingOn
	StreamingOff
)

type Process struct {
	Command    string
	Args       []string
	CommandEnv []string
	ArgsEnv    string
}

type ResponsesQuirks struct {
	DropItemIDs        bool
	NoPromptCacheKey   bool
	IssuerKind         string
	DistrustsCompleted bool
	NoNativeCompaction bool
	ProfileReasoning   bool
}

type PickerGroup struct {
	ID          string
	Label       string
	Description string
}

type Picker struct {
	After          string
	Group          *PickerGroup
	CatalogAliases bool
	Aggregator     bool
}

type Overlay struct {
	Transport       string
	Aggregator      bool
	AuthType        string
	ExtraEnv        []string
	BaseURLOverride string
	BaseURLEnv      string
}

type AccountCard struct {
	Name       string
	Flow       string
	CLICommand string
	DocsURL    string
}

func (c AccountCard) IsZero() bool {
	return c.Name == "" && c.Flow == "" && c.CLICommand == "" && c.DocsURL == ""
}

type GeminiClient struct {
	Options           wire.GeminiOptions
	DefaultBaseURL    string
	MissingKeyMessage string
}

type Spec struct {
	Name        string
	Aliases     []string
	DisplayName string
	Description string
	SignupURL   string
	Hidden      bool

	Mode             wire.Mode
	AuthType         string
	RegistryAuthType string
	RegistryAfter    string
	EnvVars          []string
	BaseURL          string
	BaseURLEnv       string
	ModelsURL        string
	Streaming        Streaming

	SkipHealthCheck           bool
	SkipModelListing          bool
	Vision                    bool
	RejectsVisionToolMessages bool
	PromptCacheKey            bool

	NativeDetailsType     string
	CarrierOpaque         string
	CarrierText           string
	ReplaysToolSignatures bool

	Process Process

	FallbackModels []string
	ModelAliases   map[string]string
	StaticModels   []string

	Hostname          string
	Hostnames         []string
	MetadataHostnames []string
	ModelsDevID       string

	DefaultHeaders             map[string]string
	FixedTemperature           *float64
	OmitTemperature            bool
	DefaultMaxTokens           int
	UnsupportedResponseFormats []string

	DefaultAuxModel   string
	ModelCapabilities map[string]config.ModelOverride

	AmbientCredentials bool

	Responses ResponsesQuirks
	Picker    Picker

	Overlay        *Overlay
	OverlayAliases []string

	AccountCard AccountCard

	CatalogCredentialFiles []string

	Gemini *GeminiClient
}

func (s *Spec) ModeOrDefault() wire.Mode {
	if s.Mode == "" {
		return wire.ModeChat
	}
	return s.Mode
}

func (s *Spec) AuthTypeOrDefault() string {
	if s.AuthType == "" {
		return authTypeAPIKey
	}
	return s.AuthType
}

func (s *Spec) Host() string {
	if s.Hostname != "" {
		return s.Hostname
	}
	if s.BaseURL != "" {
		return URLHostname(s.BaseURL)
	}
	return ""
}

func (s *Spec) Label() string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Name
}

func (s *Spec) KeyEnvVars() []string {
	keys := []string{}
	for _, v := range s.EnvVars {
		if !isURLVar(v) {
			keys = append(keys, v)
		}
	}
	return keys
}

func (s *Spec) URLEnvVar() string {
	for _, v := range s.EnvVars {
		if isURLVar(v) {
			return v
		}
	}
	return ""
}

func MaxTokensFor(p Profile, model string) (int, bool) {
	if c, ok := p.(MaxTokensCapper); ok {
		return c.MaxTokens(model)
	}
	s := p.Spec()
	return s.DefaultMaxTokens, s.DefaultMaxTokens > 0
}

func isURLVar(name string) bool {
	return strings.HasSuffix(name, "_BASE_URL") || strings.HasSuffix(name, "_URL")
}

const (
	authTypeAPIKey          = "api_key"
	authTypeOAuthDeviceCode = "oauth_device_code"
	authTypeOAuthExternal   = "oauth_external"
	authTypeOAuthMiniMax    = "oauth_minimax"
	authTypeExternalProcess = "external_process"
	authTypeAWSSDK          = "aws_sdk"
	authTypeVertex          = "vertex"
)
