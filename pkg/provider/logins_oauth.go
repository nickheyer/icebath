package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// States of a hosted OAuth sign in
const (
	LoginOAuthRunning      = "running"
	LoginOAuthWaitingInput = "waiting_input"
	LoginOAuthDone         = "done"
	LoginOAuthFailed       = "failed"
	LoginOAuthCancelled    = "cancelled"
)

// How long a finished sign in stays pollable
const loginOAuthJobTTL = 10 * time.Minute

var (
	loginOAuthURLRE  = regexp.MustCompile(`https?://[^\s"'<>]+`)
	loginOAuthCodeRE = regexp.MustCompile(`\b[A-Z0-9]{4,}-[A-Z0-9]{4,}\b`)
)

// LoginOAuthOptions shapes the login a hosted OAuth sign in stores
type LoginOAuthOptions struct {
	Name        string
	Rotate      bool
	MakeDefault bool
	// openai-codex only, the browser PKCE flow instead of the device code; the client opens the link, never this process
	Browser bool
}

// LoginOAuthSnapshot is what a poll of a hosted sign in sees
type LoginOAuthSnapshot struct {
	ID       string
	Provider string
	State    string
	Lines    []string
	URL      string
	UserCode string
	Prompt   string
	Login    *Login
	Error    string
	// The client opens URL in a browser on the user's machine, the service never opens one
	Open bool
	// The sign in finishes through a redirect to the background service machine's own localhost
	Loopback bool
}

// LoginOAuthJob is one OAuth sign in running in the background until it stores a new owned login
type LoginOAuthJob struct {
	id       string
	provider string
	mu       sync.Mutex
	lines    []string
	partial  string
	state    string
	prompt   string
	answers  chan string
	login    *Login
	err      string
	ctx      context.Context
	cancel   context.CancelFunc
	finished time.Time
	loopback bool
}

var (
	loginOAuthJobsMu sync.Mutex
	loginOAuthJobs   = map[string]*LoginOAuthJob{}
)

// Writes sign in output into the job's line buffer
func (j *LoginOAuthJob) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	text := j.partial + string(p)
	parts := strings.Split(text, "\n")
	j.partial = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		j.lines = append(j.lines, strings.TrimRight(line, "\r"))
	}
	return len(p), nil
}

// Blocks the sign in on a prompt until the client answers it or the job is cancelled
func (j *LoginOAuthJob) ask(prompt string) (string, bool) {
	j.mu.Lock()
	j.prompt, j.state = strings.TrimSpace(prompt), LoginOAuthWaitingInput
	j.mu.Unlock()
	var answer string
	ok := true
	select {
	case answer = <-j.answers:
	case <-j.ctx.Done():
		ok = false
	}
	j.mu.Lock()
	j.prompt = ""
	if j.state == LoginOAuthWaitingInput {
		j.state = LoginOAuthRunning
	}
	j.mu.Unlock()
	return answer, ok
}

// Answer hands the client's input to the prompt the sign in waits on
func (j *LoginOAuthJob) Answer(text string) error {
	j.mu.Lock()
	waiting := j.state == LoginOAuthWaitingInput
	j.mu.Unlock()
	if !waiting {
		return loginFault(LoginFaultNotWaiting, "the sign in is not waiting for input")
	}
	select {
	case j.answers <- text:
		return nil
	case <-time.After(2 * time.Second):
		return loginFault(LoginFaultNotWaiting, "the sign in did not take the input")
	}
}

// Cancel stops a running sign in, leaving nothing stored
func (j *LoginOAuthJob) Cancel() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != LoginOAuthRunning && j.state != LoginOAuthWaitingInput {
		return
	}
	j.state, j.finished = LoginOAuthCancelled, nowFunc()
	j.cancel()
}

// Whether the client cancelled the sign in
func (j *LoginOAuthJob) cancelled() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state == LoginOAuthCancelled
}

// Snapshot is the job's state, output and result as of now
func (j *LoginOAuthJob) Snapshot() LoginOAuthSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	lines := append([]string(nil), j.lines...)
	if strings.TrimSpace(j.partial) != "" {
		lines = append(lines, j.partial)
	}
	snap := LoginOAuthSnapshot{ID: j.id, Provider: j.provider, State: j.state, Lines: lines, Prompt: j.prompt, Login: j.login, Error: j.err, Loopback: j.loopback}
	for _, line := range lines {
		if snap.URL == "" {
			snap.URL = strings.TrimRight(loginOAuthURLRE.FindString(line), ".,)")
		}
		if snap.UserCode == "" {
			snap.UserCode = loginOAuthCodeRE.FindString(line)
		}
	}
	snap.Open = snap.URL != "" && snap.UserCode == ""
	return snap
}

// Records the outcome unless the job was cancelled meanwhile
func (j *LoginOAuthJob) finish(login *Login, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state == LoginOAuthCancelled {
		return
	}
	j.finished = nowFunc()
	if err != nil {
		j.state, j.err = LoginOAuthFailed, err.Error()
		return
	}
	j.state, j.login = LoginOAuthDone, login
}

// LoginOAuthJobByID is a hosted sign in still held, nil when unknown or expired
func LoginOAuthJobByID(id string) *LoginOAuthJob {
	loginOAuthJobsMu.Lock()
	defer loginOAuthJobsMu.Unlock()
	now := nowFunc()
	for key, job := range loginOAuthJobs {
		job.mu.Lock()
		expired := !job.finished.IsZero() && now.Sub(job.finished) > loginOAuthJobTTL
		job.mu.Unlock()
		if expired {
			delete(loginOAuthJobs, key)
		}
	}
	return loginOAuthJobs[id]
}

// Provider id an OAuth sign in stores under, the OAuth sibling of an API key provider
func loginOAuthProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if sibling, ok := oauthSiblingProviders[provider]; ok {
		return sibling
	}
	return provider
}

// Profile of a provider that signs in through its own device login hooks instead of an icebath OAuth flow, false for any other provider
func deviceLoginProfile(provider string) (Profile, bool) {
	if _, builtin := authOAuthAddSpecs[provider]; builtin {
		return Profile{}, false
	}
	profile, ok := pluginProfile(provider)
	return profile, ok && profileHasDeviceLogin(profile)
}

// Login a device login profile's sign in left behind, the provider's login in use else a row standing for the sign in
func deviceLoginResult(env PoolEnv, provider string) Login {
	group := ListLogins(env, provider)
	for _, l := range group.Logins {
		if l.Active {
			return l
		}
	}
	if len(group.Logins) > 0 {
		return group.Logins[0]
	}
	return Login{Provider: provider, Name: provider, Label: group.Label, AuthType: AuthTypeOAuth, Origin: LoginOriginOwned, Status: LoginStatusReady}
}

// Login a device login profile's own save stored, named, put in rotation and made the default as asked; the provider's login in use, with a line saying the options did not apply, when the profile keeps the sign in outside the provider's logins
func adoptDeviceLogin(env PoolEnv, out io.Writer, provider string, before []PooledCredential, previousActive string, o LoginOAuthOptions) (Login, error) {
	entry, isNew, found := loginStoredSince(env, provider, before)
	if !found {
		if o.Name != "" || o.Rotate || o.MakeDefault {
			fmt.Fprintf(out, "%s keeps this sign-in itself, so the name, rotation and default asked for did not apply.\n", provider)
		}
		return deviceLoginResult(env, provider), nil
	}
	var signedIn Login
	var err error
	if isNew {
		signedIn, err = NameNewLogin(env, provider, entry.ID, o.Name, o.Rotate, previousActive)
	} else {
		for _, held := range ListLogins(env, provider).Logins {
			if held.ID == entry.ID {
				signedIn = held
			}
		}
		if o.Name != "" && o.Name != entry.LoginName() {
			signedIn, err = RenameLogin(env, provider, entry.ID, o.Name)
		}
		if err == nil && o.Rotate {
			signedIn, err = SetLoginRotation(env, provider, entry.ID, true)
		}
	}
	if err == nil && o.MakeDefault {
		_, err = SetLoginDefault(env, provider, entry.ID)
	}
	return signedIn, err
}

// StartLoginOAuth starts a provider's OAuth sign in in the background, reusing the device code and PKCE flows of icebath auth add
func StartLoginOAuth(env PoolEnv, provider string, o LoginOAuthOptions) (*LoginOAuthJob, error) {
	asked := strings.ToLower(strings.TrimSpace(provider))
	provider = loginOAuthProvider(asked)
	profile, deviceLogin := deviceLoginProfile(provider)
	if _, ok := authOAuthAddSpecs[provider]; !ok && !deviceLogin {
		return nil, loginFault(LoginFaultAuthType, "%s", noOAuthSignInMessage(asked))
	}
	if o.Name != "" {
		if err := checkNewLoginName(env, provider, o.Name); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(env.ctx()))
	job := &LoginOAuthJob{id: newPoolID() + newPoolID(), provider: provider, state: LoginOAuthRunning, answers: make(chan string), ctx: ctx, cancel: cancel,
		loopback: provider == "openrouter" || (provider == "openai-codex" && o.Browser)}
	loginOAuthJobsMu.Lock()
	loginOAuthJobs[job.id] = job
	loginOAuthJobsMu.Unlock()
	jobEnv := env
	jobEnv.Ctx = ctx
	ns := map[string]any{"provider": provider, "auth_type": AuthTypeOAuth, "no_browser": true, "browser": o.Browser, "label": nil}
	if asked == "minimax-cn" {
		ns["region"] = "cn"
	}
	cli := &authCLI{ctx: ctx, env: jobEnv, ns: ns, out: job, errw: job, input: job.ask}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				job.finish(nil, fmt.Errorf("sign in failed: %v", r))
			}
		}()
		if deviceLogin {
			before, previousActive := cli.storedLogins(provider)
			if err := profileDeviceLogin(ctx, job, profile.Label(), profile, false); err != nil {
				if !isSignInCancelled(err) {
					job.finish(nil, err)
				}
				return
			}
			signedIn, err := adoptDeviceLogin(jobEnv, job, provider, before, previousActive, o)
			if err != nil {
				job.finish(nil, err)
				return
			}
			job.finish(&signedIn, nil)
			return
		}
		named, err := cli.signInLogin(provider, o, job.cancelled)
		if errors.Is(err, errSignInCancelled) {
			return
		}
		if err != nil {
			job.finish(nil, err)
			return
		}
		job.finish(&named, nil)
	}()
	return job, nil
}

// Sign in that completed after its client cancelled it, its login removed again
var errSignInCancelled = errors.New("the sign in was cancelled")

// Runs a provider's OAuth sign in and stores it as a new owned login, named and made the default as asked, removed again when cancelled meanwhile
func (c *authCLI) signInLogin(provider string, o LoginOAuthOptions, cancelled func() bool) (Login, error) {
	previousActive := currentLoginID(c.env, provider)
	c.unsuppressProviderSources(provider)
	entry, err := c.addCredential(provider, LoadPool(c.env, provider), AuthTypeOAuth)
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			err = errors.New(formatAuthError(authErr))
		}
		return Login{}, err
	}
	if cancelled != nil && cancelled() {
		if _, err := RemoveLogin(c.env, provider, entry.ID); err != nil {
			c.env.log().Warn(fmt.Sprintf("%s: a cancelled sign in completed and its login %s could not be removed (%v)", provider, entry.ID, err))
		}
		return Login{}, errSignInCancelled
	}
	named, err := NameNewLogin(c.env, provider, entry.ID, o.Name, o.Rotate, previousActive)
	if err == nil && o.MakeDefault {
		_, err = SetLoginDefault(c.env, provider, named.ID)
	}
	return named, err
}

// OAuthSignIn are the terminal flags of an OAuth sign in
type OAuthSignIn struct {
	// The browser PKCE flow over the device code where a provider has both
	Browser bool
	// Print the link instead of opening a browser
	NoBrowser bool
	Region    string
	Timeout   time.Duration
	// Make the new login the provider's profile default
	MakeDefault bool
}

// SignInOAuthLogin runs a provider's OAuth sign in in the terminal and stores it as a new owned login, the profile default when asked
func SignInOAuthLogin(env PoolEnv, out io.Writer, provider string, o OAuthSignIn) (Login, error) {
	provider = loginOAuthProvider(provider)
	if profile, deviceLogin := deviceLoginProfile(provider); deviceLogin {
		before, previousActive := LoadPool(env, provider).Entries(), currentLoginID(env, provider)
		if err := profileDeviceLogin(env.ctx(), out, profile.Label(), profile, !o.NoBrowser); err != nil {
			return Login{}, err
		}
		return adoptDeviceLogin(env, out, provider, before, previousActive, LoginOAuthOptions{MakeDefault: o.MakeDefault})
	}
	if _, ok := authOAuthAddSpecs[provider]; !ok {
		return Login{}, loginFault(LoginFaultAuthType, "%s", noOAuthSignInMessage(provider))
	}
	ns := map[string]any{"provider": provider, "auth_type": AuthTypeOAuth, "no_browser": o.NoBrowser || isRemoteSession(), "browser": o.Browser, "label": nil}
	if o.Region != "" {
		ns["region"] = o.Region
	}
	if o.Timeout > 0 {
		ns["timeout"] = o.Timeout.Seconds()
	}
	cli := &authCLI{ctx: env.ctx(), env: env, ns: ns, out: out, errw: out}
	return cli.signInLogin(provider, LoginOAuthOptions{MakeDefault: o.MakeDefault}, nil)
}
