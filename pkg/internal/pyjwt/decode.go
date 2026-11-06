package pyjwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// DecodeOptions carries the jwt.decode keywords the callers pass
type DecodeOptions struct {
	Algorithms []string
	Audience   string
	Issuer     string
	Leeway     time.Duration
	Require    []string
	Now        func() time.Time
}

// Error is a PyJWT exception with its class name and message
type Error struct {
	Class string
	Msg   string
}

func (e *Error) Error() string { return e.Msg }

func jwtErr(class, format string, args ...any) error {
	return &Error{Class: class, Msg: fmt.Sprintf(format, args...)}
}

func b64Segment(seg, what string) ([]byte, error) {
	out, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
	if err != nil {
		return nil, jwtErr("DecodeError", "Invalid %s padding", what)
	}
	return out, nil
}

// Header of a compact token read without verifying its signature
func UnverifiedHeader(token string) (*pyjson.Object, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, jwtErr("DecodeError", "Not enough segments")
	}
	raw, err := b64Segment(parts[0], "header")
	if err != nil {
		return nil, err
	}
	v, err := pyjson.Loads(string(raw))
	header, ok := v.(*pyjson.Object)
	if err != nil || !ok {
		return nil, jwtErr("DecodeError", "Invalid header string: must be a json object")
	}
	return header, nil
}

func hashFor(alg string) (crypto.Hash, bool) {
	switch alg[2:] {
	case "256":
		return crypto.SHA256, true
	case "384":
		return crypto.SHA384, true
	case "512":
		return crypto.SHA512, true
	}
	return 0, false
}

func digest(h crypto.Hash, msg []byte) []byte {
	switch h {
	case crypto.SHA384:
		d := sha512.Sum384(msg)
		return d[:]
	case crypto.SHA512:
		d := sha512.Sum512(msg)
		return d[:]
	}
	d := sha256.Sum256(msg)
	return d[:]
}

// Public key of a PEM public key, certificate or private key for the algorithm family
func prepareKey(alg string, key any) (any, error) {
	text, isText := key.(string)
	if !isText {
		return key, nil
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, jwtErr("InvalidKeyError", "Could not parse the provided public key.")
	}
	var parsed any
	var err error
	switch block.Type {
	case "CERTIFICATE":
		var cert *x509.Certificate
		if cert, err = x509.ParseCertificate(block.Bytes); err == nil {
			parsed = cert.PublicKey
		}
	case "RSA PUBLIC KEY":
		parsed, err = x509.ParsePKCS1PublicKey(block.Bytes)
	case "RSA PRIVATE KEY":
		var k *rsa.PrivateKey
		if k, err = x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			parsed = &k.PublicKey
		}
	case "EC PRIVATE KEY":
		var k *ecdsa.PrivateKey
		if k, err = x509.ParseECPrivateKey(block.Bytes); err == nil {
			parsed = &k.PublicKey
		}
	case "PRIVATE KEY":
		var k any
		if k, err = x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			if signer, ok := k.(crypto.Signer); ok {
				parsed = signer.Public()
			}
		}
	default:
		parsed, err = x509.ParsePKIXPublicKey(block.Bytes)
	}
	if err != nil || parsed == nil {
		return nil, jwtErr("InvalidKeyError", "Could not parse the provided public key.")
	}
	return parsed, nil
}

func verifySignature(alg string, key any, signing, sig []byte) error {
	h, ok := hashFor(alg)
	if !ok {
		return jwtErr("NotImplementedError", "Algorithm not supported")
	}
	key, err := prepareKey(alg, key)
	if err != nil {
		return err
	}
	sum := digest(h, signing)
	switch alg[:2] {
	case "RS":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return jwtErr("InvalidKeyError", "Expecting a PEM-formatted key.")
		}
		if rsa.VerifyPKCS1v15(pub, h, sum, sig) != nil {
			return jwtErr("InvalidSignatureError", "Signature verification failed")
		}
		return nil
	case "PS":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return jwtErr("InvalidKeyError", "Expecting a PEM-formatted key.")
		}
		if rsa.VerifyPSS(pub, h, sum, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) != nil {
			return jwtErr("InvalidSignatureError", "Signature verification failed")
		}
		return nil
	case "ES":
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return jwtErr("InvalidKeyError", "Expecting a EllipticCurvePublicKey.")
		}
		size := (pub.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return jwtErr("InvalidSignatureError", "Signature verification failed")
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(pub, sum, r, s) {
			return jwtErr("InvalidSignatureError", "Signature verification failed")
		}
		return nil
	}
	return jwtErr("NotImplementedError", "Algorithm not supported")
}

func numericClaim(payload *pyjson.Object, name string) (float64, error) {
	v, _ := payload.Get(name)
	switch n := v.(type) {
	case int64:
		return float64(n), nil
	case float64:
		return math.Trunc(n), nil
	case *big.Int:
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case string:
		if i, ok := pystr.IntText(n); ok {
			f, _ := new(big.Float).SetInt(i).Float64()
			return f, nil
		}
	}
	switch name {
	case "iat":
		return 0, jwtErr("InvalidIssuedAtError", "Issued At claim (iat) must be an integer.")
	case "nbf":
		return 0, jwtErr("DecodeError", "Not Before claim (nbf) must be an integer.")
	}
	return 0, jwtErr("DecodeError", "Expiration Time claim (exp) must be an integer.")
}

// Claims checks in PyJWT order of required, iat, nbf, exp, iss and aud
func validateClaims(payload *pyjson.Object, o DecodeOptions) error {
	for _, claim := range o.Require {
		if v, ok := payload.Get(claim); !ok || v == nil {
			return jwtErr("MissingRequiredClaimError", "Token is missing the %q claim", claim)
		}
	}
	nowFn := o.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := float64(nowFn().UnixNano()) / 1e9
	leeway := o.Leeway.Seconds()
	if _, ok := payload.Get("iat"); ok {
		iat, err := numericClaim(payload, "iat")
		if err != nil {
			return err
		}
		if iat > now+leeway {
			return jwtErr("ImmatureSignatureError", "The token is not yet valid (iat)")
		}
	}
	if _, ok := payload.Get("nbf"); ok {
		nbf, err := numericClaim(payload, "nbf")
		if err != nil {
			return err
		}
		if nbf > now+leeway {
			return jwtErr("ImmatureSignatureError", "The token is not yet valid (nbf)")
		}
	}
	if _, ok := payload.Get("exp"); ok {
		exp, err := numericClaim(payload, "exp")
		if err != nil {
			return err
		}
		if exp <= now-leeway {
			return jwtErr("ExpiredSignatureError", "Signature has expired")
		}
	}
	if o.Issuer != "" {
		iss, ok := payload.Get("iss")
		if !ok {
			return jwtErr("MissingRequiredClaimError", "Token is missing the %q claim", "iss")
		}
		if text, isText := iss.(string); !isText || text != o.Issuer {
			return jwtErr("InvalidIssuerError", "Invalid issuer")
		}
	}
	aud, hasAud := payload.Get("aud")
	if o.Audience == "" {
		if hasAud {
			return jwtErr("InvalidAudienceError", "Invalid audience")
		}
		return nil
	}
	if !hasAud {
		return jwtErr("MissingRequiredClaimError", "Token is missing the %q claim", "aud")
	}
	var auds []string
	switch a := aud.(type) {
	case string:
		auds = []string{a}
	case []any:
		for _, item := range a {
			text, ok := item.(string)
			if !ok {
				return jwtErr("InvalidAudienceError", "Invalid claim format in token")
			}
			auds = append(auds, text)
		}
	default:
		return jwtErr("InvalidAudienceError", "Invalid claim format in token")
	}
	if !slices.Contains(auds, o.Audience) {
		return jwtErr("InvalidAudienceError", "Audience doesn't match")
	}
	return nil
}

// Verified claims of a compact JWS token with PyJWT jwt.decode checks and messages
func Decode(token string, key any, o DecodeOptions) (*pyjson.Object, error) {
	header, err := UnverifiedHeader(token)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(token, ".")
	rawPayload, err := b64Segment(parts[1], "payload")
	if err != nil {
		return nil, err
	}
	sig, err := b64Segment(parts[2], "crypto")
	if err != nil {
		return nil, err
	}
	algValue, _ := header.Get("alg")
	alg, _ := algValue.(string)
	if !slices.Contains(o.Algorithms, alg) || len(alg) < 5 {
		return nil, jwtErr("InvalidAlgorithmError", "The specified alg value is not allowed")
	}
	if err := verifySignature(alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, err
	}
	v, err := pyjson.Loads(string(rawPayload))
	payload, ok := v.(*pyjson.Object)
	if err != nil || !ok {
		return nil, jwtErr("DecodeError", "Invalid payload string: must be a json object")
	}
	if err := validateClaims(payload, o); err != nil {
		return nil, err
	}
	return payload, nil
}
