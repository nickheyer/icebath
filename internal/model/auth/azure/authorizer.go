package azure

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	DetectTimeout   = 6 * time.Second
	detectUserAgent = "icebath/azure-detect"
	detectBodyLimit = 1024 * 1024
)

type Mode string

const (
	ModeAPIKey  Mode = "api_key"
	ModeEntraID Mode = "entra_id"
)

var authHeaders = []string{"Authorization", "Api-Key", "X-Api-Key"}

type BearerAuthorizer struct {
	Tokens        auth.TokenSource
	SendsAsAPIKey func(token string) bool
}

func (a BearerAuthorizer) Authorize(ctx context.Context, r *http.Request, _ []byte) error {
	var value string
	err := errors.New("no usable token provider")
	if a.Tokens != nil {
		var token auth.Token
		if token, err = a.Tokens.Token(ctx); err == nil {
			if value = token.Value.Reveal(); value == "" {
				err = errors.New("token provider returned empty value")
			}
		}
	}
	for _, name := range authHeaders {
		r.Header.Del(name)
	}
	if err != nil {
		slog.Warn("Bearer hook: Entra ID token provider returned empty - stripping Authorization headers. Azure will respond 401. Run `icebath doctor` or `az login` to recover.", "error", err)
		return nil
	}
	if a.SendsAsAPIKey != nil && a.SendsAsAPIKey(value) {
		r.Header.Set("x-api-key", value)
	} else {
		r.Header.Set("Authorization", "Bearer "+value)
	}
	return nil
}

func ResolveCredential(ctx context.Context, cred auth.Credential) (auth.Secret, Mode) {
	if cred.Tokens == nil {
		return cred.Key, ModeAPIKey
	}
	token, err := cred.Tokens.Token(ctx)
	if err != nil {
		slog.Debug("azure_detect: token provider failed", "error", err)
		return auth.Secret{}, ModeEntraID
	}
	return token.Value, ModeEntraID
}

func AuthorizeDetection(ctx context.Context, r *http.Request, cred auth.Credential) {
	token, mode := ResolveCredential(ctx, cred)
	if !token.Empty() {
		if mode != ModeEntraID {
			r.Header.Set("api-key", token.Reveal())
		}
		r.Header.Set("Authorization", "Bearer "+token.Reveal())
	}
	r.Header.Set("User-Agent", detectUserAgent)
}

func GetJSON(ctx context.Context, hc *http.Client, target string, cred auth.Credential, timeout time.Duration) (int, map[string]any) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		slog.Debug("azure_detect: GET could not be built", "error", err)
		return 0, nil
	}
	AuthorizeDetection(ctx, req, cred)
	resp, err := hc.Do(req)
	if err != nil {
		slog.Debug("azure_detect: GET failed", "error", httpclient.Message(err))
		return 0, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, detectBodyLimit+1))
	if err != nil || len(data) > detectBodyLimit {
		return resp.StatusCode, nil
	}
	var body map[string]any
	if json.Unmarshal(data, &body) != nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, body
}
