package secrets

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"
)

// Endings of a variable's name a profile suffixed secret may carry before the profile's name, OPENAI_API_KEY_WORK
var aliasEndings = []string{"_API_KEY", "_TOKEN", "_SECRET", "_KEY", "_PASSWORD"}

// Where one variable the stores set came from
type Applied struct {
	Source string
	// The store's value wins over one the .env gains later, the store overriding and the variable not kept local
	Authoritative bool
}

// What one store did in a pass
type SourceReport struct {
	Name, Label string
	Result      Result
	Applied     []string
	// Variables it supplied that were kept: set already and not replaced, taken by an earlier store, the credential of a store, not names
	Existing, Claimed, Protected, Invalid []string
}

// What a pass over every enabled store did for a profile
type Report struct {
	Sources []SourceReport
	// Values the stores set and where each came from, by variable
	Values  map[string]string
	Applied map[string]Applied
	// Variables two stores supplied, the first keeping it
	Conflicts []string
}

// The secret named for the profile under its plain name, OPENAI_API_KEY_WORK as OPENAI_API_KEY in profile work, empty when it is not
func alias(name, profile string) string {
	if profile == "" {
		return ""
	}
	suffix := "_" + strings.ToUpper(strings.ReplaceAll(profile, "-", "_"))
	plain, ok := strings.CutSuffix(name, suffix)
	if !ok || plain == "" || !config.EnvName(plain) {
		return ""
	}
	if slices.ContainsFunc(aliasEndings, func(ending string) bool { return strings.HasSuffix(plain, ending) }) {
		return plain
	}
	return ""
}

// One pass of applying fetched secrets over the variables a profile has
type applier struct {
	h         *Secrets
	cfg       *config.Config
	env       map[string]string
	report    *Report
	protected map[string]string
	preserve  map[string]bool
	claimed   map[string]string
}

// Fetches every enabled store of the profile, mapped ones first, and applies what they supplied over env: the first store to supply a
// variable keeps it, a variable set already stays unless the store overrides and it is not kept local, no store sets another's credential
func (h *Secrets) apply(ctx context.Context, cfg *config.Config, env map[string]string, fresh bool) Report {
	report := Report{Values: map[string]string{}, Applied: map[string]Applied{}}
	var enabled []Source
	for _, shape := range []string{Mapped, Bulk} {
		for _, source := range h.Ordered(cfg) {
			if source.Spec().Shape == shape && h.Enabled(cfg, source.Spec().Name) {
				enabled = append(enabled, source)
			}
		}
	}
	if len(enabled) == 0 {
		return report
	}
	preserve := map[string]bool{}
	for _, name := range cfg.GetStringSlice(secretsconfig.Preserve) {
		if name = strings.TrimSpace(name); name != "" {
			preserve[name] = true
		}
	}
	profile := ""
	if cfg.GetBool(secretsconfig.ProfileAlias) && cfg.Profile() != "default" {
		profile = cfg.Profile()
	}
	results := make([]Result, len(enabled))
	protected := map[string]string{}
	for i, source := range enabled {
		spec := source.Spec()
		results[i] = h.fetch(ctx, source, Fetch{Config: cfg, Env: maps.Clone(env), Fresh: fresh})
		if spec.TokenSetting != "" || spec.TokenEnv != "" {
			if token := h.TokenEnv(cfg, spec); token != "" {
				if _, has := protected[token]; !has {
					protected[token] = spec.Name
				}
			}
		}
	}
	// Where the settings come from and which bearer goes to the config plane are never a store's to decide
	for _, name := range cfg.ProtectedEnv() {
		if _, has := protected[name]; !has {
			protected[name] = "Remote Config"
		}
	}
	direct := map[string]bool{}
	for _, result := range results {
		if result.OK() {
			for name := range result.Secrets {
				direct[name] = true
			}
		}
	}
	a := &applier{h: h, cfg: cfg, env: env, report: &report, protected: protected, preserve: preserve, claimed: map[string]string{}}
	for i, source := range enabled {
		a.source(source.Spec(), results[i], profile, direct)
	}
	return report
}

func (a *applier) source(spec Spec, result Result, profile string, direct map[string]bool) {
	report := SourceReport{Name: spec.Name, Label: spec.Label, Result: result}
	defer func() { a.report.Sources = append(a.report.Sources, report) }()
	if !result.OK() {
		return
	}
	override := a.h.Overrides(a.cfg, spec.Name)
	for _, name := range slices.Sorted(maps.Keys(result.Secrets)) {
		value := result.Secrets[name]
		if !a.set(&report, spec, override, name, value) {
			continue
		}
		plain := alias(name, profile)
		if plain == "" || direct[plain] {
			continue
		}
		if _, taken := a.claimed[plain]; taken {
			continue
		}
		if a.set(&report, spec, override, plain, value) {
			report.Result.Warnings = append(report.Result.Warnings, fmt.Sprintf("applied profile-scoped %s as %s (active profile %q)", name, plain, profile))
		}
	}
}

// Sets one variable through the checks every store shares, true when it was set
func (a *applier) set(report *SourceReport, spec Spec, override bool, name, value string) bool {
	if !config.EnvName(name) {
		report.Invalid = append(report.Invalid, name)
		return false
	}
	if _, protected := a.protected[name]; protected {
		report.Protected = append(report.Protected, name)
		return false
	}
	if owner, taken := a.claimed[name]; taken {
		report.Claimed = append(report.Claimed, name)
		a.report.Conflicts = append(a.report.Conflicts, fmt.Sprintf("%s: kept value from %s; %s also supplies it (first source wins — remove one binding or reorder secrets.sources)",
			name, owner, spec.Name))
		return false
	}
	existed := a.env[name] != ""
	if existed && (a.preserve[name] || !override) {
		report.Existing = append(report.Existing, name)
		return false
	}
	a.env[name] = value
	a.claimed[name] = spec.Name
	report.Applied = append(report.Applied, name)
	a.report.Values[name] = value
	a.report.Applied[name] = Applied{Source: spec.Name, Authoritative: override && !a.preserve[name]}
	return true
}
