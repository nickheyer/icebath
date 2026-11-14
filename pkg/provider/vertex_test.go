package provider

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var vertexTestKey *rsa.PrivateKey

func vertexKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	if vertexTestKey == nil {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		vertexTestKey = k
	}
	return vertexTestKey
}

// Clears every Vertex and ADC env var, the credential cache and the seams for one test
func vertexIsolate(t *testing.T) {
	t.Helper()
	for _, name := range []string{"VERTEX_CREDENTIALS_PATH", "GOOGLE_APPLICATION_CREDENTIALS", "VERTEX_PROJECT_ID", "VERTEX_REGION",
		"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT", "GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	resetVertexCredentialCache()
	origResolve, origADC, origSleep, origProduct := vertexResolveCredentialsPathFn, vertexADCFn, gcpBackoffSleep, gcpProductNamePath
	gcpBackoffSleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() {
		vertexResolveCredentialsPathFn, vertexADCFn, gcpBackoffSleep, gcpProductNamePath = origResolve, origADC, origSleep, origProduct
		resetVertexCredentialCache()
	})
}

// Token endpoint verifying the RS256 jwt-bearer assertion and answering a fixed token
func vertexTokenServer(t *testing.T, calls *int32, expiresIn int) *httptest.Server {
	t.Helper()
	pub := &vertexKey(t).PublicKey
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != gcpJWTGrantType {
			http.Error(w, `{"error":"unsupported_grant_type"}`, 400)
			return
		}
		parts := strings.Split(r.Form.Get("assertion"), ".")
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
			http.Error(w, `{"error":"invalid_grant","error_description":"bad signature"}`, 400)
			return
		}
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		_ = json.Unmarshal(payload, &claims)
		if claims["aud"] != gcpOAuth2TokenEndpoint || claims["scope"] != gcpCloudPlatformScope || claims["iss"] != "sa@p.iam.gserviceaccount.com" {
			http.Error(w, `{"error":"invalid_grant","error_description":"bad claims"}`, 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "ya29.FAKE", "expires_in": expiresIn, "token_type": "Bearer"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func vertexWriteSA(t *testing.T, path, tokenURI, project string, extra map[string]any) {
	t.Helper()
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, vertexKey(t))})
	info := map[string]any{"type": "service_account", "project_id": project, "private_key_id": "kid1", "private_key": string(pemKey),
		"client_email": "sa@p.iam.gserviceaccount.com", "token_uri": tokenURI}
	for k, v := range extra {
		info[k] = v
	}
	b, _ := json.Marshal(info)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustPKCS8(t *testing.T, k *rsa.PrivateKey) []byte {
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func vertexCacheSnapshot() map[vertexCacheKey]*vertexCacheEntry {
	vertexCredsMu.Lock()
	defer vertexCredsMu.Unlock()
	out := map[vertexCacheKey]*vertexCacheEntry{}
	for k, v := range vertexCredsCache {
		out[k] = v
	}
	return out
}

func onlyKey(t *testing.T) (vertexCacheKey, *vertexCacheEntry) {
	t.Helper()
	snap := vertexCacheSnapshot()
	if len(snap) != 1 {
		t.Fatalf("cache has %d entries, want 1", len(snap))
	}
	for k, v := range snap {
		return k, v
	}
	return vertexCacheKey{}, nil
}

func TestVertexHasCredentialsViaConfigProject(t *testing.T) {
	vertexIsolate(t)
	e := vertexEnv{cfg: map[string]any{"vertex": map[string]any{"project_id": "p"}}}
	if !hasVertexCredentials(context.Background(), e) {
		t.Fatal("config project must count as Vertex credentials")
	}
}

func TestVertexHasCredentialsFalseWhenNothingSet(t *testing.T) {
	vertexIsolate(t)
	if hasVertexCredentials(context.Background(), vertexEnv{cfg: map[string]any{}}) {
		t.Fatal("nothing configured must report no credentials")
	}
}

func TestVertexMultiplexScopeTakesPrecedenceOverRawEnviron(t *testing.T) {
	vertexIsolate(t)
	t.Setenv("VERTEX_PROJECT_ID", "other-profile-project")
	ctx := WithMultiplexContext(WithSecretScope(context.Background(), map[string]string{"VERTEX_PROJECT_ID": "this-profile-project"}, ""), true)
	if got := (vertexEnv{}).projectOverride(ctx); got != "this-profile-project" {
		t.Fatalf("project = %q", got)
	}
}

func TestVertexMultiplexUnscopedReadFailsClosed(t *testing.T) {
	vertexIsolate(t)
	t.Setenv("VERTEX_PROJECT_ID", "leaked-project")
	ctx := WithMultiplexContext(context.Background(), true)
	defer func() {
		r := recover()
		err, _ := r.(error)
		var unscoped *UnscopedSecretError
		if !errors.As(err, &unscoped) {
			t.Fatalf("want UnscopedSecretError panic, got %v", r)
		}
	}()
	(vertexEnv{}).projectOverride(ctx)
}

func TestVertexADCRefusesForeignProfileGoogleApplicationCredentials(t *testing.T) {
	vertexIsolate(t)
	sa := filepath.Join(t.TempDir(), "other_profile_sa.json")
	os.WriteFile(sa, []byte(`{"project_id": "other-profile"}`), 0o600)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", sa)
	vertexADCFn = func(context.Context, []string) (*gcpCredentials, string, error) {
		t.Fatal("ADC must not run for a foreign profile's GOOGLE_APPLICATION_CREDENTIALS")
		return nil, "", nil
	}
	ctx := WithMultiplexContext(WithSecretScope(context.Background(), map[string]string{}, ""), true)
	if tok, proj := vertexGetCredentials(ctx, vertexEnv{}, ""); tok != "" || proj != "" {
		t.Fatalf("got (%q, %q), want empty", tok, proj)
	}
}

func TestVertexSAFileRotationInvalidatesCredsCache(t *testing.T) {
	vertexIsolate(t)
	var calls int32
	srv := vertexTokenServer(t, &calls, 3600)
	sa := filepath.Join(t.TempDir(), "sa.json")
	vertexWriteSA(t, sa, srv.URL, "first-identity", nil)
	vertexResolveCredentialsPathFn = func(context.Context, vertexEnv, string) string { return sa }
	ctx := context.Background()
	tok, proj := vertexGetCredentials(ctx, vertexEnv{}, "")
	if tok != "ya29.FAKE" || proj != "first-identity" {
		t.Fatalf("got (%q, %q)", tok, proj)
	}
	key1, entry1 := onlyKey(t)
	vertexGetCredentials(ctx, vertexEnv{}, "")
	key1b, entry1b := onlyKey(t)
	if key1b != key1 || entry1b.creds != entry1.creds || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("untouched file must hit the cache, token calls %d", calls)
	}
	vertexWriteSA(t, sa, srv.URL, "second-identity", map[string]any{"rotated": true})
	st, _ := os.Stat(sa)
	os.Chtimes(sa, st.ModTime(), st.ModTime().Add(time.Millisecond))
	if _, proj = vertexGetCredentials(ctx, vertexEnv{}, ""); proj != "second-identity" {
		t.Fatalf("rotated project = %q", proj)
	}
	key2, entry2 := onlyKey(t)
	if key2 == key1 || entry2.creds == entry1.creds {
		t.Fatal("rotation must replace the cache entry")
	}
}

func TestVertexADCFailureRetriesWithLateAddedSAFile(t *testing.T) {
	vertexIsolate(t)
	var calls int32
	srv := vertexTokenServer(t, &calls, 3600)
	sa := filepath.Join(t.TempDir(), "late_sa.json")
	vertexWriteSA(t, sa, srv.URL, "sa-project", nil)
	var n int32
	vertexResolveCredentialsPathFn = func(context.Context, vertexEnv, string) string {
		if atomic.AddInt32(&n, 1) == 1 {
			return ""
		}
		return sa
	}
	vertexADCFn = func(context.Context, []string) (*gcpCredentials, string, error) {
		return nil, "", errors.New("ADC expired")
	}
	tok, proj := vertexGetCredentials(context.Background(), vertexEnv{}, "")
	if tok != "ya29.FAKE" || proj != "sa-project" || atomic.LoadInt32(&n) < 2 {
		t.Fatalf("got (%q, %q) after %d resolutions", tok, proj, n)
	}
}

func TestVertexMetadataPreservingRotationInvalidatesCredsCache(t *testing.T) {
	vertexIsolate(t)
	var calls int32
	srv := vertexTokenServer(t, &calls, 3600)
	dir := t.TempDir()
	sa := filepath.Join(dir, "sa.json")
	vertexWriteSA(t, sa, srv.URL, "AAAA-identity", nil)
	vertexResolveCredentialsPathFn = func(context.Context, vertexEnv, string) string { return sa }
	vertexGetCredentials(context.Background(), vertexEnv{}, "")
	key1, entry1 := onlyKey(t)
	st, _ := os.Stat(sa)
	next := filepath.Join(dir, "sa.json.new")
	vertexWriteSA(t, next, srv.URL, "BBBB-identity", nil)
	os.Chtimes(next, st.ModTime(), st.ModTime())
	if err := os.Rename(next, sa); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(sa)
	if st2.Size() != st.Size() || !st2.ModTime().Equal(st.ModTime()) {
		t.Fatal("replacement must keep size and mtime")
	}
	vertexGetCredentials(context.Background(), vertexEnv{}, "")
	key2, entry2 := onlyKey(t)
	if key2 == key1 || entry2.creds == entry1.creds {
		t.Fatal("content change must produce a new cache key")
	}
}

func TestVertexNearExpiryTokenRefreshesAndBaseURLDoesNotMint(t *testing.T) {
	vertexIsolate(t)
	var calls int32
	srv := vertexTokenServer(t, &calls, 200)
	sa := filepath.Join(t.TempDir(), "sa.json")
	vertexWriteSA(t, sa, srv.URL, "proj", nil)
	t.Setenv("VERTEX_CREDENTIALS_PATH", sa)
	e := vertexEnv{cfg: map[string]any{"vertex": map[string]any{"region": "us-central1"}}}
	ctx := context.Background()
	if base := vertexGetBaseURL(ctx, e, "", ""); base != "https://us-central1-aiplatform.googleapis.com/v1beta1/projects/proj/locations/us-central1/endpoints/openapi" {
		t.Fatalf("base = %q", base)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("base URL resolution must not mint a token")
	}
	vertexGetCredentials(ctx, e, "")
	vertexGetCredentials(ctx, e, "")
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("a token inside the five minute window must refresh, calls %d", calls)
	}
	if got := buildVertexBaseURL("p", "global"); got != "https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi" {
		t.Fatalf("global base = %q", got)
	}
}

func TestVertexRuntimeConfigMintsUnlessDeferred(t *testing.T) {
	vertexIsolate(t)
	var calls int32
	srv := vertexTokenServer(t, &calls, 3600)
	h := t.TempDir()
	sa := filepath.Join(h, "sa.json")
	vertexWriteSA(t, sa, srv.URL, "proj", nil)
	os.WriteFile(filepath.Join(h, ".env"), []byte("VERTEX_CREDENTIALS_PATH="+sa+"\nVERTEX_REGION=europe-west4\n"), 0o600)
	src, base, ok := vertexRuntimeConfig(WithDeferredTokenMint(context.Background()), map[string]any{}, nil, h)
	if !ok || !strings.Contains(base, "europe-west4-aiplatform") || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("deferred: ok=%v base=%q calls=%d", ok, base, calls)
	}
	if tok, err := src(context.Background()); err != nil || tok != "ya29.FAKE" {
		t.Fatalf("deferred token = %q, %v", tok, err)
	}
	resetVertexCredentialCache()
	src, base, ok = vertexRuntimeConfig(context.Background(), map[string]any{}, nil, h)
	if !ok || base == "" || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("eager: ok=%v base=%q calls=%d", ok, base, calls)
	}
	if tok, _ := src(context.Background()); tok != "ya29.FAKE" || atomic.LoadInt32(&calls) != 2 {
		t.Fatal("the eager mint must be reused by the token source")
	}
	os.Remove(sa)
	resetVertexCredentialCache()
	vertexADCFn = func(context.Context, []string) (*gcpCredentials, string, error) { return nil, "", errors.New("no ADC") }
	if _, _, ok = vertexRuntimeConfig(context.Background(), map[string]any{}, nil, h); ok {
		t.Fatal("unresolvable credentials must report not ok")
	}
}

func TestHasExplicitVertexConfig(t *testing.T) {
	vertexIsolate(t)
	ga := filepath.Join(t.TempDir(), "ga.json")
	os.WriteFile(ga, []byte("{}"), 0o600)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", ga)
	env := PoolEnv{Config: func() map[string]any { return map[string]any{} }}
	if hasExplicitVertexConfig(env) {
		t.Fatal("ambient GOOGLE_APPLICATION_CREDENTIALS must not count as explicit")
	}
	t.Setenv("VERTEX_CREDENTIALS_PATH", ga)
	if !hasExplicitVertexConfig(env) {
		t.Fatal("a readable VERTEX_CREDENTIALS_PATH is explicit")
	}
	t.Setenv("VERTEX_CREDENTIALS_PATH", "")
	env.Config = func() map[string]any { return map[string]any{"vertex": map[string]any{"project_id": "p"}} }
	if !hasExplicitVertexConfig(env) {
		t.Fatal("a configured project is explicit")
	}
}

func TestGCPTokenEndpointRetriesAndErrorText(t *testing.T) {
	vertexIsolate(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1, 2:
			w.WriteHeader(503)
		default:
			w.Write([]byte(`{"access_token":"t","expires_in":"60"}`))
		}
	}))
	defer srv.Close()
	data, err := gcpTokenEndpointRequest(context.Background(), srv.URL, url.Values{}, nil, nil, true, nil)
	if err != nil || data["access_token"] != "t" || calls != 3 {
		t.Fatalf("data=%v err=%v calls=%d", data, err, calls)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"Bad Request"}`))
	}))
	defer bad.Close()
	_, err = gcpTokenEndpointRequest(context.Background(), bad.URL, url.Values{}, nil, nil, true, nil)
	var rerr *gcpRefreshError
	if !errors.As(err, &rerr) || rerr.Msg != "invalid_grant: Bad Request" || rerr.Retryable {
		t.Fatalf("err = %v", err)
	}
}

func TestGCPAuthorizedUserADCFromGcloudConfig(t *testing.T) {
	vertexIsolate(t)
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form = r.Form
		w.Write([]byte(`{"access_token":"user-token","expires_in":3600,"refresh_token":"rotated","scope":"` + gcpCloudPlatformScope + ` openid"}`))
	}))
	defer srv.Close()
	dir := os.Getenv("CLOUDSDK_CONFIG")
	adc, _ := json.Marshal(map[string]any{"type": "authorized_user", "client_id": "cid", "client_secret": "cs", "refresh_token": "rt",
		"token_uri": srv.URL, "quota_project_id": "quota"})
	os.WriteFile(filepath.Join(dir, "application_default_credentials.json"), adc, 0o600)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")
	creds, project, err := gcpDefaultCredentials(context.Background(), vertexCloudPlatformScopes)
	if err != nil || project != "env-project" || creds.kind != "authorized_user" {
		t.Fatalf("creds=%v project=%q err=%v", creds, project, err)
	}
	if err := creds.Refresh(context.Background()); err != nil || creds.token != "user-token" {
		t.Fatalf("refresh: %v", err)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt" || form.Get("scope") != gcpCloudPlatformScope {
		t.Fatalf("form = %v", form)
	}
	creds.Refresh(context.Background())
	if form.Get("refresh_token") != "rotated" {
		t.Fatal("a rotated refresh token must be used next")
	}
	h := http.Header{}
	creds.beforeRequest(context.Background(), h)
	if h.Get("x-goog-user-project") != "quota" {
		t.Fatal("quota project header missing")
	}
}

func TestGCPComputeEngineMetadataServer(t *testing.T) {
	vertexIsolate(t)
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("metadata-flavor") != "Google" {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Metadata-Flavor", "Google")
		switch {
		case r.URL.Path == "/":
		case r.URL.Path == "/computeMetadata/v1/project/project-id":
			w.Header().Set("Content-Type", "application/text")
			w.Write([]byte("gce-project"))
		case r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/" && r.URL.Query().Get("recursive") == "true":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"email":"vm@p.iam.gserviceaccount.com","scopes":["x"]}`))
		case r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/token" && r.URL.Query().Get("scopes") == gcpCloudPlatformScope:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"gce-token","expires_in":3599}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer md.Close()
	host := strings.TrimPrefix(md.URL, "http://")
	t.Setenv("GCE_METADATA_HOST", host)
	t.Setenv("GCE_METADATA_IP", host)
	creds, project, err := gcpDefaultCredentials(context.Background(), vertexCloudPlatformScopes)
	if err != nil || project != "gce-project" || creds.kind != "compute_engine" {
		t.Fatalf("creds=%v project=%q err=%v", creds, project, err)
	}
	if err := creds.Refresh(context.Background()); err != nil || creds.token != "gce-token" {
		t.Fatalf("refresh err %v token %q", err, creds.token)
	}
	md.Close()
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	gcpProductNamePath = filepath.Join(t.TempDir(), "absent")
	if _, _, err := gcpDefaultCredentials(context.Background(), vertexCloudPlatformScopes); err == nil || !strings.Contains(err.Error(), "default credentials were not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestGCPImpersonatedServiceAccountFile(t *testing.T) {
	vertexIsolate(t)
	var impBody map[string]any
	var impAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":generateAccessToken") {
			impAuth = r.Header.Get("Authorization")
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &impBody)
			w.Write([]byte(`{"accessToken":"impersonated","expireTime":"2099-01-01T00:00:00Z"}`))
			return
		}
		w.Write([]byte(`{"access_token":"source-token","expires_in":3600}`))
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "imp.json")
	b, _ := json.Marshal(map[string]any{"type": "impersonated_service_account",
		"service_account_impersonation_url": srv.URL + "/v1/projects/-/serviceAccounts/target@p.iam.gserviceaccount.com:generateAccessToken",
		"delegates":                         []string{"d@p.iam.gserviceaccount.com"},
		"source_credentials":                map[string]any{"type": "authorized_user", "client_id": "c", "client_secret": "s", "refresh_token": "r", "token_uri": srv.URL}})
	os.WriteFile(file, b, 0o600)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", file)
	creds, _, err := gcpDefaultCredentials(context.Background(), vertexCloudPlatformScopes)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Refresh(context.Background()); err != nil || creds.token != "impersonated" {
		t.Fatalf("err %v token %q", err, creds.token)
	}
	if impAuth != "Bearer source-token" || impBody["lifetime"] != "3600s" || len(impBody["delegates"].([]any)) != 1 {
		t.Fatalf("auth %q body %v", impAuth, impBody)
	}
}

func TestGCPExternalAccountFileAndAWSSources(t *testing.T) {
	vertexIsolate(t)
	var form url.Values
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form = r.Form
		w.Write([]byte(`{"access_token":"federated","expires_in":"3600","token_type":"Bearer"}`))
	}))
	defer sts.Close()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "oidc.json")
	os.WriteFile(tokenFile, []byte(`{"id_token":"oidc-subject"}`), 0o600)
	info := map[string]any{"type": "external_account", "audience": "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/p/providers/x",
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt", "token_url": sts.URL,
		"credential_source": map[string]any{"file": tokenFile, "format": map[string]any{"type": "json", "subject_token_field_name": "id_token"}}}
	creds, err := gcpExternalAccountFromInfo(info, vertexCloudPlatformScopes)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Refresh(context.Background()); err != nil || creds.token != "federated" {
		t.Fatalf("err %v token %q", err, creds.token)
	}
	if form.Get("subject_token") != "oidc-subject" || form.Get("grant_type") != gcpSTSGrantType || form.Get("scope") != gcpCloudPlatformScope {
		t.Fatalf("form = %v", form)
	}
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKID")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SECRET")
	info["subject_token_type"] = gcpAWSSubjectTokenType
	info["credential_source"] = map[string]any{"environment_id": "aws1", "regional_cred_verification_url": gcpAWSDefaultVerifyURL}
	creds, err = gcpExternalAccountFromInfo(info, vertexCloudPlatformScopes)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, _ := url.QueryUnescape(form.Get("subject_token"))
	var signed struct {
		URL     string              `json:"url"`
		Method  string              `json:"method"`
		Headers []map[string]string `json:"headers"`
	}
	json.Unmarshal([]byte(raw), &signed)
	if signed.URL != "https://sts.us-east-1.amazonaws.com?Action=GetCallerIdentity&Version=2011-06-15" || signed.Method != "POST" || len(signed.Headers) != 4 ||
		!strings.HasPrefix(signed.Headers[0]["value"], "AWS4-HMAC-SHA256 Credential=AKID/") {
		t.Fatalf("signed request = %+v", signed)
	}
	info["credential_source"] = map[string]any{"environment_id": "aws2"}
	if _, err := gcpExternalAccountFromInfo(info, nil); err == nil || !strings.Contains(err.Error(), "aws version '2' is not supported") {
		t.Fatalf("err = %v", err)
	}
}

func TestGCPExecutableSubjectToken(t *testing.T) {
	vertexIsolate(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "token.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho '{\"version\":1,\"success\":true,\"token_type\":\"urn:ietf:params:oauth:token-type:id_token\",\"id_token\":\"'$GOOGLE_EXTERNAL_ACCOUNT_AUDIENCE'\"}'\n"), 0o700)
	supplier, err := gcpExecutableSubjectToken(map[string]any{"executable": map[string]any{"command": script}}, "aud-x", "urn:ietf:params:oauth:token-type:id_token", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supplier(context.Background()); err == nil || !strings.Contains(err.Error(), "explicitly allowed") {
		t.Fatalf("executables must need the allow flag, err %v", err)
	}
	t.Setenv("GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES", "1")
	if tok, err := supplier(context.Background()); err != nil || tok != "aud-x" {
		t.Fatalf("tok %q err %v", tok, err)
	}
	if _, err := gcpParseExecutableResponse(map[string]any{"version": 1.0, "success": false, "code": "401", "message": "denied"}); err == nil ||
		err.Error() != "Executable returned unsuccessful response: code: 401, message: denied." {
		t.Fatalf("err = %v", err)
	}
}

func TestGCPLoadCredentialsRejectsUnknownType(t *testing.T) {
	vertexIsolate(t)
	f := filepath.Join(t.TempDir(), "x.json")
	os.WriteFile(f, []byte(`{"type":"mystery"}`), 0o600)
	_, _, err := gcpLoadCredentialsFromFile(context.Background(), f, nil)
	if err == nil || !strings.Contains(err.Error(), "does not have a valid type. Type is mystery") {
		t.Fatalf("err = %v", err)
	}
	if _, err := gcpServiceAccountFromInfo(map[string]any{"project_id": "p"}, nil); err == nil ||
		err.Error() != "Service account info was not in the expected format, missing fields client_email, token_uri." {
		t.Fatalf("err = %v", err)
	}
}

func TestVertexAnthropicTransportPaths(t *testing.T) {
	vertexIsolate(t)
	route := Route{BaseURL: "https://us-east5-aiplatform.googleapis.com/v1beta1/projects/proj/locations/us-east5/endpoints/openapi",
		Model: "claude-sonnet-4-5", TokenSource: func(context.Context) (string, error) { return "tok", nil }}
	tr, _ := vertexTransport(route)
	req, err := tr.Request(context.Background(), anthOpMessages, map[string]any{"model": "claude-sonnet-4-5@20250929", "stream": true, "max_tokens": 5},
		map[string]string{"anthropic-version": "2023-06-01", "anthropic-beta": "b1"})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://us-east5-aiplatform.googleapis.com/v1/projects/proj/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-5@20250929:streamRawPredict"
	if req.URL.String() != want || req.Header.Get("Authorization") != "Bearer tok" || req.Header.Get("anthropic-version") != "" || req.Header.Get("anthropic-beta") != "b1" {
		t.Fatalf("url %s headers %v", req.URL, req.Header)
	}
	var body map[string]any
	b, _ := io.ReadAll(req.Body)
	json.Unmarshal(b, &body)
	if _, has := body["model"]; has || body["anthropic_version"] != vertexAnthropicVersion {
		t.Fatalf("body = %v", body)
	}
	req, _ = tr.Request(context.Background(), anthOpMessages, map[string]any{"messages": []any{}}, nil)
	if !strings.HasSuffix(req.URL.Path, "/models/claude-sonnet-4-5:rawPredict") {
		t.Fatalf("non stream path %s", req.URL.Path)
	}
	req, _ = tr.Request(context.Background(), anthOpCountTokens, map[string]any{"model": "claude-sonnet-4-5"}, nil)
	b, _ = io.ReadAll(req.Body)
	if !strings.HasSuffix(req.URL.Path, "/publishers/anthropic/models/count-tokens:rawPredict") || !strings.Contains(string(b), `"model":"claude-sonnet-4-5"`) {
		t.Fatalf("count tokens %s %s", req.URL.Path, b)
	}
	for region, origin := range map[string]string{"global": "https://aiplatform.googleapis.com", "us": "https://aiplatform.us.rep.googleapis.com",
		"eu": "https://aiplatform.eu.rep.googleapis.com", "europe-west1": "https://europe-west1-aiplatform.googleapis.com"} {
		if got := vertexAnthropicOrigin(region); got != origin {
			t.Fatalf("%s origin %s", region, got)
		}
	}
}

func TestVertexGetConfigExportedResolvesProfileHome(t *testing.T) {
	vertexIsolate(t)
	var calls int32
	srv := vertexTokenServer(t, &calls, 3600)
	sa := filepath.Join(t.TempDir(), "sa.json")
	vertexWriteSA(t, sa, srv.URL, "proj", nil)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if token, base := VertexGetConfig(context.Background()); token != "" || base != "" {
		t.Fatalf("no credentials must resolve empty, got %q %q", token, base)
	}
	os.WriteFile(filepath.Join(h, ".env"), []byte("VERTEX_CREDENTIALS_PATH="+sa+"\nVERTEX_REGION=europe-west4\n"), 0o600)
	token, base := VertexGetConfig(context.Background())
	if token == "" || base != buildVertexBaseURL("proj", "europe-west4") {
		t.Fatalf("token %q base %q", token, base)
	}
}
