// Package remote serves each profile's settings from a config plane instead of its config.yaml: fetched once as the profile is first read,
// failing closed when the plane can't be reached, polled while icebath runs, and written back key by key against the version read, with
// the keys a level above the profile locks refused
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// How often a profile is polled, never more often than the floor
const (
	pollEvery = 5 * time.Minute
	pollFloor = 30 * time.Second
	// Longest wait between the tries of a first fetch
	retryCap = 20 * time.Second
)

// Waits before the second and third try of a first fetch that failed for a reason worth retrying
var bootRetries = []time.Duration{10 * time.Second, 20 * time.Second}

// Name the plane takes for a profile
var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Codes of the plane that refuse a value, kept as the refusal's code
var valueRefusals = map[string]bool{
	"400 config_secret_literal": true, "400 config_value_invalid": true, "400 config_path_invalid": true, "400 config_path_reserved": true,
	"413 config_level_too_large": true, "413 config_body_too_large": true,
}

// The plane can't serve a profile's settings, which icebath does not start without
type UnavailableError struct{ Message string }

func (e *UnavailableError) Error() string { return e.Message }

// Write the plane refused or that could not be sent, nothing changed
type WriteError struct {
	Message string
	Code    string
}

func (e *WriteError) Error() string { return e.Message }

// Write touching a key a level above the profile locks
type LockedError struct {
	*WriteError
	Path  string
	Level string
}

func (e *LockedError) Unwrap() error { return e.WriteError }

func lockedError(lock Lock) *LockedError {
	path := Encode(lock.Path)
	return &LockedError{WriteError: &WriteError{Message: fmt.Sprintf("%s is locked by the %s level of Remote Config", path, lock.Level),
		Code: "config_key_locked"}, Path: path, Level: lock.Level}
}

// One fetch that failed, whether another try may help and how long the plane asked to wait
type fetchFailure struct {
	detail     string
	retryable  bool
	retryAfter time.Duration
	asked      bool
	status     int
}

func (e *fetchFailure) Error() string { return e.detail }

// Remote Config of every profile this process reads
type Backend struct {
	mu       sync.Mutex
	profiles map[string]*Profile
	fetching map[string]*sync.Mutex
	grants   map[string]grant
	polling  bool
	stop     chan struct{}
	closed   bool
}

func New() *Backend {
	return &Backend{profiles: map[string]*Profile{}, fetching: map[string]*sync.Mutex{}, grants: map[string]grant{}, stop: make(chan struct{})}
}

// What the plane holds for one profile: its settings as every level resolves them, and the keys an upper level locks
type Profile struct {
	backend *Backend
	name    string
	env     Env

	mu        sync.Mutex
	doc       map[string]any
	etag      string
	version   int
	locks     []Lock
	fetched   time.Time
	failure   string
	next      time.Time
	installed int
}

// Settings of the profile, fetched the first time it is asked for: tried again twice for a failure worth it, an UnavailableError when they
// can't be had
func (b *Backend) Profile(ctx context.Context, name string, env Env) (*Profile, error) {
	b.mu.Lock()
	if p := b.profiles[name]; p != nil {
		b.mu.Unlock()
		return p, nil
	}
	lock := b.fetching[name]
	if lock == nil {
		lock = &sync.Mutex{}
		b.fetching[name] = lock
	}
	b.mu.Unlock()
	p, err := b.boot(ctx, name, env, lock)
	if err != nil {
		return nil, err
	}
	b.poll()
	return p, nil
}

func (b *Backend) boot(ctx context.Context, name string, env Env, lock *sync.Mutex) (*Profile, error) {
	if !profileName.MatchString(name) {
		return nil, &UnavailableError{Message: fmt.Sprintf("Remote Config: %q is not a profile the config plane serves "+
			"(ICEBATH_CONFIG_BACKEND=remote serves profiles named with lowercase letters, digits, '-' and '_').", name)}
	}
	if strings.TrimSpace(env(InstanceEnv)) == "" {
		return nil, &UnavailableError{Message: "Remote Config: " + InstanceEnv + " is not set, so Icebath cannot identify this agent " +
			"to the config plane. Icebath does not start without its remote config."}
	}
	delays := append([]time.Duration(nil), bootRetries...)
	for {
		p, err := func() (*Profile, error) {
			token, err := b.fetchBearer(ctx, env)
			if err != nil {
				return nil, err
			}
			lock.Lock()
			defer lock.Unlock()
			b.mu.Lock()
			p := b.profiles[name]
			b.mu.Unlock()
			if p != nil {
				return p, nil
			}
			p = &Profile{backend: b, name: name, env: env, doc: map[string]any{}}
			if _, err := p.fetch(ctx, false, token, false); err != nil {
				return nil, err
			}
			b.mu.Lock()
			b.profiles[name] = p
			b.mu.Unlock()
			return p, nil
		}()
		if err == nil {
			return p, nil
		}
		failed, ok := errors.AsType[*fetchFailure](err)
		if !ok {
			return nil, err
		}
		if failed.retryable && len(delays) > 0 {
			wait := delays[0]
			if failed.asked && failed.retryAfter > wait {
				wait = failed.retryAfter
			}
			wait, delays = min(wait, retryCap), delays[1:]
			slog.Warn(fmt.Sprintf("Remote Config: fetch for profile %s failed (%s); retrying in %.0fs", strconv.Quote(name), failed, wait.Seconds()))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return nil, &UnavailableError{Message: fmt.Sprintf("Remote Config: cannot load the config for profile %s from %s: %s. Icebath does not "+
			"start without its remote config (ICEBATH_CONFIG_BACKEND=remote has no local fallback).", strconv.Quote(name), shownURL(env), failed)}
	}
}

// Bearer of a fetch, a credential failure counted as a fetch that failed
func (b *Backend) fetchBearer(ctx context.Context, env Env) (string, error) {
	token, err := b.bearer(ctx, env)
	if credential, ok := errors.AsType[*CredentialError](err); ok {
		return "", &fetchFailure{detail: err.Error(), retryable: credential.Retryable}
	}
	return token, err
}

// GET of the profile's route, only when changed for a conditional one: whether the settings changed. held when the caller holds p.mu
func (p *Profile) fetch(ctx context.Context, conditional bool, token string, held bool) (bool, error) {
	lock := func() func() {
		if held {
			return func() {}
		}
		p.mu.Lock()
		return p.mu.Unlock
	}
	unlock := lock()
	started, etag := p.installed, ""
	if conditional {
		etag = p.etag
	}
	unlock()
	if token == "" {
		var err error
		if token, err = p.backend.fetchBearer(ctx, p.env); err != nil {
			return false, err
		}
	}
	resp, err := request(ctx, p.env, "GET", p.name, etag, nil, token)
	if err != nil {
		if credential, ok := errors.AsType[*CredentialError](err); ok {
			return false, &fetchFailure{detail: err.Error(), retryable: credential.Retryable}
		}
		if _, ok := errors.AsType[*TransportError](err); ok {
			return false, &fetchFailure{detail: err.Error(), retryable: true}
		}
		return false, err
	}
	if resp.status == 304 && conditional {
		unlock := lock()
		p.fetched, p.failure = time.Now(), ""
		unlock()
		return false, nil
	}
	if resp.status == 200 {
		if problem := effectiveProblem(resp.body); problem != "" {
			return false, &fetchFailure{detail: "the plane returned a malformed response: " + problem, retryable: true}
		}
		unlock := lock()
		defer unlock()
		if p.installed != started {
			slog.Debug(fmt.Sprintf("Remote Config: dropped a fetch for profile %s that a write overtook", strconv.Quote(p.name)))
			return false, nil
		}
		p.install(resp.body, resp.etag)
		return true, nil
	}
	return false, &fetchFailure{detail: fmt.Sprintf("HTTP %d %s: %s", resp.status, resp.code(), resp.message()),
		retryable: resp.status >= 500 || resp.status == 429, retryAfter: resp.retryAfter, asked: resp.hasRetryAfter, status: resp.status}
}

// Version an answer carries: a whole number, false for anything else
func versionOf(v any) (int, bool) {
	if n, ok := v.(int); ok {
		return n, true
	}
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || f < math.MinInt64 || f >= math.MaxInt64 {
		return 0, false
	}
	return int(f), true
}

// What makes an answer unusable, empty when it holds a config mapping, a whole number profileVersion and a list of locks
func effectiveProblem(body map[string]any) string {
	if _, ok := body["config"].(map[string]any); !ok {
		return fmt.Sprintf("its config is %s where a mapping was expected", kind(body["config"]))
	}
	if _, ok := versionOf(body["profileVersion"]); !ok {
		return fmt.Sprintf("its profileVersion is %s where a whole number was expected", kind(body["profileVersion"]))
	}
	if _, ok := body["locks"].([]any); !ok {
		return fmt.Sprintf("its locks are %s where a list was expected", kind(body["locks"]))
	}
	return ""
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "a mapping"
	case []any:
		return "a list"
	case string:
		return "text"
	case bool:
		return "a boolean"
	case int, float64:
		return "a number"
	}
	return fmt.Sprintf("%T", v)
}

// Takes an answer as the profile's settings, under p.mu, its malformed locks told and left out
func (p *Profile) install(body map[string]any, etag string) {
	var locks []Lock
	raw, _ := body["locks"].([]any)
	for _, entry := range raw {
		lock, ok := decodeLock(entry)
		if !ok {
			shown, _ := json.Marshal(entry)
			slog.Warn("Remote Config: ignoring malformed lock entry " + string(shown))
			continue
		}
		locks = append(locks, lock)
	}
	p.doc = cloneDoc(body["config"].(map[string]any))
	p.etag = etag
	if p.etag == "" {
		p.etag, _ = body["etag"].(string)
	}
	p.version, _ = versionOf(body["profileVersion"])
	p.locks = locks
	p.installed++
	p.fetched, p.failure = time.Now(), ""
	p.next = time.Now().Add(time.Duration(float64(max(pollEvery, pollFloor)) * (0.9 + 0.2*rand.Float64())))
}

// Lock one entry of the plane's list names, false without a text path or a level; a null level reads as the upper level
func decodeLock(entry any) (Lock, bool) {
	m, ok := entry.(map[string]any)
	if !ok {
		return Lock{}, false
	}
	raw, ok := m["path"].(string)
	if !ok {
		return Lock{}, false
	}
	level, ok := m["level"]
	if !ok {
		return Lock{}, false
	}
	path, err := Decode(raw)
	if err != nil {
		return Lock{}, false
	}
	name := "upper"
	if level != nil {
		name = fmt.Sprint(level)
	}
	return Lock{Path: path, Level: name}, true
}

// Settings of the profile as the plane resolves them, a copy
func (p *Profile) Doc() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneDoc(p.doc)
}

// Level that locks the dotted key or a key above it, false when none does
func (p *Profile) Locked(key string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lock, ok := covering(p.locks, FromDotted(key)); ok {
		return lock.Level, true
	}
	return "", false
}

// Fetches the settings again when they changed, an error leaving them as they were
func (p *Profile) Refresh(ctx context.Context) error {
	_, err := p.fetch(ctx, true, "", false)
	if err != nil {
		p.mu.Lock()
		p.failure = err.Error()
		p.mu.Unlock()
	}
	return err
}

// Saves the value under the dotted key at the profile's level, nil removing it: sent as one change against the version read, read again
// and sent once more when another write came first
func (p *Profile) Set(ctx context.Context, key string, value any) error {
	token, err := p.backend.bearer(ctx, p.env)
	if err != nil {
		if _, ok := errors.AsType[*CredentialError](err); ok {
			return &WriteError{Message: "Remote Config write failed: " + err.Error(), Code: "config_plane_unreachable"}
		}
		return err
	}
	path := FromDotted(key)
	keyed := &pathMap{}
	if value != nil {
		wire, err := toWire(value, path)
		if err != nil {
			return err
		}
		if m, ok := wire.(map[string]any); ok && len(m) > 0 {
			leaves(m, path, keyed)
		} else {
			keyed.set(path, wire)
		}
	}
	for attempt := 1; attempt <= 2; attempt++ {
		p.mu.Lock()
		body, unsets, err := p.change(keyed, path, value == nil)
		if err != nil || body == nil {
			p.mu.Unlock()
			return err
		}
		resp, err := request(ctx, p.env, "PATCH", p.name, "", body, token)
		if err != nil {
			p.mu.Unlock()
			_, credential := errors.AsType[*CredentialError](err)
			if _, transport := errors.AsType[*TransportError](err); credential || transport {
				return &WriteError{Message: "Remote Config write failed: " + err.Error(), Code: "config_plane_unreachable"}
			}
			return err
		}
		if resp.status == 200 && effectiveProblem(resp.body) == "" {
			p.install(resp.body, resp.etag)
			for _, u := range unsets {
				if _, still := lookup(p.doc, u); still {
					slog.Warn("Remote Config: " + Encode(u) + " is still set by an upper level; removing it from this profile cannot remove an " +
						"inherited value (set it to null for 'no value')")
				}
			}
			p.mu.Unlock()
			return nil
		}
		if resp.status == 409 && attempt == 1 {
			_, err := p.fetch(ctx, false, token, true)
			p.mu.Unlock()
			if err != nil {
				if _, failed := errors.AsType[*fetchFailure](err); failed {
					return &WriteError{Message: "Remote Config write conflict and re-read failed: " + err.Error(), Code: "config_version_conflict"}
				}
				return err
			}
			continue
		}
		p.mu.Unlock()
		return writeError(resp)
	}
	return nil
}

// Body of the change that applies the keyed write to the settings as read, nil when it changes nothing; under p.mu
func (p *Profile) change(keyed *pathMap, path Path, unset bool) (map[string]any, []Path, error) {
	base := cloneDoc(p.doc)
	next := cloneDoc(base)
	var unsets []Path
	if unset {
		unsets = []Path{path}
	}
	apply(next, keyed, unsets)
	sets, removed := diff(base, next, nil)
	// A key written at the profile's level is sent even when an upper level already gives it the same value, so the profile holds it
	for _, e := range keyed.entries {
		if m, ok := e.value.(map[string]any); ok && len(m) == 0 {
			continue
		}
		if _, locked := covering(p.locks, e.path); sets.has(e.path) || locked {
			continue
		}
		covered := false
		for _, s := range sets.entries {
			covered = covered || Covers(s.path, e.path)
		}
		if !covered {
			sets.set(e.path, clone(e.value))
		}
	}
	if lock, refused := writeCheck(sets, removed, p.locks); refused {
		return nil, nil, lockedError(lock)
	}
	for _, e := range sets.entries {
		if bad, ok := secretLiteral(e.path, e.value); ok {
			return nil, nil, &WriteError{Message: Encode(bad) + " is a secret-shaped key: Remote Config stores only a ${VAR} reference there, never the " +
				"secret itself. Put the value in .env and set a ${VAR} reference.", Code: "config_secret_literal"}
		}
	}
	if len(sets.entries) == 0 && len(removed) == 0 {
		return nil, nil, nil
	}
	setBody, unsetBody := encodeChanges(sets, removed)
	body := map[string]any{"expectedVersion": p.version}
	if len(setBody) > 0 {
		body["set"] = setBody
	}
	if len(unsetBody) > 0 {
		body["unset"] = unsetBody
	}
	return body, removed, nil
}

// Refusal of a write as the plane answered it, a lock or a value keeping the plane's code
func writeError(resp response) error {
	if resp.status == 403 && resp.code() == "config_key_locked" {
		path, _ := resp.body["path"].(string)
		level, _ := resp.body["lockedBy"].(string)
		if level == "" {
			level = "upper"
		}
		return &LockedError{WriteError: &WriteError{Message: "Remote Config refused the write: " + resp.message(), Code: "config_key_locked"}, Path: path,
			Level: level}
	}
	if valueRefusals[strconv.Itoa(resp.status)+" "+resp.code()] {
		detail := resp.message()
		var extras []string
		for _, name := range []string{"path", "reason"} {
			if v, ok := resp.body[name].(string); ok && v != "" && !strings.Contains(detail, v) {
				extras = append(extras, name+" "+v)
			}
		}
		if len(extras) > 0 {
			detail = fmt.Sprintf("%s (%s)", detail, strings.Join(extras, ", "))
		}
		return &WriteError{Message: "Remote Config refused the write: " + detail, Code: resp.code()}
	}
	return &WriteError{Message: fmt.Sprintf("Remote Config refused the write (%d %s): %s", resp.status, resp.code(), resp.message()), Code: resp.code()}
}

// Where the profile's writes go, as a person reads it
func (p *Profile) Destination() string {
	return fmt.Sprintf("Remote Config (profile %s level)", strconv.Quote(p.name))
}

// The plane, this agent and how the profile stands with it
func (p *Profile) Describe() string {
	instance := strings.TrimSpace(p.env(InstanceEnv))
	if instance == "" {
		instance = "<unset>"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	tail := ""
	if p.failure != "" {
		tail = ", last poll failed: " + p.failure
	}
	return fmt.Sprintf("Remote Config %s (instance %s): profile %s, profile level v%d, %d lock(s), fetched %ds ago%s", shownURL(p.env), instance,
		strconv.Quote(p.name), p.version, len(p.locks), int(time.Since(p.fetched).Seconds()), tail)
}

// Starts the goroutine that polls every profile, unless one runs or the backend is closed
func (b *Backend) poll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.polling || b.closed {
		return
	}
	b.polling = true
	go func() {
		for {
			wake := b.tick()
			timer := time.NewTimer(max(time.Second, time.Until(wake)))
			select {
			case <-b.stop:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// Polls every profile that is due and says when the next one is, a panic trying again after the floor
func (b *Backend) tick() (wake time.Time) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("Remote Config: poll loop error; retrying", "error", r)
			wake = time.Now().Add(pollFloor)
		}
	}()
	now := time.Now()
	b.mu.Lock()
	all := make([]*Profile, 0, len(b.profiles))
	for _, p := range b.profiles {
		all = append(all, p)
	}
	b.mu.Unlock()
	wake = now.Add(max(pollEvery, pollFloor))
	for _, p := range all {
		p.mu.Lock()
		due := !p.next.After(now)
		p.mu.Unlock()
		if due {
			p.pollOnce()
		}
		p.mu.Lock()
		if p.next.Before(wake) {
			wake = p.next
		}
		p.mu.Unlock()
	}
	return wake
}

// One conditional GET that never fails, an error keeping the settings loaded until the next one
func (p *Profile) pollOnce() {
	changed, err := p.fetch(context.Background(), true, "", false)
	if err != nil {
		p.mu.Lock()
		p.failure = err.Error()
		p.mu.Unlock()
		msg := fmt.Sprintf("Remote Config: poll for profile %s failed (%s); keeping the loaded config", strconv.Quote(p.name), err)
		if failed, ok := errors.AsType[*fetchFailure](err); ok && (failed.status == 401 || failed.status == 403) {
			slog.Error(msg)
		} else {
			slog.Warn(msg)
		}
	}
	p.mu.Lock()
	p.next = time.Now().Add(time.Duration(float64(max(pollEvery, pollFloor)) * (0.9 + 0.2*rand.Float64())))
	version := p.version
	p.mu.Unlock()
	if changed {
		slog.Info(fmt.Sprintf("Remote Config: profile %s changed (profile level v%d)", strconv.Quote(p.name), version))
	}
}

// Forgets a profile, its settings fetched again the next time they are asked for
func (b *Backend) Forget(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.profiles, name)
}

// Stops polling
func (b *Backend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		close(b.stop)
	}
}
