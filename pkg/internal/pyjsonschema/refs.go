package pyjsonschema

import (
	"math"
	"math/big"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Port of the referencing library, version 0.37.0, the registry and resolver jsonschema walks $ref through

type specification struct {
	name               string
	idOf               func(contents any) any
	subresourcesOf     func(contents any) []any
	anchorsIn          func(spec *specification, contents any) []*anchor
	maybeInSubresource func(segments []any, r *resolver, sub *resource) *resolver
}

func (s *specification) createResource(contents any) *resource {
	return &resource{contents: contents, spec: s}
}

// Specification a document declares through $schema, the given default when it declares none or an unknown one
func (s *specification) detect(contents any) *specification {
	d, ok := contents.(*pyjson.Object)
	if !ok {
		return s
	}
	dialect, present := d.Vals["$schema"]
	if !present || dialect == nil {
		return s
	}
	return specificationWith(dialect, s)
}

// Known specification of a dialect id, the default otherwise
func specificationWith(dialect any, def *specification) *specification {
	id, ok := dialect.(string)
	if !ok {
		raise("AttributeError", "'"+typeName(dialect)+"' object has no attribute 'rstrip'")
	}
	if spec, known := specsByDialect()[strings.TrimRight(id, "#")]; known {
		return spec
	}
	return def
}

type resource struct {
	contents any
	spec     *specification
}

// Identifier the specification reads off the contents with trailing hashes stripped, id and id_of on the stack
func (r *resource) id(st *state) (string, bool) {
	st.touch(2)
	id := r.spec.idOf(r.contents)
	if id == nil {
		return "", false
	}
	s, ok := id.(string)
	if !ok {
		raise("AttributeError", "'"+typeName(id)+"' object has no attribute 'rstrip'")
	}
	return strings.TrimRight(s, "#"), true
}

func (r *resource) subresources() []*resource {
	var out []*resource
	for _, each := range r.spec.subresourcesOf(r.contents) {
		out = append(out, r.spec.detect(each).createResource(each))
	}
	return out
}

func (r *resource) anchors() []*anchor { return r.spec.anchorsIn(r.spec, r.contents) }

// Frames of Resource.anchors and the anchors_in function of its specification, deeper when an anchor is built
func (r *resource) anchorsDepth(found bool) int {
	switch r.spec {
	case spec202012:
		if found {
			return 3
		}
		return 2
	case specOpaque:
		return 3
	}
	if found {
		return 5
	}
	return 3
}

// Contents a JSON pointer reaches inside the resource, entering subresources along the way
func (r *resource) pointer(pointer string, res *resolver) (any, *resolver) {
	st := res.st
	defer st.call()()
	st.touch(unquoteDepth(pointer[1:]))
	contents := r.contents
	var segments []any
	for _, raw := range strings.Split(pyurl.Unquote(pointer[1:]), "/") {
		st.touch(1)
		var segment any
		switch c := contents.(type) {
		case []any:
			n := pyInt(raw)
			i, ok := sequenceIndex(n, len(c))
			if !ok {
				pointerToNowhere(pointer, r, st)
			}
			segment, contents = pyValue(n), c[i]
		case string:
			n := pyInt(raw)
			rs := pystr.Runes(c)
			i, ok := sequenceIndex(n, len(rs))
			if !ok {
				pointerToNowhere(pointer, r, st)
			}
			segment, contents = pyValue(n), pystr.FromRunes(rs[i:i+1])
		case *pyjson.Object:
			key := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
			v, ok := c.Vals[key]
			if !ok {
				pointerToNowhere(pointer, r, st)
			}
			segment, contents = key, v
		default:
			raise("TypeError", "'"+typeName(contents)+"' object is not subscriptable")
		}
		segments = append(segments, segment)
		last := res
		st.touch(2)
		sub := r.spec.createResource(contents)
		func() {
			defer st.call()()
			res = r.spec.maybeInSubresource(segments, res, sub)
		}()
		if res != last {
			segments = nil
		}
	}
	st.touch(1)
	return contents, res
}

// Raises the PointerToNowhere a missing pointer target gives
func pointerToNowhere(pointer string, r *resource, st *state) {
	st.touch(1)
	msg := repr(pointer) + " does not exist within " + repr(r.contents)
	if pointer == "/" {
		msg += ". The pointer '/' is a valid JSON Pointer but it points to an empty string property ''. If you intended to point to the entire resource, you should use '#'."
	}
	raise("PointerToNowhere", msg)
}

// Python int of a pointer segment, the ValueError int raises for anything else
func pyInt(s string) *big.Int {
	n, ok := pystr.IntText(s)
	if !ok {
		raise("ValueError", "invalid literal for int() with base 10: "+repr(s))
	}
	return n
}

// Python sequence index with negative positions counted from the end, false when out of range
func sequenceIndex(i *big.Int, n int) (int, bool) {
	if !i.IsInt64() {
		return 0, false
	}
	j := i.Int64()
	if j < 0 {
		j += int64(n)
	}
	return int(j), j >= 0 && j < int64(n)
}

type anchor struct {
	name     any
	dynamic  bool
	resource *resource
}

// Anchor.resolve or DynamicAnchor.resolve, the dynamic form walking the dynamic scope for the outermost dynamic anchor
func (a *anchor) resolve(res *resolver) (any, *resolver) {
	st := res.st
	defer st.call()()
	if !a.dynamic {
		st.touch(1)
		return a.resource.contents, res
	}
	last := a.resource
	for uri := res.previous; uri != nil; uri = uri.next {
		st.touch(1)
		found := res.registry.anchorOrNone(uri.uri, a.name, st)
		if found != nil && found.dynamic {
			last = found.resource
		}
	}
	st.touch(1)
	out := res.inSubresource(last)
	st.touch(1)
	return last.contents, out
}

type anchorKey struct{ uri, name string }

// Hash key of an anchor name, numbers equal under Python == sharing one key and unhashable names raising
func anchorNameKey(name any) string {
	switch n := name.(type) {
	case string:
		return "s" + n
	case []any, *pyjson.Object:
		raise("TypeError", "unhashable type: '"+typeName(n)+"'")
	case nil:
		return "n"
	}
	if numeric(name) {
		if f, ok := name.(float64); ok && f != math.Trunc(f) {
			return "f" + repr(f)
		}
		if f, ok := name.(float64); ok {
			if math.IsInf(f, 0) {
				return "f" + repr(f)
			}
			n, _ := new(big.Float).SetFloat64(f).Int(nil)
			return "i" + n.String()
		}
		return "i" + bigOf(name).String()
	}
	return "o" + repr(name)
}

// Immutable resource store crawled lazily like referencing.Registry
type registry struct {
	resources map[string]*resource
	anchors   map[anchorKey]*anchor
	uncrawled []string
	retrieve  func(uri string, st *state) *resource
	crawled   *registry
	// Frames a crawl of this registry pushes below its own frame
	crawlPeak int
}

func (g *registry) withResources(pairs ...uriResource) *registry {
	out := &registry{resources: make(map[string]*resource, len(g.resources)+len(pairs)), anchors: g.anchors, retrieve: g.retrieve}
	for k, v := range g.resources {
		out.resources[k] = v
	}
	out.uncrawled = append([]string(nil), g.uncrawled...)
	for _, p := range pairs {
		uri := strings.TrimRight(p.uri, "#")
		if !slices.Contains(out.uncrawled, uri) {
			out.uncrawled = append(out.uncrawled, uri)
		}
		out.resources[uri] = p.res
	}
	return out
}

type uriResource struct {
	uri string
	res *resource
}

// Registry with every resource walked for subresources and anchors, the crawl frames pushed each time Python would crawl
func (g *registry) crawl(st *state) *registry {
	if g.crawled == nil {
		g.crawled = g.walk()
	}
	st.touch(1 + g.crawlPeak)
	return g.crawled
}

func (g *registry) walk() *registry {
	resources := make(map[string]*resource, len(g.resources))
	for k, v := range g.resources {
		resources[k] = v
	}
	anchors := make(map[anchorKey]*anchor, len(g.anchors))
	for k, v := range g.anchors {
		anchors[k] = v
	}
	var pending []uriResource
	for _, uri := range g.uncrawled {
		pending = append(pending, uriResource{uri, resources[uri]})
	}
	peak := attrsEvolveDepth
	for len(pending) > 0 {
		p := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		uri := p.uri
		peak = max(peak, 2)
		if id, ok := p.res.id(nil); ok {
			peak = max(peak, urljoinDepth(uri, id))
			uri = urljoin(uri, id)
			resources[uri] = p.res
		}
		found := p.res.anchors()
		peak = max(peak, p.res.anchorsDepth(len(found) > 0))
		for _, a := range found {
			anchors[anchorKey{uri, anchorNameKey(a.name)}] = a
		}
		subs := p.res.subresources()
		if len(subs) > 0 {
			peak = max(peak, 5)
		} else {
			peak = max(peak, 3)
		}
		for _, sub := range subs {
			pending = append(pending, uriResource{uri, sub})
		}
	}
	g.crawlPeak = peak
	return &registry{resources: resources, anchors: anchors, retrieve: g.retrieve}
}

// Resource under uri, crawling and then retrieving when it is not yet known
func (g *registry) getOrRetrieve(uri string, st *state) (*resource, *registry) {
	defer st.call()()
	if r, ok := g.resources[uri]; ok {
		st.touch(1)
		return r, g
	}
	crawled := g
	if len(g.uncrawled) > 0 {
		crawled = g.crawl(st)
	} else {
		st.touch(1 + attrsEvolveDepth)
	}
	if r, ok := crawled.resources[uri]; ok {
		st.touch(1)
		return r, crawled
	}
	if crawled.retrieve == nil {
		st.touch(2)
		raise("NoSuchResource", repr(uri))
	}
	r := crawled.retrieve(uri, st)
	st.touch(2 + attrsEvolveDepth)
	st.touch(1)
	return r, crawled.withResources(uriResource{uri, r})
}

// Anchor lookup raising NoSuchAnchor, InvalidAnchor or NoSuchResource like Registry.anchor
func (g *registry) anchor(uri string, name any, st *state) (*anchor, *registry) {
	defer st.call()()
	key := anchorNameKey(name)
	if a, ok := g.anchors[anchorKey{uri, key}]; ok {
		st.touch(1)
		return a, g
	}
	crawled := g
	if len(g.uncrawled) > 0 {
		crawled = g.crawl(st)
	} else {
		st.touch(1 + attrsEvolveDepth)
	}
	if a, ok := crawled.anchors[anchorKey{uri, key}]; ok {
		st.touch(1)
		return a, crawled
	}
	r, ok := g.resources[strings.TrimRight(uri, "#")]
	if !ok {
		st.touch(2)
		raise("NoSuchResource", repr(uri))
	}
	st.touch(1)
	if canonical, ok := r.id(st); ok {
		if a, ok := crawled.anchors[anchorKey{canonical, key}]; ok {
			st.touch(1)
			return a, crawled
		}
	}
	st.touch(1)
	if pyIn("/", name) {
		text := pyStr(name)
		raise("InvalidAnchor", "'#"+text+"' is not a valid anchor, neither as a plain name anchor nor as a JSON Pointer. You may have intended to use '#/"+text+"', as the slash is required *before each segment* of a JSON pointer.")
	}
	raise("NoSuchAnchor", repr(name)+" does not exist within "+repr(r.contents))
	return nil, nil
}

// Anchor or nil for a missing one, the NoSuchAnchor the dynamic scope walk skips
func (g *registry) anchorOrNone(uri string, name any, st *state) (found *anchor) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*Exception); ok && e.Type == "NoSuchAnchor" {
				found = nil
				return
			}
			panic(r)
		}
	}()
	a, _ := g.anchor(uri, name, st)
	return a
}

type uriNode struct {
	uri  string
	next *uriNode
}

// Base URI paired with a resource store and the dynamic scope behind it
type resolver struct {
	baseURI  string
	registry *registry
	previous *uriNode
	st       *state
}

// Resolver._evolve, the base URI moved with the old one pushed onto the dynamic scope
func (r *resolver) evolve(baseURI string, g *registry) *resolver {
	r.st.touch(1 + attrsEvolveDepth)
	previous := r.previous
	if r.baseURI != "" && (previous == nil || baseURI != r.baseURI) {
		previous = &uriNode{uri: r.baseURI, next: previous}
	}
	return &resolver{baseURI: baseURI, registry: g, previous: previous, st: r.st}
}

// Resolves a reference to its contents and the resolver to continue with
func (r *resolver) lookup(ref string) (any, *resolver) {
	st := r.st
	defer st.call()()
	var uri, fragment string
	if strings.HasPrefix(ref, "#") {
		uri, fragment = r.baseURI, ref[1:]
	} else {
		joined := urljoin(r.baseURI, ref)
		st.touch(urljoinDepth(r.baseURI, ref))
		st.touch(urldefragDepth(joined))
		uri, fragment = urldefrag(joined)
	}
	res, g := r.retrieveOrUnresolvable(uri, ref)
	if strings.HasPrefix(fragment, "/") {
		return res.pointer(fragment, r.evolve(uri, g))
	}
	if fragment != "" {
		a, g2 := g.anchor(uri, fragment, st)
		return a.resolve(r.evolve(uri, g2))
	}
	out := r.evolve(uri, g)
	st.touch(1)
	return res.contents, out
}

// Registry lookup with NoSuchResource and Unretrievable both raised as Unresolvable
func (r *resolver) retrieveOrUnresolvable(uri, ref string) (res *resource, g *registry) {
	defer func() {
		if rec := recover(); rec != nil {
			if e, ok := rec.(*Exception); ok && (e.Type == "NoSuchResource" || e.Type == "Unretrievable") {
				r.st.touch(1)
				raise("Unresolvable", ref)
			}
			panic(rec)
		}
	}()
	return r.registry.getOrRetrieve(uri, r.st)
}

// Resolver.in_subresource, a new base URI when the subresource declares an id
func (r *resolver) inSubresource(sub *resource) *resolver {
	st := r.st
	defer st.call()()
	id, ok := sub.id(st)
	if !ok {
		return r
	}
	st.touch(urljoinDepth(r.baseURI, id))
	st.touch(attrsEvolveDepth)
	return &resolver{baseURI: urljoin(r.baseURI, id), registry: r.registry, previous: r.previous, st: st}
}

// Resolution of $recursiveRef, the outermost dynamic scope entry still carrying $recursiveAnchor
func lookupRecursiveRef(r *resolver) (any, *resolver) {
	defer r.st.call()()
	contents, res := r.lookup("#")
	if d, ok := contents.(*pyjson.Object); ok && pyrepr.Truthy(d.Vals["$recursiveAnchor"]) {
		for uri := r.previous; uri != nil; uri = uri.next {
			r.st.touch(1)
			next, nextRes := r.lookup(uri.uri)
			nd, ok := next.(*pyjson.Object)
			if !ok || !pyrepr.Truthy(nd.Vals["$recursiveAnchor"]) {
				break
			}
			contents, res = next, nextRes
		}
		r.st.touch(1)
	}
	return contents, res
}

func urljoin(base, ref string) string {
	out, err := pyurl.Join(base, ref)
	if err != nil {
		raise("ValueError", err.Error())
	}
	return out
}

// urllib.parse.urldefrag, the URL without its fragment and the fragment
func urldefrag(url string) (string, string) {
	if !strings.Contains(url, "#") {
		return url, ""
	}
	u, err := pyurl.Split(url)
	if err != nil {
		raise("ValueError", err.Error())
	}
	rest := strings.TrimLeft(url, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	rest = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(rest)
	if u.Scheme != "" {
		rest = rest[strings.IndexByte(rest, ':')+1:]
	}
	hasNetloc := strings.HasPrefix(rest, "//")
	if hasNetloc {
		rest = rest[2+len(u.Netloc):]
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	hasQuery := strings.Contains(rest, "?")
	path := u.Path
	if hasNetloc {
		if path != "" && path[0] != '/' {
			path = "/" + path
		}
		path = "//" + u.Netloc + path
	} else if strings.HasPrefix(path, "//") {
		path = "//" + path
	}
	if u.Scheme != "" {
		path = u.Scheme + ":" + path
	}
	if hasQuery {
		path += "?" + u.Query
	}
	return path, u.Fragment
}

// geturl of urlsplit, the normalized key URIDict files a URI under
func normalizeURI(uri any) string {
	s, ok := uri.(string)
	if !ok {
		raise("AttributeError", "'"+typeName(uri)+"' object has no attribute 'decode'")
	}
	u, err := pyurl.Split(s)
	if err != nil {
		raise("ValueError", err.Error())
	}
	return pyurl.Unsplit(u)
}
