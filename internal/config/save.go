package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

// Commented security block appended when a save creates the file and redaction is not configured
const securityComment = `
# ── Security ──────────────────────────────────────────────────────────
# Secret redaction is ON by default — strings that look like API keys,
# tokens, and passwords are masked in tool output, logs, and chat
# responses before the model or user ever sees them. Set redact_secrets
# to false to disable (e.g. when developing the redactor itself).
#
# security:
#   redact_secrets: true
`

// Commented fallback block appended when a save creates the file and no fallback route is configured
const fallbackComment = `
# ── Fallback Model ────────────────────────────────────────────────────
# Automatic provider failover when primary is unavailable.
# Uncomment and configure to enable. Triggers on rate limits (429),
# overload (529), service errors (503), or connection failures.
#
# Supported providers:
#   openrouter   (OPENROUTER_API_KEY)  — routes to any model
#   openai-codex (OAuth — icebath auth) — OpenAI Codex
#   portal       (OAuth — icebath auth) — Icebath Portal
#   zai          (ZAI_API_KEY)         — Z.AI / GLM
#   kimi-coding  (KIMI_API_KEY)        — Kimi / Moonshot
#   kimi-coding-cn (KIMI_CN_API_KEY)   — Kimi / Moonshot (China)
#   minimax      (MINIMAX_API_KEY)     — MiniMax
#   minimax-cn   (MINIMAX_CN_API_KEY)  — MiniMax (China)
#   bedrock      (AWS IAM / boto3)     — AWS Bedrock (Converse API)
#
# For custom OpenAI-compatible endpoints, add base_url and key_env.
#
# fallback_model:
#   provider: openrouter
#   model: anthropic/claude-sonnet-4
`

// User refusal for a change a package manager owned install does not allow
func ManagedInstallMessage(action, system string) string {
	if system == "" {
		system = "a package manager"
	}
	return fmt.Sprintf("Cannot %s: this icebath installation is managed by %s.\nUse your package manager to upgrade or reinstall icebath.", action, system)
}

// Settings of one save
type saveOptions struct {
	keepDefaults  bool
	preserve      [][]string
	mergeExisting bool
}

// Setting of one save
type SaveOption func(*saveOptions)

// Writes values equal to the defaults too instead of leaving them out
func KeepDefaults() SaveOption { return func(o *saveOptions) { o.keepDefaults = true } }

// Keeps these key paths even when their value equals the default
func PreservePaths(paths ...[]string) SaveOption {
	return func(o *saveOptions) { o.preserve = append(o.preserve, paths...) }
}

// Merges the saved state over the file so sections the caller left out stay as written
func MergeExisting() SaveOption { return func(o *saveOptions) { o.mergeExisting = true } }

// Saves a config, only values the user set or that differ from the defaults written, references and comments kept
func (s *Store) Save(ctx context.Context, cfg *Config, opts ...SaveOption) error {
	return s.saveConfig(ctx, cfg, nil, opts)
}

// Saves a document as the config state the way a typed save does, sections it omits deleted unless merged
func (s *Store) SaveDocument(ctx context.Context, doc *YAMLDocument, opts ...SaveOption) error {
	if doc == nil {
		doc = NewYAMLDocument()
	}
	root := plainCopy(doc.root)
	return s.saveTree(ctx, func(context.Context, *yaml.Node) (*yaml.Node, *yaml.Node) { return root, nil }, doc.readErr, opts)
}

// Typed save, the changes made since load applied onto the user file as it is under the write lock
func (s *Store) saveConfig(ctx context.Context, cfg *Config, edit func(after *yaml.Node), opts []SaveOption) error {
	n, err := encodeValue(reflect.ValueOf(cfg))
	if err != nil {
		return err
	}
	after := plainCopy(n)
	if edit != nil {
		edit(after)
	}
	ctx = withManagedNotes(ctx, after)
	h, err := s.homes.Home(ctx)
	if err != nil {
		return err
	}
	build := func(context.Context, *yaml.Node) (*yaml.Node, *yaml.Node) { return after, nil }
	if s.worker == nil && cfg.origin != nil && cfg.origin.baseline != nil && cfg.origin.home == h {
		build = func(ctx context.Context, raw *yaml.Node) (*yaml.Node, *yaml.Node) {
			base := s.expandTree(ctx, canonicalize(moveRootMaxTurns(raw)))
			input := applyDelta(base, cfg.origin.baseline, after)
			if stamp := childOf(after, "_config_version"); stamp != nil && mappingIndex(input, "_config_version") < 0 {
				input = plainCopy(input)
				putKey(input, "_config_version", plainCopy(stamp))
			}
			return input, cfg.origin.user
		}
	}
	return s.saveTree(ctx, build, cfg.ReadError(), opts)
}

// Context key of the full tree the managed key strip reports from
type managedNotesKey struct{}

// Context naming the full tree the managed key strip reports from
func withManagedNotes(ctx context.Context, n *yaml.Node) context.Context {
	return context.WithValue(ctx, managedNotesKey{}, n)
}

// State a save writes from the user file read under the write lock, with the expanded tree it loaded as
type stateBuilder func(ctx context.Context, raw *yaml.Node) (state, loaded *yaml.Node)

// Save pipeline over a config state under the write lock, the observer told afterwards
func (s *Store) saveTree(ctx context.Context, build stateBuilder, readErr error, opts []SaveOption) error {
	_, path, err := s.configPath(ctx)
	if err != nil {
		return err
	}
	locked, release, err := s.lockWrite(ctx, path)
	if err != nil {
		return err
	}
	before, after, err := s.saveLocked(locked, build, readErr, opts)
	err = errors.Join(err, release())
	if err == nil && after != nil && s.onSave != nil {
		s.onSave(ctx, before, after)
	}
	return err
}

// One save with the lock held, returning the user document before and the state written, nil when nothing was written
func (s *Store) saveLocked(ctx context.Context, build stateBuilder, readErr error, opts []SaveOption) (*YAMLDocument, *YAMLDocument, error) {
	var o saveOptions
	for _, opt := range opts {
		opt(&o)
	}
	h, path, err := s.configPath(ctx)
	if err != nil {
		return nil, nil, err
	}
	if sys := home.ManagedSystem(h); sys != "" {
		fmt.Fprintln(os.Stderr, ManagedInstallMessage("save configuration", sys))
		return nil, nil, nil
	}
	if err := s.refuseFailedRead(ctx, path, readErr); err != nil {
		return nil, nil, err
	}
	if err := s.EnsureHome(ctx); err != nil {
		return nil, nil, err
	}
	b, err := s.backend(ctx)
	if err != nil {
		return nil, nil, err
	}
	rawForPaths, err := s.requireReadable(b.PreserveReadBase(ctx), path)
	if err != nil {
		return nil, nil, err
	}
	input, loaded := build(ctx, rawForPaths)
	if !isMapping(input) {
		input = emptyMapping()
	}
	if input, err = s.stripManagedForSave(ctx, plainCopy(input)); err != nil {
		return nil, nil, err
	}
	config, err := s.rebaseForSave(ctx, input, rawForPaths, o.mergeExisting)
	if err != nil {
		return nil, nil, err
	}
	normalized := canonicalize(config)
	current := normalized
	if len(rawForPaths.Content) > 0 {
		if loaded == nil {
			s.cacheMu.Lock()
			loaded = s.lastExpanded(path)
			s.cacheMu.Unlock()
		}
		normalized = s.preserveEnvTemplates(ctx, normalized, canonicalize(rawForPaths), loaded)
	}
	if !o.keepDefaults {
		defaults, err := s.defaults()
		if err != nil {
			return nil, nil, err
		}
		preserve := explicitPaths(rawForPaths)
		for _, p := range o.preserve {
			preserve[pathKey(p)] = true
		}
		normalized = stripDefaults(normalized, defaults, preserve)
	}
	if s.firstStamp != nil && s.worker == nil && mappingIndex(rawForPaths, "_config_version") < 0 && mappingIndex(normalized, "_config_version") >= 0 {
		if err := s.firstStamp(ctx, h, rawForPaths, normalized); err != nil {
			return nil, nil, err
		}
	}
	if err := s.writeState(ctx, path, normalized, true, commentedSectionsForSave(normalized)); err != nil {
		return nil, nil, err
	}
	home.ApplySecureFilePolicy(path, h)
	s.cacheMu.Lock()
	delete(s.raws, path)
	s.lastGood[path] = plainCopy(current)
	s.cacheMu.Unlock()
	return &YAMLDocument{root: rawForPaths}, &YAMLDocument{root: plainCopy(current)}, nil
}

// Expanded user tree the last load or save of path left, the base template preservation compares against
func (s *Store) lastExpanded(path string) *yaml.Node {
	return s.lastGood[path]
}

// Tree without the leaves the managed layer pins, telling the user on stderr which ones were not saved
func (s *Store) stripManagedForSave(ctx context.Context, input *yaml.Node) (*yaml.Node, error) {
	keys, err := s.ManagedKeys(ctx)
	if err != nil || len(keys) == 0 {
		return input, err
	}
	stripped, removed := stripDottedKeys(input, keys)
	if full, ok := ctx.Value(managedNotesKey{}).(*yaml.Node); ok {
		_, removed = stripDottedKeys(full, keys)
	}
	if len(removed) > 0 {
		slices.Sort(removed)
		fmt.Fprintf(os.Stderr, "Note: %d managed setting(s) were not saved (managed by your administrator): %s\n", len(removed), strings.Join(removed, ", "))
	}
	return stripped, nil
}

// Save state against the live file, a frozen worker landing only its edits and a merging save keeping omissions
func (s *Store) rebaseForSave(ctx context.Context, input, liveRaw *yaml.Node, mergeExisting bool) (*yaml.Node, error) {
	if s.worker != nil {
		loaded, err := s.Load(ctx)
		if err != nil {
			return nil, err
		}
		base, err := encodeValue(reflect.ValueOf(loaded))
		if err != nil {
			return nil, err
		}
		frozen := s.worker.root
		if s.worker.ignoreUserConfig {
			defaults, err := s.defaults()
			if err != nil {
				return nil, err
			}
			frozen = schemaDefaultBranch(frozen, defaults)
		}
		return threeWay(liveRaw, input, plainCopy(base), frozen), nil
	}
	if mergeExisting && len(liveRaw.Content) > 0 {
		return mergePartialSave(liveRaw, input), nil
	}
	return input, nil
}

// Commented example blocks for features that are off or unconfigured
func commentedSectionsForSave(normalized *yaml.Node) string {
	var b strings.Builder
	if isNull(treeGet(normalized, "security", "redact_secrets")) {
		b.WriteString(securityComment)
	}
	fb := childOf(normalized, "fallback_model")
	entries := []*yaml.Node{fb}
	if isSequence(fb) {
		entries = fb.Content
	}
	configured := slices.ContainsFunc(entries, func(e *yaml.Node) bool {
		return isMapping(e) && isSet(childOf(e, "provider")) && isSet(childOf(e, "model"))
	})
	if !configured {
		b.WriteString(fallbackComment)
	}
	return b.String()
}

// Comment keeping write of a whole state, refusing to drop existing mapping paths unless omissions are allowed
func (s *Store) writeState(ctx context.Context, path string, data *yaml.Node, allowOmissions bool, extra string) error {
	b, h, err := s.route(ctx, path)
	if err != nil {
		return err
	}
	if !allowOmissions {
		existing, err := s.requireReadable(b.PreserveReadBase(ctx), path)
		if err != nil {
			return err
		}
		if omitted := omittedPaths(existing, data, nil); len(omitted) > 0 {
			shown := strings.Join(omitted[:min(12, len(omitted))], ", ")
			if len(omitted) > 12 {
				shown += fmt.Sprintf(", +%d more", len(omitted)-12)
			}
			return &RefusedWriteError{Path: path, Reason: "would lose settings omitted by this write", Cause: fmt.Errorf("omitted config paths: %s", shown),
				Fix: "Pass the complete current config, or replace the document only when deletion by omission is deliberate."}
		}
	}
	return b.WriteChanges(ctx, h, Changes{Document: &YAMLDocument{root: data}, ExtraOnCreate: extra})
}

// Writes a document over the config without letting it delete any existing setting it omits
func (s *Store) WriteDocument(ctx context.Context, doc *YAMLDocument) error {
	return s.writeDocument(ctx, doc, false)
}

// Writes a document as the complete config, deleting every setting it omits
func (s *Store) ReplaceDocument(ctx context.Context, doc *YAMLDocument) error {
	return s.writeDocument(ctx, doc, true)
}

// Shared document write under the write lock, a fallback document refused
func (s *Store) writeDocument(ctx context.Context, doc *YAMLDocument, allowOmissions bool) error {
	_, path, err := s.configPath(ctx)
	if err != nil {
		return err
	}
	if err := s.refuseFailedRead(ctx, path, doc.readErr); err != nil {
		return err
	}
	root := doc.root
	if root == nil || isNull(root) {
		root = emptyMapping()
	}
	locked, release, err := s.lockWrite(ctx, path)
	if err != nil {
		return err
	}
	return errors.Join(s.writeState(locked, path, root, allowOmissions, ""), release())
}

// Sets one dotted key in the config, a nil value removing it, comments and order of the rest kept
func (s *Store) WriteKey(ctx context.Context, key string, value any) error {
	return s.WriteKeys(ctx, []KeyValue{{Path: key, Value: value}})
}

// Sets related dotted keys as one write, a nil value removing its key, so a refusal changes none
func (s *Store) WriteKeys(ctx context.Context, updates []KeyValue) error {
	var c Changes
	for _, u := range updates {
		if u.Value == nil {
			c.Unset = append(c.Unset, u.Path)
			continue
		}
		c.Set = append(c.Set, u)
	}
	return s.WriteChanges(ctx, c)
}

// One write of changes, a nil value in Set a kept null or a removal from a file
func (s *Store) WriteChanges(ctx context.Context, c Changes) error {
	_, path, err := s.configPath(ctx)
	if err != nil {
		return err
	}
	locked, release, err := s.lockWrite(ctx, path)
	if err != nil {
		return err
	}
	b, h, err := s.route(locked, path)
	if err != nil {
		return errors.Join(err, release())
	}
	return errors.Join(b.WriteChanges(locked, h, c), release())
}

// Persists one messaging platform block field, from the raw user document when raw is set, else the loaded config
func (s *Store) WritePlatformField(ctx context.Context, platform, field string, value any, raw bool) error {
	n, err := nodeFor(value)
	if err != nil {
		return err
	}
	set := func(root *yaml.Node) {
		platforms := childOf(root, "platforms")
		if !isMapping(platforms) {
			platforms = emptyMapping()
			putKey(root, "platforms", platforms)
		}
		block := childOf(platforms, platform)
		if !isMapping(block) {
			block = emptyMapping()
			putKey(platforms, platform, block)
		}
		putKey(block, field, n)
	}
	if raw {
		_, path, err := s.configPath(ctx)
		if err != nil {
			return err
		}
		locked, release, err := s.lockWrite(ctx, path)
		if err != nil {
			return err
		}
		doc, err := s.RawDocument(locked)
		if err != nil {
			return errors.Join(err, release())
		}
		if !isMapping(doc.root) {
			doc.root = emptyMapping()
		}
		set(doc.root)
		return errors.Join(s.SaveDocument(locked, doc), release())
	}
	cfg, err := s.Load(ctx)
	if err != nil {
		return err
	}
	return s.saveConfig(ctx, cfg, set, nil)
}

// Key the managed layer pins, refused for a set or unset because the next load would put it back
type ManagedKeyError struct {
	Action string
	Key    string
	Source string
}

// The refusal and who to ask
func (e *ManagedKeyError) Error() string {
	return fmt.Sprintf("Cannot %s '%s': it is managed by your administrator (%s) and cannot be changed. Contact your administrator to modify it.", e.Action, e.Key, e.Source)
}

// Matches ErrWriteRefused
func (e *ManagedKeyError) Unwrap() error { return ErrWriteRefused }

// Refusal of a set or unset of key held by the managed layer or a backend lock, nil when writable
func (s *Store) CheckKeyWritable(ctx context.Context, action, key string) error {
	managed, err := s.IsKeyManaged(ctx, key)
	if err != nil {
		return err
	}
	if managed {
		source := "the managed scope"
		if p := managedFile("config.yaml"); p != "" {
			source = p
		}
		return &ManagedKeyError{Action: action, Key: key, Source: source}
	}
	b, err := s.backend(ctx)
	if err != nil {
		return err
	}
	h, err := s.homes.Home(ctx)
	if err != nil {
		return err
	}
	if level, locked := b.Locked(ctx, h, key); locked {
		return &LockedError{Path: key, LockedBy: level, Message: fmt.Sprintf(
			"Cannot %s '%s': it is locked by the %s level of Remote Config and cannot be changed from this agent.", action, key, level)}
	}
	return nil
}

// Comment keeping save of a whole state onto the file, unchanged nodes kept as written and absent keys deleted
func (s *Store) roundTripSave(ctx context.Context, path string, desired *YAMLDocument, extra string) error {
	if err := s.homes.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	if _, err := s.requireReadable(ctx, path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	creating := strings.TrimSpace(string(data)) == ""
	current, err := ParseYAMLDocument(data)
	if err != nil {
		return err
	}
	root := desired.root
	if root == nil || isNull(root) {
		root = emptyMapping()
	}
	out, err := current.withRoot(root).Render()
	if err != nil {
		return err
	}
	if creating {
		out = append(out, strings.ReplaceAll(extra, "\n", lineBreakOf(string(data)))...)
	}
	return writeConfigText(ctx, path, out)
}

// Comment keeping write of one dotted key, a nil value removing it and no write when its parent is missing
func (s *Store) roundTripUpdate(ctx context.Context, path, key string, value *yaml.Node) error {
	if err := s.homes.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	current, err := ParseYAMLDocument(data)
	if err != nil {
		return err
	}
	if badRoot(current) {
		return &YAMLError{Problem: fmt.Sprintf(nonMappingRootProblem, rootKindName(current))}
	}
	root := emptyMapping()
	if isMapping(current.root) {
		root = plainCopy(current.root)
	}
	if !writeNested(root, key, value) {
		return nil
	}
	out, err := current.withRoot(root).render(keyWriteStyle())
	if err != nil {
		return err
	}
	return writeConfigText(ctx, path, out)
}
