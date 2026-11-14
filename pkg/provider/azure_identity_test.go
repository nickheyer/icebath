package provider

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Credential double answering a scope bound token and counting mints
type fakeEntraCredential struct {
	clientID string
	delay    time.Duration
	calls    int32
	expires  int64
}

func (f *fakeEntraCredential) Name() string { return "FakeCredential" }

func (f *fakeEntraCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	exp := f.expires
	if exp == 0 {
		exp = 9999999999
	}
	return EntraAccessToken{Token: "jwt-for-" + strings.Join(scopes, " "), ExpiresOn: exp}, nil
}

// Clears AZURE_ and managed identity env vars and swaps the default chain builder for a counting double
func entraIsolate(t *testing.T) *int32 {
	t.Helper()
	for _, n := range []string{"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET", "AZURE_FEDERATED_TOKEN_FILE", "AZURE_AUTHORITY_HOST",
		"AZURE_CLIENT_CERTIFICATE_PATH", "AZURE_USERNAME", "AZURE_PASSWORD", "AZURE_TOKEN_CREDENTIALS", "IDENTITY_ENDPOINT", "IDENTITY_HEADER",
		"IDENTITY_SERVER_THUMBPRINT", "IMDS_ENDPOINT", "MSI_ENDPOINT", "MSI_SECRET", "AZURE_POD_IDENTITY_AUTHORITY_HOST"} {
		t.Setenv(n, "")
		os.Unsetenv(n)
	}
	ResetEntraCredentialCache()
	var count int32
	orig, origSleep := entraBuildDefaultFn, entraSleep
	entraBuildDefaultFn = func(EntraIdentityConfig) (entraCredential, error) {
		atomic.AddInt32(&count, 1)
		return &fakeEntraCredential{}, nil
	}
	entraSleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { entraBuildDefaultFn, entraSleep = orig, origSleep; ResetEntraCredentialCache() })
	return &count
}

type headerCapture struct{ got http.Header }

func (h *headerCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	h.got = r.Header.Clone()
	return &http.Response{StatusCode: 200, Body: http.NoBody, Request: r}, nil
}

func TestEntraBearerTransportOverridesAuthorizationHeader(t *testing.T) {
	base := &headerCapture{}
	tr, err := NewEntraBearerTransport(func(context.Context) (string, error) { return "fresh-jwt", nil }, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://r.services.ai.azure.com/anthropic/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer entra-id-bearer-via-http-hook")
	req.Header.Set("api-key", "k")
	req.Header.Set("x-api-key", "k")
	tr.RoundTrip(req)
	if base.got.Get("Authorization") != "Bearer fresh-jwt" || base.got.Get("api-key") != "" || base.got.Get("x-api-key") != "" {
		t.Fatalf("headers = %v", base.got)
	}
	tr.SendsAsAPIKey = func(tok string) bool { return strings.HasPrefix(tok, "fresh") }
	tr.RoundTrip(req)
	if base.got.Get("x-api-key") != "fresh-jwt" || base.got.Get("Authorization") != "" {
		t.Fatalf("api key headers = %v", base.got)
	}
	if _, err := NewEntraBearerTransport(nil, nil, base); err == nil {
		t.Fatal("a nil provider must be rejected")
	}
}

func TestEntraBearerTransportStripsAuthHeadersAndWarnsWhenProviderFails(t *testing.T) {
	var logs bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(orig)
	base := &headerCapture{}
	tr, _ := NewEntraBearerTransport(func(context.Context) (string, error) { return "", nil }, nil, base)
	req, _ := http.NewRequest(http.MethodPost, "https://r.services.ai.azure.com/anthropic/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer entra-id-bearer-via-http-hook")
	req.Header.Set("Api-Key", "k")
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if base.got.Get("Authorization") != "" || base.got.Get("Api-Key") != "" {
		t.Fatalf("auth headers must be stripped, got %v", base.got)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "token provider returned empty value") {
		t.Fatalf("missing warning: %s", logs.String())
	}
}

func TestEntraIdentityConfigToDictRoundTrip(t *testing.T) {
	cfg := NewEntraIdentityConfig("https://cognitiveservices.azure.com/.default", false)
	if back := EntraIdentityConfigFromDict(cfg.ToDict(), ""); back != cfg {
		t.Fatalf("round trip = %+v", back)
	}
	if got := NewEntraIdentityConfig("  ", true); got.Scope != ScopeAIAzureDefault {
		t.Fatalf("blank scope = %q", got.Scope)
	}
	if got := EntraIdentityConfigFromDict(nil, "https://x/.default"); got.Scope != "https://x/.default" || !got.ExcludeInteractiveBrowser {
		t.Fatalf("default scope = %+v", got)
	}
}

func TestBuildEntraCredentialCachedPerConfig(t *testing.T) {
	count := entraIsolate(t)
	ctx := context.Background()
	cfg := NewEntraIdentityConfig("", true)
	a, _ := buildEntraCredential(ctx, cfg)
	b, _ := buildEntraCredential(ctx, cfg)
	if a != b || atomic.LoadInt32(count) != 1 {
		t.Fatal("one config must reuse one credential")
	}
	c, _ := buildEntraCredential(ctx, NewEntraIdentityConfig("https://other/.default", true))
	if c == a || atomic.LoadInt32(count) != 2 {
		t.Fatal("distinct configs must get distinct credentials")
	}
}

func TestEntraScopedCredentialTwoHomesMultiplexRefusesAmbientChain(t *testing.T) {
	count := entraIsolate(t)
	root := t.TempDir()
	homeA, homeB := filepath.Join(root, "home-A"), filepath.Join(root, "home-B")
	os.MkdirAll(homeA, 0o700)
	os.MkdirAll(homeB, 0o700)
	os.WriteFile(filepath.Join(homeA, ".env"), []byte("AZURE_TENANT_ID=tenant-A\nAZURE_CLIENT_ID=client-A\nAZURE_CLIENT_SECRET=secret-A\n"), 0o600)
	os.WriteFile(filepath.Join(homeB, ".env"), nil, 0o600)
	t.Setenv("AZURE_TENANT_ID", "tenant-A")
	t.Setenv("AZURE_CLIENT_ID", "client-A")
	t.Setenv("AZURE_CLIENT_SECRET", "secret-A")
	cfg := NewEntraIdentityConfig("", true)
	if _, err := buildEntraCredential(context.Background(), cfg); err != nil || atomic.LoadInt32(count) != 1 {
		t.Fatalf("standalone keeps the ambient chain, err %v", err)
	}
	inScope := func(h string) context.Context {
		ctx := home.WithOverride(context.Background(), h)
		return WithMultiplexContext(WithSecretScope(ctx, config.LoadEnvFile(filepath.Join(h, ".env")), h), true)
	}
	credA, err := buildEntraCredential(inScope(homeA), cfg)
	if cs, ok := credA.(*entraClientSecretCredential); err != nil || !ok || cs.clientID != "client-A" {
		t.Fatalf("home A credential = %#v err %v", credA, err)
	}
	if _, err := buildEntraCredential(inScope(homeB), cfg); err == nil || !strings.Contains(err.Error(), "refused for this profile") {
		t.Fatalf("home B err = %v", err)
	}
	if probe := entraProbeToken(inScope(homeB), cfg, 5*time.Second); !strings.Contains(anyString(probe["error"]), "refused for this profile") {
		t.Fatalf("probe = %v", probe)
	}
	if again, _ := buildEntraCredential(inScope(homeA), cfg); again != credA {
		t.Fatal("home A must keep its cached credential")
	}
	if atomic.LoadInt32(count) != 1 {
		t.Fatal("home B must never reach the ambient chain")
	}
}

func TestBuildEntraTokenProviderReturnsCallableForScope(t *testing.T) {
	entraIsolate(t)
	src := BuildEntraTokenProvider(nil, "https://cognitiveservices.azure.com/.default", true)
	if tok, err := src(context.Background()); err != nil || tok != "jwt-for-https://cognitiveservices.azure.com/.default" {
		t.Fatalf("tok %q err %v", tok, err)
	}
	cfg := NewEntraIdentityConfig("https://from-config/.default", true)
	if tok, _ := BuildEntraTokenProvider(&cfg, "https://from-kwarg/.default", true)(context.Background()); tok != "jwt-for-https://from-config/.default" {
		t.Fatalf("config object must win, tok %q", tok)
	}
	src, err := entraTokenProvider("https://ai.azure.com/.default")
	if tok, _ := src(context.Background()); err != nil || tok != "jwt-for-https://ai.azure.com/.default" {
		t.Fatalf("azure foundry scope tok %q err %v", tok, err)
	}
}

func TestEntraProviderReusesTokenUntilRefreshWindow(t *testing.T) {
	entraIsolate(t)
	fake := &fakeEntraCredential{expires: time.Now().Unix() + 3600}
	entraBuildDefaultFn = func(EntraIdentityConfig) (entraCredential, error) { return fake, nil }
	src := BuildEntraTokenProvider(nil, "", true)
	src(context.Background())
	src(context.Background())
	if atomic.LoadInt32(&fake.calls) != 1 {
		t.Fatal("a token far from expiry must be reused")
	}
	fake.expires = time.Now().Unix() + 100
	ResetEntraCredentialCache()
	src = BuildEntraTokenProvider(nil, "", true)
	src(context.Background())
	src(context.Background())
	if atomic.LoadInt32(&fake.calls) != 3 {
		t.Fatalf("a token inside the five minute window must be reminted, calls %d", fake.calls)
	}
}

func TestEntraChainNeedsNoInstall(t *testing.T) {
	entraIsolate(t)
	info := DescribeActiveEntraCredential(context.Background(), nil, "", time.Second)
	if info["ok"] != true || info["hint"] != nil || info["scope"] != ScopeAIAzureDefault {
		t.Fatalf("info = %v", info)
	}
}

func TestHasAzureIdentityCredentialsReturnsFalseOnTimeout(t *testing.T) {
	entraIsolate(t)
	entraBuildDefaultFn = func(EntraIdentityConfig) (entraCredential, error) {
		return &fakeEntraCredential{delay: 300 * time.Millisecond}, nil
	}
	if HasAzureIdentityCredentials(context.Background(), nil, "", 20*time.Millisecond) {
		t.Fatal("a probe past its deadline must report false")
	}
	if !HasAzureIdentityCredentials(context.Background(), nil, "", 5*time.Second) {
		t.Fatal("a probe inside its deadline must report true")
	}
	info := DescribeActiveEntraCredential(context.Background(), nil, "", 20*time.Millisecond)
	if info["error"] != "Token probe timed out after 0s" || info["hint"] != entraProbeTimeoutHint {
		t.Fatalf("info = %v", info)
	}
}

func TestDescribeActiveCredentialReportsEnvSourcesForManagedIdentity(t *testing.T) {
	entraIsolate(t)
	t.Setenv("IDENTITY_ENDPOINT", "http://localhost:42356/msi/token")
	t.Setenv("AZURE_TENANT_ID", "tenant-x")
	info := DescribeActiveEntraCredential(context.Background(), nil, "", 5*time.Second)
	sources, _ := info["env_sources"].([]string)
	if info["ok"] != true || len(sources) != 1 || sources[0] != "ManagedIdentityCredential (IDENTITY_ENDPOINT)" || info["tenant_id_env"] != "tenant-x" ||
		info["expires_on"] != int64(9999999999) {
		t.Fatalf("info = %v", info)
	}
}

// AAD v2 token endpoint double recording each form
func entraAuthorityServer(t *testing.T, calls *int32, forms chan map[string]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		r.ParseForm()
		f := map[string]string{"path": r.URL.Path}
		for k := range r.Form {
			f[k] = r.Form.Get(k)
		}
		select {
		case forms <- f:
		default:
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "aad-" + f["grant_type"], "expires_in": 3600, "token_type": "Bearer"})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AZURE_AUTHORITY_HOST", srv.URL)
	return srv
}

func TestEntraEnvironmentClientSecretAndWorkloadIdentity(t *testing.T) {
	entraIsolate(t)
	var calls int32
	forms := make(chan map[string]string, 8)
	entraAuthorityServer(t, &calls, forms)
	t.Setenv("AZURE_TENANT_ID", "tenant-1")
	t.Setenv("AZURE_CLIENT_ID", "client-1")
	t.Setenv("AZURE_CLIENT_SECRET", "secret-1")
	dac, err := newEntraDefaultAzureCredential(true)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := dac.GetToken(context.Background(), ScopeAIAzureDefault)
	f := <-forms
	if err != nil || tok.Token != "aad-client_credentials" || f["path"] != "/tenant-1/oauth2/v2.0/token" || f["client_secret"] != "secret-1" || f["scope"] != ScopeAIAzureDefault {
		t.Fatalf("tok %v err %v form %v", tok, err, f)
	}
	dac.GetToken(context.Background(), ScopeAIAzureDefault)
	if atomic.LoadInt32(&calls) != 1 || dac.successful == nil || dac.successful.Name() != "EnvironmentCredential" {
		t.Fatal("the cached token and the winning credential must be reused")
	}
	tokenFile := filepath.Join(t.TempDir(), "fed")
	os.WriteFile(tokenFile, []byte("federated-jwt"), 0o600)
	w, err := newEntraWorkloadIdentityCredential("tenant-1", "client-1", tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	w.GetToken(context.Background(), ScopeAIAzureDefault)
	f = <-forms
	if f["client_assertion"] != "federated-jwt" || f["client_assertion_type"] != entraClientAssertionType {
		t.Fatalf("form = %v", f)
	}
	if _, err := newEntraWorkloadIdentityCredential("", "c", tokenFile); err == nil || !strings.Contains(err.Error(), "'tenant_id' is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestEntraManagedIdentityIMDSAndAppService(t *testing.T) {
	entraIsolate(t)
	var status int32 = 200
	var seen http.Header
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, query = r.Header.Clone(), r.URL.RawQuery
		w.WriteHeader(int(atomic.LoadInt32(&status)))
		w.Write([]byte(`{"access_token":"mi-token","expires_in":"86399","error_description":"Identity not found"}`))
	}))
	defer srv.Close()
	t.Setenv("AZURE_POD_IDENTITY_AUTHORITY_HOST", srv.URL)
	mi := newEntraManagedIdentityCredential("", true, true)
	tok, err := mi.GetToken(context.Background(), ScopeAIAzureDefault)
	if err != nil || tok.Token != "mi-token" || seen.Get("Metadata") != "true" || !strings.Contains(query, "resource=https%3A%2F%2Fai.azure.com") || tok.RefreshOn == 0 {
		t.Fatalf("tok %+v err %v query %s", tok, err, query)
	}
	atomic.StoreInt32(&status, 400)
	_, err = newEntraManagedIdentityCredential("", true, true).GetToken(context.Background(), ScopeAIAzureDefault)
	var unavailable *entraUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "No identity has been assigned to this resource. Error: Identity not found") {
		t.Fatalf("err = %v", err)
	}
	atomic.StoreInt32(&status, 200)
	t.Setenv("IDENTITY_ENDPOINT", srv.URL+"/msi/token")
	t.Setenv("IDENTITY_HEADER", "hdr")
	if tok, err := newEntraManagedIdentityCredential("uami", true, false).GetToken(context.Background(), ScopeAIAzureDefault); err != nil || tok.Token != "mi-token" ||
		seen.Get("X-IDENTITY-HEADER") != "hdr" || !strings.Contains(query, "api-version=2019-08-01") || !strings.Contains(query, "client_id=uami") {
		t.Fatalf("app service tok %+v err %v query %s", tok, err, query)
	}
}

func TestEntraAzureCLICredential(t *testing.T) {
	entraIsolate(t)
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	cli := &entraAzureCLICredential{timeout: 10 * time.Second, inChain: true}
	if _, err := cli.GetToken(context.Background(), ScopeAIAzureDefault); err == nil || err.Error() != "Azure CLI not found on path" {
		t.Fatalf("err = %v", err)
	}
	writeFakeAz(t, bin, true)
	if tok, err := cli.GetToken(context.Background(), ScopeAIAzureDefault); err != nil || tok.Token != "cli-token" || tok.ExpiresOn != 1999999999 {
		t.Fatalf("tok %+v err %v", tok, err)
	}
	writeFakeAz(t, bin, false)
	if _, err := cli.GetToken(context.Background(), ScopeAIAzureDefault); err == nil || err.Error() != "Please run 'az login' to set up an account" {
		t.Fatalf("err = %v", err)
	}
}

func TestEntraDefaultChainReportsEveryUnavailableCredential(t *testing.T) {
	entraIsolate(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	t.Setenv("AZURE_POD_IDENTITY_AUTHORITY_HOST", dead.URL)
	dac, err := newEntraDefaultAzureCredential(true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = dac.GetToken(context.Background(), ScopeAIAzureDefault)
	msg := err.Error()
	for _, want := range []string{"DefaultAzureCredential failed to retrieve a token from the included credentials.\nAttempted credentials:\n\tEnvironmentCredential: " + entraEnvUnavailable,
		"\tManagedIdentityCredential: " + entraIMDSUnavailable, "SharedTokenCacheCredential authentication unavailable. No accounts were found in the cache.",
		"\tAzureCliCredential: Azure CLI not found on path", "\tAzurePowerShellCredential: PowerShell is not installed",
		"\tAzureDeveloperCliCredential: " + entraAzdNotFound, "\tBrokerCredential: " + entraBrokerPlatformOnly, entraDACTroubleshoot} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "InteractiveBrowserCredential") {
		t.Fatal("the browser credential is excluded by default")
	}
	t.Setenv("AZURE_TOKEN_CREDENTIALS", "bogus")
	if _, err := newEntraDefaultAzureCredential(true); err == nil || !strings.Contains(err.Error(), "Invalid value for AZURE_TOKEN_CREDENTIALS: bogus") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("AZURE_TOKEN_CREDENTIALS", "prod")
	prod, _ := newEntraDefaultAzureCredential(true)
	if len(prod.creds) != 2 {
		t.Fatalf("prod chain = %d credentials", len(prod.creds))
	}
}

// Self signed RSA certificate for the certificate credential tests
func entraTestCert(t *testing.T) (*rsa.PrivateKey, *x509.Certificate) {
	key := vertexKey(t)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sp"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return key, cert
}

type testP12ContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue
}

type testP12SafeBag struct {
	ID    asn1.ObjectIdentifier
	Value asn1.RawValue
}

type testP12CertBag struct {
	ID   asn1.ObjectIdentifier
	Data asn1.RawValue
}

type testP12PFX struct {
	Version  int
	AuthSafe testP12ContentInfo
	MacData  p12MacData
}

func explicit0(inner []byte) asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: inner}
}

func mustDER(t *testing.T, v any) []byte {
	b, err := asn1.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// PBES2 AES-256-CBC encryption with PBKDF2-HMAC-SHA256 as OpenSSL 3 writes PKCS12 archives
func testPBES2Encrypt(t *testing.T, plain []byte, password string) (pkix.AlgorithmIdentifier, []byte) {
	salt, iv := []byte("saltsalt"), []byte("0123456789abcdef")
	key, _ := pbkdf2.Key(sha256.New, password, salt, 2048, 32)
	kdf := p12PBKDF2Params{Salt: salt, Iterations: 2048, PRF: pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}, Parameters: asn1.NullRawValue}}
	params := p12PBES2Params{KDF: pkix.AlgorithmIdentifier{Algorithm: oidPBKDF2, Parameters: asn1.RawValue{FullBytes: mustDER(t, kdf)}},
		Scheme: pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}, Parameters: asn1.RawValue{FullBytes: mustDER(t, iv)}}}
	pad := 16 - len(plain)%16
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(key)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return pkix.AlgorithmIdentifier{Algorithm: oidPBES2, Parameters: asn1.RawValue{FullBytes: mustDER(t, params)}}, out
}

// PFX with a shrouded key in plain safe contents and the certificate in encrypted safe contents, SHA-256 MAC
func testBuildPFX(t *testing.T, key *rsa.PrivateKey, cert *x509.Certificate, password string) []byte {
	alg, encKey := testPBES2Encrypt(t, mustPKCS8(t, key), password)
	keyBag := testP12SafeBag{ID: oidP12ShroudedBag, Value: explicit0(mustDER(t, p12EncryptedKeyInfo{Algorithm: alg, Data: encKey}))}
	certBag := testP12SafeBag{ID: oidP12CertBag, Value: explicit0(mustDER(t, testP12CertBag{ID: oidP12X509Cert, Data: explicit0(mustDER(t, cert.Raw))}))}
	certAlg, encCerts := testPBES2Encrypt(t, mustDER(t, []testP12SafeBag{certBag}), password)
	encData := p12EncryptedData{Info: p12EncryptedContentInfo{ContentType: oidP12Data, Algorithm: certAlg, Content: encCerts}}
	authSafe := mustDER(t, []testP12ContentInfo{
		{ContentType: oidP12Data, Content: explicit0(mustDER(t, mustDER(t, []testP12SafeBag{keyBag})))},
		{ContentType: oidP12EncryptedData, Content: explicit0(mustDER(t, encData))},
	})
	salt := []byte("macsalt!")
	macKey := p12KDF(sha256.New, salt, p12BMPPassword(password), 2048, 3, 32)
	mac := hmac.New(sha256.New, macKey)
	mac.Write(authSafe)
	pfx := testP12PFX{Version: 3, AuthSafe: testP12ContentInfo{ContentType: oidP12Data, Content: explicit0(mustDER(t, authSafe))},
		MacData: p12MacData{Mac: p12DigestInfo{Algorithm: pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}, Parameters: asn1.NullRawValue},
			Digest: mac.Sum(nil)}, MacSalt: salt, Iterations: 2048}}
	return mustDER(t, pfx)
}

func TestEntraCertificateCredentialPEMAndPKCS12(t *testing.T) {
	entraIsolate(t)
	key, cert := entraTestCert(t)
	dir := t.TempDir()
	pemPath := filepath.Join(dir, "sp.pem")
	pemData := append(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, key)}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	os.WriteFile(pemPath, pemData, 0o600)
	var calls int32
	forms := make(chan map[string]string, 4)
	entraAuthorityServer(t, &calls, forms)
	cc, err := newEntraCertificateCredential("tenant-1", "client-1", pemPath, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := cc.GetToken(context.Background(), ScopeAIAzureDefault); err != nil || tok.Token != "aad-client_credentials" {
		t.Fatalf("tok %+v err %v", tok, err)
	}
	parts := strings.Split((<-forms)["client_assertion"], ".")
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var header map[string]any
	json.Unmarshal(hb, &header)
	thumb := sha1.Sum(cert.Raw)
	if header["x5t"] != base64.RawURLEncoding.EncodeToString(thumb[:]) || header["alg"] != "RS256" || header["x5c"] == nil {
		t.Fatalf("header = %v", header)
	}
	pfx := testBuildPFX(t, key, cert, "pfx-pass")
	gotKey, certs, err := entraLoadCertificate(pfx, "pfx-pass")
	if err != nil || !gotKey.Equal(key) || len(certs) != 1 || !certs[0].Equal(cert) {
		t.Fatalf("pkcs12 key %v certs %d err %v", gotKey != nil, len(certs), err)
	}
	if _, _, err := entraLoadCertificate(pfx, "wrong"); err == nil || err.Error() != "Invalid password or PKCS12 data" {
		t.Fatalf("wrong password err = %v", err)
	}
}

func TestRC2MatchesRFC2268Vectors(t *testing.T) {
	seen := map[byte]bool{}
	for _, b := range rc2PiTable {
		seen[b] = true
	}
	if len(seen) != 256 {
		t.Fatal("PITABLE must be a permutation")
	}
	for _, v := range []struct {
		key  string
		bits int
		pt   string
		ct   string
	}{
		{"0000000000000000", 63, "0000000000000000", "ebb773f993278eff"},
		{"ffffffffffffffff", 64, "ffffffffffffffff", "278b27e42e2f0d49"},
		{"3000000000000000", 64, "1000000000000001", "30649edf9be7d2c2"},
		{"88", 64, "0000000000000000", "61a8a244adacccf0"},
	} {
		key, _ := hex.DecodeString(v.key)
		pt, _ := hex.DecodeString(v.pt)
		c := newRC2Cipher(key, v.bits)
		out := make([]byte, 8)
		c.Encrypt(out, pt)
		if hex.EncodeToString(out) != v.ct {
			t.Fatalf("key %s encrypt = %x want %s", v.key, out, v.ct)
		}
		back := make([]byte, 8)
		c.Decrypt(back, out)
		if !bytes.Equal(back, pt) {
			t.Fatalf("key %s decrypt = %x", v.key, back)
		}
	}
}

func TestEntraTokenCacheRefreshKeepsCachedTokenOnFailure(t *testing.T) {
	var cache entraTokenCache
	now := time.Now().Unix()
	calls := 0
	cache.get(context.Background(), []string{"s"}, func(context.Context) (EntraAccessToken, error) {
		calls++
		return EntraAccessToken{Token: "first", ExpiresOn: now + 7200, RefreshOn: now - 1}, nil
	})
	cache.lastRequest = now - 60
	tok, err := cache.get(context.Background(), []string{"s"}, func(context.Context) (EntraAccessToken, error) {
		calls++
		return EntraAccessToken{}, errors.New("token service down")
	})
	if err != nil || tok.Token != "first" || calls != 2 {
		t.Fatalf("tok %q err %v calls %d", tok.Token, err, calls)
	}
	cache.get(context.Background(), []string{"s"}, func(context.Context) (EntraAccessToken, error) { calls++; return EntraAccessToken{}, nil })
	if calls != 2 {
		t.Fatal("refresh attempts are throttled to one per thirty seconds")
	}
}

func TestMaterializeBearerForHTTP(t *testing.T) {
	if tok, err := MaterializeBearerForHTTP(context.Background(), "static-key"); err != nil || tok != "static-key" {
		t.Fatalf("tok %q err %v", tok, err)
	}
	if _, err := MaterializeBearerForHTTP(context.Background(), ""); err == nil || err.Error() != "no usable api_key / token provider" {
		t.Fatalf("err = %v", err)
	}
	src := TagCredentialSource(func(context.Context) (string, error) { return "minted", nil }, CredentialSourceEntraID)
	if !IsTokenProvider(src) || IsTokenProvider("key") {
		t.Fatal("token provider detection")
	}
	if tok, _ := MaterializeBearerForHTTP(context.Background(), src); tok != "minted" {
		t.Fatalf("tok = %q", tok)
	}
}
