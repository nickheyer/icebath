package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"reflect"

	"gopkg.in/yaml.v3"
)

// Inputs one load decoded a config from, shared by every copy of that load and never changed
type loadOrigin struct {
	home      string
	user      *yaml.Node
	managed   *yaml.Node
	baseline  *yaml.Node
	readErr   error
	fieldErrs error
}

// Origin over the expanded user and managed trees, decoding once to record field errors and the baseline encoding
func newOrigin(h string, user, managed *yaml.Node) (*loadOrigin, *Config, error) {
	o := &loadOrigin{home: h, user: user, managed: managed}
	cfg, fieldErrs := o.decode()
	o.fieldErrs = fieldErrs
	base, err := encodeValue(reflect.ValueOf(cfg))
	if err != nil {
		return nil, nil, fmt.Errorf("encode the loaded config: %w", err)
	}
	o.baseline = plainCopy(base)
	return o, cfg, nil
}

// Fresh config decoded from the origin over the defaults, the managed layer over the user layer
func (o *loadOrigin) decode() (*Config, error) {
	cfg := Default()
	var errs []error
	if o.user != nil {
		errs = append(errs, cfg.UnmarshalYAML(o.user))
	}
	if o.managed != nil {
		errs = append(errs, cfg.UnmarshalYAML(o.managed))
	}
	cfg.origin = o
	return cfg, errors.Join(errs...)
}

// Origin standing in for a file that could not be read
func (o *loadOrigin) failed(err error) *loadOrigin {
	c := *o
	c.readErr = err
	return &c
}

// Signature the load cache matches on, the user layer version and the managed file version
type loadSig struct {
	user    Revision
	hasUser bool
	managed Revision
}

// Cached load of one config path with the variable values its expansion used
type loadEntry struct {
	sig    loadSig
	cfg    *Config
	origin *loadOrigin
	env    map[string]envReading
}

// Current load signature, ok false when neither the user layer nor a managed file exists
func (s *Store) loadSignature(ctx context.Context, b Backend, h string) (loadSig, bool, error) {
	var sig loadSig
	v, err := b.Version(ctx, h)
	switch {
	case err == nil:
		if sig.user, err = b.CacheVersion(ctx, h, v); err != nil {
			return sig, false, err
		}
		sig.hasUser = true
	case !errors.Is(err, fs.ErrNotExist):
		return sig, false, err
	}
	if path := managedFile("config.yaml"); path != "" {
		if mv, err := fileCacheVersion(path, false); err == nil {
			sig.managed = mv
		}
	}
	return sig, sig.hasUser || sig.managed != (Revision{}), nil
}

// Cached load still valid for the signature and the environment, a read failure served only while the file stays unreadable
func (s *Store) loadHit(ctx context.Context, b Backend, h, path string, sig loadSig) *loadEntry {
	s.cacheMu.Lock()
	e := s.loads[path]
	s.cacheMu.Unlock()
	if e == nil || e.sig != sig {
		return nil
	}
	if e.origin.readErr != nil && isReadError(e.origin.readErr) {
		if b.ProbeReadable(ctx, h) != nil {
			return e
		}
		return nil
	}
	if !s.envUnchanged(ctx, e.env) {
		return nil
	}
	return e
}

// Merged config of the home of ctx as an independent copy the caller may change and save
func (s *Store) Load(ctx context.Context) (*Config, error) {
	cfg, err := s.load(ctx, false)
	return cfg, err
}

// Merged config of the home of ctx shared with every other readonly caller, which none may change
func (s *Store) LoadReadonly(ctx context.Context) (*Config, error) {
	return s.load(ctx, true)
}

// Defaults, user file and managed overlay merged and expanded, cached on their versions and the variables referenced
func (s *Store) load(ctx context.Context, shared bool) (*Config, error) {
	if s.worker != nil {
		cfg := Default()
		fieldErrs := cfg.UnmarshalYAML(s.worker.root)
		cfg.origin = &loadOrigin{user: plainCopy(s.worker.root), fieldErrs: fieldErrs}
		return cfg, nil
	}
	h, path, err := s.configPath(ctx)
	if err != nil {
		return nil, err
	}
	b, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	pick := func(e *loadEntry) (*Config, error) {
		if shared {
			return e.cfg, nil
		}
		cfg, _ := e.origin.decode()
		return cfg, nil
	}
	if sig, ok, err := s.loadSignature(ctx, b, h); err == nil && ok {
		if e := s.loadHit(ctx, b, h, path, sig); e != nil {
			return pick(e)
		}
	}
	ctx, unlock := s.Lock(ctx)
	defer unlock()
	if err := s.EnsureHome(ctx); err != nil {
		return nil, err
	}
	sig, cacheable, err := s.loadSignature(ctx, b, h)
	if err != nil {
		return nil, err
	}
	if cacheable {
		if e := s.loadHit(ctx, b, h, path, sig); e != nil {
			return pick(e)
		}
	}
	e, err := s.buildLoad(ctx, b, h, path, sig)
	if err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	if cacheable {
		s.loads[path] = e
	} else {
		delete(s.loads, path)
	}
	s.cacheMu.Unlock()
	return pick(e)
}

// One full load, the last known good config standing in when the user file cannot be read or parsed
func (s *Store) buildLoad(ctx context.Context, b Backend, h, path string, sig loadSig) (*loadEntry, error) {
	user := emptyMapping()
	if sig.hasUser {
		doc, err := b.Read(ctx, h)
		if err == nil && badRoot(doc) {
			err = &YAMLError{Problem: fmt.Sprintf(nonMappingRootProblem, rootKindName(doc))}
		}
		if err != nil {
			if errors.Is(err, ErrBackendUnavailable) {
				return nil, err
			}
			return s.lastKnownGood(ctx, h, path, sig, err)
		}
		s.forgetFailure(path)
		if isMapping(doc.root) {
			user = plainCopy(doc.root)
		}
		s.backupConfig(ctx, path, "good")
	}
	managed := s.managedTree(b)
	canonical := canonicalize(moveRootMaxTurns(user))
	origin, cfg, err := s.originFor(ctx, h, canonical, managed)
	if err != nil {
		return nil, err
	}
	env := map[string]envReading{}
	s.envSnapshot(ctx, canonical, env)
	s.envSnapshot(ctx, managed, env)
	s.cacheMu.Lock()
	s.lastGood[path] = origin.user
	s.cacheMu.Unlock()
	s.noteConfigPolicy(h, origin)
	return &loadEntry{sig: sig, cfg: cfg, origin: origin, env: env}, nil
}

// Reports the redaction switch a loaded config carries, the managed value over the user value over the default on
func (s *Store) noteConfigPolicy(h string, o *loadOrigin) {
	raw, set := "True", true
	for _, layer := range []*yaml.Node{o.managed, o.user} {
		if at := mappingIndex(childOf(layer, "security"), "redact_secrets"); at >= 0 {
			value := childOf(layer, "security").Content[at]
			raw, set = displayText(value), !isNull(value)
			break
		}
	}
	s.envs.notePolicy(s.homeKey(h), PolicyConfig, raw, set)
}

// Origin of a canonical user tree, expanded and overlaid with the managed layer
func (s *Store) originFor(ctx context.Context, h string, canonical, managed *yaml.Node) (*loadOrigin, *Config, error) {
	return newOrigin(h, s.expandTree(ctx, canonical), s.managedOverlay(ctx, managed))
}

// Fallback for an unreadable user file, the config of this process, else the newest good backup, else the defaults
func (s *Store) lastKnownGood(ctx context.Context, h, path string, sig loadSig, readErr error) (*loadEntry, error) {
	b, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	lastTree := s.lastGood[path]
	s.cacheMu.Unlock()
	managed := s.managedTree(b)
	var lkg *loadOrigin
	fallback := fallbackLastGood
	if lastTree != nil {
		if lkg, _, err = s.originFor(ctx, h, lastTree, managed); err != nil {
			return nil, err
		}
	} else if raw, ok := s.newestGoodBackup(ctx, path); ok {
		if lkg, _, err = s.originFor(ctx, h, canonicalize(moveRootMaxTurns(raw)), managed); err != nil {
			return nil, err
		}
		fallback = fallbackLastGoodFile
	}
	if lkg == nil {
		fallback = fallbackDefaults
	}
	s.warnParseFailure(ctx, path, readErr, fallback)
	if lkg == nil {
		if lkg, _, err = s.originFor(ctx, h, canonicalize(emptyMapping()), managed); err != nil {
			return nil, err
		}
	}
	origin := lkg.failed(readErr)
	cfg, _ := origin.decode()
	return &loadEntry{sig: sig, cfg: cfg, origin: origin, env: map[string]envReading{}}, nil
}

// Cached raw user document of one config path
type rawEntry struct {
	version Revision
	doc     *YAMLDocument
}

// User document of the home of ctx as written without defaults or expansion, empty when absent, as a copy
func (s *Store) RawDocument(ctx context.Context) (*YAMLDocument, error) {
	return s.raw(ctx, false)
}

// User document as written shared with every other readonly caller, which none may change
func (s *Store) RawDocumentReadonly(ctx context.Context) (*YAMLDocument, error) {
	return s.raw(ctx, true)
}

// Raw read cached on the cache version, a read or parse failure served as an empty document carrying its error
func (s *Store) raw(ctx context.Context, shared bool) (*YAMLDocument, error) {
	if s.worker != nil {
		return &YAMLDocument{root: plainCopy(s.worker.root)}, nil
	}
	h, path, err := s.configPath(ctx)
	if err != nil {
		return nil, err
	}
	b, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	give := func(d *YAMLDocument) *YAMLDocument {
		if shared {
			return d
		}
		return d.Clone()
	}
	hit := func(v Revision) *YAMLDocument {
		s.cacheMu.Lock()
		defer s.cacheMu.Unlock()
		if e := s.raws[path]; e != nil && e.version == v {
			return e.doc
		}
		return nil
	}
	if v, err := s.cacheVersion(ctx, b, h); err == nil {
		if d := hit(v); d != nil {
			return give(d), nil
		}
	}
	ctx, unlock := s.Lock(ctx)
	defer unlock()
	v, err := s.cacheVersion(ctx, b, h)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return NewYAMLDocument(), nil
	case err != nil:
		return &YAMLDocument{root: emptyMapping(), readErr: err}, nil
	}
	if d := hit(v); d != nil {
		return give(d), nil
	}
	doc, err := b.Read(ctx, h)
	if err != nil {
		if errors.Is(err, ErrBackendUnavailable) {
			return nil, err
		}
		s.warnParseFailure(ctx, path, err, fallbackDefaults)
		return &YAMLDocument{root: emptyMapping(), readErr: err}, nil
	}
	if badRoot(doc) {
		return &YAMLDocument{root: emptyMapping(), readErr: &YAMLError{Problem: fmt.Sprintf(nonMappingRootProblem, rootKindName(doc))}}, nil
	}
	if !isMapping(doc.root) {
		doc = doc.withRoot(emptyMapping())
	}
	s.forgetFailure(path)
	s.cacheMu.Lock()
	s.raws[path] = &rawEntry{version: v, doc: doc}
	s.cacheMu.Unlock()
	return give(doc), nil
}

// Cache version of the user layer of h, an error matching fs.ErrNotExist when absent
func (s *Store) cacheVersion(ctx context.Context, b Backend, h string) (Revision, error) {
	v, err := b.Version(ctx, h)
	if err != nil {
		return Revision{}, err
	}
	return b.CacheVersion(ctx, h, v)
}

// User document read exactly as written for a write back, uncached, an empty document when absent, other failures returned
func (s *Store) ReadDocument(ctx context.Context) (*YAMLDocument, error) {
	_, path, err := s.configPath(ctx)
	if err != nil {
		return nil, err
	}
	b, h, err := s.route(ctx, path)
	if err != nil {
		return nil, err
	}
	doc, err := b.Read(ctx, h)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return NewYAMLDocument(), nil
	case err != nil:
		return nil, err
	case !doc.IsMapping() || doc.root == nil || isNull(doc.root):
		return doc.withRoot(emptyMapping()), nil
	}
	return doc, nil
}

// Mapping of an existing config file, refusing one that cannot be accessed, read or parsed before a write replaces it
func (s *Store) requireReadable(ctx context.Context, path string) (*yaml.Node, error) {
	b, h, err := s.route(ctx, path)
	if err != nil {
		return nil, err
	}
	if _, err := b.Version(ctx, h); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return emptyMapping(), nil
		}
		return nil, &RefusedWriteError{Path: path, Reason: "cannot be accessed", Fix: fixPermissions, Cause: err}
	}
	doc, err := b.Read(ctx, h)
	switch {
	case errors.Is(err, ErrBackendUnavailable):
		return nil, err
	case err != nil && isReadError(err):
		return nil, &RefusedWriteError{Path: path, Reason: "cannot be read", Fix: fixPermissions, Cause: err}
	case err != nil:
		s.warnParseFailure(ctx, path, err, fallbackRefusedWrite)
		return nil, &RefusedWriteError{Path: path, Reason: "has a formatting error", Fix: s.fixYAML(ctx), Cause: err}
	case doc.root == nil || isNull(doc.root):
		return emptyMapping(), nil
	case !doc.IsMapping():
		kind := rootKindName(doc)
		cause := &YAMLError{Problem: fmt.Sprintf(nonMappingRootProblem, kind)}
		s.warnParseFailure(ctx, path, cause, fallbackRefusedWrite)
		return nil, &RefusedWriteError{Path: path, Reason: fmt.Sprintf(nonMappingRootReasonTemplate, kind), Fix: s.fixYAML(ctx), Cause: cause}
	}
	return plainCopy(doc.root), nil
}
