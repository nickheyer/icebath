package pyjwt

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func signRS256(t *testing.T, key *rsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	signing := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + enc.EncodeToString(sig)
}

func publicPEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestDecodeChecksSignatureAlgorithmAndClaims(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub := publicPEM(t, key)
	now := time.Now().Unix()
	base := func(over map[string]any) map[string]any {
		c := map[string]any{"aud": "agent:a", "iss": "https://iss", "iat": now, "nbf": now - 5, "exp": now + 300}
		for k, v := range over {
			if v == nil {
				delete(c, k)
			} else {
				c[k] = v
			}
		}
		return c
	}
	hdr := map[string]any{"alg": "RS256", "typ": "JWT"}
	opts := DecodeOptions{Algorithms: []string{"RS256", "ES256"}, Audience: "agent:a", Issuer: "https://iss", Leeway: 30 * time.Second, Require: []string{"exp", "aud"}}
	claims, err := Decode(signRS256(t, key, hdr, base(nil)), pub, opts)
	if err != nil {
		t.Fatal(err)
	}
	if aud, _ := claims.Get("aud"); aud != "agent:a" {
		t.Fatalf("claims %v", claims)
	}
	cases := []struct {
		name  string
		token string
		class string
	}{
		{"wrong audience", signRS256(t, key, hdr, base(map[string]any{"aud": "agent:b"})), "InvalidAudienceError"},
		{"audience list", signRS256(t, key, hdr, base(map[string]any{"aud": []any{"x", 1}})), "InvalidAudienceError"},
		{"missing exp", signRS256(t, key, hdr, base(map[string]any{"exp": nil})), "MissingRequiredClaimError"},
		{"expired past leeway", signRS256(t, key, hdr, base(map[string]any{"exp": now - 31})), "ExpiredSignatureError"},
		{"future iat", signRS256(t, key, hdr, base(map[string]any{"iat": now + 600})), "ImmatureSignatureError"},
		{"wrong issuer", signRS256(t, key, hdr, base(map[string]any{"iss": "https://other"})), "InvalidIssuerError"},
		{"tampered", signRS256(t, other, hdr, base(nil)), "InvalidSignatureError"},
		{"alg not allowed", signRS256(t, key, map[string]any{"alg": "RS512"}, base(nil)), "InvalidAlgorithmError"},
		{"segments", "a.b", "DecodeError"},
	}
	for _, tc := range cases {
		_, err := Decode(tc.token, pub, opts)
		if e, ok := err.(*Error); !ok || e.Class != tc.class {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if _, err := Decode(signRS256(t, key, hdr, base(map[string]any{"exp": now - 10})), pub, opts); err != nil {
		t.Fatalf("expiry inside leeway refused %v", err)
	}
}

func TestJWKClientMatchesKidRefetchesAndSendsHeaders(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var hits atomic.Int32
	var accept, agent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		accept.Store(r.Header.Get("Accept"))
		agent.Store(r.Header.Get("User-Agent"))
		enc := base64.RawURLEncoding
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{
			map[string]any{"kty": "RSA", "kid": "k1", "use": "sig", "n": enc.EncodeToString(key.N.Bytes()), "e": enc.EncodeToString(big.NewInt(int64(key.E)).Bytes())},
			map[string]any{"kty": "RSA", "kid": "enc", "use": "enc", "n": enc.EncodeToString(key.N.Bytes()), "e": "AQAB"},
			map[string]any{"kty": "bogus"},
		}})
	}))
	defer srv.Close()
	c := NewJWKClient(srv.URL, map[string]string{"Accept": "application/json", "User-Agent": "icebath-agent/1.0"})
	now := time.Now().Unix()
	token := signRS256(t, key, map[string]any{"alg": "RS256", "kid": "k1"}, map[string]any{"aud": "a", "exp": now + 60})
	jwk, err := c.SigningKeyFromJWT(token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(token, jwk.Key, DecodeOptions{Algorithms: []string{"RS256"}, Audience: "a"}); err != nil {
		t.Fatal(err)
	}
	if accept.Load() != "application/json" || agent.Load() != "icebath-agent/1.0" || hits.Load() != 1 {
		t.Fatalf("request %v %v %d", accept.Load(), agent.Load(), hits.Load())
	}
	if _, err := c.SigningKeyFromJWT(token); err != nil || hits.Load() != 1 {
		t.Fatalf("cached set refetched %v %d", err, hits.Load())
	}
	_, err = c.SigningKey("enc")
	if err == nil || err.Error() != `Unable to find a signing key that matches: "enc"` || hits.Load() != 2 {
		t.Fatalf("unknown kid %v %d", err, hits.Load())
	}
	noKid := signRS256(t, key, map[string]any{"alg": "RS256"}, map[string]any{})
	if _, err := c.SigningKeyFromJWT(noKid); err == nil || err.Error() != `Unable to find a signing key that matches: "None"` {
		t.Fatalf("missing kid %v", err)
	}
}
