package secret

import (
	"context"
	"errors"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/toolchain"
)

// What the secret sources of a program are built from
type SourcesOptions struct {
	Homes *home.Resolver
	// Store the Bitwarden helper program is installed through
	Tools *toolchain.Store
	// Environment a helper child inherits when a fetch runs for the process's own environment
	ProcessChildEnv func(ctx context.Context) []string
}

// Secret sources of the program, the registry with its bundled sources, as the config env loader drives them
type Sources struct {
	registry    *Registry
	childEnv    func(ctx context.Context) []string
	bitwarden   *BitwardenSource
	onepassword *OnePasswordSource
}

// Sources is what the config env loader applies after the env files
var _ config.SecretSources = (*Sources)(nil)

// Registry holding the bundled Bitwarden, 1Password and command sources in that order
func NewSources(o SourcesOptions) (*Sources, error) {
	if o.Homes == nil || o.Tools == nil || o.ProcessChildEnv == nil {
		return nil, errors.New("secret: a home resolver, the toolchain store and the child environment are required")
	}
	s := &Sources{
		registry: NewRegistry(o.Homes), childEnv: o.ProcessChildEnv,
		bitwarden: NewBitwardenSource(o.Tools), onepassword: NewOnePasswordSource(),
	}
	for _, src := range []Source{s.bitwarden, s.onepassword, &CommandSource{}} {
		if !s.registry.Register(src, Registration{APIVersion: APIVersion, Origin: Builtin}) {
			return nil, errors.New("secret: bundled source " + src.Name() + " could not be registered")
		}
	}
	return s, nil
}

// Set of sources that plugins add theirs to
func (s *Sources) Registry() *Registry { return s.registry }

// Bundled source of the bws program, for setup and token commands
func (s *Sources) Bitwarden() *BitwardenSource { return s.bitwarden }

// Bundled 1Password source, for setup and sync commands
func (s *Sources) OnePassword() *OnePasswordSource { return s.onepassword }

// Raw view of the secrets section node the env loader hands over
func rawSection(secrets *yaml.Node) config.Raw {
	if secrets == nil {
		return config.Raw{}
	}
	raw, err := config.RawOf(secrets)
	if err != nil {
		return config.Raw{}
	}
	return raw
}

// Fetches and applies every enabled source of a home into env with the full per source detail
func (s *Sources) ApplyDetailed(ctx context.Context, env map[string]string, secrets config.Raw, homeDir string) *Report {
	return s.registry.Apply(ctx, env, secrets, homeDir, s.childEnv)
}

// Fetches and applies every enabled source of a home into env, reported the way the env loader records it
func (s *Sources) Apply(ctx context.Context, homeDir string, secrets *yaml.Node, env map[string]string) *config.SecretReport {
	report := s.ApplyDetailed(ctx, env, rawSection(secrets), homeDir)
	out := &config.SecretReport{Provenance: map[string]config.AppliedSecret{}, Conflicts: report.Conflicts}
	for _, sr := range report.Sources {
		out.Sources = append(out.Sources, config.SecretSourceReport{
			Name: sr.Name, Label: sr.Label, Applied: sr.Applied, SkippedExisting: sr.SkippedExisting,
			Error: sr.Result.Err, ErrorKind: string(sr.Result.Kind), Warnings: sr.Result.Warnings,
		})
	}
	for name, applied := range report.Provenance {
		out.Provenance[name] = config.AppliedSecret{Source: applied.Source, Authoritative: applied.Authoritative}
	}
	return out
}

// Names of the sources a pass for a home would fetch now
func (s *Sources) Enabled(_ context.Context, homeDir string, secrets *yaml.Node) []string {
	return s.registry.EnabledNames(rawSection(secrets), homeDir)
}

// Next step for a failed fetch of a source, empty for an unknown source or a class without one
func (s *Sources) Remediation(_ context.Context, homeDir, source, errorKind string, secrets *yaml.Node) string {
	src, ok := s.registry.Get(source, homeDir)
	if !ok {
		return ""
	}
	return src.Remediation(ErrorKind(errorKind), sourceSection(rawSection(secrets), source))
}

// Words status lines name a source with, empty for an unknown source
func (s *Sources) Label(_ context.Context, homeDir, source string) string {
	src, ok := s.registry.Get(source, homeDir)
	if !ok {
		return ""
	}
	return src.Label()
}
