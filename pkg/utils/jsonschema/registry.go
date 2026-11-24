package jsonschemautil

import (
	_ "embed"
	"strconv"
	"strings"
	"sync"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Metaschemas of every supported draft, keyed by URI
//
//go:embed metaschemas.json
var metaschemaText []byte

// Schema document, or a part of one with its own $id, and the anchors it declares
type resource struct {
	uri     string
	dialect *dialect
	root    any
	// Plain-name anchors: $anchor, $dynamicAnchor and the fragment-only ids of older drafts
	anchors map[string]any
	// $dynamicAnchor names
	dynamic map[string]any
	// The root holds "$recursiveAnchor": true
	recursive bool
	owner     *registry
}

// Place of a schema object: its resource, the JSON pointer to it from the resource root, and the dialect it is read in
type loc struct {
	res     *resource
	ptr     string
	dialect *dialect
}

// Absolute location of the schema, the resource URI with a JSON pointer fragment
func (l loc) uri() string { return l.res.uri + "#" + l.ptr }

// Resources by URI and the place of every schema object crawled, over a parent registry consulted for anything not here
type registry struct {
	resources map[string]*resource
	locs      map[*jsonutil.Object]loc
	parent    *registry
}

func newRegistry(parent *registry) *registry {
	return &registry{resources: map[string]*resource{}, locs: map[*jsonutil.Object]loc{}, parent: parent}
}

// Resource registered under uri here or in a parent, nil when none is
func (r *registry) resource(uri string) *resource {
	for g := r; g != nil; g = g.parent {
		if res := g.resources[uri]; res != nil {
			return res
		}
	}
	return nil
}

// Place of a crawled schema object, false when no crawl reached it
func (r *registry) loc(o *jsonutil.Object) (loc, bool) {
	for g := r; g != nil; g = g.parent {
		if l, ok := g.locs[o]; ok {
			return l, true
		}
	}
	return loc{}, false
}

// Registers a document under uri and under the id its root declares, crawls it for embedded resources and anchors, and gives back its root resource
func (r *registry) addDocument(uri string, doc any, d *dialect) *resource {
	uri = normalizeURI(uri)
	d = docDialect(doc, d)
	res := &resource{uri: uri, dialect: d, root: doc, owner: r}
	if o, ok := doc.(*jsonutil.Object); ok {
		if id, ok := d.id(o); ok {
			base, frag := splitFragment(resolveURI(uri, id))
			res.uri = normalizeURI(base)
			if frag != "" && d.legacyID && !strings.HasPrefix(frag, "/") {
				r.anchor(res, urlutil.Unescape(frag), o, false)
			}
		}
	}
	r.resources[uri] = res
	if r.resources[res.uri] == nil {
		r.resources[res.uri] = res
	}
	r.crawl(doc, res, "", d)
	return res
}

// Records the place of a schema object and of every subschema below it, registering the resources their ids open and the anchors they name
func (r *registry) crawl(v any, res *resource, ptr string, d *dialect) {
	o, ok := v.(*jsonutil.Object)
	if !ok {
		return
	}
	if _, seen := r.loc(o); seen {
		return
	}
	d = docDialect(o, d)
	if ptr != "" || o != res.root {
		if id, ok := d.id(o); ok {
			base, frag := splitFragment(resolveURI(res.uri, id))
			if base = normalizeURI(base); base != res.uri {
				sub := &resource{uri: base, dialect: d, root: o, owner: r}
				if r.resources[base] == nil {
					r.resources[base] = sub
				}
				res, ptr = sub, ""
			}
			if frag != "" && d.legacyID && !strings.HasPrefix(frag, "/") {
				r.anchor(res, urlutil.Unescape(frag), o, false)
			}
		}
	}
	if d.anchors {
		if name, ok := o.Value("$anchor").(string); ok {
			r.anchor(res, name, o, false)
		}
	}
	if d.dynamic {
		if name, ok := o.Value("$dynamicAnchor").(string); ok {
			r.anchor(res, name, o, true)
		}
	}
	if d.recursive && ptr == "" && res.owner == r {
		if on, _ := o.Value("$recursiveAnchor").(bool); on {
			res.recursive = true
		}
	}
	r.locs[o] = loc{res: res, ptr: ptr, dialect: d}
	for key, val := range o.All() {
		pos := d.subschemas[key]
		if pos == 0 {
			continue
		}
		at := ptr + "/" + escapeToken(key)
		switch x := val.(type) {
		case *jsonutil.Object:
			switch pos {
			case posSchema, posEither:
				r.crawl(x, res, at, d)
			case posMap:
				for name, sub := range x.All() {
					r.crawl(sub, res, at+"/"+escapeToken(name), d)
				}
			}
		case []any:
			if pos == posArray || pos == posEither {
				for i, sub := range x {
					r.crawl(sub, res, at+"/"+strconv.Itoa(i), d)
				}
			}
		}
	}
}

// Names a schema object as an anchor of its resource, the first declaration of a name kept; resources of other registries are left as they are
func (r *registry) anchor(res *resource, name string, o *jsonutil.Object, dynamic bool) {
	if res.owner != r || name == "" {
		return
	}
	if res.anchors == nil {
		res.anchors = map[string]any{}
	}
	if _, taken := res.anchors[name]; !taken {
		res.anchors[name] = o
	}
	if !dynamic {
		return
	}
	if res.dynamic == nil {
		res.dynamic = map[string]any{}
	}
	if _, taken := res.dynamic[name]; !taken {
		res.dynamic[name] = o
	}
}

var (
	bundleOnce sync.Once
	bundleReg  *registry
)

// Registry of the bundled metaschemas, decoded and crawled on first use and shared read-only after that
func bundled() *registry {
	bundleOnce.Do(func() {
		v, err := jsonutil.Decode(metaschemaText)
		if err != nil {
			panic("jsonschemautil: bundled metaschemas: " + err.Error())
		}
		all, ok := v.(*jsonutil.Object)
		if !ok {
			panic("jsonschemautil: bundled metaschemas are not an object")
		}
		reg := newRegistry(nil)
		for uri, doc := range all.All() {
			reg.addDocument(uri, doc, dialectOf(Draft202012))
		}
		bundleReg = reg
	})
	return bundleReg
}
