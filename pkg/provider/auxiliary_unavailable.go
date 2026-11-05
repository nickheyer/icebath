package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// AuxiliaryClientUnavailable reports that no auxiliary client could be built for the task
type AuxiliaryClientUnavailable struct{ Message string }

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
	auxNousMu            sync.Mutex
	auxLastNousDetail    string
	auxHasLastNousDetail bool
	auxWarnedNousDetails = map[string]bool{}
	auxUnavailableClock  = time.Now
)

// Persisted terminal quarantine marker in place of the error, either sentence terminated
func auxQuarantinedNousError(env PoolEnv, err error) error {
	if state, serr := env.Store.GetProviderAuthState("nous"); serr == nil {
		if marker := nousTerminalQuarantineMarker(state); marker != nil {
			if msg, ok := marker["message"]; ok && msg != nil && pyStr(msg) != "" {
				code := ""
				if c := marker["code"]; c != nil {
					code = pyStr(c)
				}
				return &AuthError{Message: auxSentence(pyStr(msg)), Provider: "nous", Code: code, ReloginRequired: true}
			}
		}
	}
	var authErr *AuthError
	if errors.As(err, &authErr) {
		return &AuthError{Message: auxSentence(authErr.Error()), Provider: authErr.Provider, Code: authErr.Code,
			ReloginRequired: authErr.ReloginRequired, RetryAfter: authErr.RetryAfter, HasRetryAfter: authErr.HasRetryAfter,
			Retryable: authErr.Retryable}
	}
	return err
}

func auxSentence(text string) string {
	return strings.TrimRight(pystr.Strip(text), ".") + "."
}

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
	detail := pystr.Strip(pick.LastErrorMessage)
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
		remedy = fmt.Sprintf("Set the %s_API_KEY environment variable", strings.ReplaceAll(pystr.Upper(providerID), "-", "_"))
	case strings.HasPrefix(pconfig.AuthType, "oauth"):
		problem, remedy = "no credentials were found", fmt.Sprintf("Run `icebath auth add %s` to sign in", providerID)
	default:
		problem = "no credentials were found"
	}
	switchText := "switch to a different provider with `icebath model`."
	if remedy != "" {
		return fmt.Sprintf("Provider '%s' is set in config.yaml but %s. %s, or %s", providerID, problem, remedy, switchText)
	}
	return fmt.Sprintf("Provider '%s' is set in config.yaml but %s. %s", providerID, problem, pystr.Capitalize(switchText))
}

// True when a Nous credential exists that failed, a coded error or persisted Nous state
func auxNousCredentialPresent(env PoolEnv, err error) bool {
	var authErr *AuthError
	if errors.As(err, &authErr) && authErr.Code != "" {
		return true
	}
	state, serr := env.Store.GetProviderAuthState("nous")
	return serr == nil && len(state) > 0
}

// RecordNousCredentialFailure remembers the latest Nous credential failure and logs each distinct one once
func RecordNousCredentialFailure(ctx context.Context, err error) string {
	env := modelsPoolEnv(ctx)
	err = auxQuarantinedNousError(env, err)
	var message string
	var authErr *AuthError
	if errors.As(err, &authErr) {
		message = formatAuthError(authErr)
	} else {
		message = err.Error()
	}
	message = pystr.Strip(message)
	if message == "" {
		message = auxPyExceptionName(err)
	}
	if authErr != nil && authErr.Code != "" && !strings.Contains(message, authErr.Code) {
		message = fmt.Sprintf("%s (code: %s)", message, authErr.Code)
	}
	detail := "Nous Portal runtime credentials unavailable: " + message
	auxNousMu.Lock()
	auxLastNousDetail, auxHasLastNousDetail = detail, true
	first := !auxWarnedNousDetails[detail]
	auxWarnedNousDetails[detail] = true
	auxNousMu.Unlock()
	if first {
		level := slog.LevelDebug
		if auxNousCredentialPresent(env, err) {
			level = slog.LevelWarn
		}
		slog.Log(ctx, level, "Auxiliary Nous client unavailable: "+detail)
	}
	return detail
}

// ClearNousCredentialFailure forgets the latest Nous credential failure after a successful resolution
func ClearNousCredentialFailure() {
	auxNousMu.Lock()
	auxLastNousDetail, auxHasLastNousDetail = "", false
	auxNousMu.Unlock()
}

// NousCredentialFailureDetail is the latest recorded Nous credential failure
func NousCredentialFailureDetail() (string, bool) {
	auxNousMu.Lock()
	defer auxNousMu.Unlock()
	return auxLastNousDetail, auxHasLastNousDetail
}

// Drops the remembered Nous failure and the warned set as a fresh process starts
func auxResetNousCredentialFailures() {
	auxNousMu.Lock()
	auxLastNousDetail, auxHasLastNousDetail = "", false
	auxWarnedNousDetails = map[string]bool{}
	auxNousMu.Unlock()
}
