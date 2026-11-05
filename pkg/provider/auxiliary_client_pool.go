package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Runs fn and turns a raised error panic into a returned error the way a Python except Exception catches it
func auxRecoverRaise(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			raised, ok := r.(error)
			if !ok {
				panic(r)
			}
			err = raised
		}
	}()
	fn()
	return nil
}

// Credential pool of the provider when it holds credentials, nil otherwise with a load failure logged at debug
func auxLoadPoolWithCredentials(ctx context.Context, provider, note string) *Pool {
	var pool *Pool
	if err := auxRecoverRaise(func() { pool = LoadPool(modelsPoolEnv(ctx), provider) }); err != nil {
		slog.Debug(fmt.Sprintf("Auxiliary client: could not load pool for %s%s: %v", provider, note, err))
		return nil
	}
	if pool == nil || !pool.HasCredentials() {
		return nil
	}
	return pool
}

// Whether a credentialed pool exists for the provider and the entry it selects, nil when selection fails
func auxSelectPoolEntry(ctx context.Context, provider string) (bool, *PooledCredential) {
	pool := auxLoadPoolWithCredentials(ctx, provider, "")
	if pool == nil {
		return false, nil
	}
	var entry PooledCredential
	var found bool
	if err := auxRecoverRaise(func() { entry, found = pool.Select("") }); err != nil {
		slog.Debug(fmt.Sprintf("Auxiliary client: could not select pool entry for %s: %v", provider, err))
		return true, nil
	}
	if !found {
		return true, nil
	}
	return true, &entry
}

// Current or next pool entry without moving the selection, reusing a pool the caller already loaded
func auxPeekPoolEntry(ctx context.Context, provider string, pool *Pool) *PooledCredential {
	if pool == nil {
		pool = auxLoadPoolWithCredentials(ctx, provider, " (peek)")
	}
	if pool == nil {
		return nil
	}
	var entry PooledCredential
	var found bool
	err := auxRecoverRaise(func() {
		if entry, found = pool.Current(); !found {
			entry, found = pool.Peek()
		}
	})
	if err != nil {
		slog.Debug(fmt.Sprintf("Auxiliary client: could not peek pool entry for %s: %v", provider, err))
		return nil
	}
	if !found {
		return nil
	}
	return &entry
}

// Runtime key of a pool entry falling back to its access token, empty for no entry
func auxPoolRuntimeAPIKey(entry *PooledCredential) string {
	if entry == nil {
		return ""
	}
	key := entry.RuntimeAPIKey()
	if key == "" {
		key = entry.AccessToken
	}
	return pyStripSpace(key)
}

// Runtime base URL of a pool entry with the Nous operator override first and the fallback last, trailing slashes removed
func auxPoolRuntimeBaseURL(ctx context.Context, entry *PooledCredential, fallback string) string {
	if entry == nil {
		return strings.TrimRight(pyStripSpace(fallback), "/")
	}
	if entry.Provider == "nous" {
		if envURL := nousInferenceEnvOverrideIn(modelsPoolEnv(ctx)); envURL != "" {
			return envURL
		}
	}
	url := entry.RuntimeBaseURL()
	for _, next := range []string{entry.InferenceBaseURL, entry.BaseURL, fallback} {
		if url != "" {
			break
		}
		url = next
	}
	return strings.TrimRight(pyStripSpace(url), "/")
}

// Hosts the auxiliary Anthropic path accepts from model.base_url, anything else falls back to the Anthropic default
var auxAnthropicCompatibleHosts = map[string]bool{"api.anthropic.com": true}

// True for native Anthropic hosts and gateways serving Messages under an anthropic or anthropic/v1 path
func auxIsAnthropicCompatibleHost(url string) bool {
	if url == "" {
		return false
	}
	parsed, err := pyURLParse(url, "")
	if err != nil {
		return false
	}
	hostname, _, _ := pyNetlocHostPort(parsed.netloc)
	if auxAnthropicCompatibleHosts[strings.TrimRight(strings.ToLower(pyStripSpace(hostname)), ".")] {
		return true
	}
	path := strings.ToLower(strings.TrimRight(parsed.path, "/"))
	return strings.HasSuffix(path, "/anthropic") || strings.HasSuffix(path, "/anthropic/v1")
}

// Minimum remaining Nous agent key lifetime in seconds, never below 60 and 1800 for an unset or invalid value
func auxNousMinKeyTTLSeconds() int64 {
	raw, ok := os.LookupEnv("ICEBATH_NOUS_MIN_KEY_TTL_SECONDS")
	if !ok {
		raw = "1800"
	}
	n, valid := pyIntText(raw)
	if !valid {
		return 1800
	}
	return max(60, n)
}

// Secret reader the scoped key lookup goes through, swapped by tests for a failing scope resolver
var auxGetSecretFn = GetSecret

// Provider key or paired base URL through the profile secret scope, the process env only on an unscoped read
func auxScopedKeyEnv(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	value, _, err := auxGetSecretFn(ctx, name)
	if err != nil {
		var unscoped *UnscopedSecretError
		if errors.As(err, &unscoped) {
			return pyStripSpace(os.Getenv(name)), nil
		}
		return "", err
	}
	return pyStripSpace(value), nil
}
