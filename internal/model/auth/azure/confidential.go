package azure

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	assertionType     = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	assertionLifetime = 600
	tokenFileInterval = 600
)

type confidential struct {
	c         *client
	label     string
	tenant    string
	clientID  string
	authority string
	secret    string
	assertion func(ctx context.Context, audience string) (string, error)
	cache     tokenCache
}

func (s *confidential) name() string { return s.label }

func (s *confidential) String() string { return s.label + " for client " + s.clientID }

func (s *confidential) GoString() string { return s.String() }

func (s *confidential) token(ctx context.Context, scope string, _ bool) (accessToken, error) {
	return s.cache.get(ctx, scope, func(ctx context.Context) (accessToken, error) {
		found, err := s.c.discover(ctx, s.label, s.authority, s.tenant)
		if err != nil {
			return accessToken{}, err
		}
		form := []pair{{"client_id", s.clientID}, {"grant_type", "client_credentials"}}
		if s.assertion != nil {
			assertion, err := s.assertion(ctx, found.token)
			if err != nil {
				return accessToken{}, err
			}
			form = append(form, pair{"client_assertion", assertion}, pair{"client_info", "1"}, pair{"client_assertion_type", assertionType})
		} else {
			form = append(form, pair{"client_info", "1"})
		}
		form = append(form, pair{"scope", scope})
		if s.secret != "" {
			form = append(form, pair{"client_secret", s.secret})
		}
		token, _, err := s.c.requestToken(ctx, s.label, found.token, form)
		return token, err
	})
}

func checkApplication(label, tenant, clientID string) error {
	if clientID == "" {
		return &AuthError{Credential: label, Message: "client_id should be the id of a Microsoft Entra application"}
	}
	if !validTenant(tenant) {
		return &AuthError{Credential: label, Message: invalidTenantText}
	}
	return nil
}

func (c *client) clientSecret(tenant, clientID, secret, authority string) (source, error) {
	if err := checkApplication(nameClientSecret, tenant, clientID); err != nil {
		return nil, err
	}
	if secret == "" {
		return nil, &AuthError{Credential: nameClientSecret, Message: "client_secret should be a Microsoft Entra application's client secret"}
	}
	return &confidential{c: c, label: nameClientSecret, tenant: tenant, clientID: clientID, authority: authority, secret: secret}, nil
}

func (c *client) workloadIdentity(tenant, clientID, tokenFile, authority string) (source, error) {
	switch {
	case tenant == "":
		return nil, &AuthError{Credential: nameWorkload, Message: "'tenant_id' is required. Please pass it in or set the AZURE_TENANT_ID environment variable"}
	case clientID == "":
		return nil, &AuthError{Credential: nameWorkload, Message: "'client_id' is required. Please pass it in or set the AZURE_CLIENT_ID environment variable"}
	case tokenFile == "":
		return nil, &AuthError{Credential: nameWorkload, Message: "'token_file_path' is required. Please pass it in or set the AZURE_FEDERATED_TOKEN_FILE environment variable"}
	}
	if err := checkApplication(nameWorkload, tenant, clientID); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var assertion string
	var lastRead int64
	read := func(context.Context, string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if now := time.Now().Unix(); now-lastRead > tokenFileInterval {
			data, err := readBounded(tokenFile)
			if err != nil {
				return "", &AuthError{Credential: nameWorkload, Message: "federated token file " + tokenFile + " could not be read", Err: err}
			}
			assertion, lastRead = string(data), now
		}
		return assertion, nil
	}
	return &confidential{c: c, label: nameWorkload, tenant: tenant, clientID: clientID, authority: authority, assertion: read}, nil
}

func (c *client) certificate(tenant, clientID, path, password string, sendChain bool, authority string) (source, error) {
	if err := checkApplication(nameCertificate, tenant, clientID); err != nil {
		return nil, err
	}
	data, err := readBounded(path)
	if err != nil {
		return nil, &AuthError{Credential: nameCertificate, Message: "certificate file " + path + " could not be read", Err: err}
	}
	key, chain, err := loadCertificate(data, password)
	if err != nil {
		return nil, &AuthError{Credential: nameCertificate, Message: err.Error()}
	}
	thumbprint := sha1.Sum(chain[0].Raw)
	header := struct {
		Alg string   `json:"alg"`
		Typ string   `json:"typ"`
		X5c []string `json:"x5c,omitempty"`
		X5t string   `json:"x5t"`
	}{Alg: "RS256", Typ: "JWT", X5t: base64.URLEncoding.EncodeToString(thumbprint[:])}
	if sendChain {
		for _, cert := range chain {
			header.X5c = append(header.X5c, base64.StdEncoding.EncodeToString(cert.Raw))
		}
	}
	var mu sync.Mutex
	var cached string
	var cachedFor string
	var renewAt int64
	sign := func(_ context.Context, audience string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now().Unix()
		if cached != "" && cachedFor == audience && now < renewAt {
			return cached, nil
		}
		claims := struct {
			Aud string `json:"aud"`
			Iss string `json:"iss"`
			Sub string `json:"sub"`
			Exp int64  `json:"exp"`
			Iat int64  `json:"iat"`
			Jti string `json:"jti"`
		}{Aud: audience, Iss: clientID, Sub: clientID, Exp: now + assertionLifetime, Iat: now, Jti: uuid.NewString()}
		headerJSON, err := json.Marshal(header)
		if err != nil {
			return "", err
		}
		claimsJSON, err := json.Marshal(claims)
		if err != nil {
			return "", err
		}
		enc := base64.RawURLEncoding
		signing := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(claimsJSON)
		signature, err := jwt.SigningMethodRS256.Sign(signing, key)
		if err != nil {
			return "", &AuthError{Credential: nameCertificate, Message: "client assertion could not be signed", Err: err}
		}
		cached, cachedFor, renewAt = signing+"."+enc.EncodeToString(signature), audience, now+assertionLifetime/2
		return cached, nil
	}
	return &confidential{c: c, label: nameCertificate, tenant: tenant, clientID: clientID, authority: authority, assertion: sign}, nil
}

func readBounded(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > fileLimit {
		return nil, errors.New("file is larger than 1 MiB")
	}
	return os.ReadFile(path)
}

func parseKeyDER(der []byte) (any, error) {
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	return x509.ParseECPrivateKey(der)
}

func loadCertificate(data []byte, password string) (*rsa.PrivateKey, []*x509.Certificate, error) {
	var key any
	var chain []*x509.Certificate
	if bytes.Contains(data, []byte("-----BEGIN")) {
		rest := data
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			switch {
			case block.Type == "CERTIFICATE":
				cert, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return nil, nil, errors.New("malformed certificate")
				}
				chain = append(chain, cert)
			case key != nil:
			case block.Type == "ENCRYPTED PRIVATE KEY":
				der, err := decryptShroudedKey(block.Bytes, password)
				if err != nil {
					return nil, nil, err
				}
				if key, err = parseKeyDER(der); err != nil {
					return nil, nil, errors.New("private key could not be read")
				}
			case len(block.Type) >= 11 && block.Type[len(block.Type)-11:] == "PRIVATE KEY":
				der := block.Bytes
				if block.Headers["DEK-Info"] != "" {
					plain, err := decryptLegacyPEM(block, password)
					if err != nil {
						return nil, nil, err
					}
					der = plain
				}
				var err error
				if key, err = parseKeyDER(der); err != nil {
					return nil, nil, errors.New("private key could not be read")
				}
			}
		}
	} else {
		var err error
		if key, chain, err = decodePKCS12(data, password); err != nil {
			return nil, nil, errors.New("failed to deserialize certificate in PEM or PKCS12 format")
		}
	}
	if key == nil {
		return nil, nil, errors.New("the certificate must include its private key")
	}
	if len(chain) == 0 {
		return nil, nil, errors.New("failed to deserialize certificate in PEM or PKCS12 format")
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("the certificate must have an RSA private key because RS256 is used for signing")
	}
	return rsaKey, chain, nil
}
