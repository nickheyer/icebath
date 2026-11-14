package providers

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/toolchain"
)

type Store interface {
	State(ctx context.Context, provider string) (json.RawMessage, bool, error)
	Update(ctx context.Context, provider string, fn func(state json.RawMessage) (json.RawMessage, error)) error
	Pool(ctx context.Context, provider string) (*auth.Pool, error)
	PoolProviders(ctx context.Context) ([]string, error)
	ResolveAPIKey(ctx context.Context, provider string) (auth.APIKeyCredentials, error)
	PKCEAuth(ctx context.Context, c auth.Console, provider string, cfg auth.PKCEConfig, action string, noBrowser bool) (bool, error)
	PKCERefresher(cfg auth.PKCEConfig) auth.Refresher
}

var ErrStoreUnbound = errors.New("providers: the credential store is not open yet")

type StoreRef struct {
	s atomic.Pointer[auth.Store]
}

func (x *StoreRef) Bind(s *auth.Store) { x.s.Store(s) }

func (x *StoreRef) store() (*auth.Store, error) {
	if s := x.s.Load(); s != nil {
		return s, nil
	}
	return nil, ErrStoreUnbound
}

func (x *StoreRef) State(ctx context.Context, provider string) (json.RawMessage, bool, error) {
	s, err := x.store()
	if err != nil {
		return nil, false, err
	}
	return s.State(ctx, provider)
}

func (x *StoreRef) Update(ctx context.Context, provider string, fn func(state json.RawMessage) (json.RawMessage, error)) error {
	s, err := x.store()
	if err != nil {
		return err
	}
	return s.Update(ctx, provider, fn)
}

func (x *StoreRef) Pool(ctx context.Context, provider string) (*auth.Pool, error) {
	s, err := x.store()
	if err != nil {
		return nil, err
	}
	return s.Pool(ctx, provider)
}

func (x *StoreRef) PoolProviders(ctx context.Context) ([]string, error) {
	s, err := x.store()
	if err != nil {
		return nil, err
	}
	return s.PoolProviders(ctx)
}

func (x *StoreRef) ResolveAPIKey(ctx context.Context, provider string) (auth.APIKeyCredentials, error) {
	s, err := x.store()
	if err != nil {
		return auth.APIKeyCredentials{}, err
	}
	return s.ResolveAPIKey(ctx, provider)
}

func (x *StoreRef) PKCEAuth(ctx context.Context, c auth.Console, provider string, cfg auth.PKCEConfig, action string, noBrowser bool) (bool, error) {
	s, err := x.store()
	if err != nil {
		return false, err
	}
	return s.PKCEAuth(ctx, c, provider, cfg, action, noBrowser)
}

func (x *StoreRef) PKCERefresher(cfg auth.PKCEConfig) auth.Refresher {
	return lateRefresher{ref: x, cfg: cfg}
}

type lateRefresher struct {
	ref *StoreRef
	cfg auth.PKCEConfig
}

func (l lateRefresher) Refresh(ctx context.Context, r auth.RefreshRequest) (auth.Entry, error) {
	s, err := l.ref.store()
	if err != nil {
		return auth.Entry{}, err
	}
	return s.PKCERefresher(l.cfg).Refresh(ctx, r)
}

type RegistryRef struct {
	r atomic.Pointer[Registry]
}

func (x *RegistryRef) Get(ctx context.Context, name string) (Profile, bool) {
	if x == nil {
		return nil, false
	}
	if r := x.r.Load(); r != nil {
		return r.Get(ctx, name)
	}
	return nil, false
}

type CatalogFacts interface {
	PortalRecommendedAuxModel(ctx context.Context, vision bool) string
	DeepInfraModelsByTag(ctx context.Context, tag string) ([]wire.Value, bool)
}

type Deps struct {
	Net       Net
	Store     Store
	Secrets   *secret.Scope
	Config    *config.Store
	Homes     *home.Resolver
	Portal    *portal.Account
	Catalog   CatalogFacts
	Registry  *RegistryRef
	HTTP      httpclient.Options
	Host      *hostinfo.Host
	Toolchain *toolchain.Store
}

var errHomesUnbound = errors.New("providers: the home resolver is not set")

func (d Deps) homeDir(ctx context.Context) (string, error) {
	if d.Homes == nil {
		return "", errHomesUnbound
	}
	return d.Homes.Home(ctx)
}

func (d Deps) multiplexed(ctx context.Context) bool {
	return d.Secrets != nil && d.Secrets.Multiplexed(ctx)
}

func (d Deps) routed(ctx context.Context) bool {
	return d.Secrets != nil && d.Secrets.Routed(ctx)
}

type homeKey struct {
	home      string
	multiplex bool
	routed    bool
}

type homeEntry[T any] struct {
	ctx   atomic.Pointer[context.Context]
	value T
}

type homeBound[T any] struct {
	mu      sync.Mutex
	entries map[homeKey]*homeEntry[T]
}

func (b *homeBound[T]) get(ctx context.Context, d Deps, build func(env func(name string) string, key homeKey) (T, error)) (T, error) {
	var zero T
	dir, err := d.homeDir(ctx)
	if err != nil {
		return zero, err
	}
	key := homeKey{home: dir, multiplex: d.multiplexed(ctx), routed: d.routed(ctx)}
	detached := context.WithoutCancel(ctx)
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.entries[key]; ok {
		e.ctx.Store(&detached)
		return e.value, nil
	}
	e := &homeEntry[T]{}
	e.ctx.Store(&detached)
	value, err := build(func(name string) string { return d.secret(*e.ctx.Load(), name) }, key)
	if err != nil {
		return zero, err
	}
	e.value = value
	if b.entries == nil {
		b.entries = map[homeKey]*homeEntry[T]{}
	}
	b.entries[key] = e
	return value, nil
}

func (d Deps) secret(ctx context.Context, name string) string {
	if d.Secrets == nil {
		return ""
	}
	v, _, err := d.Secrets.Lookup(ctx, name)
	if err != nil {
		return ""
	}
	return v
}

func (d Deps) envPreferFile(ctx context.Context, name string) string {
	if d.Config == nil {
		return ""
	}
	v, _, err := d.Config.Env().GetPreferFile(ctx, name)
	if err != nil {
		return ""
	}
	return v
}

func Builtin(d Deps) []Profile {
	return []Profile{
		newActual(d), newAIGateway(d),
		newDashScope(d, alibabaSpec()), newDashScope(d, alibabaCNSpec()), newDashScope(d, alibabaTokenPlanSpec()), newDashScope(d, alibabaTokenPlanCNSpec()),
		newDashScope(d, alibabaCodingPlanSpec()), newDashScope(d, alibabaCodingPlanCNSpec()),
		newAnthropic(d), newArcee(), newAzureFoundry(d), newBedrock(d),
		newCommandCode(d, false), newCommandCode(d, true), newCustom(d), newDeepInfra(d), newDeepSeek(),
		newFireworks(d), newGemini(), newGMI(d), newHuggingFace(), newKilocode(),
		newKimi(d, "kimi-coding"), newKimi(d, "kimi-coding-cn"), newLMStudio(), newMetaAI(d),
		newMiniMax(minimaxSpec()), newMiniMax(minimaxCNSpec()), newMiniMax(minimaxOAuthSpec()),
		newNebius(), newPortal(d), newNovita(), newNvidia(), newOllamaCloud(), newOpenAICodex(),
		newOpenCodeZen(d), newOpenCodeGo(d), newOpenRouter(d), newQwen(), newRouter(d),
		newSolstice(d), newStepFun(), newTokenHub(), newUpstage(), newVertex(d), newXAI(d), newXiaomi(), newZai(d),
	}
}

type ctxKey uint8

const (
	conversationKey ctxKey = iota
	affinityKey
	cronKey
)

type cronExecution struct {
	sessionID string
	jobID     string
}

func WithConversation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, conversationKey, id)
}

func Conversation(ctx context.Context) string {
	v, _ := ctx.Value(conversationKey).(string)
	return v
}

func WithAffinityScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, affinityKey, scope)
}

func AffinityScope(ctx context.Context) string {
	v, _ := ctx.Value(affinityKey).(string)
	return v
}

func WithCronExecution(ctx context.Context, sessionID, jobID string) context.Context {
	return context.WithValue(ctx, cronKey, cronExecution{sessionID: sessionID, jobID: jobID})
}

var cronSessionID = regexp.MustCompile(`^(cron_.+)_\d{8}_\d{6}$`)

func CacheScopeFromSession(ctx context.Context, sessionID string) string {
	if m := cronSessionID.FindStringSubmatch(sessionID); m != nil {
		return m[1]
	}
	if exec, ok := ctx.Value(cronKey).(cronExecution); ok && exec.sessionID == sessionID {
		if job := strings.TrimSpace(exec.jobID); job != "" {
			return "cron_" + job
		}
	}
	return sessionID
}

func AffinityKey(ctx context.Context, sessionID string) string {
	scope := AffinityScope(ctx)
	if scope == "" {
		scope = Conversation(ctx)
	}
	if scope == "" {
		scope = sessionID
	}
	return CacheScopeFromSession(ctx, scope)
}

func reasoningObject(r *wire.Reasoning) json.RawMessage {
	if len(r.Payload) > 0 {
		if v, err := wire.ParseValue(r.Payload); err == nil && v.IsObject() {
			return r.Payload
		}
	}
	d := (&wire.Dict{}).SetBool("enabled", r.Enabled)
	if r.Enabled && r.Effort != "" {
		d.SetString("effort", r.Effort)
	}
	return d.JSON()
}

func reasoningEffortRaw(r *wire.Reasoning) string {
	if r == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(r.Effort))
}

func topLevel(key string, value json.RawMessage) Extras {
	return Extras{TopLevel: map[string]json.RawMessage{key: value}}
}

func effortTop(effort string) Extras {
	return topLevel("reasoning_effort", wire.Quote(effort))
}

func flatModelName(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return strings.ToLower(m)
}

func userAgent(d Deps, product string) string {
	if d.Net.Version == "" {
		return product
	}
	return product + "/" + d.Net.Version
}
