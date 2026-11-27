package mcp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/terminal"
)

// TLS settings of a server: verification on, off or against a CA bundle, and the client certificate when it has one
func tlsConfig(server string, verify, cert, key any) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	switch v := verify.(type) {
	case nil:
	case bool:
		config.InsecureSkipVerify = !v
	case string:
		path := terminal.ExpandUser(strings.TrimSpace(v))
		bundle, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("MCP server '%s': ssl_verify CA bundle not readable at %q: %w", server, path, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(bundle) {
			return nil, fmt.Errorf("MCP server '%s': ssl_verify CA bundle at %q holds no certificates", server, path)
		}
		config.RootCAs = pool
	default:
		return nil, fmt.Errorf("MCP server '%s': ssl_verify must be a boolean or a CA bundle path", server)
	}
	certificate, err := clientCertificate(server, cert, key)
	if err != nil {
		return nil, err
	}
	if certificate != nil {
		config.Certificates = []tls.Certificate{*certificate}
	}
	return config, nil
}

// Client certificate of client_cert and client_key: one PEM path holding both, a path for each, or a list of cert, key and passphrase
func clientCertificate(server string, rawCert, rawKey any) (*tls.Certificate, error) {
	if rawCert == nil && rawKey == nil {
		return nil, nil
	}
	prefix := fmt.Sprintf("MCP server '%s': ", server)
	file := func(v any, label string) (string, error) {
		text, ok := v.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("%s%s must be a non-empty path", prefix, label)
		}
		path := terminal.ExpandUser(strings.TrimSpace(text))
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s%s not found at %q: %w", prefix, label, path, os.ErrNotExist)
		}
		return path, nil
	}
	list, isList := rawCert.([]any)
	if !isList {
		certPath, err := file(rawCert, "client_cert")
		if err != nil {
			return nil, err
		}
		keyPath := certPath
		if rawKey != nil {
			if keyPath, err = file(rawKey, "client_key"); err != nil {
				return nil, err
			}
		}
		return keyPair(certPath, keyPath, "")
	}
	if rawKey != nil {
		return nil, fmt.Errorf("%sspecify either client_cert as a list [cert, key] OR client_cert + client_key, not both", prefix)
	}
	if len(list) != 2 && len(list) != 3 {
		return nil, fmt.Errorf("%sclient_cert list form must have 2 or 3 elements (got %d)", prefix, len(list))
	}
	certPath, err := file(list[0], "client_cert[0]")
	if err != nil {
		return nil, err
	}
	keyPath, err := file(list[1], "client_cert[1]")
	if err != nil {
		return nil, err
	}
	password := ""
	if len(list) == 3 {
		text, ok := list[2].(string)
		if !ok {
			return nil, fmt.Errorf("%sclient_cert[2] (key passphrase) must be a string", prefix)
		}
		password = text
	}
	return keyPair(certPath, keyPath, password)
}

// Certificate and key of PEM files, the key decrypted with the passphrase when it is protected
func keyPair(certPath, keyPath, password string) (*tls.Certificate, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM := certPEM
	if keyPath != certPath {
		if keyPEM, err = os.ReadFile(keyPath); err != nil {
			return nil, err
		}
	}
	der, kind, err := privateKey(keyPEM, password)
	if err != nil {
		return nil, fmt.Errorf("client key %s: %w", keyPath, err)
	}
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

// Plain DER of the first private key block and the PEM type it has decrypted
func privateKey(data []byte, password string) ([]byte, string, error) {
	for rest := data; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			return nil, "", errors.New("no private key block found")
		}
		switch {
		case block.Type == "ENCRYPTED PRIVATE KEY":
			der, err := decryptPKCS8(block.Bytes, []byte(password))
			return der, "PRIVATE KEY", err
		case strings.HasSuffix(block.Type, "PRIVATE KEY"):
			if block.Headers["Proc-Type"] == "4,ENCRYPTED" {
				der, err := x509.DecryptPEMBlock(block, []byte(password)) //nolint:staticcheck // legacy encrypted PEM keys are still in use
				return der, block.Type, err
			}
			return block.Bytes, block.Type, nil
		}
	}
}

var (
	oidPBES2      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHMACSHA1   = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHMACSHA224 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 8}
	oidHMACSHA256 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidHMACSHA384 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 10}
	oidHMACSHA512 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 11}
	oidAES128CBC  = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC  = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC  = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	oidDESEDE3CBC = asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 7}
)

type algorithm struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

// EncryptedPrivateKeyInfo protected with PBES2 and PBKDF2, decrypted
func decryptPKCS8(der, password []byte) ([]byte, error) {
	var outer struct {
		Algo asn1.RawValue
		Data []byte
	}
	if _, err := asn1.Unmarshal(der, &outer); err != nil {
		return nil, fmt.Errorf("parse encrypted key: %w", err)
	}
	var algo algorithm
	if _, err := asn1.Unmarshal(outer.Algo.FullBytes, &algo); err != nil {
		return nil, fmt.Errorf("parse key algorithm: %w", err)
	}
	if !algo.Algorithm.Equal(oidPBES2) {
		return nil, fmt.Errorf("unsupported encrypted key algorithm %v", algo.Algorithm)
	}
	var params struct {
		KDF    algorithm
		Scheme algorithm
	}
	if _, err := asn1.Unmarshal(algo.Parameters.FullBytes, &params); err != nil {
		return nil, fmt.Errorf("parse PBES2 parameters: %w", err)
	}
	if !params.KDF.Algorithm.Equal(oidPBKDF2) {
		return nil, fmt.Errorf("unsupported key derivation %v", params.KDF.Algorithm)
	}
	var kdf struct {
		Salt       []byte
		Iterations int
		KeyLength  int       `asn1:"optional"`
		PRF        algorithm `asn1:"optional"`
	}
	if _, err := asn1.Unmarshal(params.KDF.Parameters.FullBytes, &kdf); err != nil {
		return nil, fmt.Errorf("parse PBKDF2 parameters: %w", err)
	}
	prf := hmacHash(kdf.PRF.Algorithm)
	if prf == nil {
		return nil, fmt.Errorf("unsupported PBKDF2 pseudo random function %v", kdf.PRF.Algorithm)
	}
	var iv []byte
	if _, err := asn1.Unmarshal(params.Scheme.Parameters.FullBytes, &iv); err != nil {
		return nil, fmt.Errorf("parse cipher IV: %w", err)
	}
	keyLen, newCipher, err := blockCipher(params.Scheme.Algorithm)
	if err != nil {
		return nil, err
	}
	key, err := pbkdf2.Key(prf, string(password), kdf.Salt, kdf.Iterations, keyLen)
	if err != nil {
		return nil, err
	}
	block, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	if len(outer.Data) == 0 || len(outer.Data)%block.BlockSize() != 0 || len(iv) != block.BlockSize() {
		return nil, errors.New("encrypted key payload is not block aligned")
	}
	plain := make([]byte, len(outer.Data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, outer.Data)
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > block.BlockSize() || pad > len(plain) {
		return nil, errors.New("wrong passphrase or corrupt encrypted key")
	}
	return plain[:len(plain)-pad], nil
}

// Hash of an HMAC identifier, SHA-1 when none is named
func hmacHash(oid asn1.ObjectIdentifier) func() hash.Hash {
	switch {
	case len(oid) == 0, oid.Equal(oidHMACSHA1):
		return sha1.New
	case oid.Equal(oidHMACSHA224):
		return sha256.New224
	case oid.Equal(oidHMACSHA256):
		return sha256.New
	case oid.Equal(oidHMACSHA384):
		return sha512.New384
	case oid.Equal(oidHMACSHA512):
		return sha512.New
	}
	return nil
}

func blockCipher(oid asn1.ObjectIdentifier) (int, func([]byte) (cipher.Block, error), error) {
	switch {
	case oid.Equal(oidAES128CBC):
		return 16, aes.NewCipher, nil
	case oid.Equal(oidAES192CBC):
		return 24, aes.NewCipher, nil
	case oid.Equal(oidAES256CBC):
		return 32, aes.NewCipher, nil
	case oid.Equal(oidDESEDE3CBC):
		return 24, des.NewTripleDESCipher, nil
	}
	return 0, nil, fmt.Errorf("unsupported encryption scheme %v", oid)
}
