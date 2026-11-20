package provider

import (
	"context"
)

// Credential is one usable login for a provider
type Credential struct {
	APIKey      string
	TokenSource func(ctx context.Context) (string, error)
	BaseURL     string
	Source      string
	Pool        *Pool
}

// CredentialSource answers which login a provider has
type CredentialSource interface {
	Credential(provider string) (Credential, bool)
}
