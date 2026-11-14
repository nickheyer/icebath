package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Variable holding the 1Password service account token, whose presence keeps the op env file from loading
const envOnePasswordToken = "OP_SERVICE_ACCOUNT_TOKEN"

// One load of the env files into the process, for the process home unless ContextHome picks the context home
type EnvLoad struct {
	ContextHome       bool
	ProjectEnv        string
	SkipSecretSources bool
}

// Logs an env file statement the loader could not read, by the line it starts on
func warnEnvStatement(line int) {
	slog.Warn(fmt.Sprintf("dotenv could not parse statement starting at line %d", line))
}

// Assignments of env file bytes as the loader reads them, unreadable statements logged
func readEnvStatements(raw []byte) []envAssignment {
	return parseEnvStatements(envLoaderText(raw), warnEnvStatement)
}

// Assignments of an env file as the loader will see it once sanitized, UTF-16 decoded and NUL bytes dropped
func bootstrapStatements(path string) ([]envAssignment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var text string
	if hasUTF16Mark(raw) {
		decoded, ok := utf16Text(raw)
		if ok {
			text = strings.ReplaceAll(strings.ReplaceAll(decoded, "\r\n", "\n"), "\r", "\n")
		} else {
			text = latin1Text(raw)
		}
	} else {
		text = envLoaderText(raw)
	}
	return parseEnvStatements(strings.ReplaceAll(text, "\x00", ""), warnEnvStatement), nil
}

// Process environment with every value another pass published put back to what the variable held before
func (st *envState) lookupEnv(pass int, every bool) map[string]string {
	env := processEnviron()
	for name, rec := range st.published {
		if cur, ok := env[name]; ok && cur == rec.value && (every || rec.pass != pass) {
			if rec.hadBaseline {
				env[name] = rec.baseline
			} else {
				delete(env, name)
			}
		}
	}
	return env
}

// Next load pass number, the caller holding the state lock
func (st *envState) nextPass() int {
	st.passes++
	return st.passes - 1
}

// Loads one env file into the process, references resolved against what variables held before earlier passes published them
func (st *envState) loadEnvFile(ctx context.Context, path string, override bool, pass int, managed bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	assignments := readEnvStatements(raw)
	st.mu.Lock()
	for _, a := range resolveEnvAssignments(assignments, st.lookupEnv(pass, false), override) {
		current, present := os.LookupEnv(a.name)
		if !a.set || (!override && present) {
			continue
		}
		rec, recorded := st.published[a.name]
		ours := recorded && present && current == rec.value
		if slices.Contains(spawnCredentialKeys(), a.name) && current != "" && !ours {
			continue
		}
		next := publishedVar{baseline: current, hadBaseline: present, value: a.value, pass: pass}
		if ours {
			next.baseline, next.hadBaseline = rec.baseline, rec.hadBaseline
		}
		if err := setProcessEnv(a.name, a.value); err != nil {
			st.mu.Unlock()
			return fmt.Errorf("publish %s from %s: %w", a.name, path, err)
		}
		st.published[a.name] = next
	}
	keys := st.loadedKeys
	if managed {
		keys = st.managedKeys
	}
	for _, a := range assignments {
		keys[a.name] = true
	}
	st.mu.Unlock()
	return st.sanitizeCredentials(ctx)
}

// Code points outside ASCII in a value, at most limit distinct ones, each with its character when it prints
func offendingChars(value string, limit int) string {
	var seen []string
	for i := 0; i < len(value) && len(seen) < limit; {
		r, size := utf8.DecodeRuneInString(value[i:])
		label := ""
		switch {
		case r == utf8.RuneError && size == 1:
			label = fmt.Sprintf("U+%04X", 0xdc00+int(value[i]))
		case r > 127:
			if label = fmt.Sprintf("U+%04X", r); unicode.IsPrint(r) {
				label += " (" + charLiteral(r) + ")"
			}
		}
		if label != "" && !slices.Contains(seen, label) {
			seen = append(seen, label)
		}
		i += size
	}
	return strings.Join(seen, ", ")
}

// Strips characters outside ASCII from every credential variable of the process, telling the person once per variable
func (st *envState) sanitizeCredentials(ctx context.Context) error {
	names, env := processEnvList()
	for _, name := range names {
		value := env[name]
		if !isCredentialName(name) {
			continue
		}
		cleaned := strings.Map(func(r rune) rune {
			if r > 127 {
				return -1
			}
			return r
		}, value)
		if cleaned == value {
			continue
		}
		if err := setProcessEnv(name, cleaned); err != nil {
			return err
		}
		st.mu.Lock()
		warned := st.warnedKeys[name]
		st.warnedKeys[name] = true
		st.mu.Unlock()
		if warned {
			continue
		}
		stripped := utf8.RuneCountInString(value) - utf8.RuneCountInString(cleaned)
		detail := offendingChars(value, 3)
		if detail == "" {
			detail = "non-printable"
		}
		plural := "s"
		if stripped == 1 {
			plural = ""
		}
		st.notify(ctx, fmt.Sprintf("  Warning: %s contained %d non-ASCII character%s (%s) — stripped so the key can be sent as an HTTP header.",
			name, stripped, plural, detail))
		st.notify(ctx, "  This usually means the key was copy-pasted from a PDF, rich-text editor, or web page that substituted lookalike\n"+
			"  Unicode glyphs for ASCII letters. If authentication fails (e.g. \"API key not valid\"), re-copy the key from the\n"+
			"  provider's dashboard and run `icebath setup` (or edit the .env file in a plain-text editor).")
	}
	return nil
}

// Resolved form of a file path for once only warnings
func resolvedPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// Rewrites an untidy or UTF-16 env file as clean UTF-8 before loading, under the home write lock
func (e *Env) sanitizeBeforeLoad(ctx context.Context, path, lockHome string) {
	st := e.s.envs
	if _, untidy := st.sanitizedEnvText(path); !untidy {
		return
	}
	rewrite := func(ctx context.Context) error {
		text, untidy := st.sanitizedEnvText(path)
		if !untidy {
			return nil
		}
		return fileutil.WriteFile(ctx, path, []byte(text), fileutil.Perm(0o600), fileutil.TempPrefix(".env_"), fileutil.TempSuffix(".tmp"))
	}
	var err error
	if lockHome != "" {
		err = e.locked(ctx, lockHome, path, rewrite)
	} else {
		err = rewrite(ctx)
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not rewrite %s before loading it: %v", path, err))
	}
}

// Clean UTF-8 text an env file is rewritten as, ok false for a tidy, UTF-32 or undecodable file
func (st *envState) sanitizedEnvText(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var original []string
	force := false
	switch {
	case hasUTF32Mark(raw):
		key := resolvedPath(path)
		st.mu.Lock()
		warned := st.warnedUTF32[key]
		st.warnedUTF32[key] = true
		st.mu.Unlock()
		if !warned {
			slog.Warn(fmt.Sprintf("Skipping .env sanitize for %s: UTF-32 BOM detected; leaving file untouched to avoid corruption", path))
		}
		return "", false
	case hasUTF16Mark(raw):
		text, ok := utf16Text(raw)
		if !ok {
			return "", false
		}
		original, force = textLines(text), true
	default:
		original = textLines(utf8ReplacedText(raw))
		if len(original) > 0 && strings.HasPrefix(original[0], string(utf8.RuneError)) {
			return "", false
		}
	}
	stripped := make([]string, len(original))
	for i, line := range original {
		stripped[i] = strings.ReplaceAll(line, "\x00", "")
	}
	sanitized := sanitizeEnvLines(stripped)
	if !force && slices.Equal(sanitized, original) {
		return "", false
	}
	return strings.ReplaceAll(strings.Join(sanitized, ""), "\n", nativeNewline), true
}

// Removes inherited profile managed routing keys that an existing home env file does not define
func (e *Env) clearMissingProfileKeys(path string) error {
	if !fileExists(path) {
		return nil
	}
	_, defined := e.s.envs.loadFile(path)
	for _, key := range ProfileManagedEnvKeys() {
		if _, ok := defined[key]; ok {
			continue
		}
		if _, present := os.LookupEnv(key); present {
			if err := os.Unsetenv(key); err != nil {
				return err
			}
		}
	}
	return nil
}

// Loads the home env file, false when it cannot be read while the managed scope pins the terminal backend
func (e *Env) loadUserEnv(ctx context.Context, path string, pass int) (bool, error) {
	st := e.s.envs
	err := st.loadEnvFile(ctx, path, true, pass, false)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err == nil, err
	}
	if _, pinned, perr := e.s.PinnedTerminalBackend(ctx); perr != nil || !pinned {
		return false, errors.Join(err, perr)
	}
	st.mu.Lock()
	warned := st.warnedUnreadable[path]
	st.warnedUnreadable[path] = true
	st.mu.Unlock()
	if !warned {
		slog.Warn(fmt.Sprintf("Cannot read %s; managed scope pins terminal.backend, so it loads as absent", path))
	}
	return false, nil
}

// Loads the home, project and managed env files into the process and applies the secret sources, returning the files loaded
func (e *Env) Load(ctx context.Context, o EnvLoad) ([]string, error) {
	s, st := e.s, e.s.envs
	if s.worker != nil {
		return nil, nil
	}
	launch, err := home.RoutingHome()
	if err != nil {
		return nil, err
	}
	active, err := s.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	target := active
	if !o.ContextHome {
		if target, err = home.ProcessHome(); err != nil {
			return nil, err
		}
	}
	if st.scope.Multiplexed(ctx) && home.Override(ctx) != "" && (!s.sameHome(target, launch) || !s.sameHome(active, launch)) {
		key := s.homeKey(target)
		st.mu.Lock()
		logged := st.skipLogged[key]
		st.skipLogged[key] = true
		st.mu.Unlock()
		if !logged {
			slog.Debug(fmt.Sprintf("multiplex: skipping process-global dotenv load for routed profile home %s (credentials resolve via the profile scope)", target))
		}
		if !o.SkipSecretSources {
			if _, err := e.hydrateSecretSources(ctx, target); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	var loaded []string
	userEnv := home.EnvFile.In(target)
	st.mu.Lock()
	pass := st.nextPass()
	st.mu.Unlock()
	s.bootOnce.Do(func() {})
	if err := s.publishDeployment(ctx, target, o.ProjectEnv); err != nil {
		return nil, err
	}
	if fileExists(userEnv) {
		e.sanitizeBeforeLoad(ctx, userEnv, target)
	}
	if o.ProjectEnv != "" && fileExists(o.ProjectEnv) {
		e.sanitizeBeforeLoad(ctx, o.ProjectEnv, "")
	}
	if fileExists(userEnv) {
		ok, err := e.loadUserEnv(ctx, userEnv, pass)
		if err != nil {
			return nil, err
		}
		if ok {
			loaded = append(loaded, userEnv)
			if err := e.clearMissingProfileKeys(userEnv); err != nil {
				return nil, err
			}
		}
	}
	if opEnv := home.OpEnvFile.In(target); fileExists(opEnv) && os.Getenv(envOnePasswordToken) == "" {
		if err := st.loadEnvFile(ctx, opEnv, false, pass, false); err != nil {
			return nil, err
		}
	}
	if o.ProjectEnv != "" && fileExists(o.ProjectEnv) {
		if err := st.loadEnvFile(ctx, o.ProjectEnv, !fileExists(userEnv), pass, false); err != nil {
			return nil, err
		}
		loaded = append(loaded, o.ProjectEnv)
	}
	if err := st.reassertSourceValues(s.homeKey(target)); err != nil {
		return nil, err
	}
	if err := s.publishDeployment(ctx, target, o.ProjectEnv); err != nil {
		return nil, err
	}
	backend, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	if err := backend.Boot(ctx, target); err != nil {
		return nil, err
	}
	if !o.SkipSecretSources {
		if err := e.applySecretSources(ctx, target); err != nil {
			return nil, err
		}
	}
	if managed := managedFile(".env"); managed != "" && fileExists(managed) {
		e.sanitizeBeforeLoad(ctx, managed, "")
		if err := st.loadEnvFile(ctx, managed, true, pass, true); err != nil {
			return nil, err
		}
	}
	if s.sameHome(target, launch) {
		if err := s.ApplyTerminalEnv(home.WithOverride(ctx, target), ProcessEnv{}, nil, nil); err != nil {
			slog.Debug(fmt.Sprintf("Could not bridge the terminal config of %s into the environment: %v", target, err))
		}
	}
	return loaded, nil
}

// Loads the home env file of ctx into the process as its own pass, reporting whether the file exists
func (e *Env) LoadHomeFile(ctx context.Context) (bool, error) {
	_, path, err := e.path(ctx)
	if err != nil || !fileExists(path) {
		return false, err
	}
	st := e.s.envs
	st.mu.Lock()
	pass := st.nextPass()
	st.mu.Unlock()
	return true, st.loadEnvFile(ctx, path, true, pass, false)
}

// What a load would publish from the home env file of ctx then the managed one, nothing published
func (e *Env) Resolve(ctx context.Context) (map[string]string, error) {
	st := e.s.envs
	_, path, err := e.path(ctx)
	if err != nil {
		return nil, err
	}
	layers := []string{path}
	if managed := managedFile(".env"); managed != "" {
		layers = append(layers, managed)
	}
	st.mu.Lock()
	env := st.lookupEnv(0, true)
	st.mu.Unlock()
	out := map[string]string{}
	for _, path := range layers {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, a := range resolveEnvAssignments(readEnvStatements(raw), env, true) {
			if a.set {
				env[a.name], out[a.name] = a.value, a.value
			}
		}
	}
	return out, nil
}

// Values the loads of this process last wrote to a variable, its name matched without regard to case
func (e *Env) PublishedValues(name string) []string {
	st := e.s.envs
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for key, rec := range st.published {
		if strings.EqualFold(key, name) && !slices.Contains(out, rec.value) {
			out = append(out, rec.value)
		}
	}
	slices.Sort(out)
	return out
}

// Names every env file but the managed one loaded into this process so far, sorted
func (e *Env) LoadedKeys() []string { return e.s.envs.keySet(false) }

// Names the administrator managed env file loaded into this process, sorted
func (e *Env) ManagedKeys() []string { return e.s.envs.keySet(true) }

// Sorted copy of the loaded or the managed key set
func (st *envState) keySet(managed bool) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	keys := st.loadedKeys
	if managed {
		keys = st.managedKeys
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
