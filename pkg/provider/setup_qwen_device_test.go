package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Home with an empty user home, so no Qwen CLI credentials exist
func qwenDeviceHome(t *testing.T) PoolEnv {
	t.Helper()
	h := newTestHome(t)
	t.Setenv("HOME", t.TempDir())
	return testEnv(t, h)
}

func TestQwenDeviceLoginWritesTheQwenCLICredentials(t *testing.T) {
	env := qwenDeviceHome(t)
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/device/code":
			if req.Form.Get("code_challenge_method") != "S256" || req.Form.Get("client_id") != QwenOAuthClientID {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"device_code": "dc", "user_code": "QW-1", "verification_uri_complete": "https://chat.qwen.ai/authorize?user_code=QW-1", "expires_in": 600}`))
		case "/token":
			polls++
			if polls == 1 {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error": "authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token": "qa", "refresh_token": "qr", "token_type": "Bearer", "expires_in": 3600, "resource_url": "portal.qwen.ai"}`))
		}
	}))
	defer server.Close()
	swapFlowSeam(t, &qwenOAuthDeviceCodeURL, server.URL+"/device/code")
	swapFlowSeam(t, &qwenOAuthTokenURL, server.URL+"/token")
	var out strings.Builder
	if err := qwenDeviceCodeLogin(env, &out, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".qwen", "oauth_creds.json"))
	if err != nil {
		t.Fatalf("credentials %v\n%s", err, out.String())
	}
	var creds map[string]any
	if err := json.Unmarshal(raw, &creds); err != nil || creds["access_token"] != "qa" || creds["refresh_token"] != "qr" || creds["expiry_date"] == nil {
		t.Fatalf("credentials %s", raw)
	}
	if polls != 2 || !strings.Contains(out.String(), "QW-1") {
		t.Fatalf("polls %d output %s", polls, out.String())
	}
	resolved, err := resolveQwenRuntimeCredentials(env, false)
	if err != nil || resolved["api_key"] != "qa" {
		t.Fatalf("the written login does not resolve: %v %v", resolved, err)
	}
}

func TestQwenDeviceLoginStopsWhenTheSignInIsCancelled(t *testing.T) {
	env := qwenDeviceHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/device/code" {
			_, _ = w.Write([]byte(`{"device_code": "dc", "user_code": "QW-2", "verification_uri": "https://chat.qwen.ai/authorize", "expires_in": 600}`))
			return
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error": "authorization_pending"}`))
	}))
	defer server.Close()
	swapFlowSeam(t, &qwenOAuthDeviceCodeURL, server.URL+"/device/code")
	swapFlowSeam(t, &qwenOAuthTokenURL, server.URL+"/token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env.Ctx = ctx
	slept := 0
	swapFlowSeam(t, &oauthSleep, func(time.Duration) {
		if slept++; slept == 3 {
			cancel()
		}
	})
	if err := qwenDeviceCodeLogin(env, io.Discard, false); !isSignInCancelled(err) {
		t.Fatalf("a cancelled sign in ended the poll with %v", err)
	}
}
