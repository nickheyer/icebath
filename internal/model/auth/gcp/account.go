package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	jwtGrant        = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	exchangeGrant   = "urn:ietf:params:oauth:token-type:token-exchange"
	accessTokenType = "urn:ietf:params:oauth:token-type:access_token"
	k8sAccountType  = "urn:k8s:params:oauth:token-type:serviceaccount"
)

func (c *client) serviceAccount(info document, scopes []string) (*credentials, error) {
	if missing := info.missing("client_email", "token_uri"); len(missing) > 0 {
		return nil, &Error{Kind: kindServiceAccount, Message: "key file is not a service account key, missing fields " + strings.Join(missing, ", ")}
	}
	if !info.has("private_key") {
		return nil, &Error{Kind: kindServiceAccount, Message: "key file has no private_key field"}
	}
	key, err := parseRSAKey(info.text("private_key"))
	if err != nil {
		return nil, &Error{Kind: kindServiceAccount, Message: err.Error()}
	}
	email, tokenURI, keyID := info.text("client_email"), info.text("token_uri"), info.text("private_key_id")
	creds := &credentials{kind: kindServiceAccount, universe: info.text("universe_domain"), quotaProject: info.text("quota_project_id")}
	creds.mint = func(ctx context.Context) (string, time.Time, error) {
		now := time.Now().UTC()
		expiry := now.Add(tokenLifetime)
		if creds.domain() != defaultUniverse {
			claims := object{{"iss", email}, {"sub", email}, {"iat", now.Unix()}, {"exp", expiry.Unix()}}
			if len(scopes) > 0 {
				claims = append(claims, member{"scope", strings.Join(scopes, " ")})
			}
			token, err := signJWT(jwt.SigningMethodRS256, key, keyID, claims)
			if err != nil {
				return "", time.Time{}, &Error{Kind: kindServiceAccount, Message: "self-signed token could not be signed", Err: err}
			}
			return token, expiry, nil
		}
		assertion, err := signJWT(jwt.SigningMethodRS256, key, keyID, object{
			{"iat", now.Unix()}, {"exp", expiry.Unix()}, {"iss", email}, {"aud", tokenEndpoint}, {"scope", strings.Join(scopes, " ")},
		})
		if err != nil {
			return "", time.Time{}, &Error{Kind: kindServiceAccount, Message: "grant assertion could not be signed", Err: err}
		}
		reply, err := c.tokenRequest(ctx, c.api, kindServiceAccount, tokenURI, formType, encodeForm(pair{"assertion", assertion}, pair{"grant_type", jwtGrant}))
		if err != nil {
			return "", time.Time{}, err
		}
		return accessToken(kindServiceAccount, reply, time.Now().UTC())
	}
	creds.allowed = c.allowedFor(kindServiceAccount, func(context.Context) (string, error) {
		return fmt.Sprintf(serviceAccountBoundary, creds.domain(), email), nil
	})
	return creds, nil
}

func (c *client) gdchAccount(info document) (*credentials, error) {
	if missing := info.missing("format_version", "private_key_id", "private_key", "name", "project", "token_uri"); len(missing) > 0 {
		return nil, &Error{Kind: kindGDCH, Message: "key file is not a GDCH service account key, missing fields " + strings.Join(missing, ", ")}
	}
	if info.text("format_version") != "1" {
		return nil, &Error{Kind: kindGDCH, Message: "only format version 1 is supported"}
	}
	key, err := parseECKey(info.text("private_key"))
	if err != nil {
		return nil, &Error{Kind: kindGDCH, Message: err.Error()}
	}
	tokenURI, keyID := info.text("token_uri"), info.text("private_key_id")
	issuer := "system:serviceaccount:" + info.text("project") + ":" + info.text("name")
	hc := c.api
	if caPath := info.text("ca_cert_path"); caPath != "" {
		opts := c.http
		opts.TLS.RootCAFile = caPath
		opts.Timeouts = httpclient.EachPhase(requestTimeout)
		transport, err := httpclient.New(opts)
		if err != nil {
			return nil, &Error{Kind: kindGDCH, Message: "ca_cert_path could not be loaded", Err: err}
		}
		hc = transport.Client()
	}
	creds := &credentials{kind: kindGDCH}
	creds.mint = func(ctx context.Context) (string, time.Time, error) {
		now := time.Now().UTC()
		subject, err := signJWT(jwt.SigningMethodES256, key, keyID, object{
			{"iss", issuer}, {"sub", issuer}, {"aud", tokenURI}, {"iat", now.Unix()}, {"exp", now.Add(tokenLifetime).Unix()},
		})
		if err != nil {
			return "", time.Time{}, &Error{Kind: kindGDCH, Message: "subject token could not be signed", Err: err}
		}
		body := spacedJSON(object{
			{"grant_type", exchangeGrant}, {"audience", nil}, {"requested_token_type", accessTokenType},
			{"subject_token", subject}, {"subject_token_type", k8sAccountType},
		})
		reply, err := c.tokenRequest(ctx, hc, kindGDCH, tokenURI, jsonType, body)
		if err != nil {
			return "", time.Time{}, err
		}
		return accessToken(kindGDCH, reply, time.Now().UTC())
	}
	return creds, nil
}

func (c *credentials) adoptFileToken(info document) error {
	token, expiry := info.text("token"), info.text("expiry")
	if expiry == "" {
		return nil
	}
	expiry, _, _ = strings.Cut(strings.TrimRight(expiry, "Z"), ".")
	t, err := time.Parse("2006-01-02T15:04:05", expiry)
	if err != nil {
		return &Error{Kind: c.kind, Message: "file carries an unreadable expiry"}
	}
	if token != "" {
		c.token, c.expiry = token, t
	}
	return nil
}

func (c *client) authorizedUser(info document) (*credentials, error) {
	if missing := info.missing("refresh_token", "client_id", "client_secret"); len(missing) > 0 {
		return nil, &Error{Kind: kindUser, Message: "file is not an authorized user credential, missing fields " + strings.Join(missing, ", ")}
	}
	clientID, clientSecret := info.text("client_id"), info.text("client_secret")
	refreshToken, rapt := info.text("refresh_token"), info.text("rapt_token")
	scopes := info.strings("scopes")
	creds := &credentials{kind: kindUser, universe: info.text("universe_domain"), quotaProject: info.text("quota_project_id")}
	if err := creds.adoptFileToken(info); err != nil {
		return nil, err
	}
	creds.mint = func(ctx context.Context) (string, time.Time, error) {
		if creds.domain() != defaultUniverse {
			return "", time.Time{}, &Error{Kind: kindUser, Message: "user credential refresh is only supported in the googleapis.com universe domain, this credential names " + creds.domain()}
		}
		if refreshToken == "" || clientID == "" || clientSecret == "" {
			return "", time.Time{}, &Error{Kind: kindUser, Message: "refresh needs refresh_token, client_id and client_secret"}
		}
		form := []pair{{"grant_type", "refresh_token"}, {"client_id", clientID}, {"client_secret", clientSecret}, {"refresh_token", refreshToken}}
		if len(scopes) > 0 {
			form = append(form, pair{"scope", strings.Join(scopes, " ")})
		}
		if rapt != "" {
			form = append(form, pair{"rapt", rapt})
		}
		reply, err := c.tokenRequest(ctx, c.api, kindUser, tokenEndpoint, formType, encodeForm(form...))
		if err != nil {
			return "", time.Time{}, err
		}
		token, expiry, err := accessToken(kindUser, reply, time.Now().UTC())
		if err != nil {
			return "", time.Time{}, err
		}
		if next := reply.text("refresh_token"); next != "" {
			refreshToken = next
		}
		if granted := reply.text("scope"); len(scopes) > 0 && reply.has("scope") {
			have := map[string]bool{}
			for _, s := range strings.Fields(granted) {
				have[s] = true
			}
			var absent []string
			for _, s := range scopes {
				if !have[s] {
					absent = append(absent, s)
				}
			}
			if len(absent) > 0 {
				slog.Warn("Not all requested scopes were granted by the authorization server", "missing", strings.Join(absent, ", "))
			}
		}
		return token, expiry, nil
	}
	return creds, nil
}

func (c *client) externalUser(info document) (*credentials, error) {
	refreshToken, tokenURL := info.text("refresh_token"), info.text("token_url")
	clientID, clientSecret := info.text("client_id"), info.text("client_secret")
	creds := &credentials{kind: kindExternalUser, universe: info.text("universe_domain"), quotaProject: info.text("quota_project_id")}
	if err := creds.adoptFileToken(info); err != nil {
		return nil, err
	}
	refreshable := refreshToken != "" && tokenURL != "" && clientID != "" && clientSecret != ""
	usable := creds.token != "" && time.Until(creds.expiry) > refreshThreshold
	if !usable && !refreshable {
		return nil, &Error{Kind: kindExternalUser, Message: "file needs token and expiry, or refresh_token, token_url, client_id and client_secret"}
	}
	creds.mint = func(ctx context.Context) (string, time.Time, error) {
		if !refreshable {
			return "", time.Time{}, &Error{Kind: kindExternalUser, Message: "refresh needs refresh_token, token_url, client_id and client_secret"}
		}
		now := time.Now().UTC()
		reply, err := c.stsRequest(ctx, c.api, kindExternalUser, tokenURL, clientID, clientSecret, encodeForm(pair{"grant_type", "refresh_token"}, pair{"refresh_token", refreshToken}))
		if err != nil {
			return "", time.Time{}, err
		}
		token, expiry, err := accessToken(kindExternalUser, reply, now)
		if err != nil {
			return "", time.Time{}, err
		}
		if reply.has("refresh_token") {
			refreshToken = reply.text("refresh_token")
		}
		return token, expiry, nil
	}
	return creds, nil
}

func principalOf(impersonationURL string) (string, bool) {
	start := strings.LastIndex(impersonationURL, "/")
	end := strings.Index(impersonationURL, ":generateAccessToken")
	if start == -1 || end == -1 || start >= end {
		return "", false
	}
	return impersonationURL[start+1 : end], true
}

func (c *client) impersonated(source *credentials, principal string, delegates, scopes []string, lifetime int, endpoint string) *credentials {
	if lifetime <= 0 {
		lifetime = int(tokenLifetime / time.Second)
	}
	if endpoint == "" {
		endpoint = fmt.Sprintf(strings.Replace(iamTokenEndpoint, defaultUniverse, source.domain(), 1), principal)
	}
	creds := &credentials{kind: kindImpersonated, universe: source.universe}
	creds.mint = func(ctx context.Context) (string, time.Time, error) {
		header := http.Header{"Content-Type": {jsonType}}
		if err := source.authorize(ctx, header); err != nil {
			return "", time.Time{}, err
		}
		body := spacedJSON(object{{"delegates", delegates}, {"scope", scopes}, {"lifetime", strconv.Itoa(lifetime) + "s"}})
		status, _, data, err := c.send(ctx, c.api, requestTimeout, http.MethodPost, endpoint, header, body)
		if err != nil {
			return "", time.Time{}, &Error{Kind: kindImpersonated, Message: "IAM credentials endpoint could not be reached: " + httpclient.Message(err), Retryable: true, Err: err}
		}
		if status != http.StatusOK {
			return "", time.Time{}, &Error{Kind: kindImpersonated, Status: status, Message: "unable to acquire impersonated credentials for " + principal + googleErrorText(data)}
		}
		var reply struct {
			AccessToken string `json:"accessToken"`
			ExpireTime  string `json:"expireTime"`
		}
		if err := json.Unmarshal(data, &reply); err != nil || reply.AccessToken == "" {
			return "", time.Time{}, &Error{Kind: kindImpersonated, Message: "IAM credentials reply carried no access token"}
		}
		expiry, err := time.Parse("2006-01-02T15:04:05Z", reply.ExpireTime)
		if err != nil {
			return "", time.Time{}, &Error{Kind: kindImpersonated, Message: "IAM credentials reply carried an unreadable expireTime"}
		}
		return reply.AccessToken, expiry, nil
	}
	creds.allowed = c.allowedFor(kindImpersonated, func(context.Context) (string, error) {
		return fmt.Sprintf(serviceAccountBoundary, creds.domain(), principal), nil
	})
	return creds
}

func googleErrorText(data []byte) string {
	var reply struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Error.Message == "" {
		return ""
	}
	if reply.Error.Status != "" {
		return ": " + reply.Error.Status + ": " + reply.Error.Message
	}
	return ": " + reply.Error.Message
}

func (c *client) impersonatedAccount(info document, scopes []string) (*credentials, error) {
	sourceInfo, ok := info.object("source_credentials")
	impersonationURL := info.text("service_account_impersonation_url")
	if !ok || impersonationURL == "" {
		return nil, &Error{Kind: kindImpersonated, Message: "file needs source_credentials and service_account_impersonation_url"}
	}
	var source *credentials
	var err error
	switch kind := sourceInfo.text("type"); kind {
	case kindUser:
		source, err = c.authorizedUser(sourceInfo)
	case kindServiceAccount:
		source, err = c.serviceAccount(sourceInfo, []string{iamScope})
	case kindExternalUser:
		source, err = c.externalUser(sourceInfo)
	default:
		return nil, &Error{Kind: kindImpersonated, Message: "source credential of type " + kind + " is not supported"}
	}
	if err != nil {
		return nil, err
	}
	principal, ok := principalOf(impersonationURL)
	if !ok {
		return nil, &Error{Kind: kindImpersonated, Message: "cannot extract target principal from " + impersonationURL}
	}
	if len(scopes) == 0 {
		scopes = info.strings("scopes")
	}
	creds := c.impersonated(source, principal, info.strings("delegates"), scopes, 0, "")
	creds.quotaProject = info.text("quota_project_id")
	return creds, nil
}
