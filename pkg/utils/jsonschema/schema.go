package jsonschemautil

import (
	"context"
	"errors"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// $schema documents a compile follows to custom metaschemas before it stops checking against them
const maxMetaDepth = 4

// JSON Schema compiled once, every reference resolved: immutable and safe for concurrent use
type Schema struct {
	root *node
}

// Schema compiled from a decoded JSON value (*jsonutil.Object, map[string]any, []any, numbers, strings, booleans, nil) after checking it against the metaschema its $schema names; a *SchemaError when it is not a valid schema or a reference cannot be resolved
func Compile(schema any, opts ...Option) (*Schema, error) {
	return CompileContext(context.Background(), schema, opts...)
}

// Schema compiled as by Compile, ctx passed to the loaders that fetch referenced documents
func CompileContext(ctx context.Context, schema any, opts ...Option) (*Schema, error) {
	return compileDoc(ctx, schema, newConfig(opts))
}

// Most relevant failure of the value against the schema as a *ValidationError, a *SchemaError when the schema does not compile, nil when the value conforms
func Validate(value, schema any, opts ...Option) error {
	s, err := Compile(schema, opts...)
	if err != nil {
		return err
	}
	return s.Validate(value)
}

// Most relevant failure of the value, chosen by BestMatch, as a *ValidationError; nil when the value conforms
func (s *Schema) Validate(value any) error {
	if errs := s.Errors(value); len(errs) > 0 {
		return BestMatch(errs)
	}
	return nil
}

// Every failure of the value in the order the keywords of the schema produced them, those of anyOf and oneOf alternatives as Causes; nil when the value conforms; a validation that cannot finish (a pattern match out of time, a schema whose work explodes) gives only the error saying why
func (s *Schema) Errors(value any) []*ValidationError {
	st := newState()
	st.validate(s.root, value, nil)
	errs := st.result()
	st.release()
	return errs
}

// Whether the value conforms, stopping at the first failure without describing it
func (s *Schema) Valid(value any) bool {
	st := newState()
	st.quick = true
	ok := st.validate(s.root, value, nil) && st.halt == nil
	st.release()
	return ok
}

// Copies, checks and compiles a schema document under the settings, a document compiled before under the same settings coming from the cache
func compileDoc(ctx context.Context, schema any, cfg *config) (*Schema, error) {
	doc, err := normalize(schema)
	if err != nil {
		return nil, &SchemaError{Location: "#", Message: err.Error(), Err: err}
	}
	key, cacheable := cfg.cacheKey(doc)
	if cacheable {
		if s := schemaCache.get(key); s != nil {
			return s, nil
		}
	}
	s, err := compileNormalized(ctx, doc, cfg)
	if err != nil {
		return nil, err
	}
	if cacheable {
		schemaCache.put(key, s)
	}
	return s, nil
}

// Checks and compiles a document normalize gave
func compileNormalized(ctx context.Context, doc any, cfg *config) (*Schema, error) {
	c := newCompiler(ctx, cfg)
	for _, r := range cfg.resources {
		rd, err := normalize(r.doc)
		if err != nil {
			return nil, &SchemaError{Location: r.uri, Message: err.Error(), Err: err}
		}
		c.reg.addDocument(r.uri, rd, cfg.dialect())
	}
	d := docDialect(doc, cfg.dialect())
	if !cfg.skipMeta {
		checked, err := c.check(doc, cfg.dialect())
		if err != nil {
			return nil, err
		}
		d = checked
	}
	root := c.reg.addDocument(cfg.baseURI, doc, d)
	at := loc{res: root, dialect: root.dialect}
	if o, ok := doc.(*jsonutil.Object); ok {
		if known, ok := c.reg.loc(o); ok {
			at = known
		}
	}
	n, err := c.compile(doc, at)
	if err != nil {
		return nil, err
	}
	if err := c.finish(); err != nil {
		return nil, err
	}
	return &Schema{root: n}, nil
}

// Dialect a document is read in after checking it against its metaschema: the bundled one of a dialect $schema names, a custom one $schema reaches through the resources and loaders, else the bundled one of fallback
func (c *compiler) check(doc any, fallback *dialect) (*dialect, error) {
	d := fallback
	var meta *Schema
	if o, ok := doc.(*jsonutil.Object); ok {
		if uri, ok := o.Value("$schema").(string); ok {
			if known := dialectFor(uri); known != nil {
				d = known
			} else {
				custom, cd, err := c.customMetaschema(uri, fallback)
				if err != nil {
					return nil, err
				}
				if custom != nil {
					meta, d = custom, cd
				}
			}
		}
	}
	if meta == nil {
		m, err := d.metaschema()
		if err != nil {
			return nil, &SchemaError{Location: "#", Message: "the bundled metaschema does not compile: " + err.Error(), Err: err}
		}
		meta = m
	}
	if meta.Valid(doc) {
		return d, nil
	}
	if v := BestMatch(meta.Errors(doc)); v != nil {
		return nil, &SchemaError{Location: "#" + v.Location(), Message: v.Message, Violation: v}
	}
	return nil, &SchemaError{Location: "#", Message: "the schema does not conform to its metaschema"}
}

// Metaschema a $schema URI the package does not know reaches through the resources and loaders, compiled, with the dialect it is written in; nil without an error when nothing serves the URI
func (c *compiler) customMetaschema(uri string, fallback *dialect) (*Schema, *dialect, error) {
	if c.cfg.metaDepth >= maxMetaDepth {
		return nil, nil, nil
	}
	base, _ := splitFragment(uri)
	res, err := c.resource(normalizeURI(base), fallback)
	if err != nil {
		if errors.Is(err, ErrUnresolvable) {
			return nil, nil, nil
		}
		return nil, nil, &SchemaError{Location: "#/$schema", Message: "metaschema " + quote(uri, schemaQuoteLimit) + " cannot be loaded: " + err.Error(), Err: err}
	}
	meta, err := compileDoc(c.ctx, res.root, c.cfg.forMetaschema(res.uri))
	if err != nil {
		return nil, nil, &SchemaError{Location: "#/$schema", Message: "metaschema " + quote(uri, schemaQuoteLimit) + " is not a valid schema: " + err.Error(), Err: err}
	}
	return meta, docDialect(res.root, fallback), nil
}
