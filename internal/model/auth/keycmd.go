package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
)

const (
	tokenRefreshLeeway = 60 * time.Second
	mintTimeout        = 15 * time.Second
	noTTLRefresh       = 900 * time.Second
)

type CommandTokenError struct {
	Message string
	Cause   error
}

func (e *CommandTokenError) Error() string { return e.Message }

func (e *CommandTokenError) Unwrap() error { return e.Cause }

type CommandTokenSource struct {
	command  string
	label    string
	childEnv func(ctx context.Context) []string

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (s *Store) CommandTokenSource(keyCmd, label string) (*CommandTokenSource, bool) {
	command := strings.TrimSpace(keyCmd)
	if command == "" {
		return nil, false
	}
	if label == "" {
		label = "custom"
	}
	childEnv := func(ctx context.Context) []string { return s.childEnv(s.bind(ctx)) }
	return &CommandTokenSource{command: command, label: label, childEnv: childEnv}, true
}

func (c *CommandTokenSource) Kind() SourceKind { return SourceKeyCmd }

func (c *CommandTokenSource) CacheIdentity() string { return "cmd:" + c.command }

func (c *CommandTokenSource) mint(ctx context.Context) (string, time.Duration, error) {
	fail := func(cause error, format string, args ...any) (string, time.Duration, error) {
		return "", 0, &CommandTokenError{Message: fmt.Sprintf("key_cmd for provider '%s' ", c.label) + fmt.Sprintf(format, args...), Cause: cause}
	}
	cmd, err := proc.ShellCommand(c.command)
	if err != nil {
		return fail(err, "could not be executed: %v", err)
	}
	cmd.Env = c.childEnv(ctx)
	proc.Hide(cmd)
	runCtx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	result, err := proc.Run(runCtx, cmd)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fail(runCtx.Err(), "timed out after %ds", int(mintTimeout.Seconds()))
	}
	if err != nil {
		return fail(err, "could not be executed: %v", err)
	}
	if !result.Exit.Success() {
		return fail(nil, "exited %d. Run that provider's key_cmd manually to see why "+
			"(e.g. `databricks auth login` if its OAuth session expired).", result.Exit.Status())
	}
	stdout := strings.ToValidUTF8(string(result.Stdout), "�")
	if strings.TrimSpace(stdout) == "" {
		return fail(nil, "produced no output")
	}
	if strings.HasPrefix(strings.TrimLeft(stdout, " \t\r\n\v\f"), "{") {
		if payload, err := parseObject([]byte(stdout)); err == nil {
			token := strings.TrimSpace(textOf(payload.value("access_token")))
			if token == "" {
				return fail(nil, "returned JSON without an 'access_token' field")
			}
			if ttl, ok := numberOf(payload.value("expires_in")); ok && ttl > 0 {
				return token, time.Duration(ttl * float64(time.Second)), nil
			}
			for _, field := range []string{"expiry", "expiresOn"} {
				if deadline, ok := parseISOStamp(payload.value(field)); ok {
					if remaining := deadline - nowEpoch(); remaining > 0 {
						return token, time.Duration(remaining * float64(time.Second)), nil
					}
				}
			}
			return token, 0, nil
		}
	}
	token := strings.TrimSpace(stdout)
	if strings.Contains(token, "\n") {
		return fail(nil, "printed multiple lines; it must print only the token (or JSON with an 'access_token' field)")
	}
	return token, 0, nil
}

func (c *CommandTokenSource) Token(ctx context.Context) (Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return Token{Value: NewSecret(c.token), Expiry: c.expires}, nil
	}
	token, ttl, err := c.mint(ctx)
	if err != nil {
		return Token{}, err
	}
	lifetime := noTTLRefresh
	shown := "unknown"
	if ttl > 0 {
		lifetime = max(ttl-tokenRefreshLeeway, 5*time.Second)
		shown = fmt.Sprintf("%ds", int(ttl.Seconds()))
	}
	c.token, c.expires = token, time.Now().Add(lifetime)
	slog.Debug(fmt.Sprintf("Minted key_cmd token for provider %s (ttl=%s)", c.label, shown))
	return Token{Value: NewSecret(token), Expiry: c.expires}, nil
}
