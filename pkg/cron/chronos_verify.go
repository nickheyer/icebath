package cron

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyjwt"
)

const chronosFirePurpose = "cron_fire"

// Source of a JWKS signing key for a token, the PyJWKClient surface verify uses
type chronosSigningKeySource interface {
	SigningKeyFromJWT(token string) (*pyjwt.JWK, error)
}

var (
	chronosJWKClients   = map[string]chronosSigningKeySource{}
	chronosJWKClientsMu sync.Mutex
	newChronosJWKClient = func(url string, headers map[string]string) chronosSigningKeySource {
		return pyjwt.NewJWKClient(url, headers)
	}
)

// Cached JWKS client per URL sending explicit Accept and User-Agent headers
func chronosJWKClient(jwksURL string) chronosSigningKeySource {
	chronosJWKClientsMu.Lock()
	defer chronosJWKClientsMu.Unlock()
	client, ok := chronosJWKClients[jwksURL]
	if !ok {
		client = newChronosJWKClient(jwksURL, map[string]string{"Accept": "application/json", "User-Agent": "icebath-agent/1.0"})
		chronosJWKClients[jwksURL] = client
	}
	return client
}

// NASFireTokenOptions are the verify_nas_fire_token keywords, a nil leeway is the 30 second default
type NASFireTokenOptions struct {
	Token            string
	ExpectedAudience string
	JWKSOrKey        string
	Issuer           string
	LeewaySeconds    *int
}

// Claims of a NAS signed cron fire token, nil for any token that fails signature, claims or purpose
func VerifyNASFireToken(o NASFireTokenOptions) *pyjson.Object {
	log := slog.Default().With("logger", "cron.chronos.verify")
	if o.Token == "" || o.ExpectedAudience == "" {
		return nil
	}
	if o.JWKSOrKey == "" {
		log.Warn("cron fire: no JWKS/key configured; refusing token")
		return nil
	}
	leeway := 30
	if o.LeewaySeconds != nil {
		leeway = *o.LeewaySeconds
	}
	claims, err := func() (*pyjson.Object, error) {
		var key any = o.JWKSOrKey
		if strings.HasPrefix(o.JWKSOrKey, "http://") || strings.HasPrefix(o.JWKSOrKey, "https://") {
			jwk, err := chronosJWKClient(o.JWKSOrKey).SigningKeyFromJWT(o.Token)
			if err != nil {
				return nil, err
			}
			key = jwk.Key
		}
		return pyjwt.Decode(o.Token, key, pyjwt.DecodeOptions{
			Algorithms: []string{"RS256", "RS384", "RS512", "ES256", "ES384"},
			Audience:   o.ExpectedAudience,
			Issuer:     o.Issuer,
			Leeway:     time.Duration(leeway) * time.Second,
			Require:    []string{"exp", "aud"},
		})
	}()
	if err != nil {
		log.Warn(fmt.Sprintf("cron fire: token verification failed: %v", err))
		return nil
	}
	if purpose, _ := claims.Get("purpose"); purpose != chronosFirePurpose {
		log.Warn(fmt.Sprintf("cron fire: token missing/!=%s purpose claim", chronosFirePurpose))
		return nil
	}
	return claims
}

// Verifier the gateway cron fire webhook calls on every NAS callback
func GetFireVerifier() func(NASFireTokenOptions) *pyjson.Object { return VerifyNASFireToken }
