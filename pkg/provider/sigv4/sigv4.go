// AWS Signature Version 4 credential carrier for Bedrock and other AWS endpoints
package sigv4

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const algorithm = "AWS4-HMAC-SHA256"

// One set of AWS credentials, permanent when Expires is zero
type Credentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	Expires      time.Time
}

func (c Credentials) valid() bool { return c.AccessKey != "" && c.SecretKey != "" }

// Supplies credentials on demand
type Source interface {
	Credentials(ctx context.Context) (Credentials, error)
}

// Credentials given directly
type Static Credentials

func (s Static) Credentials(context.Context) (Credentials, error) {
	if !Credentials(s).valid() {
		return Credentials{}, errors.New("sigv4: static credentials are incomplete")
	}
	return Credentials(s), nil
}

// Credentials from AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN
type Env struct{}

func (Env) Credentials(context.Context) (Credentials, error) {
	c := Credentials{AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN")}
	if !c.valid() {
		return Credentials{}, errors.New("sigv4: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are not both set")
	}
	return c, nil
}

type chain []Source

func (c chain) Credentials(ctx context.Context) (Credentials, error) {
	var errs []error
	for _, s := range c {
		creds, err := s.Credentials(ctx)
		if err == nil {
			return creds, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return Credentials{}, errors.New("sigv4: no credential sources")
	}
	return Credentials{}, errors.Join(errs...)
}

// Source taking the first credentials any of the sources can supply
func Chain(sources ...Source) Source { return chain(sources) }

// Environment, web identity, shared config, container and instance metadata in the order the AWS SDKs try them
func Default() Source { return Chain(Env{}, WebIdentity{}, Profile{}, Container{}, IMDS{}) }

// Region from AWS_REGION or AWS_DEFAULT_REGION, empty when neither is set
func Region() string {
	if r := os.Getenv("AWS_REGION"); r != "" {
		return r
	}
	return os.Getenv("AWS_DEFAULT_REGION")
}

// Signer carrying credentials from a source, refreshed when they expire
type Auth struct {
	Region  string
	Service string
	Source  Source
	// Clock for signing, the wall clock when nil
	Now   func() time.Time
	mu    sync.Mutex
	creds Credentials
}

// Loads credentials when none are held or the held ones expire within a minute
func (a *Auth) Refresh(ctx context.Context) error {
	if a.Region == "" || a.Service == "" || a.Source == nil {
		return errors.New("sigv4: region, service and source are required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.creds.valid() && (a.creds.Expires.IsZero() || a.now().Before(a.creds.Expires.Add(-time.Minute))) {
		return nil
	}
	creds, err := a.Source.Credentials(ctx)
	if err != nil {
		return err
	}
	a.creds = creds
	return nil
}

func (a *Auth) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

// Signs the request with the held credentials, making it fail on send when signing fails
func (a *Auth) Apply(r *http.Request) {
	a.mu.Lock()
	creds := a.creds
	a.mu.Unlock()
	if err := Sign(r, creds, a.Region, a.Service, a.now()); err != nil {
		r.Header.Del("Authorization")
		r.Body = io.NopCloser(failing{err})
		r.GetBody = func() (io.ReadCloser, error) { return nil, err }
	}
}

// Reader that reports the signing failure instead of a body
type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

// Signs r in place following the Signature Version 4 process
func Sign(r *http.Request, creds Credentials, region, service string, now time.Time) error {
	if !creds.valid() {
		return errors.New("sigv4: credentials are incomplete")
	}
	hash, err := payloadHash(r)
	if err != nil {
		return fmt.Errorf("sigv4: %w", err)
	}
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	r.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	headers := map[string]string{"host": host}
	for _, name := range []string{"Content-Type", "X-Amz-Date", "X-Amz-Security-Token", "X-Amz-Target", "Content-Length"} {
		if v := r.Header.Get(name); v != "" {
			headers[strings.ToLower(name)] = strings.Join(strings.Fields(v), " ")
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, k := range names {
		canonicalHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{
		r.Method,
		canonicalPath(r.URL),
		canonicalQuery(r.URL.Query()),
		canonicalHeaders.String(),
		signed,
		hash,
	}, "\n")
	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	toSign := strings.Join([]string{algorithm, amzDate, scope, hexSHA256([]byte(canonical))}, "\n")
	key := hmacSHA256([]byte("AWS4"+creds.SecretKey), dateStamp)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))
	r.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s", algorithm, creds.AccessKey, scope, signed, signature))
	return nil
}

// Encodes the already escaped path once more, as every service but S3 expects
func canonicalPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	return escape(p, true)
}

// Sorts parameters by name then value and encodes them with %20 for spaces
func canonicalQuery(values url.Values) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), values[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, escape(k, false)+"="+escape(v, false))
		}
	}
	return strings.Join(parts, "&")
}

// Percent encodes every byte outside the unreserved set, keeping slashes when asked
func escape(s string, keepSlash bool) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			sb.WriteByte(c)
		case c == '/' && keepSlash:
			sb.WriteByte(c)
		default:
			fmt.Fprintf(&sb, "%%%02X", c)
		}
	}
	return sb.String()
}

// Hashes the body, restoring it so the request can still be sent
func payloadHash(r *http.Request) (string, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return hexSHA256(nil), nil
	}
	if r.GetBody != nil {
		body, err := r.GetBody()
		if err != nil {
			return "", err
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			return "", err
		}
		return hexSHA256(data), nil
	}
	data, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return "", err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	return hexSHA256(data), nil
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// Client for calls to AWS credential services, built with the timeout when nil
func httpClient(c *http.Client, timeout time.Duration) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: timeout}
}

// Reads a JSON credential document the way the metadata, container and process sources deliver it
func readJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, v)
}
