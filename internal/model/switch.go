package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type SwitchTarget struct {
	Model      string
	Provider   string
	Credential auth.Credential
	BaseURL    string
	Mode       wire.Mode
}

type Switched struct {
	Previous *RouteState
	Model    string
	Provider string
	BaseURL  string
	Mode     wire.Mode
}

type SwitchError struct {
	Provider string
	Previous string
}

func (e *SwitchError) Error() string {
	return fmt.Sprintf("switch_model: no base_url resolved for provider '%s' (switching from '%s'); refusing to keep the previous provider's endpoint",
		e.Provider, e.Previous)
}

func (s *Session) Switch(ctx context.Context, t SwitchTarget) (*Switched, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	old := s.Route()
	oldNorm := strings.ToLower(strings.TrimSpace(old.Provider))
	newNorm := strings.ToLower(strings.TrimSpace(t.Provider))
	mode, base := s.switchDestination(ctx, t, old, oldNorm, newNorm)
	s.mu.Lock()
	previous := s.captureLocked(ctx, true, true)
	s.mu.Unlock()
	rt, err := s.switchedRoute(ctx, t, old, mode, base, oldNorm, newNorm)
	if err != nil {
		return nil, err
	}
	cfg, err := s.liveConfig(ctx)
	if err != nil {
		return nil, err
	}
	echo := cfg.Model.ReasoningEcho != nil && *cfg.Model.ReasoningEcho
	s.mu.Lock()
	s.route = rt
	s.echoOptIn = echo
	s.o.NoEncryptedReplay = false
	s.mu.Unlock()
	s.syncEntryID()
	reasoning := ResolveReasoningConfig(cfg, rt.Model)
	s.mu.Lock()
	s.o.Reasoning = reasoning
	s.mu.Unlock()
	slog.Info(fmt.Sprintf("switch_model: reasoning_config resolved for %s: %s", rt.Model, reasoningText(reasoning)))
	s.exec.resetStreak()
	s.mu.Lock()
	s.rec.primary = s.captureLocked(ctx, true, false)
	s.mu.Unlock()
	s.finishSwitch(ctx, t.Provider, oldNorm, newNorm)
	slog.Info(fmt.Sprintf("Model switched in-place: %s (%s) -> %s (%s)", old.Model, old.Provider, t.Model, t.Provider))
	s.persistBillingRoute(ctx)
	return &Switched{Previous: previous, Model: rt.Model, Provider: rt.Provider, BaseURL: rt.BaseURL, Mode: rt.Mode}, nil
}

func (s *Session) switchDestination(ctx context.Context, t SwitchTarget, old *Route, oldNorm, newNorm string) (wire.Mode, string) {
	mode, base := t.Mode, t.BaseURL
	if mode == "" {
		mode = s.r.c.o.Catalog.DetermineMode(ctx, t.Provider, base, t.Model)
	}
	if base == "" && newNorm == "openai" {
		base = "https://api.openai.com/v1"
	}
	effective := base
	if effective == "" && oldNorm == newNorm {
		effective = old.BaseURL
	}
	if s.r.c.o.Providers.IsActualRoute(ctx, t.Provider, effective) {
		mode = wire.ModeChat
		if effective != "" {
			base = auth.NormalizeActualBaseURL(effective)
		}
	}
	if _, family := s.r.c.o.Providers.OpenCodeFamily(ctx, t.Provider); mode == wire.ModeAnthropic && family && base != "" {
		base = trailingV1.ReplaceAllString(base, "")
	}
	return mode, base
}

func (s *Session) switchedRoute(ctx context.Context, t SwitchTarget, old *Route, mode wire.Mode, base, oldNorm, newNorm string) (*Route, error) {
	rt := cloneRoute(old)
	rt.Model, rt.Provider, rt.Requested = t.Model, t.Provider, t.Provider
	if base != "" {
		rt.BaseURL = base
	} else if oldNorm != newNorm {
		return nil, &SwitchError{Provider: t.Provider, Previous: old.Provider}
	}
	rt.Mode = mode
	if keyPresent(t.Credential) {
		rt.Credential = t.Credential
	}
	if oldNorm != newNorm || rt.Pool == nil {
		rt.Pool = nil
		s.rec.entryID = ""
		pool, err := s.r.c.o.Auth.Pool(ctx, t.Provider)
		if err != nil {
			slog.Warn(fmt.Sprintf("switch_model: credential pool reload failed for %s (%s); continuing without pool rotation this turn", t.Provider, err))
		} else {
			rt.Pool = pool
		}
	}
	switch {
	case newNorm == "moa":
		bound, err := s.r.Resolve(ctx, Target{Provider: "moa", Model: t.Model})
		if err != nil {
			return nil, err
		}
		rt.Mode, rt.BaseURL, rt.Credential = wire.ModeChat, bound.BaseURL, bound.Credential
		if keyPresent(t.Credential) {
			rt.Credential = t.Credential
		}
	case t.Provider == "bedrock" && (mode == wire.ModeAnthropic || mode == wire.ModeBedrock):
		bound, err := s.r.Resolve(ctx, Target{Provider: "bedrock", Model: t.Model, BaseURL: firstOf(base, old.BaseURL)})
		if err != nil {
			return nil, err
		}
		rt.Mode, rt.BaseURL, rt.Credential, rt.Region, rt.Guardrail = bound.Mode, bound.BaseURL, bound.Credential, bound.Region, bound.Guardrail
		rt.BedrockOpenAI, rt.BedrockAnthropic, rt.Source = bound.BedrockOpenAI, bound.BedrockAnthropic, bound.Source
	case mode == wire.ModeAnthropic:
		cred := t.Credential
		if !keyPresent(cred) {
			cred = old.Credential
		}
		if !keyPresent(cred) && t.Provider == "anthropic" {
			if tok, ok := s.r.c.o.Auth.ResolveAnthropicToken(ctx, t.Model); ok {
				cred = staticKey(tok.Reveal(), "")
			}
		}
		if t.Provider == "minimax-oauth" && keyText(cred) != "" {
			cred = tokenKey(s.r.c.o.Auth.MiniMaxTokenSource(), cred.Source)
		}
		rt.Credential = cred
		rt.BaseURL = firstOf(base, old.BaseURL)
	default:
		if !keyPresent(t.Credential) {
			rt.Credential = old.Credential
		}
		rt.BaseURL = firstOf(base, old.BaseURL)
	}
	return rt, nil
}

func (s *Session) finishSwitch(ctx context.Context, provider, oldNorm, newNorm string) {
	s.rec.activated = false
	s.rec.providerActive, s.rec.hasFBRoute, s.rec.fbModel, s.rec.fbProvider = false, false, "", ""
	s.rec.index = 0
	s.rec.revertID = ""
	chain := slices.Clone(s.rec.chain)
	if oldNorm != "" && newNorm != "" && oldNorm != newNorm {
		chain = slices.DeleteFunc(chain, func(e config.FallbackEntry) bool {
			p := strings.ToLower(strings.TrimSpace(e.Provider))
			return p == oldNorm || p == newNorm
		})
	}
	s.rec.chain = chain
	custom, err := s.customEndpoints(ctx)
	if err != nil {
		slog.Debug("switch_model: request_overrides re-derivation failed", "error", err)
		return
	}
	rt := s.Route()
	eb := customProviderExtraBody(provider, rt.Model, rt.BaseURL, custom)
	s.mu.Lock()
	defer s.mu.Unlock()
	overrides := cloneOverrides(s.o.Overrides)
	delete(overrides, "extra_body")
	if len(eb) > 0 {
		overrides["extra_body"] = objectJSON(eb)
	}
	s.o.Overrides = overrides
}

func (s *Session) persistBillingRoute(ctx context.Context) {
	s.mu.Lock()
	store, sid, rt := s.o.Store, s.sessionID, s.route
	s.mu.Unlock()
	if store == nil || sid == "" {
		return
	}
	if err := store.UpdateBillingRoute(ctx, sid, rt.Provider, rt.BaseURL, rt.Mode); err != nil {
		slog.Warn("Failed to persist billing route after model switch", "error", err)
	}
}

func (s *Session) EnsureLMStudioLoaded(ctx context.Context, contextLength *int) (local.LMStudioLoadResult, bool, error) {
	ctx = s.r.bind(ctx)
	return s.ensureLMStudioLoaded(ctx, s.Route(), contextLength)
}

func EffectiveLMStudioContext(configured int, load local.LMStudioLoadResult) (int, bool) {
	if load.Rejected || (load.LoadAttempted && load.ContextLength <= 0) {
		return 0, false
	}
	switch {
	case load.ContextLength > 0 && configured > 0:
		return min(load.ContextLength, configured), true
	case load.ContextLength > 0:
		return load.ContextLength, true
	case configured > 0:
		return configured, true
	}
	return 0, false
}

func LMStudioLoadUnverified(load local.LMStudioLoadResult) bool {
	return load.Rejected || (load.LoadAttempted && load.ContextLength <= 0)
}

var errNoSwitchState = errors.New("model: no route state captured before the switch")

func (s *Session) RollbackSwitch(ctx context.Context, sw *Switched) error {
	if sw == nil || sw.Previous == nil {
		return errNoSwitchState
	}
	return s.Reinstall(ctx, sw.Previous)
}
