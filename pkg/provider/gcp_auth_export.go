package provider

import (
	"context"
	"time"
)

// Google OAuth credential that mints bearer tokens on demand, safe for concurrent use
type GoogleCredentials struct{ c *gcpCredentials }

// Service account credential from a parsed key file, scoped as given
func GoogleServiceAccountCredentials(info map[string]any, scopes []string) (*GoogleCredentials, error) {
	c, err := gcpServiceAccountFromInfo(info, scopes)
	if err != nil {
		return nil, err
	}
	return &GoogleCredentials{c}, nil
}

// End user credential refreshed through its refresh token, scoped as given
func GoogleAuthorizedUserCredentials(info map[string]any, scopes []string) (*GoogleCredentials, error) {
	c, err := gcpAuthorizedUserFromInfo(info, scopes)
	if err != nil {
		return nil, err
	}
	return &GoogleCredentials{c}, nil
}

// Application default credential and its project, from the env key file, the gcloud login or the metadata server
func GoogleDefaultCredentials(ctx context.Context, scopes []string) (*GoogleCredentials, string, error) {
	c, project, err := gcpDefaultCredentials(ctx, scopes)
	if err != nil {
		return nil, "", err
	}
	return &GoogleCredentials{c}, project, nil
}

// Valid bearer token, refreshed first when missing or near expiry
func (g *GoogleCredentials) Token(ctx context.Context) (string, error) {
	g.c.mu.Lock()
	defer g.c.mu.Unlock()
	if !g.c.Valid() {
		if err := g.c.Refresh(ctx); err != nil {
			return "", err
		}
	}
	return g.c.token, nil
}

// Mints a fresh token regardless of the cached one
func (g *GoogleCredentials) Refresh(ctx context.Context) error {
	g.c.mu.Lock()
	defer g.c.mu.Unlock()
	return g.c.Refresh(ctx)
}

// Cached token and its expiry, zero values before the first mint
func (g *GoogleCredentials) Current() (string, time.Time) {
	g.c.mu.Lock()
	defer g.c.mu.Unlock()
	return g.c.token, g.c.expiry
}

// Installs a token minted earlier so it is used until its expiry passes
func (g *GoogleCredentials) Seed(token string, expiry time.Time) {
	g.c.mu.Lock()
	defer g.c.mu.Unlock()
	g.c.token, g.c.expiry = token, expiry
}

// Quota project the credential bills, empty when none is set
func (g *GoogleCredentials) QuotaProject() string { return g.c.quotaProjectID }
