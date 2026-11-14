package remote

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/testfakes/configplane"

	"github.com/nickheyer/icebath/pkg/config"
)

func readFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := loadJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func TestProfileWriteCheck(t *testing.T) {
	for _, f := range resolutionFixtures(t) {
		fx := readFixture(t, f)
		expect, _ := fx["expect"].(map[string]any)
		var locks []Lock
		rawLocks, _ := expect["locks"].([]any)
		for _, l := range rawLocks {
			lk := l.(map[string]any)
			p, _ := Decode(lk["path"].(string))
			locks = append(locks, Lock{Path: p, Level: lk["level"].(string)})
		}
		writes, _ := fx["writes"].([]any)
		for i, raw := range writes {
			w := raw.(map[string]any)
			if w["by"] != "profile" || (w["op"] != "set" && w["op"] != "unset") {
				continue
			}
			path, _ := Decode(w["path"].(string))
			sets := &PathMap{}
			var unsets []Path
			var secret bool
			if w["op"] == "set" {
				sets.Set(path, w["value"])
				_, secret = SecretLiteralPath(path, w["value"])
			} else {
				unsets = []Path{path}
			}
			lock, refused := WriteCheck(sets, unsets, locks)
			we := w["expect"].(map[string]any)
			switch {
			case we["result"] == "accept":
				if refused || secret {
					t.Fatalf("%s#%d refused", f, i)
				}
			case we["error"] == "config_key_locked":
				if !refused || Encode(lock.Path) != we["path"] || lock.Level != we["lockedBy"] {
					t.Fatalf("%s#%d lock %v", f, i, lock)
				}
			case we["error"] == "config_secret_literal":
				if refused || !secret {
					t.Fatalf("%s#%d secret", f, i)
				}
			}
		}
	}
}

func TestDefaultsMergeMatchesDeepMerge(t *testing.T) {
	for _, f := range resolutionFixtures(t) {
		fx := readFixture(t, f)
		merge, ok := fx["defaultsMerge"].(map[string]any)
		if !ok {
			continue
		}
		effective := fx["expect"].(map[string]any)["effective"].(map[string]any)
		mustEqualJSON(t, f, config.DeepMerge(merge["defaults"].(map[string]any), effective), merge["expect"])
	}
}

func TestDisplayURLDropsUserInfo(t *testing.T) {
	for url, shown := range map[string]string{
		"https://u:p@example.com":            "https://example.com",
		"https://u:p@example.com:8443/plane": "https://example.com:8443/plane",
		"http://u@[::1]:9/x":                 "http://[::1]:9/x",
		"https://u:p@example.com:bad":        "https://example.com:bad",
		"https://example.com/plane":          "https://example.com/plane",
	} {
		if got := DisplayURL(url); got != shown {
			t.Fatalf("%s -> %s want %s", url, got, shown)
		}
	}
}

func TestBearerRefusedBeforeSendForForbiddenPlaneURL(t *testing.T) {
	for _, url := range []string{"http://foreign.example:8443/plane", "http://config-config.nousresearch.com", "ftp://plane.example",
		"https://user:pw@plane.example", "https://plane.example/?next=https://evil.example", "https://"} {
		t.Setenv(URLEnv, url)
		_, err := Request(context.Background(), "GET", t.TempDir(), "default", "", nil, "never-sent")
		var derr *CredentialDestinationError
		if !errors.As(err, &derr) || derr.Retryable || strings.Contains(err.Error(), "user:pw") {
			t.Fatalf("%s: %v", url, err)
		}
	}
}

func TestBearerSentToAllowedPlaneOrigin(t *testing.T) {
	for url, expect := range map[string]string{
		"": "https://config-config.nousresearch.com/v1/config/self?profile=default",
		"https://config.corp.internal:9443/plane/": "https://config.corp.internal:9443/plane/v1/config/self?profile=default",
		"http://localhost:8080":                    "http://localhost:8080/v1/config/self?profile=default",
		"http://127.0.0.1:9":                       "http://127.0.0.1:9/v1/config/self?profile=default",
		"http://[::1]:9":                           "http://[::1]:9/v1/config/self?profile=default",
	} {
		t.Setenv(URLEnv, url)
		plane, err := CheckedURL(BaseURL(), URLEnv)
		if err != nil || plane+"/v1/config/self?profile=default" != expect {
			t.Fatalf("%q: %v %s", url, err, plane)
		}
	}
}

func TestIDPSecretRefusedBeforeSendForForbiddenTokenURL(t *testing.T) {
	clearIDPCache()
	t.Setenv(idpEnvPrefix+"CLIENT_ID", configplane.ClientID)
	t.Setenv(idpEnvPrefix+"CLIENT_SECRET", configplane.ClientSecret)
	for _, url := range []string{"http://foreign.example:8443/token", "http://idp.corp.internal/token", "https://user:pw@idp.example/token", "https://idp.example/token#frag"} {
		t.Setenv(idpEnvPrefix+"TOKEN_URL", url)
		_, err := PlaneToken(context.Background(), t.TempDir())
		var derr *CredentialDestinationError
		if !errors.As(err, &derr) {
			t.Fatalf("%s: %v", url, err)
		}
	}
}

func TestRedirectIsNotFollowedWithTheCredential(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		followed := false
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, strings.Replace(target.URL, "http://", "http://u:p@", 1)+"/x", code)
		}))
		t.Setenv(URLEnv, redirect.URL)
		_, err := Request(context.Background(), "GET", t.TempDir(), "default", "", nil, "secret-bearer")
		var derr *CredentialDestinationError
		if !errors.As(err, &derr) || followed || strings.Contains(err.Error(), "u:p") {
			t.Fatalf("%d: %v followed %v", code, err, followed)
		}
		target.Close()
		redirect.Close()
	}
}

func TestProvenanceNeverEchoesUserInfo(t *testing.T) {
	p := setupPlane(t)
	t.Setenv(URLEnv, strings.Replace(p.URL, "http://", "http://u:p@", 1))
	if _, err := testBackend().ReadUserLayer(context.Background(), p.Home); err == nil || strings.Contains(err.Error(), "u:p") {
		t.Fatalf("err %v", err)
	}
	t.Setenv(URLEnv, p.URL)
	layer, err := testBackend().ReadUserLayer(context.Background(), p.Home)
	described := testBackend().Describe(context.Background(), p.Home)
	if err != nil || !strings.HasPrefix(layer.Provenance, "remote:http://127.0.0.1:") || strings.Contains(described, "u:p") {
		t.Fatalf("layer %v %v", layer.Provenance, err)
	}
}
