// Secret redaction for tool output, logs, approval prompts, browser endpoints and egress text
package redact

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/secret"
)

const (
	WalkMaxDepth    = 320
	WalkDepthMarker = "[redacted: nesting limit]"
	Unavailable     = "[redaction-unavailable]"
)

const (
	vaultMaxPerProfile = 64
	vaultMarker        = "«redacted-vault-secret»"
)

type UnfilledError struct {
	Name string
}

func (e *UnfilledError) Error() string {
	return "redaction needs a " + e.Name + " and none was provided"
}

type Options struct {
	Homes   *home.Resolver
	Config  *config.Store
	Secrets *secret.Scope
}

type Mode struct {
	Force          bool
	CodeFile       bool
	FileRead       bool
	SecretFile     bool
	URLCredentials bool
}

type Redactor struct {
	homes   *home.Resolver
	cfg     *config.Store
	secrets *secret.Scope
	launch  bool

	policyMu  sync.RWMutex
	envPolicy map[string]*string
	cfgPolicy map[string]*string

	vaultMu sync.Mutex
	vault   map[string][]string

	patternsMu sync.Mutex
	prefixes   atomic.Pointer[prefixSet]
}

func New(ctx context.Context, o Options) (*Redactor, error) {
	switch {
	case o.Homes == nil:
		return nil, &UnfilledError{Name: "home resolver"}
	case o.Config == nil:
		return nil, &UnfilledError{Name: "config store"}
	case o.Secrets == nil:
		return nil, &UnfilledError{Name: "secret scope"}
	}
	r := &Redactor{
		homes:     o.Homes,
		cfg:       o.Config,
		secrets:   o.Secrets,
		envPolicy: map[string]*string{},
		cfgPolicy: map[string]*string{},
		vault:     map[string][]string{},
	}
	r.launch = truthy(strings.ToLower(r.launchPolicy(ctx)))
	r.prefixes.Store(&prefixSet{literals: builtinLiterals()})
	return r, nil
}

func (r *Redactor) launchPolicy(ctx context.Context) string {
	if values, err := r.cfg.Env().File(ctx); err == nil {
		if raw, ok := values[config.EnvRedactSecrets]; ok {
			return raw
		}
	}
	if raw, ok := os.LookupEnv(config.EnvRedactSecrets); ok {
		return raw
	}
	if cfg, err := r.cfg.LoadReadonly(ctx); err == nil && cfg.Security.RedactSecrets != nil {
		return strconv.FormatBool(*cfg.Security.RedactSecrets)
	}
	return "true"
}

func truthy(lowered string) bool {
	switch lowered {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (r *Redactor) homeKey(path string) string {
	if key, err := r.homes.Key(path); err == nil {
		return key
	}
	return filepath.Clean(path)
}

func (r *Redactor) NotePolicy(homeKey string, source config.PolicySource, raw string, set bool) {
	var v *string
	if set {
		v = &raw
	}
	r.policyMu.Lock()
	defer r.policyMu.Unlock()
	switch source {
	case config.PolicyEnvFile:
		r.envPolicy[homeKey] = v
	case config.PolicyConfig:
		r.cfgPolicy[homeKey] = v
	}
}

func (r *Redactor) ResolveHomePolicy(homeDir string) {
	ctx := home.WithOverride(context.Background(), homeDir)
	key := r.homeKey(homeDir)
	values, err := r.cfg.Env().File(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("redaction policy for %s not read: %v", homeDir, err))
		return
	}
	raw, set := values[config.EnvRedactSecrets]
	r.NotePolicy(key, config.PolicyEnvFile, raw, set)
	cfg, err := r.cfg.LoadReadonly(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("redaction policy for %s not read: %v", homeDir, err))
		return
	}
	if v := cfg.Security.RedactSecrets; v != nil {
		r.NotePolicy(key, config.PolicyConfig, strconv.FormatBool(*v), true)
		return
	}
	r.NotePolicy(key, config.PolicyConfig, "", false)
}

func (r *Redactor) Enabled(ctx context.Context) bool {
	override := home.Override(ctx)
	if override == "" {
		return r.launch
	}
	key := r.homeKey(override)
	mapping, bound := r.secrets.Current(ctx)
	var raw *string
	if bound && len(mapping) > 0 {
		if v, ok := mapping[config.EnvRedactSecrets]; ok {
			raw = &v
		}
	}
	r.policyMu.RLock()
	defer r.policyMu.RUnlock()
	if raw == nil && !bound {
		recorded, ok := r.envPolicy[key]
		if !ok {
			return true
		}
		raw = recorded
	}
	if raw == nil {
		raw = r.cfgPolicy[key]
	}
	if raw == nil {
		return true
	}
	return truthy(pyLower(pyStrip(*raw)))
}

func (r *Redactor) RegisterVaultValue(ctx context.Context, value string) error {
	if value == "" {
		return nil
	}
	scope, err := r.homes.Home(ctx)
	if err != nil {
		return err
	}
	normalized := strings.ReplaceAll(strings.ReplaceAll(value, "\r", ""), "\n", "")
	r.vaultMu.Lock()
	defer r.vaultMu.Unlock()
	bucket := r.vault[scope]
	for _, v := range []string{value, normalized} {
		if v == "" {
			continue
		}
		bucket = slices.DeleteFunc(bucket, func(s string) bool { return s == v })
		bucket = append(bucket, v)
	}
	for len(bucket) > vaultMaxPerProfile {
		bucket = bucket[1:]
	}
	r.vault[scope] = bucket
	return nil
}

func (r *Redactor) ClearVaultValues(ctx context.Context) error {
	scope, err := r.homes.Home(ctx)
	if err != nil {
		return err
	}
	r.vaultMu.Lock()
	defer r.vaultMu.Unlock()
	delete(r.vault, scope)
	return nil
}

func (r *Redactor) vaultValues(ctx context.Context) []string {
	r.vaultMu.Lock()
	if len(r.vault) == 0 {
		r.vaultMu.Unlock()
		return nil
	}
	r.vaultMu.Unlock()
	scope, err := r.homes.Home(ctx)
	r.vaultMu.Lock()
	defer r.vaultMu.Unlock()
	var values []string
	if err != nil {
		for _, bucket := range r.vault {
			values = append(values, bucket...)
		}
	} else {
		values = slices.Clone(r.vault[scope])
	}
	slices.SortStableFunc(values, func(a, b string) int {
		return utf8.RuneCountInString(b) - utf8.RuneCountInString(a)
	})
	return values
}

func (r *Redactor) scrubVault(ctx context.Context, text string) string {
	for _, v := range r.vaultValues(ctx) {
		if strings.Contains(text, v) {
			text = strings.ReplaceAll(text, v, vaultMarker)
		}
	}
	return text
}

func (r *Redactor) Text(ctx context.Context, text string, m Mode) string {
	out, err := r.text(ctx, text, m)
	if err != nil {
		return Unavailable
	}
	return out
}

func (r *Redactor) Redact(ctx context.Context, text string) string {
	return r.Text(ctx, text, Mode{})
}

func (r *Redactor) text(ctx context.Context, text string, m Mode) (string, error) {
	if text == "" {
		return text, nil
	}
	text = r.scrubVault(ctx, text)
	if !m.Force && !r.Enabled(ctx) {
		return text, nil
	}
	codeFile := (m.CodeFile || m.FileRead) && !m.SecretFile
	ps := r.prefixes.Load()
	mask := maskToken
	if m.FileRead {
		mask = ps.maskNonreusable
	}
	if ps.hasLiteral(text) {
		var err error
		if text, err = ps.maskControlSplit(text, mask); err != nil {
			return "", err
		}
		if text, err = ps.replace(text, mask); err != nil {
			return "", err
		}
	}
	if strings.Contains(text, ".") {
		text = redactZhipu(text, mask)
	}
	if !codeFile {
		text = redactAssignments(text, mask)
	}
	if strings.Contains(text, "uthorization") || strings.Contains(text, "UTHORIZATION") {
		text = redactAuthHeader(text)
	}
	if strings.Contains(text, ":") {
		text = redactSecretHeader(text)
		text = redactTelegram(text)
	}
	if strings.Contains(text, "BEGIN") && strings.Contains(text, "-----") {
		text = rePrivateKey.ReplaceAllLiteralString(text, "[REDACTED PRIVATE KEY]")
	}
	if strings.Contains(text, "://") {
		text = redactURLCredentials(text, codeFile)
	}
	if strings.Contains(text, "eyJ") {
		text = reJWT.ReplaceAllStringFunc(text, maskToken)
	}
	if strings.Contains(text, "icebath-session=") || strings.Contains(text, "/webapp/launch#") {
		text = redactWebappLink(text)
	}
	if m.URLCredentials {
		text = redactStrictURLCredentials(text)
	}
	if strings.Contains(text, "&") && strings.Contains(text, "=") {
		text = redactFormBody(text)
	}
	if strings.Contains(text, "+") {
		text = redactPhones(text)
	}
	return text, nil
}

func (r *Redactor) Approval(ctx context.Context, text string, force, prose bool) string {
	return Command(r.Text(ctx, text, Mode{Force: force}), prose)
}

func (r *Redactor) CDPURL(ctx context.Context, value string) string {
	text := r.Text(ctx, value, Mode{})
	if text == "" {
		return text
	}
	text = replaceSubmatch(reURLWithQuery, text, func(m []int) string {
		return group(text, m, 1) + "://" + group(text, m, 2) + group(text, m, 3) + "?" + redactQueryString(group(text, m, 4)) + group(text, m, 5)
	})
	return replaceSubmatch(reURLUserinfo, text, func(m []int) string {
		return group(text, m, 1) + "://" + group(text, m, 2) + ":***@"
	})
}

func (r *Redactor) Egress(ctx context.Context, text string) string {
	out, err := r.text(ctx, text, Mode{Force: true})
	if err != nil {
		return Unavailable
	}
	if strings.Contains(out, "earer") {
		out = redactBearerResidue(out)
	}
	return out
}

func (r *Redactor) Terminal(ctx context.Context, output, command string, force bool) string {
	if output == "" {
		return output
	}
	codeFile := !(IsEnvDumpCommand(command) || r.ReadsSecretFile(ctx, command))
	out, err := r.text(ctx, output, Mode{Force: force, CodeFile: codeFile})
	if err != nil {
		return Unavailable
	}
	if codeFile && (force || r.Enabled(ctx)) && strings.Contains(out, ":") && strings.Contains(out, "'") {
		out = redactDiagnosticReprFields(out)
	}
	return out
}

func (r *Redactor) HasCredentialPrefix(text string) bool {
	found, err := r.prefixes.Load().find(text)
	return err != nil || len(found) > 0
}
