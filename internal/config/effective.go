package config

import (
	"context"
	"errors"
	"reflect"

	"gopkg.in/yaml.v3"
)

// Source that supplies one effective config value
type Layer int

// Config layers, lowest precedence first
const (
	LayerDefault Layer = iota
	LayerUser
	LayerEnvironment
	LayerManaged
)

// Name of the layer as reports print it
func (l Layer) String() string {
	switch l {
	case LayerUser:
		return "user"
	case LayerEnvironment:
		return "environment"
	case LayerManaged:
		return "managed"
	}
	return "default"
}

// Effective value at one dotted key and the layer that supplied it
type EffectiveValue struct {
	Value *yaml.Node
	Layer Layer
}

// Cached effective user view of one config path
type effectiveEntry struct {
	sig loadSig
	doc *YAMLDocument
	env map[string]envReading
}

// Value at a dotted key of the merged config of ctx with its layer, ok false when nothing holds it
func (s *Store) Effective(ctx context.Context, key string) (EffectiveValue, bool, error) {
	cfg, err := s.LoadReadonly(ctx)
	if err != nil {
		return EffectiveValue{}, false, err
	}
	merged, err := encodeValue(reflect.ValueOf(cfg))
	if err != nil {
		return EffectiveValue{}, false, err
	}
	value, ok := getNested(plainCopy(merged), key)
	if !ok {
		return EffectiveValue{}, false, nil
	}
	out := EffectiveValue{Value: plainCopy(value), Layer: LayerDefault}
	if cfg.origin == nil {
		return out, true, nil
	}
	if _, managed := getNested(cfg.origin.managed, key); managed {
		out.Layer = LayerManaged
		return out, true, nil
	}
	if _, user := getNested(cfg.origin.user, key); !user {
		return out, true, nil
	}
	out.Layer = LayerUser
	raw, err := s.RawDocumentReadonly(ctx)
	if err != nil {
		return EffectiveValue{}, false, err
	}
	if written, ok := getNested(canonicalize(moveRootMaxTurns(raw.root)), key); ok && isStringNode(written) && envRef.MatchString(written.Value) &&
		s.expandString(ctx, written.Value) != written.Value {
		out.Layer = LayerEnvironment
	}
	return out, true, nil
}

// User layer of ctx with references expanded, the managed overlay applied and the model keys canonical, without defaults
func (s *Store) UserEffective(ctx context.Context, failClosed bool) (*YAMLDocument, error) {
	b, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	if s.worker != nil {
		return &YAMLDocument{root: s.effectiveOf(ctx, b, plainCopy(s.worker.root))}, nil
	}
	h, path, err := s.configPath(ctx)
	if err != nil {
		return nil, err
	}
	ctx, unlock := s.Lock(ctx)
	defer unlock()
	sig, cacheable, err := s.loadSignature(ctx, b, h)
	if err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	e := s.effects[path]
	s.cacheMu.Unlock()
	if e != nil && cacheable && e.sig == sig && s.envUnchanged(ctx, e.env) {
		return e.doc.Clone(), nil
	}
	raw, recovered, err := s.effectiveRaw(ctx, b, h, path, sig, failClosed)
	if err != nil {
		return nil, err
	}
	env := map[string]envReading{}
	s.envSnapshot(ctx, raw, env)
	s.envSnapshot(ctx, s.managedTree(b), env)
	doc := &YAMLDocument{root: s.effectiveOf(ctx, b, raw)}
	s.cacheMu.Lock()
	if cacheable && !recovered {
		s.effects[path] = &effectiveEntry{sig: sig, doc: doc.Clone(), env: env}
	} else {
		delete(s.effects, path)
	}
	s.cacheMu.Unlock()
	return doc, nil
}

// Raw user mapping behind the effective view, the last good one standing in for a broken file unless failing closed
func (s *Store) effectiveRaw(ctx context.Context, b Backend, h, path string, sig loadSig, failClosed bool) (*yaml.Node, bool, error) {
	if !sig.hasUser {
		return emptyMapping(), false, nil
	}
	s.cacheMu.Lock()
	cached := s.raws[path]
	s.cacheMu.Unlock()
	if cached != nil && cached.version == sig.user {
		raw := plainCopy(cached.doc.root)
		s.cacheMu.Lock()
		if s.lastUser[path] == nil {
			s.lastUser[path] = plainCopy(raw)
		}
		s.cacheMu.Unlock()
		return raw, false, nil
	}
	doc, err := b.Read(ctx, h)
	if err != nil {
		if failClosed || errors.Is(err, ErrBackendUnavailable) {
			return nil, false, err
		}
		return s.recoverUserRaw(ctx, path, err), true, nil
	}
	raw := emptyMapping()
	if isMapping(doc.root) {
		raw = plainCopy(doc.root)
	}
	s.cacheMu.Lock()
	s.raws[path] = &rawEntry{version: sig.user, doc: &YAMLDocument{raw: doc.raw, src: doc.src, doc: doc.doc, root: plainCopy(raw)}}
	s.lastUser[path] = plainCopy(raw)
	s.cacheMu.Unlock()
	s.backupConfig(ctx, path, "good")
	return raw, false, nil
}

// Last good raw user mapping of this process, else the newest good backup, else an empty mapping, with the warning
func (s *Store) recoverUserRaw(ctx context.Context, path string, readErr error) *yaml.Node {
	s.cacheMu.Lock()
	raw := s.lastUser[path]
	s.cacheMu.Unlock()
	fallback := fallbackLastGood
	if raw == nil {
		if good, ok := s.newestGoodBackup(ctx, path); ok {
			raw = good
		}
		fallback = fallbackLastGoodFile
	}
	if raw == nil {
		fallback = fallbackDefaults
	}
	s.warnParseFailure(ctx, path, readErr, fallback)
	if raw == nil {
		return emptyMapping()
	}
	return plainCopy(raw)
}

// Raw user mapping expanded, overlaid with the managed layer and given its canonical model section
func (s *Store) effectiveOf(ctx context.Context, b Backend, raw *yaml.Node) *yaml.Node {
	merged := s.expandTree(ctx, raw)
	if overlay := s.managedViewOverlay(ctx, s.managedTree(b)); overlay != nil {
		merged = deepMerge(merged, overlay)
	}
	return normalizeRootModelKeys(merged)
}
