package pyjwt

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// JWK is one usable key of a JWK set with its key id, use and public key material
type JWK struct {
	KeyID string
	Use   *string
	Key   any
}

// JWKClient fetches and caches a JWKS endpoint like PyJWKClient with the JWK set cache on
type JWKClient struct {
	URL      string
	Headers  map[string]string
	Timeout  time.Duration
	Lifespan time.Duration
	HTTP     *http.Client
	Now      func() time.Time

	mu      sync.Mutex
	cached  []JWK
	fetched time.Time
}

// Client with the PyJWKClient defaults of a 30 second timeout and a 300 second set lifespan
func NewJWKClient(url string, headers map[string]string) *JWKClient {
	return &JWKClient{URL: url, Headers: headers, Timeout: 30 * time.Second, Lifespan: 300 * time.Second}
}

func b64Int(v any) (*big.Int, bool) {
	text, ok := v.(string)
	if !ok {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(text, "="))
	if err != nil {
		return nil, false
	}
	return new(big.Int).SetBytes(raw), true
}

// Public key of one JWK dict, false for a key PyJWK would refuse
func parseJWK(m map[string]any) (any, bool) {
	switch m["kty"] {
	case "RSA":
		n, okN := b64Int(m["n"])
		e, okE := b64Int(m["e"])
		if !okN || !okE || !e.IsInt64() {
			return nil, false
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, true
	case "EC":
		var curve elliptic.Curve
		switch m["crv"] {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, false
		}
		x, okX := b64Int(m["x"])
		y, okY := b64Int(m["y"])
		if !okX || !okY {
			return nil, false
		}
		pub := &ecdsa.PublicKey{Curve: curve, X: x, Y: y}
		if _, err := pub.ECDH(); err != nil {
			return nil, false
		}
		return pub, true
	case "oct":
		k, ok := m["k"].(string)
		if !ok {
			return nil, false
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k, "="))
		return raw, err == nil
	case "OKP":
		x, ok := m["x"].(string)
		if !ok || m["crv"] != "Ed25519" {
			return nil, false
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(x, "="))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, false
		}
		return ed25519.PublicKey(raw), true
	}
	return nil, false
}

func (c *JWKClient) fetch() ([]JWK, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: c.Timeout}
	}
	req, err := http.NewRequest(http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, jwtErr("PyJWKClientConnectionError", "Fail to fetch data from the url, err: \"%v\"", err)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, jwtErr("PyJWKClientConnectionError", "Fail to fetch data from the url, err: \"%v\"", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode >= 400 {
		if err == nil {
			err = fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		return nil, jwtErr("PyJWKClientConnectionError", "Fail to fetch data from the url, err: \"%v\"", err)
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return nil, jwtErr("PyJWKClientError", "Fail to fetch data from the url, err: \"Expecting value\"")
	}
	rawKeys, ok := doc["keys"].([]any)
	if !ok || len(rawKeys) == 0 {
		return nil, jwtErr("PyJWKSetError", "The JWK Set did not contain any keys")
	}
	var keys []JWK
	for _, raw := range rawKeys {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key, ok := parseJWK(m)
		if !ok {
			continue
		}
		jwk := JWK{Key: key}
		jwk.KeyID, _ = m["kid"].(string)
		if use, isText := m["use"].(string); isText {
			jwk.Use = &use
		}
		keys = append(keys, jwk)
	}
	if len(keys) == 0 {
		return nil, jwtErr("PyJWKSetError", "The JWK Set did not contain any usable keys. Perhaps 'cryptography' is not installed?")
	}
	return keys, nil
}

func (c *JWKClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Signing keys of the set, the cached set unless refresh is asked or the lifespan passed
func (c *JWKClient) SigningKeys(refresh bool) ([]JWK, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if refresh || c.cached == nil || c.now().Sub(c.fetched) >= c.Lifespan {
		keys, err := c.fetch()
		if err != nil {
			return nil, err
		}
		c.cached, c.fetched = keys, c.now()
	}
	var signing []JWK
	for _, k := range c.cached {
		if (k.Use == nil || *k.Use == "sig") && k.KeyID != "" {
			signing = append(signing, k)
		}
	}
	if len(signing) == 0 {
		return nil, jwtErr("PyJWKClientError", "The JWKS endpoint did not contain any signing keys")
	}
	return signing, nil
}

// Key whose id matches kid, refetching the set once when the cached one has no match
func (c *JWKClient) SigningKey(kid string) (*JWK, error) { return c.signingKey(kid, kid) }

func (c *JWKClient) signingKey(kid, display string) (*JWK, error) {
	for _, refresh := range []bool{false, true} {
		keys, err := c.SigningKeys(refresh)
		if err != nil {
			return nil, err
		}
		for i := range keys {
			if keys[i].KeyID == kid {
				return &keys[i], nil
			}
		}
	}
	return nil, jwtErr("PyJWKClientError", "Unable to find a signing key that matches: \"%s\"", display)
}

// Key named by the unverified kid header of token
func (c *JWKClient) SigningKeyFromJWT(token string) (*JWK, error) {
	header, err := UnverifiedHeader(token)
	if err != nil {
		return nil, err
	}
	kidValue, _ := header.Get("kid")
	if kid, ok := kidValue.(string); ok {
		return c.SigningKey(kid)
	}
	return c.signingKey("", "None")
}
