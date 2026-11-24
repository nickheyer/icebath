package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// AuxiliaryClientUnavailable reports that no auxiliary client could be built for the task
type AuxiliaryClientUnavailable struct {
	Message string
	// No provider is configured at all, as opposed to one lacking credentials
	NoProvider bool
}

func (e *AuxiliaryClientUnavailable) Error() string { return e.Message }

// ProviderCredentialsExhaustedError reports an explicit provider whose credential pool is fully exhausted
type ProviderCredentialsExhaustedError struct {
	Message  string
	Provider string
}

func (e *ProviderCredentialsExhaustedError) Error() string { return e.Message }

// ProviderNotConfiguredError reports that agent init found no usable inference provider
type ProviderNotConfiguredError struct{ Message string }

func (e *ProviderNotConfiguredError) Error() string { return e.Message }

var (
	auxUnavailableClock = time.Now
)

// Persisted pool rows of a provider, empty when the store cannot be read
func auxPersistedPoolEntries(env PoolEnv, providerID string) []PooledCredential {
	rows, err := env.Store.ReadCredentialPool(providerID)
	if err != nil {
		return nil
	}
	out := make([]PooledCredential, 0, len(rows))
	for _, row := range rows {
		if row != nil {
			out = append(out, PooledCredentialFromMap(providerID, row))
		}
	}
	return out
}

func auxLiveEntries(entries []PooledCredential) []PooledCredential {
	var live []PooledCredential
	for _, e := range entries {
		if e.LastStatus != StatusDead {
			live = append(live, e)
		}
	}
	return live
}

func auxWhichCredentials(n int) string {
	if n == 1 {
		return "its only credential is"
	}
	return fmt.Sprintf("all %d credentials are", n)
}

// PoolCooldownMessage names a fully benched pool and its next reset, empty when a credential is usable
func PoolCooldownMessage(ctx context.Context, providerID string) (string, bool) {
	live := auxLiveEntries(auxPersistedPoolEntries(modelsPoolEnv(ctx), providerID))
	if len(live) == 0 {
		return "", false
	}
	now := float64(auxUnavailableClock().UnixNano()) / 1e9
	earliest, resets := 0.0, 0
	for _, e := range live {
		until, ok := exhaustedUntil(e, len(live) == 1)
		if ok && until > now {
			if resets == 0 || until < earliest {
				earliest = until
			}
			resets++
		}
	}
	if resets != len(live) {
		return "", false
	}
	sec := int64(earliest)
	when := time.Unix(sec, int64((earliest-float64(sec))*1e9)).Local().Format("2006-01-02 15:04 MST")
	return fmt.Sprintf("Provider '%s' is set in config.yaml but %s cooling down after a "+
		"rate limit / quota error (429); the next one resets at %s. Wait for the reset, "+
		"add another credential with `icebath auth add %s`, or switch to a "+
		"different provider with `icebath model`.", providerID, auxWhichCredentials(len(live)), when, providerID), true
}

// PoolBillingMessage names a pool burned by a billing verdict, reading persisted rows when no pool is loaded
func PoolBillingMessage(ctx context.Context, providerID, model string, pool interface{ Entries() []PooledCredential }) (string, bool) {
	var entries []PooledCredential
	if pool != nil {
		entries = pool.Entries()
	} else {
		entries = auxPersistedPoolEntries(modelsPoolEnv(ctx), providerID)
	}
	live := auxLiveEntries(entries)
	if len(live) == 0 {
		return "", false
	}
	now := float64(auxUnavailableClock().UnixNano()) / 1e9
	var billing []PooledCredential
	for _, e := range live {
		until, _ := exhaustedUntil(e, len(live) == 1)
		if e.LastStatus != StatusExhausted || until <= now {
			return "", false
		}
		if e.LastErrorCode == 402 || e.FailureReason() == FailureReasonBilling {
			billing = append(billing, e)
		}
	}
	if len(billing) == 0 {
		return "", false
	}
	pick := billing[0]
	detail := strings.TrimSpace(pick.LastErrorMessage)
	scope, codeText := "", ""
	if model != "" {
		scope = fmt.Sprintf(" for model '%s'", model)
	}
	if pick.LastErrorCode != 0 {
		if detail != "" {
			codeText = fmt.Sprintf(" (last error %d: %s)", pick.LastErrorCode, detail)
		} else {
			codeText = fmt.Sprintf(" (last error %d)", pick.LastErrorCode)
		}
	}
	return fmt.Sprintf("Provider '%s'%s is out of usable credentials: %s exhausted%s. Add credits or update billing "+
		"with that provider, then retry, or switch to a different provider with `icebath model`.",
		providerID, scope, auxWhichCredentials(len(live)), codeText), true
}

// MissingProviderCredentialsMessage is the remedy for an explicit provider with no credentials, from the registry
func MissingProviderCredentialsMessage(ctx context.Context, providerID string) string {
	if cooldown, ok := PoolCooldownMessage(ctx, providerID); ok {
		return cooldown
	}
	pconfig := registryLookup(ctx, providerID)
	var envVars []string
	if pconfig != nil {
		envVars = pconfig.KeyEnv
	}
	problem, remedy := "no API key was found", ""
	switch {
	case LlamacppAliases[providerID]:
		problem, remedy = "no local llama.cpp server is running", "Start it in Settings → Providers → Local models"
	case len(envVars) > 0:
		remedy = fmt.Sprintf("Set the %s environment variable", envVars[0])
	case pconfig == nil:
		remedy = fmt.Sprintf("Set the %s_API_KEY environment variable", strings.ReplaceAll(strings.ToUpper(providerID), "-", "_"))
	case strings.HasPrefix(pconfig.AuthType, "oauth"):
		problem, remedy = "no credentials were found", fmt.Sprintf("Run `icebath auth add %s` to sign in", providerID)
	default:
		problem = "no credentials were found"
	}
	switchText := "switch to a different provider with `icebath model`."
	if remedy != "" {
		return fmt.Sprintf("Provider '%s' is set in config.yaml but %s. %s, or %s", providerID, problem, remedy, switchText)
	}
	return fmt.Sprintf("Provider '%s' is set in config.yaml but %s. %s", providerID, problem, textutil.UpperFirst(strings.ToLower(switchText)))
}
