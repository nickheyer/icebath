package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Idle time after which a manager session token is dropped, by a timer rather than at the next call
const sessionIdleTTL = 30 * time.Minute

// Slack added to the expiry timer so it fires after the token it waits for has gone stale
const sessionTimerSlack = 10 * time.Millisecond

// Deadlines the surfaces give a master password, a save login step and a one time code prompt
const (
	UnlockPromptTimeout    = 120 * time.Second
	SaveLoginPromptTimeout = 180 * time.Second
	CodePromptTimeout      = 180 * time.Second
)

// Profile home and backend a session token belongs to
type sessionKey struct {
	home    string
	backend string
}

// Session token inside a closure no dump of process state shows, and when a manager call last used it
type sessionToken struct {
	value    func() string
	lastUsed time.Time
}

// In memory manager session tokens with lock generations and the gateway session that unlocked each
type sessions struct {
	mu          sync.Mutex
	tokens      map[sessionKey]*sessionToken
	owners      map[sessionKey]string
	generations map[sessionKey]int
	timer       *sessionTimer
}

// Expiry timer whose identity the callback compares, so a stale timer never clears a newer one
type sessionTimer struct {
	t *time.Timer
}

// Empty token store
func newSessions() *sessions {
	return &sessions{tokens: map[sessionKey]*sessionToken{}, owners: map[sessionKey]string{}, generations: map[sessionKey]int{}}
}

// Drops the token and owner of key and bumps its generation so an unlock in flight cannot commit
func (s *sessions) forget(key sessionKey) {
	delete(s.tokens, key)
	delete(s.owners, key)
	s.generations[key]++
}

// Starts the one timer for the earliest expiry when none runs and tokens exist
func (s *sessions) arm(now time.Time) {
	if s.timer != nil || len(s.tokens) == 0 {
		return
	}
	earliest := now
	for _, t := range s.tokens {
		if t.lastUsed.Before(earliest) {
			earliest = t.lastUsed
		}
	}
	delay := max(earliest.Add(sessionIdleTTL).Sub(now), 0) + sessionTimerSlack
	timer := &sessionTimer{}
	timer.t = time.AfterFunc(delay, func() { s.expire(timer) })
	s.timer = timer
}

// Stops the timer once no token is left
func (s *sessions) settle() {
	if len(s.tokens) == 0 && s.timer != nil {
		s.timer.t.Stop()
		s.timer = nil
	}
}

// Drops every token idle past its lifetime and rearms for the next one
func (s *sessions) expire(fired *sessionTimer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer == fired {
		s.timer = nil
	}
	now := time.Now()
	for key, t := range s.tokens {
		if now.Sub(t.lastUsed) > sessionIdleTTL {
			delete(s.tokens, key)
		}
	}
	s.arm(now)
}

// Live token of key, refreshing its idle time when touch is set
func (s *sessions) live(key sessionKey, touch bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[key]
	if !ok {
		return ""
	}
	now := time.Now()
	if now.Sub(t.lastUsed) > sessionIdleTTL {
		delete(s.tokens, key)
		s.settle()
		return ""
	}
	if touch {
		t.lastUsed = now
	}
	return unseal(t.value)
}

// Lock generation of key, snapshotted before a manager unlock starts
func (s *sessions) begin(key sessionKey) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generations[key]
}

// Stores a minted token unless a lock bumped the generation since begin
func (s *sessions) commit(key sessionKey, value string, generation int, owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generations[key] {
		return false
	}
	now := time.Now()
	s.tokens[key] = &sessionToken{value: seal(value), lastUsed: now}
	s.owners[key] = owner
	s.arm(now)
	return true
}

// Forgets the tokens of home for backend, every backend when backend is empty
func (s *sessions) lockHome(home, backend string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := map[sessionKey]bool{}
	collect := func(key sessionKey) {
		if key.home == home && (backend == "" || key.backend == backend) {
			keys[key] = true
		}
	}
	for key := range s.tokens {
		collect(key)
	}
	for key := range s.owners {
		collect(key)
	}
	for key := range s.generations {
		collect(key)
	}
	if backend != "" {
		keys[sessionKey{home, backend}] = true
	}
	for key := range keys {
		s.forget(key)
	}
	s.settle()
}

// Forgets every token the gateway session sessionID unlocked
func (s *sessions) release(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, owner := range s.owners {
		if owner == sessionID {
			s.forget(key)
		}
	}
	s.settle()
}

// Token store key of backend for the profile home of ctx
func (v *Vault) sessionKey(ctx context.Context, backend string) (sessionKey, error) {
	h, err := v.resolver.Home(ctx)
	if err != nil {
		return sessionKey{}, err
	}
	return sessionKey{home: h, backend: backend}, nil
}

// Token of backend for the home of ctx, refreshing its idle time when touch is set
func (v *Vault) managerToken(ctx context.Context, backend string, touch bool) (string, error) {
	key, err := v.sessionKey(ctx, backend)
	if err != nil {
		return "", err
	}
	return v.sessions.live(key, touch), nil
}

// Reports whether backend holds a live token for the home of ctx, never for an unresolvable home
func (v *Vault) hasSession(ctx context.Context, backend string) bool {
	token, err := v.managerToken(ctx, backend, false)
	return err == nil && token != ""
}

// Forgets the manager session of backend for the profile home of ctx, every manager when backend is empty
func (v *Vault) Lock(ctx context.Context, backend string) error {
	h, err := v.resolver.Home(ctx)
	if err != nil {
		return err
	}
	v.sessions.lockHome(h, backend)
	return nil
}

// Forgets only the manager sessions the gateway session sessionID unlocked, siblings in the profile keeping theirs
func (v *Vault) Release(sessionID string) {
	if sessionID != "" {
		v.sessions.release(sessionID)
	}
}

// Context keys of the prompt surface, the owning gateway session and the no user marker
type (
	prompterKey   struct{}
	sessionIDKey  struct{}
	unattendedKey struct{}
)

// Identifier and password typed into the save prompt, the password inside a closure no printing path can open
type Login struct {
	Identifier string
	password   func() string
}

// Login of the identifier and the password typed for it
func NewLogin(identifier, password string) Login {
	return Login{Identifier: identifier, password: seal(password)}
}

// Typed secret released to be saved as a vault item, empty for the zero value
func (l Login) Password() string { return unseal(l.password) }

// Writes the identifier with a redaction marker in place of the password
func (l Login) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "Login{"+l.Identifier+" "+redacted+"}")
}

// Identifier with a redaction marker for structured logs
func (l Login) LogValue() slog.Value {
	return slog.GroupValue(slog.String("identifier", l.Identifier), slog.String("password", redacted))
}

// Identifier with a redaction marker as a JSON object
func (l Login) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"identifier": l.Identifier, "password": redacted})
}

// Masked prompts a surface shows its user for a vault operation, each honouring ctx
type Prompter interface {
	MasterPassword(ctx context.Context, backend, displayName string) (string, error)
	SaveLogin(ctx context.Context, origin, site string) (Login, bool, error)
	OneTimeCode(ctx context.Context, site, hint string) (string, error)
}

// Context whose vault prompts go to p, the surface a user answers
func WithPrompter(ctx context.Context, p Prompter) context.Context {
	return context.WithValue(ctx, prompterKey{}, p)
}

// Context whose manager unlocks belong to the gateway session id and end with it
func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, id)
}

// Context in which no user can answer, such as cron, webhooks, API sessions and single queries
func Unattended(ctx context.Context) context.Context {
	return context.WithValue(ctx, unattendedKey{}, true)
}

// Gateway session ctx binds, empty when none
func sessionID(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey{}).(string)
	return id
}

// Reports whether ctx was marked as one no user can answer, so no prompt may wait on it
func unattended(ctx context.Context) bool {
	marked, _ := ctx.Value(unattendedKey{}).(bool)
	return marked
}

// Prompt surface of ctx, false when none is bound or no user can answer here
func PrompterFrom(ctx context.Context) (Prompter, bool) {
	if unattended(ctx) {
		return nil, false
	}
	p, ok := ctx.Value(prompterKey{}).(Prompter)
	return p, ok && p != nil
}

// Reports whether a user can answer a vault prompt in ctx
func CanPrompt(ctx context.Context) bool {
	_, ok := PrompterFrom(ctx)
	return ok
}
