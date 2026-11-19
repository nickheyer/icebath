package pyjwt

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
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
	parts := strings.Split(token, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if pub, ok := jwk.Key.(*rsa.PublicKey); !ok || rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
		t.Fatalf("fetched key %T does not verify the token signature", jwk.Key)
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
