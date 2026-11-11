package gateway

import (
	"context"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Identity fields a registered local source must keep to stay authorized
type localSourceIdentity struct {
	platform              Platform
	chatID                string
	userID, profile       string
	hasUserID, hasProfile bool
	chatType              string
}

// Admission source of a local route with the identity it was registered under
type savedLocalSource struct {
	source   *SessionSource
	identity localSourceIdentity
}

// Local platform adapter whose delivery stays in the canonical event stream of its authority
type LocalSessionAdapter struct {
	*BasePlatformAdapter
	Authority *SessionAuthority
	Policies  map[string]*LocalSessionPolicy

	mu      sync.Mutex
	sources map[string]savedLocalSource
}

// Adapter holding the admission sources and frozen policies of one authority's local sessions
func NewLocalSessionAdapter(a *SessionAuthority) *LocalSessionAdapter {
	l := &LocalSessionAdapter{Authority: a, Policies: map[string]*LocalSessionPolicy{}, sources: map[string]savedLocalSource{}}
	config := NewPlatformConfig()
	config.Enabled = true
	l.BasePlatformAdapter = NewBasePlatformAdapter(config, PlatformLocal, l)
	if a != nil && a.Runner != nil {
		l.GatewayRunner = a.Runner
	}
	return l
}

// Key a source is registered under, its own detached route before the chat id every conversation of the chat shares
func localSourceKey(source *SessionSource) string {
	if route := detachedRouteOf(source); route != nil && *route != "" {
		return *route
	}
	return source.ChatID
}

// Platform, chat, user, profile and chat type of a source
func localIdentityOf(source *SessionSource) localSourceIdentity {
	id := localSourceIdentity{platform: source.Platform, chatID: source.ChatID, chatType: source.ChatType}
	if source.UserID != nil {
		id.userID, id.hasUserID = *source.UserID, true
	}
	if source.Profile != nil {
		id.profile, id.hasProfile = *source.Profile, true
	}
	return id
}

// True for the very source object a live session was admitted with, its identity unchanged
func (l *LocalSessionAdapter) authorizeSource(source *SessionSource) bool {
	l.mu.Lock()
	saved, ok := l.sources[localSourceKey(source)]
	l.mu.Unlock()
	if !ok || source != saved.source || localIdentityOf(source) != saved.identity {
		return false
	}
	l.Authority.mu.Lock()
	defer l.Authority.mu.Unlock()
	for _, live := range l.Authority.Sessions {
		if live.Source == source {
			return true
		}
	}
	return false
}

// Records the source a route is admitted with under its current identity
func (l *LocalSessionAdapter) registerSource(source *SessionSource) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sources[localSourceKey(source)] = savedLocalSource{source: source, identity: localIdentityOf(source)}
}

// Source object the route was admitted with when the given one serialises the same, else the given one
func (l *LocalSessionAdapter) keptSource(source *SessionSource) *SessionSource {
	l.mu.Lock()
	saved, ok := l.sources[localSourceKey(source)]
	l.mu.Unlock()
	if ok && sameSourceDict(saved.source, source) {
		return saved.source
	}
	return source
}

// Drops the admission source, frozen policy and post delivery slot of a deleted conversation
func (l *LocalSessionAdapter) forget(source *SessionSource, route string) {
	key := localSourceKey(source)
	l.mu.Lock()
	if saved, ok := l.sources[key]; ok && saved.source == source {
		delete(l.sources, key)
	}
	owned := key == source.ChatID && source.Platform == PlatformLocal
	if owned {
		delete(l.Policies, key)
	}
	l.mu.Unlock()
	if owned {
		base := l.BasePlatformAdapter
		base.mu.Lock()
		delete(base.postDeliveryCallbacks, route)
		base.mu.Unlock()
	}
}

// Frozen policy of a chat, nil when none is held
func (l *LocalSessionAdapter) policy(chatID string) *LocalSessionPolicy {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Policies[chatID]
}

// Holds the frozen policy of a chat
func (l *LocalSessionAdapter) setPolicy(chatID string, policy *LocalSessionPolicy) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Policies[chatID] = policy
}

// Registered source saved under a key, nil when none is
func (l *LocalSessionAdapter) savedSource(key string) *SessionSource {
	l.mu.Lock()
	defer l.mu.Unlock()
	if saved, ok := l.sources[key]; ok {
		return saved.source
	}
	return nil
}

// Always connected, the local transport has no remote endpoint
func (l *LocalSessionAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	return true, nil
}

// Forgets every registered admission source
func (l *LocalSessionAdapter) Disconnect(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.sources)
	return nil
}

// Acknowledges a send with a fresh message id, the turn runner already published the deltas and settlement
func (l *LocalSessionAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (SendResult, error) {
	return SendResult{Success: true, MessageID: uuid4HexString()}, nil
}

// Acknowledges an edit under the same message id
func (l *LocalSessionAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (SendResult, error) {
	return SendResult{Success: true, MessageID: messageID}, nil
}

// Ledgers nothing, viewers replay the durable transcript and no platform acknowledgement can be lost
func (l *LocalSessionAdapter) RecordDeliveryObligation(ctx context.Context, event *MessageEvent, sessionKey, textContent string, deliveryAdapter PlatformAdapter, isEphemeral bool) *string {
	return nil
}

// Adapter that ledgers its own final reply obligation in place of the base ledger
type deliveryObligationRecorder interface {
	RecordDeliveryObligation(ctx context.Context, event *MessageEvent, sessionKey, textContent string, deliveryAdapter PlatformAdapter, isEphemeral bool) *string
}

// Sends no typing indicator
func (l *LocalSessionAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return nil
}

// Chat info of a local chat, always a direct message
func (l *LocalSessionAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID, "type": "dm"}, nil
}

// Acknowledges an approval prompt so the pending controls rendering path is selected without a text fallback
func (l *LocalSessionAdapter) SendExecApproval(ctx context.Context, chatID, command, sessionKey string, description *string, metadata map[string]any, allowPermanent, allowSession, smartDenied bool, requestID *string) (SendResult, error) {
	return SendResult{Success: true, MessageID: uuid4HexString()}, nil
}

// Asks a clarifying question with the base prompt, declining at once when nobody can answer it
func (l *LocalSessionAdapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (SendResult, error) {
	reason, err := l.unanswerableClarify(ctx, chatID)
	if err != nil {
		return SendResult{}, err
	}
	if reason != nil {
		kind := "forbidden"
		return SendResult{Success: false, Error: *reason, ErrorKind: &kind,
			RawResponse: map[string]any{"success": false, "code": EgressDeclineCode, "error": *reason}}, nil
	}
	return l.BasePlatformAdapter.SendClarify(ctx, chatID, question, choices, clarifyID, sessionKey, metadata)
}

// Why no one can answer a question asked in this chat now, nil to ask it
func (l *LocalSessionAdapter) unanswerableClarify(ctx context.Context, chatID string) (*string, error) {
	policy := l.policy(chatID)
	if policy == nil {
		return nil, nil
	}
	decline := func(text string) (*string, error) { return &text, nil }
	if policy.Source == "bot_room" {
		return decline("Clarification is unavailable in hosted rooms")
	}
	if isForwardPolicy(policy) {
		return decline("Clarification is unavailable to a forwarded peer")
	}
	live, ok := l.Authority.live(chatID)
	if !ok {
		return nil, nil
	}
	live.EventStream.Lock.Lock()
	answerable := false
	for _, member := range live.Subscribers {
		if member.Capabilities["session:respond"] {
			answerable = true
			break
		}
	}
	live.EventStream.Lock.Unlock()
	if answerable {
		return nil, nil
	}
	rows, err := l.Authority.DB.ListSessionAdmissions(ctx, chatID, true)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if rowString(row, "status") != AdmissionStarted {
			continue
		}
		payload, has := row.Get("payload")
		if !has {
			return nil, &pyKeyError{key: "payload"}
		}
		if payloadNames(payload, "local_automation_v1") {
			return decline("Clarification is unavailable: no viewer can answer this chat")
		}
		return nil, nil
	}
	return nil, nil
}

// Python membership of a key in an admission payload, a mapping key or a substring of stored text
func payloadNames(payload any, key string) bool {
	switch p := payload.(type) {
	case *pyjson.Object:
		_, ok := p.Get(key)
		return ok
	case map[string]any:
		_, ok := p[key]
		return ok
	case string:
		return strings.Contains(p, key)
	}
	return false
}

// Drops what the local adapter and the authority keyed by a deleted live session
func forgetLiveSession(ctx context.Context, a *SessionAuthority, sessionID string, live *LiveSession) error {
	a.mu.Lock()
	delete(a.launchSources, sessionID)
	a.mu.Unlock()
	if live.Source == nil || live.Source.Platform != PlatformLocal {
		return nil
	}
	adapters, err := localAdapterMap(ctx, a)
	if err != nil {
		return err
	}
	held, _ := adapters.Get(PlatformLocal)
	if local, ok := held.(*LocalSessionAdapter); ok && local != nil {
		local.forget(live.Source, live.Route)
	}
	return nil
}

// Runner resolving the adapter map of the profile a source names
type profileAdaptersRunner interface {
	AdaptersForProfile(profile *string) *pydict.Dict[Platform, PlatformAdapter]
}

// Authorization of a local source, nil for legacy and nonlocal routes while registered local adapters fail closed
func authorizeLocalSource(runner any, source *SessionSource) (*bool, error) {
	if source == nil || source.Platform != PlatformLocal {
		return nil, nil
	}
	verdict := func(v bool) (*bool, error) { return &v, nil }
	owners := allAuthorities(runner)
	profiled, ok := runner.(profileAdaptersRunner)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: "runner has no attribute '_adapters_for_profile'"}
	}
	held, _ := profiled.AdaptersForProfile(source.Profile).Get(PlatformLocal)
	adapter, isLocal := held.(*LocalSessionAdapter)
	if !isLocal || adapter == nil {
		if len(owners) > 0 {
			return verdict(false)
		}
		return nil, nil
	}
	owned := false
	for _, owner := range owners {
		if adapter.Authority == owner {
			owned = true
			break
		}
	}
	if !owned {
		return verdict(false)
	}
	return verdict(adapter.authorizeSource(source))
}

// Every authority the runner serves, its single launch authority when it predates the registry
func allAuthorities(runner any) []*SessionAuthority {
	if registry := registryOf(runner); registry != nil {
		return registry.Authorities()
	}
	if bare, ok := runner.(launchAuthorityRunner); ok {
		if authority := bare.SessionAuthority(); authority != nil {
			return []*SessionAuthority{authority}
		}
	}
	return nil
}

// Authority of the active runtime scope, the single launch authority of a runner without a registry
func activeAuthority(runner any) *SessionAuthority {
	if registry := registryOf(runner); registry != nil {
		return registry.Active()
	}
	if bare, ok := runner.(launchAuthorityRunner); ok {
		return bare.SessionAuthority()
	}
	return nil
}

// Frozen policy of a local source its adapter admits, nil for other routes, refused when none is held
func policyForSource(ctx context.Context, runner AuthorityRunner, source *SessionSource) (*LocalSessionPolicy, error) {
	if source == nil || source.Platform != PlatformLocal {
		return nil, nil
	}
	adapter, err := runner.AdapterForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	local, ok := adapter.(*LocalSessionAdapter)
	if !ok || local == nil || !local.authorizeSource(source) {
		return nil, nil
	}
	policy := local.policy(source.ChatID)
	if policy == nil {
		return nil, runtimeRefusal("storage_unavailable")
	}
	return policy, nil
}
