package auth

import (
	"context"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type CredKind int

const (
	CredNone CredKind = iota
	CredAPIKey
	CredXAPIKey
	CredToken
	CredSigned
)

type SourceKind string

const (
	SourceEntraID      SourceKind = "entra-id"
	SourceVertexOAuth  SourceKind = "vertex-oauth"
	SourceKeyCmd       SourceKind = "key-cmd"
	SourceMiniMaxOAuth SourceKind = "minimax-oauth"
)

type Token struct {
	Value  Secret
	Expiry time.Time
}

type TokenSource interface {
	Token(ctx context.Context) (Token, error)
	Kind() SourceKind
}

type Credential struct {
	Kind      CredKind
	Key       Secret
	Tokens    TokenSource
	Sign      wire.Authorizer
	Source    string
	Label     string
	ExpiresAt time.Time
}

var sourceLabelKeys = map[SourceKind]string{
	SourceEntraID:      "cli.config.microsoft_entra_id",
	SourceVertexOAuth:  "cli.config.google_vertex_oauth",
	SourceKeyCmd:       "cli.config.key_cmd_token",
	SourceMiniMaxOAuth: "cli.config.minimax_oauth",
}

const unknownSourceLabelKey = "cli.config.token_provider"

func (c Credential) SourceKind() (SourceKind, bool) {
	if c.Tokens == nil {
		return "", false
	}
	kind := c.Tokens.Kind()
	return kind, kind != ""
}

func (c Credential) LabelKey() (string, bool) {
	if c.Tokens == nil {
		return "", false
	}
	if key, ok := sourceLabelKeys[c.Tokens.Kind()]; ok {
		return key, true
	}
	return unknownSourceLabelKey, true
}

func (c Credential) ProbeKey(ctx context.Context) Secret {
	if c.Tokens == nil {
		return NewSecret(strings.TrimSpace(c.Key.Reveal()))
	}
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		return Secret{}
	}
	return NewSecret(strings.TrimSpace(token.Value.Reveal()))
}

type Status struct {
	LoggedIn        bool
	Configured      bool
	Provider        string
	Name            string
	KeySource       string
	BaseURL         string
	AuthStore       string
	LastRefresh     string
	AuthMode        string
	Source          string
	APIKey          Secret
	Error           string
	ErrorCode       string
	RateLimited     bool
	ResetAt         time.Time
	Command         string
	Args            []string
	ResolvedCommand string
	AuthVerified    bool
	AuthSource      string
	NeedsRefresh    bool
	Accounts        int
	Hint            string
	Extra           map[string]string
}
