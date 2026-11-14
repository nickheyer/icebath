package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/tool"
)

type TransportDecision struct {
	RequestID     string
	RequestDigest string
	Choice        string
}

type Transport interface {
	Name() string
	Present(ctx context.Context, p tool.ApprovalPrompt, digest string) (TransportDecision, bool, error)
}

const maxTransportWorkers = 8

type transportAttempt struct {
	selected bool
	name     string
	choice   string
	failure  string
	fallback string
}

type transportReply struct {
	decision TransportDecision
	ok       bool
	err      error
	at       time.Time
}

func (g *Gate) invokeTransport(ctx context.Context, c *tool.Call, t Transport, p tool.ApprovalPrompt, digest string, allowed []string) (string, string) {
	select {
	case g.transportSlots <- struct{}{}:
	default:
		slog.WarnContext(ctx, "Approval transport worker capacity exhausted")
		return "deny", "busy"
	}
	deadline := time.Now().Add(max(p.Timeout, 0))
	replies := make(chan transportReply, 1)
	workerCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	go func() {
		defer func() { <-g.transportSlots }()
		defer cancel()
		decision, ok, err := t.Present(workerCtx, p, digest)
		replies <- transportReply{decision, ok, err, time.Now()}
	}()
	hb := newHeartbeat(c, "waiting for plugin approval transport")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			slog.InfoContext(ctx, "Approval transport wait interrupted", "request", p.ID)
			return "deny", "interrupted"
		}
		if !time.Now().Before(deadline) {
			slog.WarnContext(ctx, "Approval transport timed out", "request", p.ID)
			return "deny", "timeout"
		}
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "Approval transport wait interrupted", "request", p.ID)
			return "deny", "interrupted"
		case <-timer.C:
			slog.WarnContext(ctx, "Approval transport timed out", "request", p.ID)
			return "deny", "timeout"
		case <-ticker.C:
			hb.beat()
		case r := <-replies:
			switch {
			case r.at.After(deadline):
				slog.WarnContext(ctx, "Approval transport timed out", "request", p.ID)
				return "deny", "timeout"
			case r.err != nil:
				slog.WarnContext(ctx, "Approval transport failed", "request", p.ID)
				return "deny", "error"
			case !r.ok:
				slog.WarnContext(ctx, "Approval transport returned an invalid decision type")
				return "deny", "invalid"
			case r.decision.RequestID != p.ID || r.decision.RequestDigest != digest:
				slog.WarnContext(ctx, "Approval transport returned a stale or mismatched decision")
				return "deny", "stale"
			case !contains(allowed, r.decision.Choice):
				slog.WarnContext(ctx, "Approval transport returned a disallowed choice")
				return "deny", "invalid"
			}
			return r.decision.Choice, ""
		}
	}
}

func (g *Gate) presentWithTransport(ctx context.Context, c *tool.Call, in decisionInput, allowSession, allowPermanent bool, surface string) transportAttempt {
	name, fallback := transportSelection(ctx, g.o.Config)
	if name == "builtin" {
		return transportAttempt{}
	}
	var chosen Transport
	for _, t := range g.o.Transports() {
		if t.Name() == name {
			chosen = t
			break
		}
	}
	if chosen == nil {
		slog.WarnContext(ctx, "Selected approval transport is unavailable", "transport", name)
		return transportAttempt{selected: true, name: name, choice: "deny", failure: "unavailable", fallback: fallback}
	}
	allowed := []string{"once"}
	if allowSession {
		allowed = append(allowed, "session")
	}
	if allowPermanent {
		allowed = append(allowed, "always")
	}
	allowed = append(allowed, "deny")
	timeout := approvalTimeout(ctx, g.o.Config)
	prompt := tool.ApprovalPrompt{
		ID:             newRequestID(),
		SessionKey:     in.sessionKey,
		Command:        g.o.Redactor.Approval(ctx, in.command, true, false),
		Description:    g.o.Redactor.Approval(ctx, in.description, true, false),
		PatternKey:     in.key,
		PatternKeys:    append([]string(nil), in.keys...),
		AllowSession:   allowSession,
		AllowPermanent: allowPermanent,
		Timeout:        time.Duration(timeout) * time.Second,
		Surface:        "transport:" + name,
	}
	transportPrompt := prompt
	transportPrompt.Surface = surface
	digest, err := RequestDigest(transportPrompt)
	if err != nil {
		slog.WarnContext(ctx, "Could not build redacted plugin approval request")
		return transportAttempt{selected: true, name: name, choice: "deny", failure: "error"}
	}
	hook := approvalHook(c, prompt, "")
	hook.RequestID = prompt.ID
	hook.RequestDigest = digest
	g.o.Hooks.Approval(ctx, "pre_approval_request", hook)
	outcome := ""
	choice, failure := "", ""
	g.humanInput(ctx, c, "approval", prompt.Command, in.sessionKey, func() string {
		done := g.humanWait(ctx, in.sessionKey)
		defer done()
		choice, failure = g.invokeTransport(ctx, c, chosen, transportPrompt, digest, allowed)
		if failure == "" {
			outcome = choice
		} else {
			outcome = "transport_" + failure
		}
		return outcome
	})
	hook.Choice = outcome
	g.o.Hooks.Approval(ctx, "post_approval_response", hook)
	return transportAttempt{selected: true, name: name, choice: choice, failure: failure, fallback: fallback}
}

func RequestDigest(p tool.ApprovalPrompt) (string, error) {
	choices := []string{"once"}
	if p.AllowSession {
		choices = append(choices, "session")
	}
	if p.AllowPermanent {
		choices = append(choices, "always")
	}
	choices = append(choices, "deny")
	keys := p.PatternKeys
	if keys == nil {
		keys = []string{}
	}
	canonical, err := fileutil.EncodeJSON(map[string]any{
		"schema_version":  1,
		"request_id":      p.ID,
		"command":         p.Command,
		"description":     p.Description,
		"pattern_key":     p.PatternKey,
		"pattern_keys":    keys,
		"surface":         p.Surface,
		"timeout_seconds": int64(p.Timeout / time.Second),
		"allowed_choices": choices,
		"session_key":     p.SessionKey,
	}, fileutil.JSONStyle{Tight: true, Sorted: true, ASCII: true})
	if err != nil {
		return "", fmt.Errorf("approval request digest: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func (g *Gate) transportChoice(ctx context.Context, c *tool.Call, a transportAttempt, key, description string) (string, *tool.Decision) {
	if !a.selected {
		return "", nil
	}
	if a.failure == "" {
		return a.choice, nil
	}
	if a.fallback == "builtin" {
		slog.WarnContext(ctx, "Approval transport failed; using explicit builtin fallback", "transport", a.name, "failure", a.failure)
		return "", nil
	}
	breaker := g.breakerAddendum(ctx, sessionKey(c))
	d := g.denied(ctx, "BLOCKED: Selected approval transport failed ("+a.failure+"); the user has NOT consented to this action. "+
		"Do NOT retry this command or attempt the same outcome through another route."+breaker,
		key, description, "transport_"+a.failure, "command", false, "")
	return "", &d
}
