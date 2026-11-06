package anonportaltest_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider/anonportaltest"
)

func portalClient(t *testing.T, p *anonportaltest.Portal) *http.Client {
	t.Helper()
	pemBytes, err := os.ReadFile(os.Getenv(anonportaltest.CABundleEnv))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("portal CA not parsed")
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
}

func portalPost(t *testing.T, c *http.Client, url, body, secret, agent string) (int, map[string]any, error) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewBufferString(body))
	if secret != "" {
		req.Header.Set("x-anonymous-api-secret", secret)
	}
	if agent != "" {
		req.Header.Set("user-agent", agent)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, nil
}

func jwtClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	json.Unmarshal(raw, &claims)
	return claims
}

func TestInstallIsolatesEnvAndServesTheDefaultSurface(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-real")
	t.Setenv(anonportaltest.DesktopEnv, "1")
	p := anonportaltest.Install(t)
	for name, want := range map[string]string{anonportaltest.HomeEnv: p.Home, anonportaltest.PortalBaseURLEnv: p.URL,
		anonportaltest.AnonSecretEnv: anonportaltest.Secret, anonportaltest.SharedAuthDirEnv: p.SharedDir, anonportaltest.CABundleEnv: p.CAPath} {
		if got := os.Getenv(name); got != want || want == "" {
			t.Fatalf("%s=%q want %q", name, got, want)
		}
	}
	for _, name := range anonportaltest.ClearedEnv {
		if _, ok := os.LookupEnv(name); ok {
			t.Fatalf("%s still set", name)
		}
	}
	c := portalClient(t, p)
	if status, body, err := portalPost(t, c, p.URL+"/api/anonymous/create", "{}", "", ""); err != nil || status != 401 || body["error"] != "invalid_shared_secret" {
		t.Fatalf("ungated create %d %v %v", status, body, err)
	}
	status, body, err := portalPost(t, c, p.URL+"/api/anonymous/create", "", anonportaltest.Secret, "")
	if err != nil || status != 201 || !reflect.DeepEqual(body, map[string]any{"user_id": "nas_user:1", "org_id": "nas_org:1", "token": "anon_0001", "idle_ttl_days": 14.0}) {
		t.Fatalf("create %d %v %v", status, body, err)
	}
	if _, _, err := portalPost(t, c, p.URL+"/api/anonymous/create", "{bad", anonportaltest.Secret, ""); err == nil {
		t.Fatal("invalid create body answered")
	}
	status, body, err = portalPost(t, c, p.URL+"/api/anonymous/token", `{"token":"anon_0001","purpose":"connectors"}`, anonportaltest.Secret, "icebath-agent/test")
	if err != nil || status != 200 || body["inference_base_url"] != anonportaltest.Welcome || body["token_type"] != "Bearer" {
		t.Fatalf("token %d %v %v", status, body, err)
	}
	if claims := jwtClaims(t, body["access_token"].(string)); claims["scope"] != "connectors:invoke" || claims["account_tier"] != "anonymous" {
		t.Fatalf("connectors claims %v", claims)
	}
	_, body, _ = portalPost(t, c, p.URL+"/api/anonymous/token", `{"token":"anon_0001"}`, anonportaltest.Secret, "")
	if claims := jwtClaims(t, body["access_token"].(string)); claims["scope"] != "inference:invoke" {
		t.Fatalf("inference claims %v", claims)
	}
	if _, _, err := portalPost(t, c, p.URL+"/api/anonymous/token", `{"purpose":"connectors"}`, anonportaltest.Secret, ""); err == nil {
		t.Fatal("token request without a token answered")
	}
	if status, body, _ := portalPost(t, c, p.URL+"/api/anonymous/challenge/status", "{}", anonportaltest.Secret, ""); status != 200 || body["status"] != "none" {
		t.Fatalf("challenge status %d %v", status, body)
	}
	if status, body, _ := portalPost(t, c, p.URL+"/elsewhere", "", "", ""); status != 500 || body["error"] != "unexpected /elsewhere" {
		t.Fatalf("unexpected path %d %v", status, body)
	}
	if p.Creates() != 3 {
		t.Fatalf("creates %d %v", p.Creates(), p.Calls())
	}
	reqs := p.TokenRequests()
	if len(reqs) != 3 || reqs[0]["user_agent"] != "icebath-agent/test" || reqs[0]["body"].(map[string]any)["purpose"] != "connectors" {
		t.Fatalf("token requests %v", reqs)
	}
	if len(p.Calls()) != 8 || p.Calls()[7] != "/elsewhere" {
		t.Fatalf("calls %v", p.Calls())
	}
}

func TestServeRoutesThroughTheGivenHandler(t *testing.T) {
	p := anonportaltest.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(418)
	}))
	status, _, err := portalPost(t, portalClient(t, p), p.URL+"/api/anonymous/create", "{}", anonportaltest.Secret, "")
	if err != nil || status != 418 || p.Creates() != 1 || len(p.TokenRequests()) != 0 {
		t.Fatalf("custom handler %d %v %v", status, err, p.Calls())
	}
}
