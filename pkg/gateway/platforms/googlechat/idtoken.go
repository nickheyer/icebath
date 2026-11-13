package googlechat

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	googleOAuth2CertsURL = "https://www.googleapis.com/oauth2/v1/certs"
	certsCacheTTL        = 5 * time.Minute
)

// Issuers Google signs OAuth2 ID tokens as
var googleIssuers = map[string]bool{"accounts.google.com": true, "https://accounts.google.com": true}

// Google signing certificates by key id with the time they were fetched
type certCache struct {
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

var googleCerts = &certCache{}

// Signing keys, fetched again once the cached set is older than the TTL
func (c *certCache) get(ctx context.Context, client *http.Client) (map[string]*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys != nil && time.Since(c.fetched) < certsCacheTTL {
		return c.keys, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleOAuth2CertsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching Google certificates: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching Google certificates returned %d", resp.StatusCode)
	}
	var certs map[string]string
	if err := json.Unmarshal(raw, &certs); err != nil {
		return nil, fmt.Errorf("decoding Google certificates: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for kid, text := range certs {
		block, _ := pem.Decode([]byte(text))
		if block == nil {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if pub, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			keys[kid] = pub
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("google certificate set holds no RSA keys")
	}
	c.keys, c.fetched = keys, time.Now()
	return keys, nil
}

// Claims of a Google signed OAuth2 ID token issued for the audience, every check failing closed
func verifyGoogleIDToken(ctx context.Context, client *http.Client, token, audience string) (jwt.MapClaims, error) {
	keys, err := googleCerts.get(ctx, client)
	if err != nil {
		return nil, err
	}
	claims := jwt.MapClaims{}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}), jwt.WithAudience(audience), jwt.WithExpirationRequired(),
		jwt.WithIssuedAt())
	_, err = parser.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if kid, _ := t.Header["kid"].(string); kid != "" {
			key, ok := keys[kid]
			if !ok {
				return nil, fmt.Errorf("certificate for key id %s not found", kid)
			}
			return key, nil
		}
		set := jwt.VerificationKeySet{}
		for _, key := range keys {
			set.Keys = append(set.Keys, key)
		}
		return set, nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := claims["iat"]; !ok {
		return nil, errors.New("token does not contain required claim iat")
	}
	if iss, _ := claims["iss"].(string); !googleIssuers[iss] {
		return nil, fmt.Errorf("wrong issuer %q", iss)
	}
	return claims, nil
}
