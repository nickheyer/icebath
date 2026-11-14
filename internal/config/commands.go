package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

// How the command line colors one stretch of output
type Tone int

// Tones of command output, plain text first
const (
	TonePlain Tone = iota
	ToneDim
	ToneWarning
	ToneError
	ToneSuccess
	ToneTitle
	ToneHeading
	ToneAlert
	ToneFrame
)

// Stretch of one output line shown in a single tone
type Span struct {
	Text string
	Tone Tone
}

// One output line of a config operation, written to stderr when Stderr is set
type Line struct {
	Spans  []Span
	Stderr bool
}

// Text of the line without tones
func (l Line) String() string {
	var b strings.Builder
	for _, s := range l.Spans {
		b.WriteString(s.Text)
	}
	return b.String()
}

// Line of plain text on stdout
func plainLine(text string) Line { return Line{Spans: []Span{{Text: text}}} }

// Line in one tone on stdout
func tonedLine(tone Tone, text string) Line { return Line{Spans: []Span{{Text: text, Tone: tone}}} }

// Line in one tone on stderr
func stderrLine(tone Tone, text string) Line {
	return Line{Spans: []Span{{Text: text, Tone: tone}}, Stderr: true}
}

// Refusal of a config command, its text what the command line prints on stderr before exiting with status one
type CommandError struct {
	Text string
	Err  error
}

// The text to print
func (e *CommandError) Error() string { return e.Text }

// The refusal behind the text
func (e *CommandError) Unwrap() error { return e.Err }

// Command refusal for a failed write, a backend refusal under its Error prefix and any other under a cross
func writeFailure(err error) error {
	var seam *SeamError
	var cmd *CommandError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &seam), errors.As(err, &cmd), errors.Is(err, ErrBackendUnavailable):
		return err
	}
	var we *WriteError
	var le *LockedError
	var re *RejectedValueError
	if errors.As(err, &we) || errors.As(err, &le) || errors.As(err, &re) {
		return &CommandError{Text: "Error: " + err.Error(), Err: err}
	}
	if strings.HasPrefix(err.Error(), "✗ ") {
		return err
	}
	return &CommandError{Text: "✗ " + err.Error(), Err: err}
}

// Config key that is empty, padded with whitespace or holds an empty path segment
type InvalidKeyError struct {
	Key          string
	EmptySegment bool
}

// The refusal naming what is wrong with the key
func (e *InvalidKeyError) Error() string {
	if e.EmptySegment {
		return fmt.Sprintf("✗ Invalid config key: %s — contains an empty path segment (leading, trailing, or doubled '.').", quoted(e.Key))
	}
	return fmt.Sprintf("✗ Invalid config key: %s (empty or surrounding whitespace).", quoted(e.Key))
}

// Matches ErrWriteRefused
func (e *InvalidKeyError) Unwrap() error { return ErrWriteRefused }

// Known key given under a wrong prefix, refused before anything is written
type UnknownKeyError struct {
	Key        string
	Suggestion string
}

// Refusal text split into toned output rows
func (e *UnknownKeyError) Lines() []Line {
	out := []Line{stderrLine(ToneError, fmt.Sprintf("✗ '%s' is not a recognized config key — nothing was written.", e.Key))}
	if e.Suggestion != "" {
		out = append(out, stderrLine(ToneWarning, "  Did you mean: "+e.Suggestion))
	}
	return append(out, stderrLine(ToneDim, "  (Custom top-level keys are supported; use --force to write this path anyway.)"))
}

// The refusal as plain text
func (e *UnknownKeyError) Error() string { return joinLines(e.Lines()) }

// Matches ErrWriteRefused
func (e *UnknownKeyError) Unwrap() error { return ErrWriteRefused }

// Plain text of lines joined by line breaks
func joinLines(lines []Line) string {
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = l.String()
	}
	return strings.Join(texts, "\n")
}

// Key display.language given a value no message catalog supplies
type LanguageError struct {
	Value     string
	Available []string
}

// The refusal listing the languages on offer and how to add one
func (e *LanguageError) Error() string {
	return fmt.Sprintf("✗ Unknown language %s for display.language. Available: %s.\n  Install a language pack plugin "+
		"(icebath plugins install <pack>) or drop <ICEBATH_HOME>/locales/<id>.yaml to add one.",
		quoted(e.Value), strings.Join(e.Available, ", "))
}

// Matches ErrWriteRefused
func (e *LanguageError) Unwrap() error { return ErrWriteRefused }

// Config key that neither the config nor the env file holds
type KeyNotSetError struct {
	Key string
}

// The line the command prints
func (e *KeyNotSetError) Error() string { return "Config key not set: " + e.Key }

// Value tree of a typed config, plain and safe to edit
func treeOf(cfg *Config) (*yaml.Node, error) {
	n, err := encodeValue(reflect.ValueOf(cfg))
	if err != nil {
		return nil, err
	}
	return plainCopy(n), nil
}

// Location of the config and of the env file of the home of ctx
func (s *Store) Paths(ctx context.Context) (configPath, envPath string, err error) {
	h, path, err := s.configPath(ctx)
	if err != nil {
		return "", "", err
	}
	return path, home.EnvFile.In(h), nil
}

// Context marker of writes the program makes by itself rather than for a choice of the user
type appliedWriteKey struct{}

// Context in which config writes are the program applying its own changes, such as migrations
func AppliedWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, appliedWriteKey{}, true)
}

// Reports a context whose config writes are the program applying its own changes
func IsAppliedWrite(ctx context.Context) bool {
	v, _ := ctx.Value(appliedWriteKey{}).(bool)
	return v
}

// Replaces the raw user config under the held write lock and tells the save observer what changed
func (s *Store) writeUserConfig(ctx, locked context.Context, path string, before, root *yaml.Node) error {
	if err := s.EnsureHome(locked); err != nil {
		return err
	}
	if err := s.writeState(locked, path, root, true, ""); err != nil {
		return err
	}
	s.cacheMu.Lock()
	delete(s.raws, path)
	s.cacheMu.Unlock()
	if s.onSave != nil {
		s.onSave(ctx, &YAMLDocument{root: before}, &YAMLDocument{root: plainCopy(root)})
	}
	return nil
}

// What a config set did, with every line the command prints in order
type SetResult struct {
	Key         string
	Value       *yaml.Node
	InEnvFile   bool
	Destination string
	Written     bool
	Lines       []Line
}

// Refusal line for a package manager owned install, nil when the install takes writes
func (s *Store) managedInstallLine(ctx context.Context, action string) (*Line, error) {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	sys := home.ManagedSystem(h)
	if sys == "" {
		return nil, nil
	}
	l := stderrLine(TonePlain, ManagedInstallMessage(action, sys))
	return &l, nil
}

// Error of an env file write as a config command reports it
func envCommandError(err error) error {
	if errors.Is(err, ErrEnvWriteRefused) {
		return &CommandError{Text: "✗ " + err.Error(), Err: err}
	}
	return err
}

// Refusal of display.language for a value no catalog supplies, nil for an acceptable one
func (s *Store) languageRefusal(ctx context.Context, value string) error {
	if trimEnvSpace(value) == "" {
		return nil
	}
	if s.know.Languages == nil {
		return &SeamError{Seam: "languages"}
	}
	if _, ok := s.know.Languages.Resolve(ctx, value); ok {
		return nil
	}
	available, err := s.know.Languages.Supported(ctx)
	if err != nil {
		return err
	}
	return &LanguageError{Value: value, Available: available}
}

// Route keys a provider switch clears from the model block, with whether a kept endpoint is of unknown ownership
func (s *Store) dropStaleModelRoute(ctx context.Context, root *yaml.Node, provider string) ([]KeyValue, bool, error) {
	model := childOf(root, "model")
	if !isMapping(model) {
		return nil, false, nil
	}
	baseURL := trimEnvSpace(orText(childOf(model, "base_url")))
	if baseURL != "" {
		if s.know.RouteOwner == nil {
			return nil, false, &SeamError{Seam: "route owner"}
		}
		owned, known := s.know.RouteOwner(ctx, provider, baseURL, plainCopy(root))
		switch {
		case !known:
			return nil, true, nil
		case owned:
			return nil, false, nil
		}
	}
	var popped []KeyValue
	for _, k := range []string{"base_url", "api_mode"} {
		v := childOf(model, k)
		if v == nil || isNull(v) || (isStringNode(v) && v.Value == "") {
			continue
		}
		popped = append(popped, KeyValue{Path: k, Value: plainCopy(v)})
		dropKey(model, k)
	}
	return popped, false, nil
}

// Sets one config key from command line text, routing credential and env shaped keys to the env file
func (s *Store) SetValue(ctx context.Context, key, value string, force bool) (*SetResult, error) {
	res := &SetResult{Key: key}
	if l, err := s.managedInstallLine(ctx, "set configuration values"); err != nil || l != nil {
		if l != nil {
			res.Lines = append(res.Lines, *l)
		}
		return res, err
	}
	if key != trimEnvSpace(key) || trimEnvSpace(key) == "" {
		return nil, &InvalidKeyError{Key: key}
	}
	if slices.Contains(SplitKeyPath(key), "") {
		return nil, &InvalidKeyError{Key: key, EmptySegment: true}
	}
	if err := s.CheckKeyWritable(ctx, "set", key); err != nil {
		return nil, err
	}
	_, envPath, err := s.Paths(ctx)
	if err != nil {
		return nil, err
	}
	env := s.Env()
	if IsEnvConfigKey(key) {
		if _, err := env.SaveCredential(ctx, upperText(key), value); err != nil {
			return nil, envCommandError(err)
		}
		res.InEnvFile, res.Written, res.Destination = true, true, envPath
		res.Lines = append(res.Lines, plainLine(fmt.Sprintf("✓ Set %s in %s", key, envPath)))
		return res, nil
	}
	if s.envs.registry.IsSettingKey(key) {
		if err := env.SaveSetting(ctx, key, value); err != nil {
			return nil, envCommandError(err)
		}
		res.Key, res.InEnvFile, res.Written, res.Destination = upperText(key), true, true, envPath
		res.Lines = append(res.Lines, plainLine(fmt.Sprintf("✓ Set %s in %s", upperText(key), envPath)))
		return res, nil
	}
	legacy := legacyGatewayPlatformsKey(key)
	key, note := redirectPlatformDisplayKey(key)
	if note != "" {
		res.Lines = append(res.Lines, plainLine(note))
	}
	res.Key = key
	verdict, err := s.ValidateKey(ctx, key)
	if err != nil {
		return res, err
	}
	if !verdict.Known && !force {
		wrong, err := s.isWrongPrefixSuggestion(ctx, key, verdict.Suggestion)
		if err != nil {
			return res, err
		}
		if wrong {
			return res, &UnknownKeyError{Key: key, Suggestion: verdict.Suggestion}
		}
	}
	if key == "display.language" {
		if err := s.languageRefusal(ctx, value); err != nil {
			return res, err
		}
	}
	defaults, err := s.defaults()
	if err != nil {
		return res, err
	}
	h, path, err := s.configPath(ctx)
	if err != nil {
		return res, err
	}
	locked, release, err := s.lockWrite(ctx, path)
	if err != nil {
		return res, writeFailure(err)
	}
	stored, routeNotice, err := s.setLocked(ctx, locked, res, defaults, path, legacy, &key, value, force)
	if err = errors.Join(err, release()); err != nil {
		return res, writeFailure(err)
	}
	res.Key, res.Value, res.Written = key, stored, true
	if envVar, ok := TerminalEnvVarForKey(key); ok && key != "terminal.cwd" {
		refusal, err := env.WriteRefusal(ctx, envVar, "set")
		switch {
		case err != nil:
			return res, err
		case refusal != "":
			res.Lines = append(res.Lines, stderrLine(TonePlain, refusal))
		default:
			if err := env.Set(ctx, envVar, TerminalEnvValue(stored)); err != nil {
				return res, envCommandError(err)
			}
		}
	}
	if key == "display.skin" && isStringNode(stored) && stored.Value != "" {
		skin := filepath.Join(home.Skins.In(h), stored.Value+".yaml")
		if _, err := os.Stat(skin); err == nil {
			now := time.Now()
			if err := os.Chtimes(skin, now, now); err != nil {
				slog.Debug(fmt.Sprintf("Could not touch skin file %s: %v", skin, err))
			}
		}
	}
	shown := displayText(stored)
	if IsSecretConfigKey(key) && isStringNode(stored) && stored.Value != "" {
		shown = MaskSecret(stored.Value)
	}
	b, err := s.backend(ctx)
	if err != nil {
		return res, err
	}
	res.Destination = b.Destination(h)
	res.Lines = append(res.Lines, plainLine(fmt.Sprintf("✓ Set %s = %s in %s", key, shown, res.Destination)))
	if routeNotice != nil {
		res.Lines = append(res.Lines, *routeNotice)
	}
	if !verdict.Known && !force {
		res.Lines = append(res.Lines, unknownKeyNotice(key, verdict.Suggestion)...)
	}
	return res, nil
}

// Lines telling the person a saved key is one the schema does not know
func unknownKeyNotice(key, suggestion string) []Line {
	out := []Line{tonedLine(ToneWarning, fmt.Sprintf("⚠ '%s' is not a recognized config key — it was saved anyway, but icebath may not read it.", key))}
	if suggestion != "" {
		out = append(out, tonedLine(ToneWarning, "  Did you mean: "+suggestion))
	}
	if len(SplitKeyPath(key)) == 1 {
		return append(out, tonedLine(ToneDim, "  (Custom top-level keys are supported and bridged to the environment for skills/external tools. Use --force to skip this notice.)"))
	}
	return append(out, tonedLine(ToneDim, "  (Use --force to skip this notice.)"))
}

// Coerces, guards and writes one config key with the write lock held, returning the value stored and the route notice
func (s *Store) setLocked(ctx, locked context.Context, res *SetResult, defaults *yaml.Node, path, legacy string, key *string, value string, force bool) (*yaml.Node, *Line, error) {
	user, err := s.requireReadable(locked, path)
	if err != nil {
		return nil, nil, err
	}
	before := plainCopy(user)
	stored, err := coerceSetValue(defaults, *key, value)
	if err != nil {
		return nil, nil, err
	}
	model := childOf(user, "model")
	oldProvider := ""
	if isMapping(model) {
		oldProvider = orText(childOf(model, "provider"))
	}
	if strings.HasPrefix(strings.ToLower(trimEnvSpace(*key)), "model.") && isStringNode(model) && model.Value != "" {
		wrapped := emptyMapping()
		putKey(wrapped, "default", plainCopy(model))
		putKey(user, "model", wrapped)
	}
	guarded, line, err := guardSectionOverwrite(user, *key, stored, force)
	if err != nil {
		return nil, nil, err
	}
	if line != "" {
		res.Lines = append(res.Lines, plainLine(line))
	}
	*key = guarded
	if stored, err = guardContainerType(defaults, user, *key, stored, force); err != nil {
		return nil, nil, err
	}
	if err := setNested(user, *key, plainCopy(stored)); err != nil {
		return nil, nil, &CommandError{Text: "✗ " + strings.TrimPrefix(err.Error(), ErrNotNavigable.Error()+": "), Err: err}
	}
	if legacy != "" && unsetNested(user, legacy) {
		res.Lines = append(res.Lines, plainLine(fmt.Sprintf("  (removed the shadowed %s duplicate)", legacy)))
	}
	var routeNotice *Line
	if oldProvider = trimEnvSpace(oldProvider); oldProvider == "" {
		oldProvider = "the previous provider"
	}
	provider := displayText(stored)
	if *key == "model.provider" && strings.ToLower(oldProvider) != strings.ToLower(trimEnvSpace(provider)) {
		popped, unverified, err := s.dropStaleModelRoute(locked, user, provider)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case len(popped) > 0:
			parts := make([]string, len(popped))
			for i, kv := range popped {
				parts[i] = fmt.Sprintf("model.%s (%s)", kv.Path, displayText(kv.Value.(*yaml.Node)))
			}
			l := plainLine("  Cleared " + strings.Join(parts, ", ") + fmt.Sprintf(" — that route belonged to %s, not %s. %s's endpoint resolves "+
				"automatically; set model.base_url again if you meant a custom endpoint.", oldProvider, provider, provider))
			routeNotice = &l
		case unverified:
			l := tonedLine(ToneWarning, fmt.Sprintf("⚠ model.base_url (%s) was set under %s and still applies to %s — requests go there. "+
				"If it is not %s's endpoint: `icebath config unset model.base_url` (and model.api_mode).",
				displayText(treeGet(user, "model", "base_url")), oldProvider, provider, provider))
			routeNotice = &l
		}
	}
	if lower := strings.ToLower(trimEnvSpace(*key)); lower == "model.api_base" || lower == "api_base" {
		user = normalizeRootModelKeys(user)
		*key = "model.base_url"
		res.Lines = append(res.Lines, plainLine("  (note: 'api_base' is an alias — saved as model.base_url)"))
	}
	return stored, routeNotice, s.writeUserConfig(ctx, PinningKeys(locked, *key), path, before, user)
}

// What a config get resolved, the text to print and the notices that follow it on stderr
type GetResult struct {
	Key   string
	Value *yaml.Node
	Text  string
	Lines []Line
}

// Resolves one config key for display, credentials masked unless raw is set or redaction is off
func (s *Store) GetValue(ctx context.Context, key string, asJSON, raw bool) (*GetResult, error) {
	res := &GetResult{}
	env := s.Env()
	var value *yaml.Node
	switch {
	case IsEnvConfigKey(key):
		v, ok, err := env.Get(ctx, upperText(key))
		if err != nil {
			return nil, err
		}
		if ok {
			value = stringNode(v)
		}
	case s.envs.registry.IsSettingKey(key):
		setting, ok, err := env.ReadSetting(ctx, key)
		if err != nil {
			return nil, err
		}
		if ok {
			value = setting.Value
			if setting.Stale {
				res.Lines = append(res.Lines, stderrLine(TonePlain, StaleSettingNote(key)))
			}
		}
	default:
		legacy := legacyGatewayPlatformsKey(key)
		key, _ = redirectPlatformDisplayKey(key)
		tree, err := s.mergedTree(ctx)
		if err != nil {
			return nil, err
		}
		if n, ok := getNested(tree, key); ok {
			value = n
		} else if legacy != "" {
			if n, ok := getNested(tree, legacy); ok {
				value = n
			}
		}
	}
	if value == nil {
		return nil, &KeyNotSetError{Key: key}
	}
	if !raw {
		if s.know.RedactionEnabled == nil {
			return nil, &SeamError{Seam: "redaction switch"}
		}
		if s.know.RedactionEnabled(ctx) {
			if isStringNode(value) {
				if IsSecretConfigKey(key) && !envReferenceOnly.MatchString(value.Value) {
					value = stringNode(MaskSecret(value.Value))
				}
			} else {
				value = RedactConfigValue(value)
			}
		}
	}
	res.Key, res.Value, res.Text = key, plainCopy(value), FormatValue(value, asJSON)
	defaults, err := s.defaults()
	if err != nil {
		return nil, err
	}
	if slices.Contains(knownTopLevelKeys(defaults), SplitKeyPath(key)[0]) {
		verdict, err := s.ValidateKey(ctx, key)
		if err != nil {
			return nil, err
		}
		if !verdict.Known {
			res.Lines = append(res.Lines, stderrLine(ToneWarning, fmt.Sprintf(
				"⚠ '%s' is not a recognized config key — icebath may not read it; the value printed above comes from your config file.", key)))
			if verdict.Suggestion != "" {
				res.Lines = append(res.Lines, stderrLine(ToneWarning, "  Did you mean: "+verdict.Suggestion))
			}
		}
	}
	return res, nil
}

// What a config unset did, with every line the command prints in order
type UnsetResult struct {
	Key         string
	InEnvFile   bool
	Destination string
	Removed     bool
	Lines       []Line
}

// Removes one user set config key or env file value
func (s *Store) UnsetValue(ctx context.Context, key string) (*UnsetResult, error) {
	res := &UnsetResult{Key: key}
	if l, err := s.managedInstallLine(ctx, "unset configuration values"); err != nil || l != nil {
		if l != nil {
			res.Lines = append(res.Lines, *l)
		}
		return res, err
	}
	if err := s.CheckKeyWritable(ctx, "unset", key); err != nil {
		return nil, err
	}
	h, path, err := s.configPath(ctx)
	if err != nil {
		return nil, err
	}
	envPath := home.EnvFile.In(h)
	env := s.Env()
	envKey, settingKey := IsEnvConfigKey(key), s.envs.registry.IsSettingKey(key)
	if envKey || settingKey {
		var found bool
		if envKey {
			var removal CredentialRemoval
			removal, err = env.RemoveCredential(ctx, upperText(key))
			found = removal.Found
		} else {
			found, err = env.RemoveSetting(ctx, key)
		}
		if err != nil {
			return nil, envCommandError(err)
		}
		if !found {
			return nil, &KeyNotSetError{Key: key}
		}
		res.InEnvFile, res.Removed, res.Destination = true, true, envPath
		res.Lines = append(res.Lines, plainLine(fmt.Sprintf("✓ Unset %s from %s", key, envPath)))
		return res, nil
	}
	locked, release, err := s.lockWrite(ctx, path)
	if err != nil {
		return nil, writeFailure(err)
	}
	user, err := s.requireReadable(locked, path)
	if err != nil {
		return nil, writeFailure(errors.Join(err, release()))
	}
	before := plainCopy(user)
	legacy := legacyGatewayPlatformsKey(key)
	key, note := redirectPlatformDisplayKey(key)
	if note != "" {
		res.Lines = append(res.Lines, plainLine(strings.ReplaceAll(note, "saved as", "resolved as")))
	}
	removed := unsetNested(user, key)
	if legacy != "" {
		removed = unsetNested(user, legacy) || removed
	}
	envVar, bridged := TerminalEnvVarForKey(key)
	if !bridged {
		envVar, bridged = leftCoreTerminalKeys()[key]
	}
	if bridged && key != "terminal.cwd" {
		refusal, err := env.WriteRefusal(ctx, envVar, "remove")
		switch {
		case err != nil:
			return nil, errors.Join(err, release())
		case refusal != "":
			res.Lines = append(res.Lines, stderrLine(TonePlain, refusal))
		default:
			gone, err := env.Remove(ctx, envVar)
			if err != nil {
				return nil, errors.Join(envCommandError(err), release())
			}
			removed = gone || removed
		}
	}
	if !removed {
		return nil, errors.Join(&KeyNotSetError{Key: key}, release())
	}
	if err := errors.Join(s.writeUserConfig(ctx, locked, path, before, user), release()); err != nil {
		return nil, writeFailure(err)
	}
	b, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	res.Key, res.Removed, res.Destination = key, true, b.Destination(h)
	res.Lines = append(res.Lines, plainLine(fmt.Sprintf("✓ Unset %s from %s", key, res.Destination)))
	return res, nil
}
