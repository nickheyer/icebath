package provider

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// ---- tests/providers/fake_pkce_idp.py ----

// Local OAuth IdP whose authorize 302s to the callback and whose token endpoint verifies S256 and rotates refresh tokens
type fakeIdP struct {
	mu            sync.Mutex
	codes         map[string]string
	refreshTokens map[string]bool
	tokenRequests []map[string]string
	issued        int
	overrideState *string
	grantedScope  *string
	tokenAnswer   *fakeIdPAnswer
	opened        []string
	server        *httptest.Server
	base          string
}

type fakeIdPAnswer struct {
	status  int
	payload map[string]any
}

// Serves the IdP over plain HTTP, or over HTTPS with a throwaway CA the returned client trusts
func newFakeIdP(t *testing.T, useTLS bool) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{codes: map[string]string{}, refreshTokens: map[string]bool{}}
	if useTLS {
		idp.server = httptest.NewTLSServer(http.HandlerFunc(idp.serve))
	} else {
		idp.server = httptest.NewServer(http.HandlerFunc(idp.serve))
	}
	t.Cleanup(idp.stop)
	idp.base = idp.server.URL
	return idp
}

func (idp *fakeIdP) stop() { idp.server.Close() }

// Client that trusts the IdP certificate and reaches plain loopback servers too
func (idp *fakeIdP) client() *http.Client { return idp.server.Client() }

func fakeIdPTokenURLSafe(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func fakeIdPTokenHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (idp *fakeIdP) json(w http.ResponseWriter, status int, payload map[string]any) {
	data, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (idp *fakeIdP) serve(w http.ResponseWriter, r *http.Request) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if r.URL.Path != "/authorize" {
			w.WriteHeader(404)
			return
		}
		q := r.URL.Query()
		if len(q["code_challenge_method"]) != 1 || q.Get("code_challenge_method") != "S256" {
			panic(http.ErrAbortHandler)
		}
		code := fakeIdPTokenURLSafe(16)
		idp.codes[code] = q.Get("code_challenge")
		state := q.Get("state")
		if idp.overrideState != nil {
			state = *idp.overrideState
		}
		w.Header().Set("Location", q.Get("redirect_uri")+"?"+url.Values{"code": {code}, "state": {state}}.Encode())
		w.WriteHeader(http.StatusFound)
	case http.MethodPost:
		if r.URL.Path != "/token" {
			w.WriteHeader(404)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		parsed, _ := url.ParseQuery(string(raw))
		form := map[string]string{}
		for k, v := range parsed {
			form[k] = v[0]
		}
		idp.tokenRequests = append(idp.tokenRequests, form)
		if idp.tokenAnswer != nil {
			idp.json(w, idp.tokenAnswer.status, idp.tokenAnswer.payload)
			return
		}
		switch form["grant_type"] {
		case "authorization_code":
			challenge, ok := idp.codes[form["code"]]
			delete(idp.codes, form["code"])
			digest := sha256.Sum256([]byte(form["code_verifier"]))
			if !ok || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
				idp.json(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
		case "refresh_token":
			if !idp.refreshTokens[form["refresh_token"]] {
				idp.json(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
			delete(idp.refreshTokens, form["refresh_token"])
		default:
			idp.json(w, 400, map[string]any{"error": "unsupported_grant_type"})
			return
		}
		idp.issued++
		refresh := fmt.Sprintf("rt-%d-%s", idp.issued, fakeIdPTokenHex(4))
		idp.refreshTokens[refresh] = true
		payload := map[string]any{"access_token": fmt.Sprintf("at-%d-%s", idp.issued, fakeIdPTokenHex(4)),
			"refresh_token": refresh, "expires_in": 3600, "token_type": "Bearer"}
		if idp.grantedScope != nil {
			payload["scope"] = *idp.grantedScope
		}
		idp.json(w, 200, payload)
	default:
		w.WriteHeader(501)
	}
}

func (idp *fakeIdP) requests() []map[string]string {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return append([]map[string]string{}, idp.tokenRequests...)
}

func (idp *fakeIdP) holds(refresh string) bool {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.refreshTokens[refresh]
}

func (idp *fakeIdP) set(fn func()) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	fn()
}

// Browser double recording each URL and following the authorize redirect to the loopback callback
func (idp *fakeIdP) installBrowser(t *testing.T) {
	scriptBrowserEnv(t, map[string]string{"BROWSER": "firefox", "DISPLAY": ":0"}, "firefox")
	prev := openBrowserFn
	client := idp.client()
	openBrowserFn = func(target string) error {
		idp.set(func() { idp.opened = append(idp.opened, target) })
		go func() {
			if resp, err := client.Get(target); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	t.Cleanup(func() { openBrowserFn = prev })
}
