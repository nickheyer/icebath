// Icebath Portal account of one profile home, its login, free tier, runtime credentials and managed tool gateway
package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/secret"
)

const (
	Provider = auth.ProviderPortal

	EnvPortalBaseURL         = "ICEBATH_PORTAL_BASE_URL"
	EnvPortalBaseURLAlias    = "ICEBATH_PORTAL_PORTAL_BASE_URL"
	EnvInferenceBaseURL      = "ICEBATH_PORTAL_INFERENCE_BASE_URL"
	EnvTimeoutSeconds        = "ICEBATH_PORTAL_TIMEOUT_SECONDS"
	EnvSharedAuthDir         = "ICEBATH_SHARED_AUTH_DIR"
	EnvOAuthTrace            = "ICEBATH_OAUTH_TRACE"
	EnvAnonSecret            = "ICEBATH_ANON_API_SECRET"
	EnvPreviewFullConnectors = "ICEBATH_PREVIEW_FULL_CONNECTORS"
	EnvExtraWelcomeHosts     = "ICEBATH_EXTRA_WELCOME_HOSTS"

	defaultTimeout = 15 * time.Second
	maxReplyBytes  = 8 << 20
)

type Surface string

const (
	SurfaceDesktop Surface = "desktop"
	SurfaceGateway Surface = "gateway"
	SurfaceCLI     Surface = "cli"
)

type Broadcaster interface {
	Broadcast(event string, payload json.RawMessage) error
	StdioRPC() bool
}

type ConfigUpdate struct {
	Provider     string
	BaseURL      string
	DefaultModel string
	ClearDefault bool
}

type Options struct {
	Home             string
	Version          string
	Store            *auth.Store
	Config           *config.Store
	Secrets          *secret.Scope
	Homes            *home.Resolver
	Host             *hostinfo.Host
	Surface          func(ctx context.Context) Surface
	Events           Broadcaster
	Notify           func(text string)
	OpenBrowser      func(ctx context.Context, target string) bool
	UpdateConfig     func(ctx context.Context, u ConfigUpdate) (string, error)
	RecommendedModel func(ctx context.Context) (string, error)
	AppTitle         func(slug string) string
	ResolveProvider  func(ctx context.Context, skipFreeTier bool) (string, error)
	LocalDesktop     func(ctx context.Context) bool
	Tools            ToolFacts
	Identity         CallerIdentity
	Install          Install
}

type Account struct {
	home        string
	root        string
	version     string
	store       *auth.Store
	cfg         *config.Store
	secrets     *secret.Scope
	homes       *home.Resolver
	host        *hostinfo.Host
	surface     func(ctx context.Context) Surface
	events      Broadcaster
	notify      func(text string)
	openBrowser func(ctx context.Context, target string) bool
	update      func(ctx context.Context, u ConfigUpdate) (string, error)
	recommended func(ctx context.Context) (string, error)
	appTitle    func(slug string) string

	resolveProvider func(ctx context.Context, skipFreeTier bool) (string, error)
	localDesktop    func(ctx context.Context) bool
	tools           ToolFacts
	identity        CallerIdentity
	install         Install

	mu          sync.Mutex
	mintFailure *MintFailure
	tokenMemo   tokenMemo
	billingMemo billingMemo
	accountMemo *accountCache
	setup       setupState
	statusMemo  *statusMemo
	challenge   challengeRecord
	opened      map[string]bool
	workMu      sync.Mutex
	keepalive   *loop
	worker      *loop
	closed      bool
}

type tokenMemo struct {
	at    time.Time
	token string
	set   bool
}

type loop struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func newLoop() *loop { return &loop{stop: make(chan struct{}), done: make(chan struct{})} }

func (l *loop) halt(timeout time.Duration) {
	l.once.Do(func() { close(l.stop) })
	select {
	case <-l.done:
	case <-time.After(timeout):
	}
}

func (l *loop) ended() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

func (l *loop) wait(d time.Duration) bool {
	if d <= 0 {
		select {
		case <-l.stop:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-l.stop:
		return true
	case <-t.C:
		return false
	}
}

func New(o Options) (*Account, error) {
	if strings.TrimSpace(o.Home) == "" {
		return nil, errors.New("portal: account needs a profile home")
	}
	if o.Store == nil || o.Config == nil || o.Secrets == nil || o.Homes == nil || o.Host == nil || o.Version == "" {
		return nil, errors.New("portal: account needs the credential store, config, secret scope, home resolver, host facts and product version")
	}
	if o.Surface == nil || o.Notify == nil || o.OpenBrowser == nil || o.UpdateConfig == nil || o.RecommendedModel == nil || o.AppTitle == nil {
		return nil, errors.New("portal: account needs the surface, notice, browser, config update, recommended model and app title functions")
	}
	if o.ResolveProvider == nil || o.LocalDesktop == nil || o.Tools == nil || o.Identity == nil || o.Install == nil {
		return nil, errors.New("portal: account needs the provider resolver, local desktop check, tool facts, caller identity and install state")
	}
	root, err := home.RootOf(o.Home)
	if err != nil {
		return nil, fmt.Errorf("portal: resolve the install root of %s: %w", o.Home, err)
	}
	a := &Account{
		home: o.Home, root: root, version: o.Version, store: o.Store, cfg: o.Config, secrets: o.Secrets, homes: o.Homes, host: o.Host,
		surface: o.Surface, events: o.Events, notify: o.Notify, openBrowser: o.OpenBrowser, update: o.UpdateConfig,
		recommended: o.RecommendedModel, appTitle: o.AppTitle, opened: map[string]bool{},
		resolveProvider: o.ResolveProvider, localDesktop: o.LocalDesktop, tools: o.Tools, identity: o.Identity, install: o.Install,
	}
	a.setup.done = make(chan struct{})
	o.Store.RegisterRefresher(Provider, &poolRefresher{a: a})
	o.Store.ObserveStateWrites(Provider, a.noteStoreWrite)
	o.Store.RegisterStatus(Provider, a.storeStatus)
	return a, nil
}

func (a *Account) Home() string { return a.home }

func (a *Account) Close() {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	a.StopKeepalive(5 * time.Second)
}

func (a *Account) bind(ctx context.Context) context.Context {
	return home.WithOverride(ctx, a.home)
}

func (a *Account) detached(ctx context.Context) (context.Context, error) {
	ctx = a.bind(ctx)
	if !a.secrets.Multiplexed(ctx) || a.secrets.Bound(ctx) {
		return ctx, nil
	}
	mapping, err := a.secrets.Build(ctx, a.cfg.Env(), a.home)
	if err != nil {
		return ctx, err
	}
	return a.secrets.Bind(ctx, mapping, a.home), nil
}

func (a *Account) homeKey() string {
	key, err := a.homes.Key(a.home)
	if err != nil {
		return filepath.Clean(a.home)
	}
	return key
}

func (a *Account) guestAllowed(ctx context.Context) bool {
	cfg, err := a.cfg.LoadReadonly(a.bind(ctx))
	if err != nil {
		slog.Debug("guest: config unreadable, treating portal.guest as false", "error", err)
		return false
	}
	return cfg.Portal.Guest
}

func (a *Account) timeoutSetting(ctx context.Context) time.Duration {
	raw, ok, err := a.secrets.Lookup(a.bind(ctx), EnvTimeoutSeconds)
	if err != nil || !ok {
		return defaultTimeout
	}
	seconds, ok := parseFloat(raw)
	if !ok {
		return defaultTimeout
	}
	return time.Duration(seconds * float64(time.Second))
}

func platformName() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

func (a *Account) machine() string {
	info, err := a.host.OS()
	if err != nil || info.Machine == "" {
		return "unknown"
	}
	return info.Machine
}

func portalErr(message, code string, relogin bool) *auth.Error {
	return &auth.Error{Message: message, Provider: Provider, Code: code, Relogin: relogin}
}

func authErrorOf(err error) (*auth.Error, bool) {
	var ae *auth.Error
	if errors.As(err, &ae) && ae != nil {
		return ae, true
	}
	return nil, false
}

func retryableOf(ae *auth.Error) bool {
	return ae != nil && ae.Retryable != nil && *ae.Retryable
}

func boolPtr(v bool) *bool { return &v }

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r reply) object() *object {
	return objectOrEmpty(r.body)
}

func exchange(ctx context.Context, client *http.Client, method, endpoint string, header map[string]string, body []byte, timeout time.Duration) (reply, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return reply{}, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if err != nil {
		return reply{}, err
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, header map[string]string, payload *object, timeout time.Duration) (reply, error) {
	merged := map[string]string{"Content-Type": "application/json"}
	for k, v := range header {
		merged[k] = v
	}
	return exchange(ctx, client, http.MethodPost, endpoint, merged, payload.raw(), timeout)
}

func envTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
