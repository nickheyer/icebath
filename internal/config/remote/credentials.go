package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Variables of the OAuth2 client credentials grant the plane's bearer comes from
const (
	TokenURLEnv     = "GATEWAY_RELAY_IDP_TOKEN_URL"
	ClientIDEnv     = "GATEWAY_RELAY_IDP_CLIENT_ID"
	ClientSecretEnv = "GATEWAY_RELAY_IDP_CLIENT_SECRET"
	ScopeEnv        = "GATEWAY_RELAY_IDP_SCOPE"
)

// Variables that decide which bearer goes to which plane, besides the one choosing Remote Config: a secret store may not supply them
var CredentialEnv = []string{URLEnv, InstanceEnv, TokenURLEnv, ClientIDEnv, ClientSecretEnv, ScopeEnv}

// No credential for the plane could be had; Retryable false when only a change of the settings helps
type CredentialError struct {
	Message   string
	Retryable bool
}

func (e *CredentialError) Error() string { return e.Message }

// Token the identity provider gave, until shortly before it expires
type grant struct {
	expires time.Time
	token   string
}

// Bearer for the plane once its address passes the transport policy
func (b *Backend) bearer(ctx context.Context, env Env) (string, error) {
	if _, err := planeURL(env); err != nil {
		return "", err
	}
	return b.token(ctx, env)
}

// Fresh bearer from the client credentials grant, an error naming what is missing when none is set up
func (b *Backend) token(ctx context.Context, env Env) (string, error) {
	tokenURL, clientID := strings.TrimSpace(env(TokenURLEnv)), strings.TrimSpace(env(ClientIDEnv))
	clientSecret, scope := strings.TrimSpace(env(ClientSecretEnv)), strings.TrimSpace(env(ScopeEnv))
	if tokenURL == "" {
		return "", &CredentialError{Message: "The remote settings need a sign-in. Set " + TokenURLEnv + ", " + ClientIDEnv + " and " +
			ClientSecretEnv + " in the keys file, as the OAuth2 client credentials grant uses them."}
	}
	if clientID == "" || clientSecret == "" {
		return "", &CredentialError{Message: TokenURLEnv + " is set, but " + ClientIDEnv + " and " + ClientSecretEnv +
			" are not both set. The remote settings need both in the keys file."}
	}
	if _, err := checkedURL(tokenURL, TokenURLEnv); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(clientSecret))
	key := strings.Join([]string{tokenURL, clientID, scope, hex.EncodeToString(digest[:])}, "\x00")
	now := time.Now()
	b.mu.Lock()
	cached, ok := b.grants[key]
	b.mu.Unlock()
	if ok && cached.expires.After(now) {
		return cached.token, nil
	}
	form := "grant_type=client_credentials&client_id=" + url.QueryEscape(clientID) + "&client_secret=" + url.QueryEscape(clientSecret)
	if scope != "" {
		form += "&scope=" + url.QueryEscape(scope)
	}
	payload, err := postGrant(ctx, tokenURL, form)
	if err != nil {
		if _, refused := errors.AsType[*DestinationError](err); refused {
			return "", err
		}
		return "", &CredentialError{Message: fmt.Sprintf("The sign-in at %s failed: %v.", DisplayURL(tokenURL), err), Retryable: true}
	}
	accessToken, _ := payload["access_token"].(string)
	if accessToken == "" {
		return "", &CredentialError{Message: "The sign-in server answered without an access token.", Retryable: true}
	}
	ttl := 60.0
	switch seconds := payload["expires_in"].(type) {
	case int:
		if seconds > 0 {
			ttl = float64(seconds)
		}
	case float64:
		if seconds > 0 {
			ttl = seconds
		}
	}
	b.mu.Lock()
	b.grants[key] = grant{expires: now.Add(time.Duration(max(ttl-30, 0) * float64(time.Second))), token: accessToken}
	b.mu.Unlock()
	return accessToken, nil
}

// Posts the client credentials form and decodes the answer, empty when it is not a JSON object
func postGrant(ctx context.Context, tokenURL, body string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := send(req, requestTimeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("the server answered HTTP %d %s", resp.StatusCode, strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" "))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 && (!utf8.Valid(raw) || !json.Valid(raw)) {
		return nil, errors.New("the answer is not valid JSON")
	}
	return decodeBody(raw), nil
}
