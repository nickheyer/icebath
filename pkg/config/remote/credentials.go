package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

const idpEnvPrefix = "GATEWAY_RELAY_IDP_"

var idpKeys = []string{"token_url", "client_id", "client_secret", "scope"}

// Every env name deciding which plane token is sent where, refused from a secret source at boot
var PlaneCredentialEnvNames = []string{
	config.BackendEnv, URLEnv, InstanceEnv,
	idpEnvPrefix + "TOKEN_URL", idpEnvPrefix + "CLIENT_ID", idpEnvPrefix + "CLIENT_SECRET", idpEnvPrefix + "SCOPE",
	"ICEBATH_SHARED_AUTH_DIR", "ICEBATH_PORTAL_BASE_URL", "NOUS_PORTAL_BASE_URL",
}

// No plane credential could be obtained, Retryable false when only a configuration change helps
type PlaneCredentialError struct {
	Message   string
	Retryable bool
}

func (e *PlaneCredentialError) Error() string { return e.Message }

// Failure of the Nous access token resolver, Fatal for a missing login or a revoked session
type NousTokenError struct {
	Err   error
	Fatal bool
}

func (e *NousTokenError) Error() string { return e.Err.Error() }
func (e *NousTokenError) Unwrap() error { return e.Err }

var (
	nousResolverMu sync.RWMutex
	nousResolver   func(ctx context.Context, home string) (string, error)
)

// Installs the auth.json access token resolver the cloud plane credential comes from
func RegisterNousAccessToken(resolve func(ctx context.Context, home string) (string, error)) {
	nousResolverMu.Lock()
	defer nousResolverMu.Unlock()
	nousResolver = resolve
}

type idpCacheKey struct{ tokenURL, clientID, scope, secretDigest string }

type idpCached struct {
	expires time.Time
	token   string
}

var (
	idpMu    sync.Mutex
	idpCache = map[idpCacheKey]idpCached{}
)

func idpEnv() map[string]string {
	out := make(map[string]string, len(idpKeys))
	for _, k := range idpKeys {
		out[k] = strings.TrimSpace(os.Getenv(idpEnvPrefix + strings.ToUpper(k)))
	}
	return out
}

// Credential source in use, idp when an IdP token URL is configured and nous otherwise
func CredentialKind() string {
	if idpEnv()["token_url"] != "" {
		return "idp"
	}
	return "nous"
}

// Fresh bearer for the requests of home, resolved again on every request
func PlaneToken(ctx context.Context, home string) (string, error) {
	if CredentialKind() == "idp" {
		return idpToken(ctx)
	}
	return nousToken(ctx, home)
}

func nousToken(ctx context.Context, home string) (string, error) {
	nousResolverMu.RLock()
	resolve := nousResolver
	nousResolverMu.RUnlock()
	if resolve == nil {
		return "", &PlaneCredentialError{Message: "cannot load the Nous auth module: no Nous access token resolver is registered"}
	}
	token, err := resolve(ctx, home)
	if err != nil {
		var nerr *NousTokenError
		fatal := errors.As(err, &nerr) && nerr.Fatal
		return "", &PlaneCredentialError{Message: fmt.Sprintf("no Nous access token for %s (auth.json): %v", home, err), Retryable: !fatal}
	}
	return token, nil
}

func idpToken(ctx context.Context) (string, error) {
	env := idpEnv()
	tokenURL, clientID, clientSecret, scope := env["token_url"], env["client_id"], env["client_secret"], env["scope"]
	if clientID == "" || clientSecret == "" {
		return "", &PlaneCredentialError{Message: idpEnvPrefix + "TOKEN_URL is set but " + idpEnvPrefix + "CLIENT_ID / " + idpEnvPrefix +
			"CLIENT_SECRET are not: Remote Config needs the OAuth2 client-credentials grant (both values in .env)."}
	}
	if _, err := CheckedURL(tokenURL, idpEnvPrefix+"TOKEN_URL"); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(clientSecret))
	key := idpCacheKey{tokenURL, clientID, scope, hex.EncodeToString(digest[:])}
	now := time.Now()
	idpMu.Lock()
	cached, ok := idpCache[key]
	idpMu.Unlock()
	if ok && cached.expires.After(now) {
		return cached.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	if scope != "" {
		form.Set("scope", scope)
	}
	payload, err := postIDPForm(ctx, tokenURL, encodeForm(form, scope != ""))
	if err != nil {
		var derr *CredentialDestinationError
		if errors.As(err, &derr) {
			return "", err
		}
		return "", &PlaneCredentialError{Message: fmt.Sprintf("IdP token request to %s failed: %v", DisplayURL(tokenURL), err), Retryable: true}
	}
	obj, _ := payload.(map[string]any)
	accessToken, _ := obj["access_token"].(string)
	if accessToken == "" {
		return "", &PlaneCredentialError{Message: "IdP client-credentials response had no access_token", Retryable: true}
	}
	ttl := 60.0
	switch v := obj["expires_in"].(type) {
	case bool:
		if v {
			ttl = 1
		}
	case int:
		if v > 0 {
			ttl = float64(v)
		}
	case float64:
		if v > 0 {
			ttl = v
		}
	}
	idpMu.Lock()
	idpCache[key] = idpCached{expires: now.Add(time.Duration(max(ttl-30, 0) * float64(time.Second))), token: accessToken}
	idpMu.Unlock()
	return accessToken, nil
}

// Form body in the insertion order urlencode keeps, scope last when present
func encodeForm(form url.Values, withScope bool) string {
	keys := []string{"grant_type", "client_id", "client_secret"}
	if withScope {
		keys = append(keys, "scope")
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(form.Get(k)))
	}
	return strings.Join(parts, "&")
}

// Posts the client credentials form, an HTTP error reading like urllib HTTPError
func postIDPForm(ctx context.Context, tokenURL, body string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := openCredentialed(req, httpTimeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" "))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return loadJSON(raw)
}
