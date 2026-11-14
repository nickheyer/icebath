package pyjwt

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestEncodeRS256MatchesPyJWTLayout(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	for _, block := range []*pem.Block{{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}, {Type: "PRIVATE KEY", Bytes: pkcs8}} {
		payload := pyjson.NewObject()
		payload.Set("iat", 100)
		payload.Set("exp", 760)
		payload.Set("iss", "12345")
		tok, err := EncodeRS256(payload, string(pem.EncodeToMemory(block)))
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(tok, ".")
		if len(parts) != 3 || parts[0] != "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9" {
			t.Fatalf("header %q", tok)
		}
		body, _ := base64.RawURLEncoding.DecodeString(parts[1])
		if string(body) != `{"iat":100,"exp":760,"iss":"12345"}` {
			t.Fatalf("payload %s", body)
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := EncodeRS256(pyjson.NewObject(), "not a key"); err == nil {
		t.Fatal("garbage key encoded")
	}
}
