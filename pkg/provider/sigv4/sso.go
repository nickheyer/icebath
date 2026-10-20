package sigv4

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Token the AWS CLI caches after aws sso login, refreshed here when it holds a registration
type ssoCache struct {
	AccessToken           string  `json:"accessToken"`
	ExpiresAt             ssoTime `json:"expiresAt"`
	Region                string  `json:"region,omitempty"`
	StartURL              string  `json:"startUrl,omitempty"`
	RefreshToken          string  `json:"refreshToken,omitempty"`
	ClientID              string  `json:"clientId,omitempty"`
	ClientSecret          string  `json:"clientSecret,omitempty"`
	RegistrationExpiresAt ssoTime `json:"registrationExpiresAt,omitempty"`
}

// Time as the CLI writes it, RFC 3339 or the older form ending in UTC
type ssoTime struct{ time.Time }

func (t *ssoTime) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		t.Time = time.Time{}
		return nil
	}
	if rest, ok := strings.CutSuffix(s, "UTC"); ok {
		s = rest + "Z"
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

func (t ssoTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte(`""`), nil
	}
	return json.Marshal(t.UTC().Format(time.RFC3339))
}

// Resolves an SSO profile through the cached login token and the portal credentials call
func (p Profile) sso(ctx context.Context, sh shared, name string, section map[string]string) (Credentials, error) {
	startURL, region, key := section["sso_start_url"], section["sso_region"], section["sso_start_url"]
	if sess := section["sso_session"]; sess != "" {
		s, ok := sh.sessions[sess]
		if !ok {
			return Credentials{}, fmt.Errorf("sigv4: profile %q names sso-session %q, which %s lacks", name, sess, sh.paths)
		}
		startURL, region, key = s["sso_start_url"], s["sso_region"], sess
	}
	account, role := section["sso_account_id"], section["sso_role_name"]
	if startURL == "" || region == "" || account == "" || role == "" {
		return Credentials{}, fmt.Errorf("sigv4: profile %q needs sso_start_url, sso_region, sso_account_id and sso_role_name", name)
	}
	token, err := ssoToken(ctx, p.client(), key, region, name)
	if err != nil {
		return Credentials{}, err
	}
	return ssoCredentials(ctx, p.client(), region, token, account, role)
}

// Path of the cached token for a session name or start URL
func ssoCachePath(key string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	sum := sha1.Sum([]byte(key))
	return filepath.Join(home, ".aws", "sso", "cache", hex.EncodeToString(sum[:])+".json"), nil
}

// Loads the cached token, refreshing an expired one through OIDC when the cache holds a registration
func ssoToken(ctx context.Context, client *http.Client, key, region, profile string) (string, error) {
	path, err := ssoCachePath(key)
	if err != nil {
		return "", fmt.Errorf("sigv4: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("sigv4: profile %q has no sso login, run aws sso login --profile %s: %w", profile, profile, err)
	}
	var cache ssoCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return "", fmt.Errorf("sigv4: %s: %w", path, err)
	}
	if cache.AccessToken != "" && time.Now().Before(cache.ExpiresAt.Time) {
		return cache.AccessToken, nil
	}
	if cache.RefreshToken == "" || cache.ClientID == "" || cache.ClientSecret == "" || (!cache.RegistrationExpiresAt.IsZero() && time.Now().After(cache.RegistrationExpiresAt.Time)) {
		return "", fmt.Errorf("sigv4: sso login for profile %q expired, run aws sso login --profile %s", profile, profile)
	}
	if cache.Region == "" {
		cache.Region = region
	}
	if err := ssoRefresh(ctx, client, &cache); err != nil {
		return "", err
	}
	if err := writeFile(path, cache); err != nil {
		return "", fmt.Errorf("sigv4: %w", err)
	}
	return cache.AccessToken, nil
}

// Trades the refresh token for a new access token at the OIDC endpoint
func ssoRefresh(ctx context.Context, client *http.Client, cache *ssoCache) error {
	body, err := json.Marshal(map[string]string{"clientId": cache.ClientID, "clientSecret": cache.ClientSecret, "grantType": "refresh_token", "refreshToken": cache.RefreshToken})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oidc."+cache.Region+".amazonaws.com/token", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sigv4: sso oidc: %w", err)
	}
	var out struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := readJSON(resp, &out); err != nil {
		return fmt.Errorf("sigv4: sso oidc: %w", err)
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return errors.New("sigv4: sso oidc returned no access token")
	}
	cache.AccessToken = out.AccessToken
	cache.ExpiresAt = ssoTime{time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).UTC()}
	if out.RefreshToken != "" {
		cache.RefreshToken = out.RefreshToken
	}
	return nil
}

// Asks the SSO portal for the role credentials the token grants
func ssoCredentials(ctx context.Context, client *http.Client, region, token, account, role string) (Credentials, error) {
	q := url.Values{"account_id": {account}, "role_name": {role}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://portal.sso."+region+".amazonaws.com/federation/credentials?"+q.Encode(), nil)
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("x-amz-sso_bearer_token", token)
	resp, err := client.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: sso portal: %w", err)
	}
	var out struct {
		RoleCredentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      int64  `json:"expiration"`
		} `json:"roleCredentials"`
	}
	if err := readJSON(resp, &out); err != nil {
		return Credentials{}, fmt.Errorf("sigv4: sso portal: %w", err)
	}
	rc := out.RoleCredentials
	c := Credentials{AccessKey: rc.AccessKeyID, SecretKey: rc.SecretAccessKey, SessionToken: rc.SessionToken}
	if !c.valid() {
		return Credentials{}, errors.New("sigv4: sso portal returned no key pair")
	}
	if rc.Expiration > 0 {
		c.Expires = time.UnixMilli(rc.Expiration).UTC()
	}
	return c, nil
}

// Writes the cache document through a temporary file readable only by the owner
func writeFile(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
