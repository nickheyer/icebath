package azure

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	browserTimeout     = 300 * time.Second
	browserScopes      = " offline_access openid profile"
	browserCompleted   = "Authentication completed. You can close this window now."
	browserReadTimeout = 10 * time.Second
)

type browser struct {
	c         *client
	tenant    string
	authority string
	host      *hostinfo.Host

	mu      sync.Mutex
	renewal string
	tokens  map[string]accessToken
}

func (s *browser) name() string { return nameBrowser }

func (s *browser) String() string { return nameBrowser + " for tenant " + s.tenant }

func (s *browser) GoString() string { return s.String() }

func (c *client) interactiveBrowser(tenant, authority string, host *hostinfo.Host) source {
	if tenant == "" {
		tenant = "organizations"
	}
	return &browser{c: c, tenant: tenant, authority: authority, host: host, tokens: map[string]accessToken{}}
}

func randomText(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func tokenNonce(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Nonce
}

func (s *browser) token(ctx context.Context, scope string, inChain bool) (accessToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	if cached, ok := s.tokens[scope]; ok && cached.expiresOn-now >= refreshOffset {
		return cached, nil
	}
	found, err := s.c.discover(ctx, nameBrowser, s.authority, s.tenant)
	if err != nil {
		return accessToken{}, err
	}
	if s.renewal != "" {
		form := []pair{{"client_id", developerClientID}, {"grant_type", "refresh_token"}, {"client_info", "1"}, {"refresh_token", s.renewal}, {"scope", scope + browserScopes}}
		if token, reply, err := s.c.requestToken(ctx, nameBrowser, found.token, form); err == nil {
			if next := reply.text("refresh_token"); next != "" {
				s.renewal = next
			}
			s.tokens[scope] = token
			return token, nil
		}
	}
	token, renewal, err := s.signIn(ctx, found, scope, inChain)
	if err != nil {
		return accessToken{}, err
	}
	s.renewal = renewal
	s.tokens[scope] = token
	return token, nil
}

func (s *browser) signIn(ctx context.Context, found endpoints, scope string, inChain bool) (accessToken, string, error) {
	if found.authorize == "" {
		return accessToken{}, "", &AuthError{Credential: nameBrowser, Message: "authority configuration carries no authorization endpoint"}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return accessToken{}, "", &UnavailableError{Credential: nameBrowser, Message: "Couldn't start an HTTP server.", Err: err}
	}
	defer listener.Close()
	state, err := randomText(16)
	if err != nil {
		return accessToken{}, "", &AuthError{Credential: nameBrowser, Message: "sign-in state could not be generated", Err: err}
	}
	verifier, err := randomText(32)
	if err != nil {
		return accessToken{}, "", &AuthError{Credential: nameBrowser, Message: "sign-in state could not be generated", Err: err}
	}
	rawNonce, err := randomText(16)
	if err != nil {
		return accessToken{}, "", &AuthError{Credential: nameBrowser, Message: "sign-in state could not be generated", Err: err}
	}
	nonceSum := sha256.Sum256([]byte(rawNonce))
	nonce := hex.EncodeToString(nonceSum[:])
	challengeSum := sha256.Sum256([]byte(verifier))
	redirect := "http://localhost:" + strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	target := withQuery(found.authorize, []pair{
		{"client_id", developerClientID}, {"response_type", "code"}, {"redirect_uri", redirect}, {"scope", scope + browserScopes},
		{"state", state}, {"code_challenge", base64.RawURLEncoding.EncodeToString(challengeSum[:])}, {"code_challenge_method", "S256"},
		{"nonce", nonce}, {"client_info", "1"}, {"prompt", "select_account"},
	})
	answers := make(chan url.Values, 1)
	server := &http.Server{ReadHeaderTimeout: browserReadTimeout, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case query.Get("code") == "" && query.Get("error") == "":
			w.WriteHeader(http.StatusNotFound)
			return
		case query.Get("state") != state:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("State mismatch."))
			return
		case query.Get("error") != "":
			_, _ = w.Write([]byte("Authentication failed. " + html.EscapeString(query.Get("error")) + ": " + html.EscapeString(query.Get("error_description"))))
		default:
			_, _ = w.Write([]byte(browserCompleted))
		}
		select {
		case answers <- query:
		default:
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	if err := proc.StartDetached(browserCommand(target, s.host)); err != nil {
		slog.Warn("Found no browser in current environment. Visit the sign-in link from a browser that can reach this host.", "auth_uri", target, "timeout", browserTimeout.String())
	}
	timer := time.NewTimer(browserTimeout)
	defer timer.Stop()
	var answer url.Values
	select {
	case answer = <-answers:
	case <-timer.C:
		return accessToken{}, "", failure(inChain, nameBrowser, "Timed out after waiting "+strconv.Itoa(int(browserTimeout/time.Second))+" seconds for the user to authenticate")
	case <-ctx.Done():
		return accessToken{}, "", &AuthError{Credential: nameBrowser, Message: "sign-in was interrupted", Err: ctx.Err()}
	}
	if code := answer.Get("error"); code != "" {
		detail := answer.Get("error_description")
		if detail == "" {
			detail = code
		}
		return accessToken{}, "", failure(inChain, nameBrowser, detail)
	}
	form := []pair{
		{"client_id", developerClientID}, {"grant_type", "authorization_code"}, {"client_info", "1"}, {"code", answer.Get("code")},
		{"redirect_uri", redirect}, {"scope", scope + browserScopes}, {"code_verifier", verifier},
	}
	token, reply, err := s.c.requestToken(ctx, nameBrowser, found.token, form)
	if err != nil {
		if inChain {
			return accessToken{}, "", &UnavailableError{Credential: nameBrowser, Message: err.Error(), Err: err}
		}
		return accessToken{}, "", err
	}
	if idToken := reply.text("id_token"); idToken != "" && tokenNonce(idToken) != nonce {
		return accessToken{}, "", &AuthError{Credential: nameBrowser, Message: "the sign-in reply does not belong to this request, its nonce does not match"}
	}
	return token, reply.text("refresh_token"), nil
}
