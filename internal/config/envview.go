package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Longest wait for the env file write lock
const envLockWait = 15 * time.Second

// Environment of the home a context serves, its env file, its secret sources and the process variables
type Env struct {
	s *Store
}

// Environment view of the store, every call resolving its home from the context it is given
func (s *Store) Env() *Env { return &Env{s: s} }

// Home of ctx and the location of its env file
func (e *Env) path(ctx context.Context) (string, string, error) {
	h, err := e.s.homes.Home(ctx)
	if err != nil {
		return "", "", err
	}
	return h, home.EnvFile.In(h), nil
}

// Reports whether a file exists at path as far as this process can tell
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Assignments of the home env file as written, names in first seen order, empty in a frozen worker
func (e *Env) fileEntries(ctx context.Context) ([]string, map[string]string, error) {
	if e.s.worker != nil {
		return nil, map[string]string{}, nil
	}
	h, path, err := e.path(ctx)
	if err != nil {
		return nil, nil, err
	}
	names, values := e.s.envs.loadFile(path)
	raw, set := values[EnvRedactSecrets]
	e.s.envs.notePolicy(e.s.homeKey(h), PolicyEnvFile, raw, set)
	return names, values, nil
}

// Variable switching secret redaction, read from the env file before the config key security.redact_secrets
const EnvRedactSecrets = "ICEBATH_REDACT_SECRETS"

// Assignments of the home env file as written, a fresh map on every call, empty when the file is absent
func (e *Env) File(ctx context.Context) (map[string]string, error) {
	_, values, err := e.fileEntries(ctx)
	return values, err
}

// Names the home env file assigns, in the order each first appears
func (e *Env) FileNames(ctx context.Context) ([]string, error) {
	names, _, err := e.fileEntries(ctx)
	return names, err
}

// Drops every parsed env file so the next read sees a write even within one file timestamp tick
func (e *Env) Invalidate() { e.s.envs.forgetFiles() }

// Secrets of the home as its env file, then its source values, then the managed env file define them
func (e *Env) Profile(ctx context.Context) (map[string]string, error) {
	h, path, err := e.path(ctx)
	if err != nil {
		return nil, err
	}
	_, out := e.s.envs.loadFile(path)
	for k, v := range e.s.envs.sourceValues(e.s.homeKey(h)) {
		if !IsGlobalEnv(k) {
			out[k] = v
		}
	}
	managed, err := e.s.ManagedEnv(ctx)
	if err != nil {
		return nil, err
	}
	for k, v := range managed {
		if !IsGlobalEnv(k) {
			out[k] = v
		}
	}
	return out, nil
}

// Value of a name through the secret scope of ctx, an error when the scope refuses an unscoped read
func (e *Env) Lookup(ctx context.Context, name string) (string, bool, error) {
	return e.s.envs.scope.Lookup(ctx, name)
}

// Value of a name from the scoped environment, else from the home env file
func (e *Env) Get(ctx context.Context, name string) (string, bool, error) {
	if v, ok, err := e.Lookup(ctx, name); ok || err != nil {
		return v, ok, err
	}
	file, err := e.File(ctx)
	if err != nil {
		return "", false, err
	}
	v, ok := file[name]
	return v, ok, nil
}

// Value of a name preferring the home env file, an unresolved op reference there yielding to the scoped value
func (e *Env) GetPreferFile(ctx context.Context, name string) (string, bool, error) {
	file, err := e.File(ctx)
	if err != nil {
		return "", false, err
	}
	raw := file[name]
	if strings.HasPrefix(strings.TrimLeftFunc(raw, isEnvSpace), "op://") {
		if v, ok, err := e.Lookup(ctx, name); err != nil || (ok && v != "") {
			return v, ok, err
		}
	}
	if raw != "" {
		return raw, true, nil
	}
	return e.Lookup(ctx, name)
}

// Refusal text for a write of key under a package manager owned install or an administrator pin, empty when allowed
func (e *Env) WriteRefusal(ctx context.Context, key, action string) (string, error) {
	h, err := e.s.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	if sys := home.ManagedSystem(h); sys != "" {
		return ManagedInstallMessage(action+" "+key, sys), nil
	}
	managed, err := e.s.IsEnvManaged(ctx, key)
	if err != nil || !managed {
		return "", err
	}
	source := "the managed scope"
	if p := managedFile(".env"); p != "" {
		source = p
	}
	return fmt.Sprintf("Cannot %s %s: it is managed by your administrator (%s) and cannot be changed.", action, key, source), nil
}

// Error for a write of key the install or the administrator forbids, nil when it is allowed
func (e *Env) RequireWritable(ctx context.Context, key, action string) error {
	refusal, err := e.WriteRefusal(ctx, key, action)
	if err != nil || refusal == "" {
		return err
	}
	return &EnvLockedError{Message: refusal}
}

// Tells the person the refusal for a write of key and reports whether the write is blocked
func (e *Env) writeBlocked(ctx context.Context, key, action string) (bool, error) {
	refusal, err := e.WriteRefusal(ctx, key, action)
	if err != nil || refusal == "" {
		return false, err
	}
	e.s.notify(ctx, refusal)
	return true, nil
}

// Error for a name the env writer never persists, malformed, on the denylist or naming a provider launch command
func (e *Env) ValidateName(key string) error {
	if !envNameShape.MatchString(key) {
		return &EnvNameError{Name: key}
	}
	policy := envPolicyName(key)
	denied := slices.Contains(envNameDenylist(), policy) ||
		slices.ContainsFunc(e.s.envs.registry.launchNames(), func(n string) bool { return envPolicyName(n) == policy }) ||
		slices.ContainsFunc(envNameDenyPrefixes(), func(p string) bool { return strings.HasPrefix(policy, p) })
	if denied {
		return &EnvNameError{Name: key, Denied: true}
	}
	return nil
}

// Text a single character shows as in a warning, in quotes and escaped when it does not print
func charLiteral(r rune) string {
	switch {
	case unicode.IsPrint(r):
		return "'" + string(r) + "'"
	case r < 0x100:
		return fmt.Sprintf(`'\x%02x'`, r)
	case r < 0x10000:
		return fmt.Sprintf(`'\u%04x'`, r)
	}
	return fmt.Sprintf(`'\U%08x'`, r)
}

// Value without its characters outside ASCII, with the warning naming the ones that went, empty when none did
func stripNonASCII(key, value string) (string, string) {
	var kept strings.Builder
	var bad []string
	at := 0
	for _, r := range value {
		if r > 127 {
			bad = append(bad, fmt.Sprintf("  position %d: %s (U+%04X)", at, charLiteral(r), r))
		} else {
			kept.WriteRune(r)
		}
		at++
	}
	if len(bad) == 0 {
		return value, ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n  Warning: %s contains non-ASCII characters that will break API requests.\n"+
		"  This usually happens when copy-pasting from a PDF, rich-text editor,\n"+
		"  or web page that substitutes lookalike Unicode glyphs for ASCII letters.\n\n", key)
	for i, line := range bad[:min(5, len(bad))] {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  " + line)
	}
	if len(bad) > 5 {
		b.WriteString("\n  ... and more")
	}
	b.WriteString("\n\n  The non-ASCII characters have been stripped automatically.\n" +
		"  If authentication fails, re-copy the key from the provider's dashboard.\n")
	return kept.String(), b.String()
}

// Runs fn holding the cross process write lock of the env file, waiting at most the lock wait for it
func (e *Env) locked(ctx context.Context, h, path string, fn func(ctx context.Context) error) error {
	wait, cancel := context.WithTimeout(ctx, envLockWait)
	lock, err := fileutil.OpenLock(wait, envLockPath(h), fileutil.RecordHolder(),
		fileutil.TimeoutMessage(fmt.Sprintf("Timed out waiting for the environment file lock (%s)", path)))
	cancel()
	if err != nil {
		return err
	}
	return errors.Join(fn(lock.Context(ctx)), lock.Release())
}

// Mirrors a persisted change into the bound secret scope and, outside a routed profile, into the process
func (e *Env) publish(ctx context.Context, name string, value *string) error {
	scope := e.s.envs.scope
	bound := scope.Bound(ctx)
	stored := bound && scope.Store(ctx, name, value)
	if scope.Routed(ctx) || (bound && !stored) {
		return nil
	}
	if value == nil {
		return os.Unsetenv(name)
	}
	return setProcessEnv(name, *value)
}

// Saves or replaces one assignment in the home env file, a refused write told to the person and skipped
func (e *Env) Set(ctx context.Context, name, value string) error {
	if blocked, err := e.writeBlocked(ctx, name, "set"); blocked || err != nil {
		return err
	}
	if err := e.ValidateName(name); err != nil {
		return err
	}
	value, warning := stripNonASCII(name, strings.NewReplacer("\n", "", "\r", "").Replace(value))
	if warning != "" {
		e.s.notify(ctx, warning)
	}
	if err := e.s.EnsureHome(ctx); err != nil {
		return err
	}
	h, path, err := e.path(ctx)
	if err != nil {
		return err
	}
	err = e.locked(ctx, h, path, func(ctx context.Context) error {
		exists := fileExists(path)
		var lines []string
		if exists {
			if lines, err = readEnvLines(path); err != nil {
				return err
			}
		}
		assignment := name + "=" + QuoteEnvValue(value) + "\n"
		if at := slices.IndexFunc(lines, func(line string) bool { return EnvLineDefines(line, name) }); at >= 0 {
			lines[at] = assignment
		} else {
			lines = append(lines, assignment)
		}
		return writeEnvLines(ctx, path, lines, exists)
	})
	if err != nil {
		return err
	}
	err = e.publish(ctx, name, &value)
	e.Invalidate()
	return err
}

// Removes every assignment of a name from the home env file and the live environment, reporting whether one existed
func (e *Env) Remove(ctx context.Context, name string) (bool, error) {
	if blocked, err := e.writeBlocked(ctx, name, "remove"); blocked || err != nil {
		return false, err
	}
	if !envNameShape.MatchString(name) {
		return false, &EnvNameError{Name: name}
	}
	h, path, err := e.path(ctx)
	if err != nil {
		return false, err
	}
	if !fileExists(path) {
		return false, e.publish(ctx, name, nil)
	}
	found := false
	err = e.locked(ctx, h, path, func(ctx context.Context) error {
		lines, err := readEnvLines(path)
		if err != nil {
			return err
		}
		kept := slices.DeleteFunc(slices.Clone(lines), func(line string) bool { return EnvLineDefines(line, name) })
		if found = len(kept) < len(lines); !found {
			return nil
		}
		return writeEnvLines(ctx, path, kept, true)
	})
	if err != nil {
		return false, err
	}
	err = e.publish(ctx, name, nil)
	e.Invalidate()
	return found, err
}

// Rewrites the lines of the home env file through edit under its write lock, the file mode kept
func (e *Env) EditLines(ctx context.Context, edit func(lines []string) ([]string, error)) error {
	h, path, err := e.path(ctx)
	if err != nil {
		return err
	}
	err = e.locked(ctx, h, path, func(ctx context.Context) error {
		exists := fileExists(path)
		var lines []string
		if exists {
			if lines, err = readEnvLines(path); err != nil {
				return err
			}
		}
		edited, err := edit(slices.Clone(lines))
		if err != nil || slices.Equal(edited, lines) {
			return err
		}
		return writeEnvLines(ctx, path, edited, exists)
	})
	e.Invalidate()
	return err
}

// Rewrites the home env file with normalized lines and returns how many lines changed
func (e *Env) Sanitize(ctx context.Context) (int, error) {
	h, path, err := e.path(ctx)
	if err != nil {
		return 0, err
	}
	if !fileExists(path) {
		return 0, nil
	}
	fixes := 0
	err = e.locked(ctx, h, path, func(ctx context.Context) error {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		original := textLines(utf8ReplacedText(raw))
		sanitized := sanitizeEnvLines(original)
		for i := range original {
			if original[i] != sanitized[i] {
				fixes++
			}
		}
		if fixes == 0 {
			return nil
		}
		return writeEnvLines(ctx, path, sanitized, false)
	})
	if err != nil {
		return 0, err
	}
	e.Invalidate()
	return fixes, nil
}

// Reports an env file that exists but that this process is refused permission to read
func envFileUnreadable(path string) bool {
	f, err := os.Open(path)
	if err == nil {
		_ = f.Close()
		return false
	}
	return errors.Is(err, fs.ErrPermission)
}

// Reads the home env file into the process again and returns how many variables changed, removing known ones it dropped
func (e *Env) Reload(ctx context.Context) (int, error) {
	names, values, err := e.fileEntries(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, name := range names {
		if cur, ok := os.LookupEnv(name); ok && cur == values[name] {
			continue
		}
		if err := setProcessEnv(name, values[name]); err != nil {
			return count, err
		}
		count++
	}
	_, path, err := e.path(ctx)
	if err != nil {
		return count, err
	}
	if envFileUnreadable(path) {
		return count, nil
	}
	for _, name := range append(e.s.envs.registry.Names(), ExtraEnvKeys()...) {
		if _, kept := values[name]; kept {
			continue
		}
		if _, present := os.LookupEnv(name); present {
			if err := os.Unsetenv(name); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

// Writes both Anthropic credential slots, one holding its value and the other cleared
func (e *Env) writeAnthropicSlots(ctx context.Context, token, apiKey string, tokenFirst bool) error {
	slots := [][2]string{{"ANTHROPIC_TOKEN", token}, {"ANTHROPIC_API_KEY", apiKey}}
	if !tokenFirst {
		slices.Reverse(slots)
	}
	for _, slot := range slots {
		if err := e.Set(ctx, slot[0], slot[1]); err != nil {
			return err
		}
	}
	return nil
}

// Persists an Anthropic OAuth or setup token and clears the API key slot
func (e *Env) SaveAnthropicOAuthToken(ctx context.Context, value string) error {
	return e.writeAnthropicSlots(ctx, value, "", true)
}

// Clears both Anthropic slots so the credential files of Claude Code are used
func (e *Env) UseAnthropicClaudeCodeCredentials(ctx context.Context) error {
	return e.writeAnthropicSlots(ctx, "", "", true)
}

// Persists an Anthropic API key and clears the OAuth and setup token slot
func (e *Env) SaveAnthropicAPIKey(ctx context.Context, value string) error {
	return e.writeAnthropicSlots(ctx, "", value, false)
}

// Registry entry of one variable that has no value
type MissingEnvVar struct {
	Name string
	Var  EnvVar
}

// Optional variables with no value in the scoped environment or the home env file, none when only required ones count
func (e *Env) Missing(ctx context.Context, requiredOnly bool) ([]MissingEnvVar, error) {
	var out []MissingEnvVar
	if requiredOnly {
		return out, nil
	}
	file, err := e.File(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range e.s.envs.registry.Names() {
		v, ok, err := e.Lookup(ctx, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			v = file[name]
		}
		if v == "" {
			entry, _ := e.s.envs.registry.Get(name)
			out = append(out, MissingEnvVar{Name: name, Var: entry})
		}
	}
	return out, nil
}

// Lock file serializing the writers of the env file of a home
func envLockPath(h string) string { return home.EnvLock.In(h) }
