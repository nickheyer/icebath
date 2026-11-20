package pyjwt

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Compact RS256 token as PyJWT jwt.encode builds it with sorted header keys and a compact payload
func EncodeRS256(payload *pyjson.Object, pemKey string) (string, error) {
	key, err := loadRSAPrivateKey(pemKey)
	if err != nil {
		return "", err
	}
	body, err := pyjson.Dumps(payload, pyjson.Separators(",", ":"))
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(body))
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// RSA private key of a PKCS1 or PKCS8 PEM block, the error PyJWT raises for anything else
func loadRSAPrivateKey(pemKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, errors.New("Could not parse the provided public key.")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("Expecting a RSAPrivateKey")
	}
	return k, nil
}
