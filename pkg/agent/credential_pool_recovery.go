package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

var statusToFailoverReason = map[int]provider.FailoverReason{
	402: provider.ReasonBilling, 429: provider.ReasonRateLimit, 401: provider.ReasonAuth, 403: provider.ReasonAuth,
}

var (
	usageLimitReasonTokens  = []string{"usage_limit_reached", "gousagelimit"}
	usageLimitMessageTokens = []string{"usage limit reached", "usage limit has been reached"}
)

// Carries a credential swap error out of pool recovery the way the Python exception unwinds it
type swapCredentialFailure struct{ err error }

// Credential adoption pool recovery performs, replaced by tests that record the adopted entry
var swapPoolCredential = func(ctx context.Context, a *AIAgent, entry provider.PooledCredential) (bool, error) {
	return a.swapCredential(ctx, entry)
}

// Swaps the entry in, raising a swap error through the recovery that asked for it
func (a *AIAgent) swapCredentialOrRaise(ctx context.Context, entry provider.PooledCredential) bool {
	swapped, err := swapPoolCredential(ctx, a, entry)
	if err != nil {
		panic(swapCredentialFailure{err: err})
	}
	return swapped
}

// The agent as the auth recovery of the provider pool sees it
type poolAuthAgent struct {
	a   *AIAgent
	ctx context.Context
}

func (p poolAuthAgent) Provider() string             { return p.a.provider }
func (p poolAuthAgent) APIMode() provider.APIMode    { return provider.APIMode(p.a.apiMode) }
func (p poolAuthAgent) Interrupted() bool            { return p.a.Interrupted() }
func (p poolAuthAgent) TouchActivity(message string) { p.a.TouchActivity(message) }
func (p poolAuthAgent) Print(message string)         { p.a.vprint(p.a.logPrefix+message, true, true) }
func (p poolAuthAgent) SwapCredential(entry provider.PooledCredential) bool {
	return p.a.swapCredentialOrRaise(p.ctx, entry)
}

// Per turn auth episodes keyed by provider and entry, created on first use
func (p poolAuthAgent) AuthEpisodes() map[string]*provider.AuthEpisode {
	if p.a.authPoolRefreshCounts == nil {
		p.a.authPoolRefreshCounts = map[string]*provider.AuthEpisode{}
	}
	return p.a.authPoolRefreshCounts
}

// Key hint and entry id of the dispatched credential, the pool cursor only when the agent has no key
func failedCredentialIdentity(a *AIAgent, pool *provider.Pool) (string, string) {
	apiKeyHint, credentialID := a.apiKey, a.credentialPoolEntryID
	if apiKeyHint == "" {
		if cur, ok := pool.Current(); ok {
			apiKeyHint = cur.RuntimeAPIKey()
			if credentialID == "" && cur.ID != "" {
				credentialID = cur.ID
			}
		}
	}
	return apiKeyHint, credentialID
}

// Rotates at once for an exhausted entry or a usage limit, otherwise retries the same credential once
func recoverRateLimit(pool *provider.Pool, hasRetried429 bool, errorContext map[string]any, apiKeyHint, credentialID string, rotateAndSwap func(int, string) bool) (bool, bool) {
	var current *provider.PooledCredential
	entries := pool.Entries()
	if credentialID != "" {
		for i := range entries {
			if entries[i].ID == credentialID {
				current = &entries[i]
				break
			}
		}
	}
	if apiKeyHint != "" && current == nil {
		for i := range entries {
			if entries[i].RuntimeAPIKey() == apiKeyHint {
				current = &entries[i]
				break
			}
		}
	}
	if current == nil {
		if cur, ok := pool.Current(); ok {
			current = &cur
		}
	}
	if current != nil && current.LastStatus == provider.StatusExhausted {
		runAgentLog().Info(fmt.Sprintf("Credential already exhausted (last_status=%s) — rotating immediately instead of retrying", current.LastStatus))
		if rotateAndSwap(429, "rate limit, pre-exhausted") {
			return true, false
		}
		return false, true
	}
	usageLimitReached := false
	if len(errorContext) > 0 {
		reason := pystr.Lower(contextString(errorContext["reason"]))
		message := pystr.Lower(contextString(errorContext["message"]))
		for _, t := range usageLimitReasonTokens {
			usageLimitReached = usageLimitReached || strings.Contains(reason, t)
		}
		for _, t := range usageLimitMessageTokens {
			usageLimitReached = usageLimitReached || strings.Contains(message, t)
		}
	}
	if !hasRetried429 && !usageLimitReached {
		return false, true
	}
	if rotateAndSwap(429, "rate limit") {
		return true, false
	}
	return false, true
}

// Python str of an error context value with None and empty values read as empty
func contextString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Recovers through pool rotation, returning recovered and the 429 retry flag, billing rotating at once and auth refreshing first
func (a *AIAgent) recoverWithCredentialPool(ctx context.Context, statusCode *int, hasRetried429 bool, classifiedReason provider.FailoverReason, errorContext map[string]any, billingUnverified bool) (recovered, retried429 bool, err error) {
	pool := a.credentialPool
	if pool == nil {
		return false, hasRetried429, nil
	}
	currentProvider := pystr.Lower(pystr.Strip(a.provider))
	poolProvider := pystr.Lower(pystr.Strip(pool.Provider()))
	if poolProvider != "" && !provider.CredentialPoolMatchesProvider(ctx, pool.Provider(), currentProvider, a.baseURL) {
		runAgentLog().Warn(fmt.Sprintf("Credential pool provider mismatch: pool=%s, agent=%s — skipping pool mutation to avoid cross-provider contamination", poolProvider, currentProvider))
		return false, hasRetried429, nil
	}
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(swapCredentialFailure)
			if !ok {
				panic(r)
			}
			recovered, retried429, err = false, hasRetried429, f.err
		}
	}()
	apiKeyHint, credentialID := failedCredentialIdentity(a, pool)
	effectiveReason := classifiedReason
	if effectiveReason == "" && statusCode != nil {
		effectiveReason = statusToFailoverReason[*statusCode]
	}

	rotateAndSwap := func(defaultStatus int, label string) bool {
		rotateStatus := defaultStatus
		if statusCode != nil {
			rotateStatus = *statusCode
		}
		req := provider.RotationRequest{StatusCode: rotateStatus, ErrorContext: errorContext, APIKeyHint: apiKeyHint, CredentialID: credentialID}
		if effectiveReason != "" {
			req.FailureReason = provider.PoolFailureReason(effectiveReason, errorContext, billingUnverified)
		}
		if pystr.Strip(a.model) != "" {
			req.Model = a.model
		}
		next, ok := pool.MarkExhaustedAndRotate(req)
		if !ok {
			return false
		}
		nextID := next.ID
		if nextID == "" {
			nextID = "?"
		}
		if !provider.CredentialPoolEntryServesEndpoint(next, a.baseURL) {
			runAgentLog().Info(fmt.Sprintf("Credential %d (%s) — pool entry %s serves another endpoint; not swapping", rotateStatus, label, nextID))
			return false
		}
		runAgentLog().Info(fmt.Sprintf("Credential %d (%s) — rotated to pool entry %s", rotateStatus, label, nextID))
		swapped := a.swapCredentialOrRaise(ctx, next)
		if swapped && credentialID != "" && a.credentialPoolRevertID == "" &&
			(effectiveReason == provider.ReasonRateLimit || effectiveReason == provider.ReasonBilling) {
			for _, benched := range pool.Entries() {
				if benched.ID == credentialID {
					if benched.Priority < next.Priority {
						a.credentialPoolRevertID = credentialID
					}
					break
				}
			}
		}
		return swapped
	}

	switch effectiveReason {
	case provider.ReasonUpstreamRateLimit:
		if upstream := contextString(errorContext["upstream_provider"]); errorContext != nil && errorContext["upstream_provider"] != nil && upstream != "" {
			runAgentLog().Info(fmt.Sprintf("Upstream provider %s rate-limited via aggregator — skipping credential rotation, deferring to fallback chain", upstream))
		} else {
			runAgentLog().Info("Upstream aggregator 429 (provider unknown) — skipping credential rotation, deferring to fallback chain")
		}
		return false, hasRetried429, nil
	case provider.ReasonBilling:
		if rotateAndSwap(402, "billing") {
			return true, false, nil
		}
		return false, hasRetried429, nil
	case provider.ReasonRateLimit:
		rec, flag := recoverRateLimit(pool, hasRetried429, errorContext, apiKeyHint, credentialID, rotateAndSwap)
		return rec, flag, nil
	case provider.ReasonModelEntitlement:
		return rotateAndSwap(400, "model entitlement"), hasRetried429, nil
	case provider.ReasonAuth:
		status := 0
		if statusCode != nil {
			status = *statusCode
		}
		rec, flag := provider.RecoverAuthFailure(poolAuthAgent{a: a, ctx: ctx}, pool, provider.AuthRecoveryRequest{
			StatusCode: status, HasRetried429: hasRetried429, ErrorContext: errorContext,
			APIKeyHint: apiKeyHint, CredentialID: credentialID, RotateAndSwap: rotateAndSwap,
		})
		return rec, flag, nil
	}
	return false, hasRetried429, nil
}
