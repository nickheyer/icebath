package gateway

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Prefill file resolver and loader of the CLI config loader, installed by the process wiring
var (
	ResolvePrefillMessagesFile func(ctx context.Context, cfg map[string]any) string
	LoadPrefillMessages        func(ctx context.Context, filePath, baseDir string) []any
)

// One creation attempt's prefill snapshot, owned by identity even when two attempts read the same contents
type prefillBox struct {
	messages []any
}

// Prefill examples of a session's config, read from its own profile home under its client environment
func readPrefill(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy) ([]any, error) {
	if ResolvePrefillMessagesFile == nil || LoadPrefillMessages == nil {
		panic("gateway.ResolvePrefillMessagesFile unassigned: the process wiring installs the CLI config prefill loaders")
	}
	base := a.ProfileID
	if !filepath.IsAbs(base) {
		base = home.Dir(ctx)
	}
	scoped, err := policy.ClientEnvScope(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := policy.config(scoped, a, false)
	if err != nil {
		return nil, err
	}
	messages := LoadPrefillMessages(ctx, ResolvePrefillMessagesFile(scoped, cfg), base)
	if len(messages) == 0 {
		return nil, nil
	}
	return messages, nil
}

// Prefill snapshot of one creation attempt, published only by the commit that creates its session
type prefillPublication struct {
	ctx       context.Context
	authority *SessionAuthority
	policy    *LocalSessionPolicy
	sessionID string
	box       *prefillBox
}

// Reads this attempt's snapshot before its commit transaction
func (p *prefillPublication) Read() error {
	messages, err := readPrefill(p.ctx, p.authority, p.policy)
	if err != nil {
		return err
	}
	p.box = &prefillBox{messages: messages}
	return nil
}

// Holds this attempt's snapshot as the created session's prefill
func (p *prefillPublication) Publish() error {
	p.authority.mu.Lock()
	defer p.authority.mu.Unlock()
	if p.authority.localPrefill == nil {
		p.authority.localPrefill = map[string]*prefillBox{}
	}
	p.authority.localPrefill[p.sessionID] = p.box
	return nil
}

// Drops the held snapshot when it is still this attempt's own
func (p *prefillPublication) Withdraw() {
	p.authority.mu.Lock()
	defer p.authority.mu.Unlock()
	if p.authority.localPrefill[p.sessionID] == p.box {
		delete(p.authority.localPrefill, p.sessionID)
	}
}

// Stages a new session's receipt with the prefill snapshot its own commit reads and publishes
func stageSessionPrefill(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy, receipt *pyjson.Object) (*state.LocalCreateReceipt, error) {
	staged := &state.LocalCreateReceipt{Receipt: receipt}
	if !policy.IgnoreUserConfig {
		sid, err := receiptField(receipt, "session_id")
		if err != nil {
			return nil, err
		}
		staged.OnCreate = &prefillPublication{ctx: context.WithoutCancel(ctx), authority: a, policy: policy, sessionID: pyStrOf(sid)}
	}
	return staged, nil
}

// Prefill examples of a session, the snapshot its creation took or one read on first use and kept
func sessionPrefill(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy, sessionID string) ([]any, error) {
	if policy.IgnoreUserConfig {
		return nil, nil
	}
	a.mu.Lock()
	box, held := a.localPrefill[sessionID]
	a.mu.Unlock()
	if held {
		return box.messages, nil
	}
	messages, err := readPrefill(ctx, a, policy)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.localPrefill == nil {
		a.localPrefill = map[string]*prefillBox{}
	}
	if kept, raced := a.localPrefill[sessionID]; raced {
		return kept.messages, nil
	}
	a.localPrefill[sessionID] = &prefillBox{messages: messages}
	return messages, nil
}

// Drops the held prefill snapshots of deleted sessions
func releaseSessionPrefill(a *SessionAuthority, sessionIDs []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, sid := range sessionIDs {
		delete(a.localPrefill, sid)
	}
}
