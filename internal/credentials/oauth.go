package credentials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/httpx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Most bytes of an OAuth endpoint's answer read; real ones are a few hundred
const maxReply = 1 << 20

// Answer of an OAuth endpoint, read whole
type Reply struct {
	Status int
	Header http.Header
	Body   []byte
}

// Body as a JSON object, an empty one for JSON of another shape, an error for anything that isn't JSON
func (r *Reply) JSON() (map[string]any, error) {
	var payload any
	if err := json.Unmarshal(r.Body, &payload); err != nil {
		return nil, err
	}
	object, _ := payload.(map[string]any)
	if object == nil {
		object = map[string]any{}
	}
	return object, nil
}

// Body as text, cut to limit characters with a mark when longer
func (r *Reply) Text(limit int) string {
	text := strings.TrimSpace(strings.ToValidUTF8(string(r.Body), "�"))
	if limit > 0 && len(text) > limit {
		return text[:limit] + "...[truncated]"
	}
	return text
}

// Wait the answer asks for before trying again, false when it asks none
func (r *Reply) RetryAfter() (time.Duration, bool) {
	return httpx.RetryAfter(r.Header.Get("Retry-After"))
}

// Client for OAuth endpoints, proxies from the environment, each call ending within the timeout
func Client(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	return &http.Client{Timeout: timeout, Transport: transport}
}

// Sends the request and reads its answer whole, refusing one larger than an OAuth endpoint sends
func Do(client *http.Client, request *http.Request) (*Reply, error) {
	if request.Header.Get("Accept") == "" {
		request.Header.Set("Accept", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReply+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxReply {
		return nil, fmt.Errorf("The answer from %s exceeded %d KiB, so it was not read.", request.URL.Host, maxReply>>10)
	}
	return &Reply{Status: response.StatusCode, Header: response.Header, Body: body}, nil
}

// Posts a form, with the headers given
func PostForm(ctx context.Context, client *http.Client, endpoint string, form url.Values, header map[string]string) (*Reply, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for name, value := range header {
		request.Header.Set(name, value)
	}
	return Do(client, request)
}

// Posts a JSON body, with the headers given
func PostJSON(ctx context.Context, client *http.Client, endpoint string, body any, header map[string]string) (*Reply, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range header {
		request.Header.Set(name, value)
	}
	return Do(client, request)
}

// Gets the address, with the headers given
func Get(ctx context.Context, client *http.Client, address string, header map[string]string) (*Reply, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	for name, value := range header {
		request.Header.Set(name, value)
	}
	return Do(client, request)
}

// Unverified claims of a JWT, empty for any other token
func Claims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return map[string]any{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil || claims == nil {
		return map[string]any{}
	}
	return claims
}

// When a JWT expires, zero when it isn't one or says nothing of it
func JWTExpiry(token string) time.Time {
	exp, ok := Claims(token)["exp"].(float64)
	if !ok {
		return time.Time{}
	}
	return time.Unix(int64(exp), int64((exp-float64(int64(exp)))*1e9))
}

// Who a token signs in as, from its claims: the email, else the user name, empty when it says neither
func Identity(token string) string {
	claims := Claims(token)
	for _, key := range []string{"email", "preferred_username", "upn"} {
		if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// Short mark of a secret that tells two apart without showing either: the last six hex characters of its SHA-256
func Fingerprint(secret string) string {
	if strings.TrimSpace(secret) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	text := hex.EncodeToString(sum[:])
	return text[len(text)-6:]
}

// PKCE code verifier made of n random bytes, at most 128 characters, and its S256 challenge
func PKCE(n int) (verifier, challenge string) {
	verifier = strs.Token(n)
	if len(verifier) > 128 {
		verifier = verifier[:128]
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// Text a JSON field holds, trimmed, empty for anything that isn't text
func Text(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

// Sleeps for d unless ctx ends first, ErrCancelled when it does
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(max(d, 0))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return errors.Join(ErrCancelled, ctx.Err())
	case <-timer.C:
		return nil
	}
}
