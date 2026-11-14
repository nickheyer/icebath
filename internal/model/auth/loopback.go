package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func pkceCodeVerifier() (string, error) {
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	if len(verifier) > 128 {
		verifier = verifier[:128]
	}
	return verifier, nil
}

func pkceCodeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func randomURLToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

type callbackResult struct {
	code             string
	state            string
	failure          string
	errorDescription string
}

type loopbackServer struct {
	listener net.Listener
	server   *http.Server

	mu     sync.Mutex
	result callbackResult
	landed chan struct{}
	once   sync.Once
}

func (s *Store) bindLoopback(host string, port int, expectedPath, displayName, provider, bindFailedCode string) (*loopbackServer, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("Could not bind callback server on %s:%d: %v", host, port, err), Provider: provider, Code: bindFailedCode, Cause: err}
	}
	l := &loopbackServer{listener: listener, landed: make(chan struct{})}
	l.server = &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusNotImplemented)
				return
			}
			if r.URL.Path != expectedPath {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("Not found."))
				return
			}
			q := r.URL.Query()
			res := callbackResult{code: q.Get("code"), state: q.Get("state"), failure: q.Get("error"), errorDescription: q.Get("error_description")}
			l.mu.Lock()
			l.result = res
			l.mu.Unlock()
			outcome := "received"
			if res.failure != "" {
				outcome = "failed"
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html><body><h1>" + html.EscapeString(displayName) + " authorization " + outcome + ".</h1>You can close this tab.</body></html>"))
			if res.code != "" || res.failure != "" {
				l.once.Do(func() { close(l.landed) })
			}
		}),
	}
	return l, nil
}

func (l *loopbackServer) port() int {
	if addr, ok := l.listener.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

func (l *loopbackServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = l.server.Shutdown(ctx)
	_ = l.listener.Close()
}

func (l *loopbackServer) wait(ctx context.Context, timeout time.Duration, provider, timeoutCode string) (callbackResult, error) {
	go func() { _ = l.server.Serve(l.listener) }()
	defer l.close()
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-l.landed:
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.result, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return callbackResult{}, ErrCancelled
		}
		return callbackResult{}, ctx.Err()
	case <-timer.C:
		return callbackResult{}, providerError(provider, "Authorization timed out waiting for the local callback.", timeoutCode)
	}
}

func (s *Store) sshUserAtHost(ctx context.Context) string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "<this-host>"
	}
	user := s.lookupEnv(ctx, "USER")
	if user == "" {
		user = s.lookupEnv(ctx, "LOGNAME")
	}
	if user == "" {
		user = "<user>"
	}
	return user + "@" + hostname
}

func (s *Store) printLoopbackSSHHint(ctx context.Context, c Console, redirectURI, docsURL string) {
	if !s.isRemoteSession(ctx) {
		return
	}
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return
	}
	host, port := parsed.Hostname(), parsed.Port()
	if (host != "127.0.0.1" && host != "::1" && host != "localhost") || port == "" || port == "0" {
		return
	}
	divider := strings.Repeat("-", 60)
	c.printf("\n%s\nRemote session detected — SSH tunnel required\n%s\n"+
		"icebath is waiting for the OAuth callback on %s\n"+
		"but your browser is on a different machine. Run this command\n"+
		"in a NEW terminal on your local machine BEFORE opening the URL:\n\n"+
		"  ssh -N -L %s:127.0.0.1:%s %s\n\n"+
		"Then open the authorize URL above in your local browser.\n",
		divider, divider, redirectURI, port, port, s.sshUserAtHost(ctx))
	if docsURL != "" {
		c.printf("Provider docs:      %s\n", docsURL)
	}
	c.printf("SSH/jump-box guide: %s\n%s\n\n", OAuthOverSSHDocsURL, divider)
}
