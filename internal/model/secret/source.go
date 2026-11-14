// Resolves env-shaped credentials from external secret managers and scopes them to the profile a task serves
package secret

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

// Version of the source contract a plugin must be built against to register
const APIVersion = 1

// Wall clock budgets of one source fetch and of one helper program run
const (
	DefaultFetchTimeout = 120 * time.Second
	DefaultCLITimeout   = 30 * time.Second
)

// How a source decides which variables it supplies
type Shape string

// Explicit per variable bindings, and whole projects injected at once
const (
	Mapped Shape = "mapped"
	Bulk   Shape = "bulk"
)

// Why a fetch failed
type ErrorKind string

// Failure classes a fetch reports
const (
	NotConfigured ErrorKind = "not_configured"
	BinaryMissing ErrorKind = "binary_missing"
	AuthFailed    ErrorKind = "auth_failed"
	AuthExpired   ErrorKind = "auth_expired"
	RefInvalid    ErrorKind = "ref_invalid"
	Network       ErrorKind = "network"
	EmptyValue    ErrorKind = "empty_value"
	Timeout       ErrorKind = "timeout"
	Internal      ErrorKind = "internal"
)

// What a source reads while fetching
type FetchInput struct {
	// Raw section of the source under secrets, possibly malformed
	Config config.Raw
	// Profile home the secrets are fetched for
	Home string
	// Environment view of this fetch, the only place bootstrap credentials are read from
	Env map[string]string
	// Environment a helper child that needs the caller's own variables runs with
	ChildEnv []string
}

// Outcome of one fetch, Err empty when it succeeded
type Result struct {
	Secrets map[string]string
	// Names of Secrets in the order the backend returned them, sorted order when empty
	Order    []string
	Warnings []string
	Err      string
	Kind     ErrorKind
	// Helper program used, shown by status commands
	Binary string
}

// Reports a fetch that succeeded
func (r Result) OK() bool { return r.Err == "" }

// Secret keys in apply order, the backend order when recorded and sorted order otherwise
func (r Result) names() []string {
	if len(r.Order) == len(r.Secrets) {
		return r.Order
	}
	names := make([]string, 0, len(r.Secrets))
	for name := range r.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Result of a failed fetch carrying what the fetch gathered before it failed
func (r Result) fail(message string, kind ErrorKind) Result {
	r.Err, r.Kind = message, kind
	return r
}

// One external backend resolving env-shaped secrets when a profile loads
type Source interface {
	// Config section key under secrets and the provenance label, lower case letters, digits and underscores
	Name() string
	// Words status lines name the backend with
	Label() string
	Shape() Shape
	// Reference scheme the source owns, unique across sources, empty for none
	Scheme() string
	Enabled(cfg config.Raw) bool
	// Reports whether fetched values may replace what the env file or shell already set
	Override(cfg config.Raw) bool
	// Variables no source may overwrite, the bootstrap credential of this one
	Protected(cfg config.Raw) []string
	Timeout(cfg config.Raw) time.Duration
	// Resolves the secrets without prompting, every failure reported in the result
	Fetch(ctx context.Context, in FetchInput) Result
	// One line next step for a failed fetch, empty for none
	Remediation(kind ErrorKind, cfg config.Raw) string
}

// Next step for each failure class when a source names none of its own
var genericRemediation = map[ErrorKind]string{
	NotConfigured: "Run `icebath secrets {name} setup` to finish configuration.",
	BinaryMissing: "Run `icebath secrets {name} setup` to install the helper CLI.",
	AuthFailed:    "Credentials rejected — run `icebath secrets {name} setup` to re-authenticate.",
	AuthExpired:   "Credentials expired — run `icebath secrets {name} setup` to re-authenticate.",
	Network:       "Network problem reaching the secrets backend — check connectivity and retry.",
	Timeout:       "Backend was slow — raise secrets.{name}.timeout_seconds if this recurs.",
}

// Next step for kind from the hints of a source, else the generic table, name and token variable filled in
func remediationFor(kind ErrorKind, hints map[ErrorKind]string, name, tokenEnv string) string {
	if kind == "" {
		return ""
	}
	template := hints[kind]
	if template == "" {
		template = genericRemediation[kind]
	}
	return strings.NewReplacer("{name}", name, "{token_env}", tokenEnv).Replace(template)
}

// Fetch budget a section asks for, the default for an unset, malformed or non positive value
func fetchTimeout(cfg config.Raw) time.Duration {
	seconds := sectionOf(cfg).float("timeout_seconds", DefaultFetchTimeout.Seconds())
	if seconds <= 0 {
		return DefaultFetchTimeout
	}
	return time.Duration(seconds * float64(time.Second))
}

// Ordered rules mapping helper failure text onto a failure class, the first matching rule winning
type errorRule struct {
	kind   ErrorKind
	tokens []string
}

// Failure class of a helper message by its text, internal when no rule matches
func classify(message string, rules []errorRule) ErrorKind {
	lowered := strings.ToLower(message)
	for _, rule := range rules {
		for _, token := range rule.tokens {
			if strings.Contains(lowered, token) {
				return rule.kind
			}
		}
	}
	return Internal
}

var (
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	ansiPattern    = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)?)`)
	ecma48Pattern  = regexp.MustCompile(`(?s)\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\].*?(?:\x07|\x1b\\)|[PX^_].*?(?:\x1b\\)|[\x20-\x2f]+[\x30-\x7e]|[\x30-\x7e])|\x{9b}[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\x{9d}.*?(?:\x07|\x{9c})|[\x{80}-\x{9f}]`)
)

// Reports a legal environment variable name
func ValidEnvName(name string) bool { return envNamePattern.MatchString(name) }

// Text without CSI and OSC escape sequences, an unterminated OSC sequence included
func ScrubANSI(text string) string { return ansiPattern.ReplaceAllString(text, "") }

// Text without any ECMA-48 sequence or stray escape byte, trimmed
func scrubTerminal(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(ecma48Pattern.ReplaceAllString(text, ""), "\x1b", ""))
}
