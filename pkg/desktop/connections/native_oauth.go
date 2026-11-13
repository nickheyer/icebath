package connections

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// NativeFlowID is the auth_flows entry a gateway advertises for the RFC 8252 loopback PKCE login
const NativeFlowID = "native_pkce"

// NativePkcePair is a PKCE verifier with its S256 challenge
type NativePkcePair struct {
	Verifier  string
	Challenge string
	Method    string
}

// NativeTokenSet is a native login's bearer, refresh token, expiry in epoch seconds and identity
type NativeTokenSet struct {
	AccessToken  string  `json:"accessToken"`
	RefreshToken string  `json:"refreshToken"`
	ExpiresAt    float64 `json:"expiresAt"`
	Provider     string  `json:"provider"`
	UserID       string  `json:"userId"`
}

func b64url(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func randomBytes(n int) []byte {
	out := make([]byte, n)
	_, _ = rand.Read(out)
	return out
}

// GeneratePkcePair makes a 43 character verifier and its SHA-256 challenge
func GeneratePkcePair() NativePkcePair {
	verifier := b64url(randomBytes(32))
	sum := sha256.Sum256([]byte(verifier))
	return NativePkcePair{Verifier: verifier, Challenge: b64url(sum[:]), Method: "S256"}
}

// GenerateState makes the URL safe CSRF state of a native login
func GenerateState() string { return b64url(randomBytes(24)) }

// StatusSupportsNativeFlow reports a gateway status advertising the native PKCE flow
func StatusSupportsNativeFlow(statusBody any) bool {
	flows, ok := jsObject(statusBody)["auth_flows"].([]any)
	if !ok {
		return false
	}
	for _, flow := range flows {
		if flow == NativeFlowID {
			return true
		}
	}
	return false
}

// ResolveLoginStrategy picks native PKCE when advertised and some provider is not password only, embedded otherwise
func ResolveLoginStrategy(statusBody any, forceEmbedded bool, providers []AdvertisedAuthProvider) string {
	if forceEmbedded || !OauthGuardMayHardFail(providers) {
		return "embedded"
	}
	if StatusSupportsNativeFlow(statusBody) {
		return "native"
	}
	return "embedded"
}

// nativeEndpoint is the gateway origin plus its path prefix and the native auth route
func nativeEndpoint(baseURL, route string) (string, error) {
	parsed, err := parseURL(baseURL)
	if err != nil {
		return "", err
	}
	prefix := strings.TrimRight(parsed.Pathname(), "/")
	return parsed.Protocol() + "//" + parsed.Host() + prefix + route, nil
}

// BuildNativeAuthorizeURL is the authorize URL a native login opens in the system browser
func BuildNativeAuthorizeURL(baseURL, challenge, redirectURI, state, provider string) (string, error) {
	endpoint, err := nativeEndpoint(baseURL, "/auth/native/authorize")
	if err != nil {
		return "", err
	}
	q := &searchParams{}
	q.set("code_challenge", challenge)
	q.set("code_challenge_method", "S256")
	q.set("redirect_uri", redirectURI)
	q.set("state", state)
	if provider != "" {
		q.set("provider", provider)
	}
	return endpoint + q.search(), nil
}

// NativeTokenURL is the code exchange endpoint of a gateway
func NativeTokenURL(baseURL string) (string, error) {
	return nativeEndpoint(baseURL, "/auth/native/token")
}

// NativeRefreshURL is the refresh endpoint of a gateway
func NativeRefreshURL(baseURL string) (string, error) {
	return nativeEndpoint(baseURL, "/auth/native/refresh")
}

// ParseLoopbackCallback is the authorization code of a loopback redirect whose state matches
func ParseLoopbackCallback(requestURL, expectedState string) (string, error) {
	parsed, err := parseURLRef(requestURL, "http://127.0.0.1")
	if err != nil {
		return "", err
	}
	params := parseSearchParams(parsed.Query())
	if gatewayError, _ := params.get("error"); gatewayError != "" {
		desc, _ := params.get("error_description")
		if desc != "" {
			desc = " (" + desc + ")"
		}
		return "", fmt.Errorf("Gateway rejected native login: %s%s", gatewayError, desc)
	}
	code, _ := params.get("code")
	state, _ := params.get("state")
	if code == "" {
		return "", errors.New("Loopback callback missing authorization code")
	}
	if expectedState == "" || state != expectedState {
		return "", errors.New("Loopback callback state mismatch (possible CSRF)")
	}
	return code, nil
}

// finiteOrZero keeps a finite JavaScript number, zero otherwise
func finiteOrZero(v any) float64 {
	n := authNumber(v)
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0
	}
	return n
}

// ParseTokenResponse normalizes a gateway token response in its snake case wire shape
func ParseTokenResponse(body any) (NativeTokenSet, error) {
	obj := jsObject(body)
	accessToken := jsOrString(obj["access_token"])
	if accessToken == "" {
		return NativeTokenSet{}, errors.New("Gateway token response missing access_token")
	}
	return NativeTokenSet{
		AccessToken:  accessToken,
		RefreshToken: jsOrString(obj["refresh_token"]),
		ExpiresAt:    finiteOrZero(obj["expires_at"]),
		Provider:     jsOrString(obj["provider"]),
		UserID:       jsOrString(obj["user_id"]),
	}, nil
}

// ParseStoredTokenSet normalizes a persisted token set in its camel case disk shape
func ParseStoredTokenSet(body any) (NativeTokenSet, error) {
	obj := jsObject(body)
	accessToken := jsOrString(obj["accessToken"])
	if accessToken == "" {
		return NativeTokenSet{}, errors.New("Stored token set missing accessToken")
	}
	return NativeTokenSet{
		AccessToken:  accessToken,
		RefreshToken: jsOrString(obj["refreshToken"]),
		ExpiresAt:    finiteOrZero(obj["expiresAt"]),
		Provider:     jsOrString(obj["provider"]),
		UserID:       jsOrString(obj["userId"]),
	}, nil
}

// TokenNeedsRefresh reports a token without expiry or within skew seconds of expiring
func TokenNeedsRefresh(expiresAt, nowSeconds, skewSeconds float64) bool {
	if math.IsNaN(expiresAt) || math.IsInf(expiresAt, 0) || expiresAt <= 0 {
		return true
	}
	return nowSeconds >= expiresAt-skewSeconds
}

// OauthSessionIsLive reports a native token or a cookie session
func OauthSessionIsLive(hasNativeToken, hasCookieSession bool) bool {
	return hasNativeToken || hasCookieSession
}

// OauthRestAuth is how an OAuth gated REST call authenticates, bearer, cookie, token or public
type OauthRestAuth struct {
	Kind  string  `json:"kind"`
	Token *string `json:"token,omitempty"`
}

// ResolveOauthRestAuth prefers the native bearer and falls back to the cookie jar
func ResolveOauthRestAuth(nativeAccessToken string) OauthRestAuth {
	if nativeAccessToken != "" {
		return OauthRestAuth{Kind: "bearer", Token: &nativeAccessToken}
	}
	return OauthRestAuth{Kind: "cookie"}
}

func tokenAuth(connectionToken *string) OauthRestAuth {
	return OauthRestAuth{Kind: "token", Token: connectionToken}
}

// ResolveReadinessProbeAuth picks the readiness probe credential for an auth mode
func ResolveReadinessProbeAuth(authMode, nativeAccessToken string, connectionToken *string) OauthRestAuth {
	switch authMode {
	case "oauth":
		return ResolveOauthRestAuth(nativeAccessToken)
	case "token":
		return tokenAuth(connectionToken)
	}
	return OauthRestAuth{Kind: "public"}
}

// AdvertisedAuthProvider is one login provider a gateway advertises
type AdvertisedAuthProvider struct {
	Name             string `json:"name"`
	SupportsPassword bool   `json:"supportsPassword"`
}

var passwordProviderNames = map[string]bool{"basic": true}

// NormalizeAdvertisedAuthProviders reads provider strings or objects, inferring password support for basic
func NormalizeAdvertisedAuthProviders(providers any) []AdvertisedAuthProvider {
	list, ok := providers.([]any)
	if !ok {
		return []AdvertisedAuthProvider{}
	}
	out := []AdvertisedAuthProvider{}
	for _, provider := range list {
		if name, isString := provider.(string); isString {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, AdvertisedAuthProvider{Name: name, SupportsPassword: passwordProviderNames[name]})
			}
			continue
		}
		raw := jsObject(provider)
		if raw == nil {
			continue
		}
		name, _ := raw["name"].(string)
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		supports := passwordProviderNames[name]
		if v, ok := raw["supportsPassword"].(bool); ok {
			supports = v
		} else if v, ok := raw["supports_password"].(bool); ok {
			supports = v
		}
		out = append(out, AdvertisedAuthProvider{Name: name, SupportsPassword: supports})
	}
	return out
}

// OauthTicketFailureAuthMessage tells an expired native session apart from never having signed in
func OauthTicketFailureAuthMessage(hasDecryptableNativeSession bool) string {
	if hasDecryptableNativeSession {
		return backend.RemoteSessionExpiredMessage
	}
	return backend.RemoteUnsignedOauthMessage
}

// OauthGuardMayHardFail reports a gateway whose providers are not all password providers
func OauthGuardMayHardFail(providers []AdvertisedAuthProvider) bool {
	named := NormalizeAdvertisedAuthProviders(toJSONValue(providers))
	if len(named) == 0 {
		return true
	}
	for _, p := range named {
		if !p.SupportsPassword {
			return true
		}
	}
	return false
}

// ShouldRotateNativeTokenAfterRejection reports a 401 the native refresh token can answer
func ShouldRotateNativeTokenAfterRejection(err error) bool {
	status, _ := readStatusCode(err)
	return status == 401
}

// Number of a decoded JSON value the way JavaScript Number() converts it
func authNumber(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return t
	case string:
		text := strings.TrimSpace(t)
		switch text {
		case "":
			return 0
		case "Infinity", "+Infinity":
			return math.Inf(1)
		case "-Infinity":
			return math.Inf(-1)
		}
		if strings.HasPrefix(text, "0x") || strings.HasPrefix(text, "0X") {
			if n, err := strconv.ParseUint(text[2:], 16, 64); err == nil {
				return float64(n)
			}
			return math.NaN()
		}
		if strings.ContainsAny(text, "_xXpP") || strings.HasPrefix(text, "0b") || strings.HasPrefix(text, "0o") {
			return math.NaN()
		}
		n, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return math.NaN()
		}
		return n
	case []any:
		if len(t) == 0 {
			return 0
		}
		if len(t) == 1 {
			return authNumber(jsString(t[0]))
		}
	}
	return math.NaN()
}
