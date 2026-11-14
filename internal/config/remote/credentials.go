package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
)

const idpPrefix = "GATEWAY_RELAY_IDP_"

type PortalCredential interface {
	AccessToken(ctx context.Context, home string) (string, error)
	NeedsLogin(err error) bool
}

func credentialEnvNames() []string {
	return []string{
		config.EnvConfigBackend, EnvURL, EnvInstance,
		idpPrefix + "TOKEN_URL", idpPrefix + "CLIENT_ID", idpPrefix + "CLIENT_SECRET", idpPrefix + "SCOPE",
		"ICEBATH_SHARED_AUTH_DIR", "ICEBATH_PORTAL_BASE_URL",
	}
}

type idpSettings struct {
	tokenURL, clientID, clientSecret, scope string
}

func idpEnv() idpSettings {
	get := func(k string) string { return strings.TrimSpace(os.Getenv(idpPrefix + k)) }
	return idpSettings{tokenURL: get("TOKEN_URL"), clientID: get("CLIENT_ID"), clientSecret: get("CLIENT_SECRET"), scope: get("SCOPE")}
}

type idpKey struct {
	tokenURL, clientID, scope, secretDigest string
}

type idpToken struct {
	expires time.Time
	token   string
}

func (b *Backend) bearer(ctx context.Context, h string) (string, error) {
	if _, err := checkedURL(baseURL(), EnvURL); err != nil {
		return "", err
	}
	return b.planeToken(ctx, h)
}

func (b *Backend) planeToken(ctx context.Context, h string) (string, error) {
	if idpEnv().tokenURL != "" {
		return b.idpToken(ctx)
	}
	return b.portalToken(ctx, h)
}

func (b *Backend) portalToken(ctx context.Context, h string) (string, error) {
	if b.portal == nil {
		return "", &credentialError{msg: fmt.Sprintf("cannot load the Icebath Portal auth module: %v", errNoPortal), err: errNoPortal}
	}
	token, err := b.portal.AccessToken(home.WithOverride(ctx, h), h)
	if err != nil {
		return "", &credentialError{msg: fmt.Sprintf("no Icebath Portal access token for %s (auth.json): %v", h, err),
			retryable: !b.portal.NeedsLogin(err), err: err}
	}
	return token, nil
}

func (b *Backend) idpToken(ctx context.Context) (string, error) {
	env := idpEnv()
	if env.clientID == "" || env.clientSecret == "" {
		return "", &credentialError{msg: idpPrefix + "TOKEN_URL is set but " + idpPrefix + "CLIENT_ID / " + idpPrefix +
			"CLIENT_SECRET are not: Remote Config needs the OAuth2 client-credentials grant (both values in .env)."}
	}
	if _, err := checkedURL(env.tokenURL, idpPrefix+"TOKEN_URL"); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(env.clientSecret))
	key := idpKey{tokenURL: env.tokenURL, clientID: env.clientID, scope: env.scope, secretDigest: hex.EncodeToString(sum[:])}
	now := time.Now()
	b.idpMu.Lock()
	if cached, ok := b.idpCache[key]; ok && cached.expires.After(now) {
		b.idpMu.Unlock()
		return cached.token, nil
	}
	b.idpMu.Unlock()
	form := "grant_type=client_credentials&client_id=" + url.QueryEscape(env.clientID) +
		"&client_secret=" + url.QueryEscape(env.clientSecret)
	if env.scope != "" {
		form += "&scope=" + url.QueryEscape(env.scope)
	}
	failed := func(reason any) error {
		return &credentialError{msg: fmt.Sprintf("IdP token request to %s failed: %v", displayURL(env.tokenURL), reason), retryable: true}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env.tokenURL, strings.NewReader(form))
	if err != nil {
		return "", failed(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, data, err := b.send(req)
	if err != nil {
		if ce, ok := err.(*credentialError); ok {
			return "", ce
		}
		return "", failed(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", failed(fmt.Sprintf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
	if len(data) == 0 {
		data = []byte("{}")
	}
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", failed(err)
	}
	obj, _ := payload.(map[string]any)
	access, _ := obj["access_token"].(string)
	if access == "" {
		return "", &credentialError{msg: "IdP client-credentials response had no access_token", retryable: true}
	}
	ttl := 60.0
	if v, ok := obj["expires_in"].(float64); ok && v > 0 {
		ttl = v
	}
	b.idpMu.Lock()
	if b.idpCache == nil {
		b.idpCache = map[idpKey]idpToken{}
	}
	b.idpCache[key] = idpToken{expires: now.Add(time.Duration(min(max(ttl-30, 0), 9e9) * float64(time.Second))), token: access}
	b.idpMu.Unlock()
	return access, nil
}
