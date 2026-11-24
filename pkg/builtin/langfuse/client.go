package langfuse

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	publicKeyEnv = "ICEBATH_LANGFUSE_PUBLIC_KEY"
	secretKeyEnv = "ICEBATH_LANGFUSE_SECRET_KEY"
)

var (
	langfuseKeyPrefixes   = map[string]string{publicKeyEnv: "pk-lf-", secretKeyEnv: "sk-lf-"}
	placeholderRemainders = map[string]bool{"": true, "...": true, "***": true}
	placeholderFragments  = []string{"placeholder", "your-", "your_", "changeme", "change-me", "test-key", "example", "unset", "dummy", "xxx"}
)

// Settled client of one profile, a nil client recording that the build failed
type clientSlot struct{ client *lfClient }

var (
	slotsMu        sync.Mutex
	launchSlot     *clientSlot
	slotsByHome    = map[string]*clientSlot{}
	slotOrder      []string
	buildMu        sync.Mutex
	finalizerArmed atomic.Bool
)

// Credential read through the profile secret scope, an unscoped multiplexed read raising
func secret(ctx context.Context, name string) string {
	v, _, err := provider.GetSecret(ctx, name)
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(v)
}

// Settled slot of the active profile, nil when its client was never built
func settledSlot(ctx context.Context) *clientSlot {
	slotsMu.Lock()
	defer slotsMu.Unlock()
	if home.Override(ctx) == "" {
		return launchSlot
	}
	return slotsByHome[home.Key(home.Dir(ctx))]
}

// Builds the active profile's client once and stores the outcome in its slot, the caller holding buildMu
func settleClient(ctx context.Context) *clientSlot {
	slot := &clientSlot{client: buildClient(ctx)}
	slotsMu.Lock()
	if home.Override(ctx) == "" {
		launchSlot = slot
	} else {
		key := home.Key(home.Dir(ctx))
		if _, seen := slotsByHome[key]; !seen {
			slotOrder = append(slotOrder, key)
		}
		slotsByHome[key] = slot
	}
	slotsMu.Unlock()
	if slot.client != nil {
		finalizerArmed.Store(true)
	}
	return slot
}

// Cached client of the active profile, nil when credentials are missing or unusable, the first build serialized
func getLangfuse(ctx context.Context) *lfClient {
	slot := settledSlot(ctx)
	if slot == nil {
		slot = func() *clientSlot {
			buildMu.Lock()
			defer buildMu.Unlock()
			if s := settledSlot(ctx); s != nil {
				return s
			}
			return settleClient(ctx)
		}()
	}
	return slot.client
}

// Every client that settled successfully, the launch slot first then one per profile home
func settledClients() []*lfClient {
	slotsMu.Lock()
	defer slotsMu.Unlock()
	var out []*lfClient
	if launchSlot != nil && launchSlot.client != nil {
		out = append(out, launchSlot.client)
	}
	for _, key := range slotOrder {
		if s := slotsByHome[key]; s != nil && s.client != nil {
			out = append(out, s.client)
		}
	}
	return out
}

// Log ready problem with a key, a missing prefix checked only when required, a template value always
func validateLangfuseKey(envName, value string, requirePrefix bool) string {
	expected := langfuseKeyPrefixes[envName]
	if expected == "" {
		return ""
	}
	preview := "<empty>"
	if value != "" {
		if pystr.Len(value) <= 12 {
			preview = pyrepr.StrRepr(value)
		} else {
			preview = pyrepr.StrRepr(pystr.Slice(value, 0, 6) + "...")
		}
	}
	if requirePrefix && !strings.HasPrefix(value, expected) {
		return fmt.Sprintf("%s=%s (expected %s prefix)", envName, preview, pyrepr.StrRepr(expected))
	}
	remainder := strings.ToLower(strings.TrimPrefix(value, expected))
	template := placeholderRemainders[remainder]
	for _, f := range placeholderFragments {
		template = template || strings.Contains(remainder, f)
	}
	if template {
		return fmt.Sprintf("%s=%s (looks like a template value, not an issued key)", envName, preview)
	}
	return ""
}

// First non empty of the Icebath setting and the standard SDK name
func setting(ctx context.Context, name string) string {
	if v := secret(ctx, "ICEBATH_LANGFUSE_"+name); v != "" {
		return v
	}
	return secret(ctx, "LANGFUSE_"+name)
}

// Client built from the profile credentials, nil with one warning when they are missing, templates or unusable
func buildClient(ctx context.Context) *lfClient {
	publicKey, secretKey := setting(ctx, "PUBLIC_KEY"), setting(ctx, "SECRET_KEY")
	if publicKey == "" || secretKey == "" {
		var missing []string
		if publicKey == "" {
			missing = append(missing, publicKeyEnv)
		}
		if secretKey == "" {
			missing = append(missing, secretKeyEnv)
		}
		pkgLog().Warn(fmt.Sprintf("Langfuse plugin: missing credentials (%s), traces will NOT be emitted. Set both "+
			"ICEBATH_LANGFUSE_PUBLIC_KEY and ICEBATH_LANGFUSE_SECRET_KEY, or disable the plugin "+
			"(`icebath plugins disable langfuse`) to silence this warning.", strings.Join(missing, ", ")))
		return nil
	}
	baseURL := setting(ctx, "BASE_URL")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	host := ""
	if u, err := url.Parse(baseURL); err == nil {
		host = strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	}
	cloud := host == "langfuse.com" || strings.HasSuffix(host, ".langfuse.com")
	keys := [][2]string{{publicKeyEnv, publicKey}, {secretKeyEnv, secretKey}}
	var placeholders, prefixes []string
	for _, k := range keys {
		if issue := validateLangfuseKey(k[0], k[1], cloud); issue != "" {
			placeholders = append(placeholders, issue)
		}
	}
	if len(placeholders) > 0 {
		pkgLog().Warn(fmt.Sprintf("Langfuse plugin: credentials look like placeholders, traces will "+
			"NOT be emitted (%s). Set real Langfuse keys (pk-lf-... / sk-lf-...) "+
			"or unset ICEBATH_LANGFUSE_PUBLIC_KEY / ICEBATH_LANGFUSE_SECRET_KEY to "+
			"silence this warning.", strings.Join(placeholders, "; ")))
		return nil
	}
	for _, k := range keys {
		if issue := validateLangfuseKey(k[0], k[1], true); issue != "" {
			prefixes = append(prefixes, issue)
		}
	}
	if len(prefixes) > 0 {
		pkgLog().Warn(fmt.Sprintf("Langfuse plugin: credentials do not look like Langfuse keys (%s). Fine for a "+
			"Langfuse-compatible endpoint that issues its own keys; otherwise every trace is "+
			"rejected at flush: check for template values or swapped public/secret keys.", strings.Join(prefixes, "; ")))
	}
	opts := clientOptions{publicKey: publicKey, secretKey: secretKey, baseURL: baseURL,
		environment: setting(ctx, "ENV"), release: setting(ctx, "RELEASE")}
	if raw := secret(ctx, "ICEBATH_LANGFUSE_SAMPLE_RATE"); raw != "" {
		if f, err := pyint.ParseFloat(raw); err == nil {
			opts.sampleRate = &f
		} else {
			pkgLog().Warn(fmt.Sprintf("Invalid ICEBATH_LANGFUSE_SAMPLE_RATE=%s", pyrepr.StrRepr(raw)))
		}
	}
	_, serviceSet := os.LookupEnv("OTEL_SERVICE_NAME")
	opts.defaultService = !serviceSet && !strings.Contains(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"), "service.name=")
	client, err := newClient(opts)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Could not initialize Langfuse client: %v", err))
		return nil
	}
	return client
}
