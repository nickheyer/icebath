package secret

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
)

// Record of one variable an apply pass set
type AppliedVar struct {
	Name   string
	Source string
	Shape  Shape
	// True when the value replaced one the env file or shell had set
	OverrodeEnv bool
	// True when the source may beat the env file for this variable on later reloads
	Authoritative bool
}

// What one source did within an apply pass
type SourceReport struct {
	Name   string
	Label  string
	Result Result
	// Variables this source set
	Applied []string
	// Variables kept at the value the env file or shell had
	SkippedExisting []string
	// Variables an earlier source had already set
	SkippedClaimed []string
	// Variables guarded as the bootstrap credential of a source
	SkippedProtected []string
	// Names that are no legal variable names
	SkippedInvalid []string
}

// Merged outcome of one apply pass
type Report struct {
	Sources    []SourceReport
	Provenance map[string]AppliedVar
	// Warnings about a variable two sources both supply
	Conflicts []string
}

// Reports whether the pass set any variable
func (r *Report) AppliedAny() bool { return len(r.Provenance) > 0 }

// State of one apply pass, sequential, the first source to set a variable keeping it
type applier struct {
	env       map[string]string
	report    *Report
	protected map[string]string
	preserve  map[string]bool
	claimed   map[string]string
}

// Sets one variable through the guard chain and reports whether it was applied
func (a *applier) tryApply(sr *SourceReport, src Source, override bool, name, value string) bool {
	if !ValidEnvName(name) {
		sr.SkippedInvalid = append(sr.SkippedInvalid, name)
		return false
	}
	if _, guarded := a.protected[name]; guarded {
		sr.SkippedProtected = append(sr.SkippedProtected, name)
		return false
	}
	if winner, taken := a.claimed[name]; taken {
		sr.SkippedClaimed = append(sr.SkippedClaimed, name)
		a.report.Conflicts = append(a.report.Conflicts, fmt.Sprintf(
			"%s: kept value from %s; %s also supplies it (first source wins — remove one binding or reorder secrets.sources)",
			name, winner, src.Name()))
		return false
	}
	existed := a.env[name] != ""
	if existed && (a.preserve[name] || !override) {
		sr.SkippedExisting = append(sr.SkippedExisting, name)
		return false
	}
	a.env[name] = value
	a.claimed[name] = src.Name()
	sr.Applied = append(sr.Applied, name)
	a.report.Provenance[name] = AppliedVar{Name: name, Source: src.Name(), Shape: src.Shape(), OverrodeEnv: existed, Authoritative: override && !a.preserve[name]}
	return true
}

// Applies the secrets of one fetched source, aliasing a profile suffixed credential to its plain name
func (a *applier) applySource(src Source, cfg config.Raw, result Result, profile string, supplied map[string]bool) {
	label := src.Label()
	if label == "" {
		label = src.Name()
	}
	a.report.Sources = append(a.report.Sources, SourceReport{Name: src.Name(), Label: label, Result: result})
	sr := &a.report.Sources[len(a.report.Sources)-1]
	if !result.OK() {
		return
	}
	override := src.Override(cfg)
	for _, name := range result.names() {
		value := result.Secrets[name]
		if !a.tryApply(sr, src, override, name, value) || profile == "" {
			continue
		}
		alias := profileAliasTarget(name, profile)
		if alias == "" || supplied[alias] {
			continue
		}
		if _, taken := a.claimed[alias]; taken {
			continue
		}
		if a.tryApply(sr, src, override, alias, value) {
			sr.Result.Warnings = append(sr.Result.Warnings, fmt.Sprintf("applied profile-scoped %s as %s (active profile '%s')", name, alias, profile))
		}
	}
}

// Reports whether env holds exactly the variables of this process
func isProcessEnv(env map[string]string) bool {
	environ := os.Environ()
	seen := 0
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if held, has := env[name]; !has || held != value {
			return false
		}
		seen++
	}
	return seen == len(env)
}

// Fetches every enabled source of a home and applies the merged result to env, mapped sources before bulk ones
func (r *Registry) Apply(ctx context.Context, env map[string]string, secrets config.Raw, homeDir string, processChildEnv func(context.Context) []string) *Report {
	report := &Report{Provenance: map[string]AppliedVar{}}
	enabled := r.enabled(secrets, homeDir)
	if len(enabled) == 0 {
		return report
	}
	root := sectionOf(secrets)
	preserve := map[string]bool{}
	if names, ok := root.stringList("preserve_existing"); ok {
		for _, name := range names {
			if name = strings.TrimSpace(name); name != "" {
				preserve[name] = true
			}
		}
	}
	profile := ""
	if root.truthy("profile_alias", true) {
		profile = activeProfileName(homeDir)
	}
	var ordered []Source
	for _, shape := range []Shape{Mapped, Bulk} {
		for _, src := range enabled {
			if src.Shape() == shape {
				ordered = append(ordered, src)
			}
		}
	}
	var childEnv []string
	if isProcessEnv(env) {
		childEnv = processChildEnv(ctx)
	} else {
		childEnv = envList(env)
	}
	type fetched struct {
		src    Source
		cfg    config.Raw
		result Result
	}
	var fetches []fetched
	protected := map[string]string{}
	for _, src := range ordered {
		cfg := sourceSection(secrets, src.Name())
		result := fetchWithTimeout(ctx, src, FetchInput{Config: cfg, Home: homeDir, Env: env, ChildEnv: childEnv})
		fetches = append(fetches, fetched{src, cfg, result})
		for _, name := range src.Protected(cfg) {
			if _, guarded := protected[name]; !guarded {
				protected[name] = src.Name()
			}
		}
	}
	supplied := map[string]bool{}
	for _, f := range fetches {
		if f.result.OK() {
			for name := range f.result.Secrets {
				supplied[name] = true
			}
		}
	}
	a := &applier{env: env, report: report, protected: protected, preserve: preserve, claimed: map[string]string{}}
	for _, f := range fetches {
		a.applySource(f.src, f.cfg, f.result, profile, supplied)
	}
	return report
}
