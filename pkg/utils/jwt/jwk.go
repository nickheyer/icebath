package jwtutil

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// One usable key of a JWK set: its key id, use and algorithm (each empty when the set leaves it out), the public key (*rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey, or the []byte secret of an oct key) and every member of the key as published
type Key struct {
	ID        string
	Use       string
	Algorithm string
	Public    any
	Params    map[string]any
}

// Key of one JWK set member, false for a member with unknown or broken key material
func parseKey(m map[string]any) (*Key, bool) {
	public, ok := publicKey(m)
	if !ok {
		return nil, false
	}
	k := &Key{Public: public, Params: m}
	k.ID, _ = m["kid"].(string)
	k.Use, _ = m["use"].(string)
	k.Algorithm, _ = m["alg"].(string)
	return k, true
}

// Whether a key may verify signatures: its use is absent or sig and its key operations are absent or include verify
func canVerify(k *Key) bool {
	if k.Use != "" && k.Use != "sig" {
		return false
	}
	ops, present := k.Params["key_ops"]
	if !present {
		return true
	}
	list, _ := ops.([]any)
	return slices.ContainsFunc(list, func(op any) bool { return op == "verify" })
}

func publicKey(m map[string]any) (any, bool) {
	switch m["kty"] {
	case "RSA":
		n, okN := member(m, "n")
		e, okE := member(m, "e")
		if !okN || !okE || len(n) == 0 {
			return nil, false
		}
		exp := new(big.Int).SetBytes(e)
		if exp.Sign() <= 0 || exp.BitLen() > 31 {
			return nil, false
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}, true
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
		x, okX := member(m, "x")
		y, okY := member(m, "y")
		size := (curve.Params().BitSize + 7) / 8
		if !okX || !okY || len(x) > size || len(y) > size {
			return nil, false
		}
		point := make([]byte, 1+2*size)
		point[0] = 4
		copy(point[1+size-len(x):1+size], x)
		copy(point[1+2*size-len(y):], y)
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, point)
		return pub, err == nil
	case "OKP":
		x, ok := member(m, "x")
		if !ok || m["crv"] != "Ed25519" || len(x) != ed25519.PublicKeySize {
			return nil, false
		}
		return ed25519.PublicKey(x), true
	case "oct":
		k, ok := member(m, "k")
		return k, ok && len(k) > 0
	}
	return nil, false
}

// Whether a public key has the type, and for ECDSA the curve, that the signing method verifies with; false for the none method
func fits(m jwt.SigningMethod, public any) bool {
	switch m := m.(type) {
	case *jwt.SigningMethodRSA, *jwt.SigningMethodRSAPSS:
		_, ok := public.(*rsa.PublicKey)
		return ok
	case *jwt.SigningMethodECDSA:
		k, ok := public.(*ecdsa.PublicKey)
		return ok && k.Curve.Params().BitSize == m.CurveBits
	case *jwt.SigningMethodEd25519:
		_, ok := public.(ed25519.PublicKey)
		return ok
	case *jwt.SigningMethodHMAC:
		_, ok := public.([]byte)
		return ok
	case nil:
		return false
	}
	return m != jwt.SigningMethodNone
}

// Bytes of a base64url member, padded or not, false when it is missing or not base64url
func member(m map[string]any, name string) ([]byte, bool) {
	text, ok := m[name].(string)
	if !ok {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(text, "="))
	return raw, err == nil
}
