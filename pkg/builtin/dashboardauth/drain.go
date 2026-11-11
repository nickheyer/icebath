package dashboardauth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	drainTag                   = "dashboard-auth-drain"
	drainDefaultMinSecretChars = 43
	drainMinDistinctChars      = 16
	drainMinShannonBits        = 128.0
	DrainRoutePath             = "/api/gateway/drain"
	drainNotInteractive        = "DrainSecretProvider is a non-interactive service credential."
	drainNoStartLogin          = "DrainSecretProvider is a non-interactive service credential; there is no login flow."
)

var (
	drainLog      = slog.Default().With("logger", "plugins.dashboard_auth.drain")
	drainSkipMu   sync.Mutex
	drainLastSkip string
)

// Reason the drain provider last declined to register, empty after a registration
func DrainLastSkipReason() string {
	drainSkipMu.Lock()
	defer drainSkipMu.Unlock()
	return drainLastSkip
}

func setDrainLastSkip(reason string) {
	drainSkipMu.Lock()
	drainLastSkip = reason
	drainSkipMu.Unlock()
}

// Total Shannon entropy in bits of a value over its character distribution
func shannonBits(value string) float64 {
	runes := []rune(value)
	if len(runes) == 0 {
		return 0
	}
	counts := map[rune]int{}
	var order []rune
	for _, r := range runes {
		if counts[r] == 0 {
			order = append(order, r)
		}
		counts[r]++
	}
	n := float64(len(runes))
	perChar := 0.0
	for _, r := range order {
		p := float64(counts[r]) / n
		perChar -= p * math.Log2(p)
	}
	return perChar * n
}

// Rejection reason of a weak secret by length, distinct characters and entropy, empty for a strong one
func AssessSecretStrength(secret string, minChars int) string {
	runes := []rune(secret)
	if len(runes) == 0 {
		return "secret is empty"
	}
	if len(runes) < minChars {
		return fmt.Sprintf("secret too short: %d chars (need >= %d; "+
			"use a >=256-bit value, e.g. `python -c \"import secrets; "+
			"print(secrets.token_urlsafe(32))\"`)", len(runes), minChars)
	}
	distinct := map[rune]bool{}
	for _, r := range runes {
		distinct[r] = true
	}
	if len(distinct) < drainMinDistinctChars {
		return fmt.Sprintf("secret has only %d distinct characters (need >= %d); looks structured/low-entropy", len(distinct), drainMinDistinctChars)
	}
	if bits := shannonBits(secret); bits < drainMinShannonBits {
		return fmt.Sprintf("secret entropy too low: %.0f bits (need >= %.0f); looks structured/repeated", bits, drainMinShannonBits)
	}
	return ""
}

// Constructor arguments of the drain provider
type drainSettings struct {
	secret string
	scope  string
}

// Non interactive shared bearer secret provider for drain control
type DrainSecretProvider struct {
	nonInteractive
	secret string
	scope  string
}

// Provider for a strong secret, a ValueError naming why a weak one was rejected
func NewDrainSecretProvider(secret, scope string) (*DrainSecretProvider, error) {
	if reason := AssessSecretStrength(secret, drainDefaultMinSecretChars); reason != "" {
		return nil, &gateway.DashboardAuthValueError{Msg: "drain secret rejected: " + reason}
	}
	if scope == "" {
		scope = "drain"
	}
	return &DrainSecretProvider{nonInteractive: nonInteractive{notInteractive: drainNotInteractive, noStartLogin: drainNoStartLogin},
		secret: secret, scope: scope}, nil
}

func (p *DrainSecretProvider) Name() string { return "drain-secret" }

func (p *DrainSecretProvider) DisplayName() string { return "Drain Control (service credential)" }

func (p *DrainSecretProvider) SupportsToken() bool { return true }

func (p *DrainSecretProvider) SupportsSession() bool { return false }

// Drain control principal for a constant time match of the secret, nil otherwise
func (p *DrainSecretProvider) VerifyToken(_ context.Context, token string) (*gateway.TokenPrincipal, error) {
	if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(p.secret)) == 1 {
		return &gateway.TokenPrincipal{Principal: "drain-control", Provider: p.Name(), Scopes: []string{p.scope}}, nil
	}
	return nil, nil
}

func (p *DrainSecretProvider) VerifySession(context.Context, string) (*gateway.DashboardSession, error) {
	return nil, nil
}

func (p *DrainSecretProvider) RefreshSession(context.Context, string) (*gateway.DashboardSession, error) {
	return nil, &gateway.DashboardAuthNotImplementedError{Msg: drainNotInteractive}
}

func (p *DrainSecretProvider) RevokeSession(context.Context, string) error { return nil }

// Python int of a config value, false where int() raises TypeError or ValueError
func configInt(v any) (int, bool, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true, nil
		}
		return 0, true, nil
	case float64:
		if math.IsNaN(x) {
			return 0, false, nil
		}
		if math.IsInf(x, 0) {
			return 0, false, &gateway.DashboardAuthValueError{Msg: "cannot convert float infinity to integer"}
		}
		return int(x), true, nil
	case string:
		n, ok := pystr.IntText(x)
		if !ok {
			return 0, false, nil
		}
		return saturatedInt(n), true, nil
	}
	if n, ok := pyrepr.Int(v); ok {
		return saturatedInt(n), true, nil
	}
	return 0, false, nil
}

// Int of a big integer clamped to the int range, the comparisons it feeds unchanged
func saturatedInt(n *big.Int) int {
	switch {
	case n.Cmp(big.NewInt(math.MaxInt)) > 0:
		return math.MaxInt
	case n.Cmp(big.NewInt(math.MinInt)) < 0:
		return math.MinInt
	}
	return int(n.Int64())
}

// Drain provider arguments from the env secret and dashboard.drain_auth, a skip when unset or weak
func drainSettingsFor(ctx context.Context) (drainSettings, error) {
	secret := pystr.Strip(os.Getenv("ICEBATH_DASHBOARD_DRAIN_SECRET"))
	if secret == "" {
		return drainSettings{}, skipDebug("ICEBATH_DASHBOARD_DRAIN_SECRET is not set. Set a per-agent >=256-bit secret " +
			"(e.g. `python -c \"import secrets; print(secrets.token_urlsafe(32))\"`) to enable " +
			"NAS-driven drain coordination; leave it unset to disable the drain endpoint.")
	}
	section := loadConfigSection(ctx, drainLog, drainTag, "dashboard", "drain_auth")
	rawScope := sectionGet(section, "scope", "drain")
	if !pyrepr.Truthy(rawScope) {
		rawScope = "drain"
	}
	scope := pystr.Strip(pyrepr.Str(rawScope))
	if scope == "" {
		scope = "drain"
	}
	minChars, ok, err := configInt(sectionGet(section, "min_secret_chars", drainDefaultMinSecretChars))
	if err != nil {
		return drainSettings{}, err
	}
	if !ok {
		minChars = drainDefaultMinSecretChars
	}
	if reason := AssessSecretStrength(secret, minChars); reason != "" {
		return drainSettings{}, skipAt("warning", "ICEBATH_DASHBOARD_DRAIN_SECRET rejected — "+reason+
			". The drain endpoint stays disabled (fail-closed).")
	}
	return drainSettings{secret: secret, scope: scope}, nil
}

// Bundled drain dashboard auth plugin
type DrainPlugin struct{}

func (DrainPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{
		Name: "drain", Key: "dashboard_auth/drain", Version: "1.0.0", Author: "NousResearch", Kind: "backend",
		Description: "Dashboard auth provider — non-interactive shared-bearer-secret for the gateway drain-control endpoint. The first consumer of the generic token-auth capability (supports_token/verify_token). nous-account-service provisions a per-agent unique secret via ICEBATH_DASHBOARD_DRAIN_SECRET; this provider verifies an inbound Authorization bearer token against it with a constant-time compare and registers /api/gateway/drain as token-authable. Fails CLOSED: a weak/short/low-entropy secret (< 256 bits) is rejected at registration and the endpoint stays disabled. No-op when the env var is unset. Behavioural knobs (scope, min_secret_chars) live under dashboard.drain_auth in config.yaml.",
		RequiresEnv: []plugins.EnvSpec{{Name: "ICEBATH_DASHBOARD_DRAIN_SECRET"}},
	}
}

// Registers the drain provider and its token route when a strong secret is set
func (DrainPlugin) Register(pctx *plugins.Context) error {
	ctx := pctx.HomeContext()
	setDrainLastSkip("")
	kwargs, reason, err := registerProvider(pctx, drainLog, drainTag, "DrainSecretProvider",
		func() (drainSettings, error) { return drainSettingsFor(ctx) },
		func(s drainSettings) (gateway.DashboardAuthProvider, error) {
			return NewDrainSecretProvider(s.secret, s.scope)
		})
	setDrainLastSkip(reason)
	if err != nil || kwargs == nil {
		return err
	}
	gateway.RegisterTokenRoute(DrainRoutePath)
	drainLog.Info(fmt.Sprintf("dashboard-auth-drain: registered drain service-credential provider (scope=%s, route=%s)",
		kwargs.scope, DrainRoutePath))
	return nil
}

func init() { plugins.RegisterBundled(DrainPlugin{}) }
