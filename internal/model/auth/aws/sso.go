package aws

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

const ssoExpiredMessage = "the SSO session associated with this profile has expired or is otherwise invalid, run aws sso login with the corresponding profile to refresh it"

type ssoTokenFile struct {
	StartURL              string `json:"startUrl"`
	Region                string `json:"region"`
	AccessToken           string `json:"accessToken"`
	ExpiresAt             string `json:"expiresAt"`
	ClientID              string `json:"clientId"`
	ClientSecret          string `json:"clientSecret"`
	RegistrationExpiresAt string `json:"registrationExpiresAt"`
	RefreshToken          string `json:"refreshToken,omitempty"`
}

func (r *resolver) sso(profile string) (provider, error) {
	const source = "sso"
	p := r.s.files.profiles[profile]
	_, hasRole := p["sso_role_name"]
	_, hasAccount := p["sso_account_id"]
	if !hasRole && !hasAccount {
		return nil, nil
	}
	resolved := make(section, len(p))
	for k, v := range p {
		resolved[k] = v
	}
	required := []string{"sso_role_name", "sso_account_id", "sso_start_url", "sso_region"}
	sessionName, hasSession := p["sso_session"]
	if hasSession {
		shared, ok := r.s.files.ssoSessions[sessionName]
		if !ok {
			return nil, &Error{Source: source, Message: fmt.Sprintf("the specified sso-session does not exist: %q", sessionName)}
		}
		for k, v := range shared {
			if prior, ok := resolved[k]; ok && prior != v {
				return nil, &Error{Source: source, Message: fmt.Sprintf("the value for %s is inconsistent between profile (%s) and sso-session (%s)", k, prior, v)}
			}
			resolved[k] = v
		}
	}
	var missing []string
	for _, k := range required {
		if _, ok := resolved[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, &Error{Source: source, Message: fmt.Sprintf("the profile %q is configured to use SSO but is missing required configuration: %s", profile, strings.Join(missing, ", "))}
	}
	startURL, region := resolved["sso_start_url"], resolved["sso_region"]
	account, role := resolved["sso_account_id"], resolved["sso_role_name"]
	if !hasSession {
		sessionName = ""
	}
	fetch := func(ctx context.Context) (Credentials, error) {
		token, err := r.ssoToken(ctx, startURL, region, sessionName, hasSession)
		if err != nil {
			return Credentials{}, err
		}
		return r.ssoRoleCredentials(ctx, region, account, role, token)
	}
	return newRefreshing(fetch, nil), nil
}

func (r *resolver) ssoToken(ctx context.Context, startURL, region, sessionName string, hasSession bool) (string, error) {
	const source = "sso"
	key := startURL
	if hasSession {
		key = sessionName
	}
	sum := sha1.Sum([]byte(key))
	path := cacheFile(awsDir("sso", "cache"), hex.EncodeToString(sum[:]))
	data, err := readBounded(path)
	if err != nil {
		return "", &Error{Source: source, Message: "error loading SSO token: token for " + key + " does not exist", Err: err}
	}
	var tok ssoTokenFile
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" || tok.ExpiresAt == "" {
		return "", &Error{Source: source, Message: "error loading SSO token: token for " + startURL + " is invalid"}
	}
	if !hasSession {
		return tok.AccessToken, nil
	}
	expires, err := parseTime(tok.ExpiresAt)
	if err != nil {
		return "", &Error{Source: source, Message: "error loading SSO token: token for " + key + " has an unreadable expiry", Err: err}
	}
	if expires.Sub(time.Now()) < advisoryRefresh {
		if fresh, freshExpires, ok := r.refreshSSOToken(ctx, startURL, region, tok); ok {
			if err := fileutil.WriteJSON(ctx, path, fresh, fileutil.JSONStyle{ASCII: true}, fileutil.Private()); err != nil {
				return "", &Error{Source: source, Message: "the refreshed SSO token could not be stored, the cached refresh token is now spent", Err: err}
			}
			tok, expires = fresh, freshExpires
		}
	}
	if !expires.After(time.Now()) {
		return "", &Error{Source: source, Message: "token has expired and refresh failed, run aws sso login"}
	}
	return tok.AccessToken, nil
}

func (r *resolver) refreshSSOToken(ctx context.Context, startURL, region string, tok ssoTokenFile) (ssoTokenFile, time.Time, bool) {
	if tok.RefreshToken == "" || tok.ClientID == "" || tok.ClientSecret == "" || tok.RegistrationExpiresAt == "" {
		slog.Info("unable to refresh SSO token, the cached token lacks its client registration")
		return ssoTokenFile{}, time.Time{}, false
	}
	registered, err := parseTime(tok.RegistrationExpiresAt)
	if err != nil || !registered.After(time.Now()) {
		slog.Info("SSO token registration expired", "at", tok.RegistrationExpiresAt)
		return ssoTokenFile{}, time.Time{}, false
	}
	body, err := json.Marshal(map[string]string{"grantType": "refresh_token", "clientId": tok.ClientID, "clientSecret": tok.ClientSecret, "refreshToken": tok.RefreshToken})
	if err != nil {
		return ssoTokenFile{}, time.Time{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.s.endpoint("sso-oidc", "oidc", region)+"/token", bytes.NewReader(body))
	if err != nil {
		return ssoTokenFile{}, time.Time{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	status, _, data, err := send(r.c.api, req, "sso")
	if err != nil {
		slog.Warn("SSO token refresh attempt failed", "error", err)
		return ssoTokenFile{}, time.Time{}, false
	}
	var out struct {
		AccessToken  string  `json:"accessToken"`
		ExpiresIn    float64 `json:"expiresIn"`
		RefreshToken string  `json:"refreshToken"`
	}
	if status != http.StatusOK || json.Unmarshal(data, &out) != nil || out.AccessToken == "" {
		slog.Warn("SSO token refresh attempt failed", "status", status)
		return ssoTokenFile{}, time.Time{}, false
	}
	expires := time.Now().UTC().Add(time.Duration(out.ExpiresIn * float64(time.Second)))
	return ssoTokenFile{
		StartURL: startURL, Region: region, AccessToken: out.AccessToken, ExpiresAt: expires.Format("2006-01-02T15:04:05") + "UTC",
		ClientID: tok.ClientID, ClientSecret: tok.ClientSecret, RegistrationExpiresAt: tok.RegistrationExpiresAt, RefreshToken: out.RefreshToken,
	}, expires, true
}

func errorType(h http.Header) string {
	v := h.Get("x-amzn-ErrorType")
	if i := strings.IndexByte(v, ':'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func (r *resolver) ssoRoleCredentials(ctx context.Context, region, account, role, token string) (Credentials, error) {
	const source = "sso"
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	query := url.Values{"role_name": {role}, "account_id": {account}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.s.endpoint("sso", "portal.sso", region)+"/federation/credentials?"+query.Encode(), nil)
	if err != nil {
		return Credentials{}, &Error{Source: source, Message: "the SSO portal endpoint is not a usable URL", Err: err}
	}
	req.Header.Set("x-amz-sso_bearer_token", token)
	status, header, data, err := send(r.c.api, req, source)
	if err != nil {
		return Credentials{}, err
	}
	if status == http.StatusUnauthorized {
		return Credentials{}, &Error{Source: source, Message: ssoExpiredMessage}
	}
	if status != http.StatusOK {
		var fault struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &fault)
		return Credentials{}, &ServiceError{Service: "sso", Operation: "GetRoleCredentials", Status: status, Code: errorType(header), Message: fault.Message}
	}
	var out struct {
		RoleCredentials struct {
			AccessKeyID     string  `json:"accessKeyId"`
			SecretAccessKey string  `json:"secretAccessKey"`
			SessionToken    string  `json:"sessionToken"`
			Expiration      float64 `json:"expiration"`
		} `json:"roleCredentials"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Credentials{}, &Error{Source: source, Message: "the SSO portal reply is not readable JSON"}
	}
	rc := out.RoleCredentials
	creds := Credentials{AccessKeyID: rc.AccessKeyID, SecretAccessKey: rc.SecretAccessKey, SessionToken: rc.SessionToken, Source: source}
	if !creds.complete() {
		return Credentials{}, &Error{Source: source, Message: "the SSO portal reply carried no credentials"}
	}
	creds.Expires = time.UnixMilli(int64(rc.Expiration)).UTC().Truncate(time.Second)
	return creds, nil
}

type loginAccess struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken"`
	AccountID       string `json:"accountId,omitempty"`
	ExpiresAt       string `json:"expiresAt,omitempty"`
}

func (r *resolver) login(profile string) (provider, error) {
	session, ok := r.s.files.profiles[profile]["login_session"]
	if !ok {
		return nil, nil
	}
	fetch := func(ctx context.Context) (Credentials, error) { return r.loginCredentials(ctx, session) }
	return newRefreshing(fetch, nil), nil
}

func loginCachePath(session string) string {
	dir := os.Getenv("AWS_LOGIN_CACHE_DIRECTORY")
	if dir == "" {
		dir = awsDir("login", "cache")
	} else {
		dir = expandPath(dir)
	}
	sum := sha256.Sum256([]byte(session))
	return cacheFile(dir, hex.EncodeToString(sum[:]))
}

func (r *resolver) loginCredentials(ctx context.Context, session string) (Credentials, error) {
	const source = "login"
	const remedy = ", run aws login to sign in again"
	path := loginCachePath(session)
	data, err := readBounded(path)
	if err != nil {
		return Credentials{}, &Error{Source: source, Message: "no cached login for session " + session + remedy, Err: err}
	}
	var members map[string]json.RawMessage
	var access loginAccess
	if err := json.Unmarshal(data, &members); err != nil || json.Unmarshal(members["accessToken"], &access) != nil {
		return Credentials{}, &Error{Source: source, Message: "the cached login for session " + session + " is unreadable" + remedy}
	}
	cached := Credentials{AccessKeyID: access.AccessKeyID, SecretAccessKey: access.SecretAccessKey, SessionToken: access.SessionToken, Source: source}
	expires, err := parseTime(access.ExpiresAt)
	if err != nil || !cached.complete() {
		return Credentials{}, &Error{Source: source, Message: "the cached login for session " + session + " is incomplete" + remedy}
	}
	cached.Expires = expires
	if !refreshDue(cached, time.Now(), advisoryRefresh) {
		return cached, nil
	}
	fresh, err := r.refreshLogin(ctx, path, members, access)
	if err != nil {
		if cached.Expires.After(time.Now()) {
			slog.Warn("refreshing the AWS login session failed, using the cached credentials", "error", err)
			return cached, nil
		}
		return Credentials{}, err
	}
	return fresh, nil
}

func memberText(members map[string]json.RawMessage, key string) string {
	var s string
	if json.Unmarshal(members[key], &s) != nil {
		return ""
	}
	return s
}

func (r *resolver) signinEndpoint() string {
	if ep := r.s.configuredEndpoint("signin"); ep != "" {
		return strings.TrimRight(ep, "/")
	}
	region := strings.TrimSpace(r.s.region())
	if region == "" {
		region = strings.TrimSpace(r.c.opts.Region)
	}
	if region == "" {
		region = "us-east-1"
	}
	p := partitionOf(region)
	switch p.name {
	case "aws":
		return "https://" + region + ".signin.aws.amazon.com"
	case "aws-cn":
		return "https://" + region + ".signin.amazonaws.cn"
	case "aws-us-gov":
		return "https://" + region + ".signin.amazonaws-us-gov.com"
	}
	return "https://signin." + region + "." + p.dnsSuffix
}

func b64url(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

func dpopProof(keyPEM, method, target string, now time.Time) (string, error) {
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return "", fmt.Errorf("the proof key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		parsed, perr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if perr != nil {
			return "", fmt.Errorf("the proof key is not an EC private key")
		}
		ec, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("the proof key is not an EC private key")
		}
		key = ec
	}
	pub, err := key.PublicKey.ECDH()
	if err != nil {
		return "", fmt.Errorf("the proof key is not on a supported curve")
	}
	point := pub.Bytes()
	if len(point) != 65 {
		return "", fmt.Errorf("the proof key is not a P-256 key")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	jti := fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
	header, err := json.Marshal(map[string]any{
		"typ": "dpop+jwt", "alg": "ES256",
		"jwk": map[string]string{"kty": "EC", "crv": "P-256", "x": b64url(point[1:33]), "y": b64url(point[33:65])},
	})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{"jti": jti, "htm": method, "htu": target, "iat": now.Unix()})
	if err != nil {
		return "", err
	}
	input := b64url(header) + "." + b64url(claims)
	digest := sha256.Sum256([]byte(input))
	sr, ss, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	sr.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return input + "." + b64url(sig), nil
}

func (r *resolver) refreshLogin(ctx context.Context, path string, members map[string]json.RawMessage, access loginAccess) (Credentials, error) {
	const source = "login"
	const remedy = ", run aws login to sign in again"
	clientID, refreshToken, proofKey := memberText(members, "clientId"), memberText(members, "refreshToken"), memberText(members, "dpopKey")
	if clientID == "" || refreshToken == "" || proofKey == "" {
		return Credentials{}, &Error{Source: source, Message: "the cached login cannot be refreshed, it lacks its client id, refresh token or proof key" + remedy}
	}
	target := r.signinEndpoint() + "/v1/token"
	proof, err := dpopProof(proofKey, http.MethodPost, target, time.Now())
	if err != nil {
		return Credentials{}, &Error{Source: source, Message: "the cached login proof key is unusable" + remedy, Err: err}
	}
	body, err := json.Marshal(map[string]string{"clientId": clientID, "refreshToken": refreshToken, "grantType": "refresh_token"})
	if err != nil {
		return Credentials{}, &Error{Source: source, Message: "the refresh request could not be encoded", Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return Credentials{}, &Error{Source: source, Message: "the sign-in endpoint is not a usable URL", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("DPoP", proof)
	status, header, data, err := send(r.c.api, req, source)
	if err != nil {
		return Credentials{}, err
	}
	if status != http.StatusOK {
		var fault struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &fault)
		code := fault.Error
		if code == "" {
			code = errorType(header)
		}
		return Credentials{}, &ServiceError{Service: "signin", Operation: "CreateOAuth2Token", Status: status, Code: code, Message: fault.Message + remedy}
	}
	var out struct {
		AccessToken  loginAccess     `json:"accessToken"`
		ExpiresIn    float64         `json:"expiresIn"`
		RefreshToken string          `json:"refreshToken"`
		IDToken      json.RawMessage `json:"idToken"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Credentials{}, &Error{Source: source, Message: "the sign-in reply is not readable JSON"}
	}
	expires := time.Now().UTC().Add(time.Duration(out.ExpiresIn * float64(time.Second))).Truncate(time.Second)
	fresh := Credentials{AccessKeyID: out.AccessToken.AccessKeyID, SecretAccessKey: out.AccessToken.SecretAccessKey, SessionToken: out.AccessToken.SessionToken, Expires: expires, Source: source}
	if !fresh.complete() {
		return Credentials{}, &Error{Source: source, Message: "the sign-in reply carried no credentials"}
	}
	stored := loginAccess{AccessKeyID: fresh.AccessKeyID, SecretAccessKey: fresh.SecretAccessKey, SessionToken: fresh.SessionToken, AccountID: access.AccountID, ExpiresAt: expires.Format("2006-01-02T15:04:05Z")}
	accessJSON, err := fileutil.EncodeJSON(stored, fileutil.JSONStyle{ASCII: true})
	if err != nil {
		return Credentials{}, &Error{Source: source, Message: "the refreshed login could not be encoded", Err: err}
	}
	members["accessToken"] = accessJSON
	if out.RefreshToken != "" {
		quoted, err := json.Marshal(out.RefreshToken)
		if err != nil {
			return Credentials{}, &Error{Source: source, Message: "the refreshed login could not be encoded", Err: err}
		}
		members["refreshToken"] = quoted
	}
	if len(out.IDToken) > 0 && string(out.IDToken) != "null" {
		members["idToken"] = out.IDToken
	}
	if err := fileutil.WriteFile(ctx, path, encodeMembers(members), fileutil.Private()); err != nil {
		return Credentials{}, &Error{Source: source, Message: "the refreshed login could not be stored, the cached refresh token is now spent", Err: err}
	}
	return fresh, nil
}

func encodeMembers(members map[string]json.RawMessage) []byte {
	order := []string{"accessToken", "tokenType", "refreshToken", "idToken", "clientId", "dpopKey"}
	known := map[string]bool{}
	for _, k := range order {
		known[k] = true
	}
	var rest []string
	for k := range members {
		if !known[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	for _, k := range append(order, rest...) {
		v, ok := members[k]
		if !ok {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		name, _ := json.Marshal(k)
		b.Write(name)
		b.WriteString(": ")
		b.Write(bytes.TrimSpace(v))
	}
	b.WriteByte('}')
	return b.Bytes()
}
