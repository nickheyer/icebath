package jsonschemautil

import "time"

// Setting for Compile, CompileContext and Validate
type Option func(*config)

// Time one pattern match may take when no WithMatchTimeout is given
const defaultMatchTimeout = 100 * time.Millisecond

// Compile settings
type config struct {
	draft         Draft
	baseURI       string
	resources     []namedDocument
	loaders       []Loader
	assertFormats bool
	formats       map[string]func(string) bool
	content       bool
	timeout       time.Duration
	// The schema is a metaschema being compiled for its own use and is not checked against another
	skipMeta bool
	// How many $schema documents deep this compile is
	metaDepth int
}

// Document registered under a URI before compiling
type namedDocument struct {
	uri string
	doc any
}

// Dialect for a schema whose $schema names none the package knows, Draft202012 when not given
func WithDraft(d Draft) Option { return func(c *config) { c.draft = d } }

// URI the root schema was retrieved from: the base its relative $id and references resolve against
func WithBaseURI(uri string) Option { return func(c *config) { c.baseURI = uri } }

// Document a reference to uri reaches, so no loader is asked for it; repeated for several documents
func WithResource(uri string, doc any) Option {
	return func(c *config) { c.resources = append(c.resources, namedDocument{uri: uri, doc: doc}) }
}

// Loader asked, after those given before it, for referenced documents the schema does not contain and no WithResource gives; without a loader such references are errors and nothing is fetched
func WithLoader(l Loader) Option {
	return func(c *config) {
		if l != nil {
			c.loaders = append(c.loaders, l)
		}
	}
}

// format asserted on string values for every format the package knows instead of only noted, as JSON Schema leaves it by default
func AssertFormats() Option { return func(c *config) { c.assertFormats = true } }

// Check asserted for the named format on string values, taking the place of the built-in one
func WithFormat(name string, valid func(string) bool) Option {
	return func(c *config) {
		if valid == nil {
			return
		}
		if c.formats == nil {
			c.formats = map[string]func(string) bool{}
		}
		c.formats[name] = valid
	}
}

// contentEncoding, contentMediaType and contentSchema asserted on string values: base16, base32 and base64 encodings, JSON media types and the schema of the decoded document
func AssertContent() Option { return func(c *config) { c.content = true } }

// Time one pattern match may take, and a tenth of what all matches of one validation may take together, before the validation stops as failed; 100ms when not given, zero or less lets matches run unbounded
func WithMatchTimeout(d time.Duration) Option { return func(c *config) { c.timeout = d } }

// Settings of the options in order over the defaults
func newConfig(opts []Option) *config {
	cfg := &config{draft: Draft202012, timeout: defaultMatchTimeout}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	return cfg
}

// Dialect for documents that name none
func (cfg *config) dialect() *dialect {
	if d := dialectOf(cfg.draft); d != nil {
		return d
	}
	return dialectOf(Draft202012)
}

// Check asserted for a format in a draft, nil when the format is only noted
func (cfg *config) format(name string, d Draft) func(string) bool {
	if f := cfg.formats[name]; f != nil {
		return f
	}
	if cfg.assertFormats {
		return formatCheck(name, d)
	}
	return nil
}

// Settings for compiling the metaschema a $schema URI names, its regex format asserted the way bundled metaschemas assert it
func (cfg *config) forMetaschema(base string) *config {
	return &config{
		draft:     cfg.draft,
		baseURI:   base,
		resources: cfg.resources,
		loaders:   cfg.loaders,
		formats:   map[string]func(string) bool{"regex": validPattern},
		timeout:   cfg.timeout,
		metaDepth: cfg.metaDepth + 1,
	}
}
