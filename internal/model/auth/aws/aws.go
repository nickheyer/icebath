// AWS credential chain, request signing, event stream framing and region resolution for the Bedrock routes
package aws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	advisoryRefresh  = 15 * time.Minute
	mandatoryRefresh = 10 * time.Minute
	apiTimeout       = 60 * time.Second
	callDeadline     = 2 * time.Minute
	replyLimit       = 1 << 20
	stderrLimit      = 4096
)

var ErrNoCredentials = errors.New("no AWS credentials available, configure AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, AWS_PROFILE, SSO, or an instance or task role")

var ErrAmbientRefused = errors.New("Bedrock auth is refused for this profile: it sets no complete AWS credential of its own, and under multiplexed profiles the ambient default chain (process env, ~/.aws, instance metadata) would sign this profile's calls with the launch context's identity. Set AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY in this profile's own config, or AWS_PROFILE to name a shared AWS config profile.")

var ErrMFARequired = errors.New("cannot refresh credentials, an MFA token is required")

var errIncomplete = errors.New("credential refresh returned no credentials")

type Error struct {
	Source  string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Source != "" {
		return "aws: retrieving credentials from " + e.Source + ": " + e.Message
	}
	return "aws: " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

type ServiceError struct {
	Service   string
	Operation string
	Status    int
	Code      string
	Message   string
}

func (e *ServiceError) Error() string {
	return fmt.Sprintf("aws: %s %s failed with status %d (%s): %s", e.Service, e.Operation, e.Status, e.Code, e.Message)
}

type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expires         time.Time
	Source          string
}

func (c Credentials) String() string {
	return "aws.Credentials{source: " + c.Source + ", key: " + maskKey(c.AccessKeyID) + "}"
}

func (c Credentials) GoString() string { return c.String() }

func (c Credentials) LogValue() slog.Value { return slog.StringValue(c.String()) }

func maskKey(id string) string {
	if len(id) <= 4 {
		return "****"
	}
	return id[:4] + "****"
}

func (c Credentials) complete() bool { return c.AccessKeyID != "" && c.SecretAccessKey != "" }

type Env func(name string) string

func ProcessEnv(name string) string { return os.Getenv(name) }

type Scope struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Profile         string
}

func (s Scope) String() string {
	return "aws.Scope{profile: " + s.Profile + ", key: " + maskKey(s.AccessKeyID) + "}"
}

func (s Scope) GoString() string { return s.String() }

func (s Scope) LogValue() slog.Value { return slog.StringValue(s.String()) }

func Scoped(secrets Env, multiplex bool) (Scope, error) {
	s := Scope{
		AccessKeyID:     strings.TrimSpace(secrets("AWS_ACCESS_KEY_ID")),
		SecretAccessKey: strings.TrimSpace(secrets("AWS_SECRET_ACCESS_KEY")),
		SessionToken:    strings.TrimSpace(secrets("AWS_SESSION_TOKEN")),
		Profile:         strings.TrimSpace(secrets("AWS_PROFILE")),
	}
	complete := (s.AccessKeyID != "" && s.SecretAccessKey != "") || s.Profile != ""
	if !complete && multiplex {
		return Scope{}, ErrAmbientRefused
	}
	return s, nil
}

type Options struct {
	Scope
	Region             string
	NoInstanceMetadata bool
	HTTP               httpclient.Options
	MFA                func(ctx context.Context, serial string) (string, error)
}

type Chain struct {
	opts   Options
	api    *http.Client
	direct *http.Client
	mu     sync.Mutex
	source provider
}

func NewChain(o Options) (*Chain, error) {
	apiOpts := o.HTTP
	apiOpts.Timeouts = httpclient.EachPhase(apiTimeout)
	if bundle := caBundle(o.Profile); bundle != "" {
		apiOpts.TLS.CAFiles = append(append([]string(nil), apiOpts.TLS.CAFiles...), bundle)
	}
	api, err := httpclient.New(apiOpts)
	if err != nil {
		return nil, err
	}
	directOpts := o.HTTP
	directOpts.Proxy = httpclient.ProxyOptions{IgnoreEnv: true}
	directOpts.Timeouts = httpclient.EachPhase(containerTimeout)
	direct, err := httpclient.New(directOpts)
	if err != nil {
		return nil, err
	}
	return &Chain{opts: o, api: api.Client(), direct: direct.Client()}, nil
}

func (c *Chain) Retrieve(ctx context.Context) (Credentials, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.source == nil {
		src, err := c.resolve(ctx)
		if err != nil {
			return Credentials{}, err
		}
		c.source = src
	}
	creds, err := c.source.retrieve(ctx)
	if err != nil {
		c.source = nil
		return Credentials{}, err
	}
	if !creds.complete() {
		c.source = nil
		return Credentials{}, &Error{Source: creds.Source, Message: "credentials are missing the access key id or the secret access key"}
	}
	return creds, nil
}

func (c *Chain) Source(ctx context.Context, env Env) string {
	if name := EnvSource(env); name != "" {
		return name
	}
	if creds, err := c.Retrieve(ctx); err == nil && creds.AccessKeyID != "" {
		return "iam-role"
	}
	return ""
}

func (c *Chain) HasCredentials(ctx context.Context, env Env) bool {
	return c.Source(ctx, env) != ""
}

type provider interface {
	retrieve(ctx context.Context) (Credentials, error)
}

type static struct{ creds Credentials }

func (p static) retrieve(context.Context) (Credentials, error) { return p.creds, nil }

type refreshing struct {
	mu    sync.Mutex
	fetch func(ctx context.Context) (Credentials, error)
	cur   Credentials
	have  bool
}

func newRefreshing(fetch func(ctx context.Context) (Credentials, error), initial *Credentials) *refreshing {
	r := &refreshing{fetch: fetch}
	if initial != nil {
		r.cur, r.have = *initial, true
	}
	return r
}

func refreshDue(c Credentials, now time.Time, window time.Duration) bool {
	if c.Expires.IsZero() {
		return false
	}
	return c.Expires.Sub(now) < window
}

func (r *refreshing) retrieve(ctx context.Context) (Credentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.have && !refreshDue(r.cur, now, advisoryRefresh) {
		return r.cur, nil
	}
	mandatory := !r.have || refreshDue(r.cur, now, mandatoryRefresh)
	fresh, err := r.fetch(ctx)
	if err != nil {
		if !mandatory && !errors.Is(err, errIncomplete) {
			slog.Warn("refreshing temporary AWS credentials failed during the advisory refresh period", "source", r.cur.Source, "error", err)
			return r.cur, nil
		}
		return Credentials{}, err
	}
	r.cur, r.have = fresh, true
	if !fresh.Expires.IsZero() && fresh.Expires.Before(time.Now()) {
		return Credentials{}, &Error{Source: fresh.Source, Message: "credentials were refreshed, but the refreshed credentials are still expired"}
	}
	return r.cur, nil
}

func (c *Chain) resolve(ctx context.Context) (provider, error) {
	o := c.opts
	if o.AccessKeyID != "" || o.SecretAccessKey != "" || o.SessionToken != "" {
		creds := Credentials{AccessKeyID: o.AccessKeyID, SecretAccessKey: o.SecretAccessKey, SessionToken: o.SessionToken, Source: "explicit"}
		if creds.AccessKeyID == "" {
			return nil, partial("explicit", "aws_access_key_id")
		}
		if creds.SecretAccessKey == "" {
			return nil, partial("explicit", "aws_secret_access_key")
		}
		return static{creds}, nil
	}
	s, err := newSession(o.Profile)
	if err != nil {
		return nil, err
	}
	if _, err := s.scoped(); err != nil {
		return nil, err
	}
	r := &resolver{c: c, s: s}
	profile := s.name()
	envDisabled := o.Profile != ""
	links := make([]func(context.Context) (provider, error), 0, 12)
	if !envDisabled {
		links = append(links, func(context.Context) (provider, error) { return envProvider() })
	}
	links = append(links, func(ctx context.Context) (provider, error) { return r.assumeRole(ctx, profile) })
	links = append(links, r.profileLinks(profile, envDisabled)...)
	links = append(links,
		func(context.Context) (provider, error) { return ec2FileProvider() },
		func(context.Context) (provider, error) { return botoProvider() },
		r.container,
		r.instanceMetadata,
	)
	for _, link := range links {
		p, err := link(ctx)
		if err != nil {
			return nil, err
		}
		if p != nil {
			return p, nil
		}
	}
	return nil, ErrNoCredentials
}

type resolver struct {
	c       *Chain
	s       *session
	visited []string
}

func (r *resolver) profileLinks(profile string, envDisabled bool) []func(context.Context) (provider, error) {
	return []func(context.Context) (provider, error){
		func(context.Context) (provider, error) { return r.webIdentity(profile, envDisabled) },
		func(context.Context) (provider, error) { return r.sso(profile) },
		func(context.Context) (provider, error) {
			return staticFromSection(r.s.files.credentials[profile], "shared-credentials-file")
		},
		func(context.Context) (provider, error) { return r.login(profile) },
		func(ctx context.Context) (provider, error) { return r.process(ctx, profile) },
		func(context.Context) (provider, error) {
			return staticFromSection(r.s.files.config[profile], "config-file")
		},
	}
}

func partial(source, missing string) error {
	return &Error{Source: source, Message: "partial credentials found, missing " + missing}
}
