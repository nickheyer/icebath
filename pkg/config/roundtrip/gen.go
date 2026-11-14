package roundtrip

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Seeded draws with the bounds of Python random.Random
type RNG struct{ r *rand.Rand }

func NewRNG(seed int64) *RNG { return &RNG{rand.New(rand.NewPCG(uint64(seed), 0xC18))} }

func (g *RNG) Random() float64              { return g.r.Float64() }
func (g *RNG) RandInt(a, b int) int         { return a + g.r.IntN(b-a+1) }
func (g *RNG) RandRange(n int) int          { return g.r.IntN(n) }
func (g *RNG) Uniform(a, b float64) float64 { return a + (b-a)*g.r.Float64() }

// One element drawn uniformly
func Choice[T any](g *RNG, items []T) T { return items[g.r.IntN(len(items))] }

// k distinct elements in draw order
func Sample[T any](g *RNG, items []T, k int) []T {
	pool := slices.Clone(items)
	for i := 0; i < k; i++ {
		j := i + g.r.IntN(len(pool)-i)
		pool[i], pool[j] = pool[j], pool[i]
	}
	return pool[:k]
}

func Shuffle[T any](g *RNG, items []T) {
	g.r.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
}

// Python round(x, n) of a float
func Round(x float64, n int) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	return v
}

// Python repr of a float
func FloatRepr(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return map[bool]string{true: "inf", false: "-inf"}[v > 0]
	}
	exp := 0
	if v != 0 {
		exp = int(math.Floor(math.Log10(math.Abs(v))))
	}
	if exp < -4 || exp >= 16 {
		s := strconv.FormatFloat(v, 'e', -1, 64)
		mant, e, _ := strings.Cut(s, "e")
		sign, digits := e[:1], strings.TrimLeft(e[1:], "0")
		if len(digits) < 2 {
			digits = fmt.Sprintf("%02s", digits)
		}
		return mant + "e" + sign + digits
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

// json.dumps with ensure_ascii off
func JSONString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

var (
	yaml11Words = map[string]bool{"y": true, "n": true, "yes": true, "no": true, "true": true, "false": true, "on": true, "off": true, "null": true, "~": true}
	PlainOK     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_\-/]*$`)
	Tricky      = []string{
		"off", "on", "yes", "No", "y", "null", "~", "True",
		"010", "1e3", "0x1F", "1_000", ".5", "+1", "12:30", "2026-09-23", "0o17", ".inf",
		"colon: space", "hash # not a comment", "*star", "&anchor", "!tag", "%pct", "@at", "`tick",
		"  leading space", "trailing space  ", "", "quote \" and ' both", "tab\there", "line1\nline2",
		"a\\b\\c", "C:\\Users\\me\\AppData", "{not: a map}", "[not, a, list]", "- dash", "? what",
	}
	Unicode = []string{"héllo wörld", "中文配置值", "emoji 😀 ok", "Ünïcödé-ß", "Ελληνικά", "日本語テキスト", "مرحبا"}
	Words   = []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet"}
)

// YAML spelling of a scalar a user would hand write, quoting whatever a plain scalar would retype
func RenderScalar(v any, g *RNG) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case float64:
		return FloatRepr(x)
	case string:
		if strings.HasPrefix(x, "${") && strings.HasSuffix(x, "}") && len(x) >= 3 && PlainOK.MatchString(x[2:len(x)-1]) {
			return x
		}
		if PlainOK.MatchString(x) && !yaml11Words[strings.ToLower(x)] {
			return x
		}
		if strings.ContainsAny(x, "\n\t\\") || g.Random() < 0.5 {
			return JSONString(x)
		}
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	}
	panic(fmt.Sprintf("unrenderable scalar %T", v))
}

// Documented default leaf
type Leaf struct {
	Path    []string
	Default any
}

// Default config facts the generators draw from
type Schema struct {
	Defaults   map[string]any
	Documented []Leaf
	GenLeaves  []Leaf
	TopLevel   []string
	Latest     int
	Load       func([]byte) (any, error)
	keys       func(map[string]any) []string
}

var genExclude = map[string]bool{Key("agent", "max_turns"): true, Key("model", "provider"): true, Key("model", "base_url"): true, Key("model", "api_mode"): true}

// Schema over the default tree, its key order and the YAML loader the oracle parses with
func NewSchema(defaults map[string]any, keys func(map[string]any) []string, load func([]byte) (any, error)) *Schema {
	s := &Schema{Defaults: defaults, Load: load, keys: keys}
	var walk func(m map[string]any, pre []string)
	walk = func(m map[string]any, pre []string) {
		for _, k := range keys(m) {
			p := append(slices.Clone(pre), k)
			switch v := m[k].(type) {
			case map[string]any:
				if len(v) > 0 {
					walk(v, p)
				}
			case bool, int, float64, string, nil:
				s.Documented = append(s.Documented, Leaf{p, v})
			}
		}
	}
	walk(defaults, nil)
	for _, l := range s.Documented {
		dotted := slices.ContainsFunc(l.Path, func(seg string) bool { return strings.Contains(seg, ".") })
		_, sectionDict := defaults[l.Path[0]].(map[string]any)
		if len(l.Path) >= 2 && len(l.Path) <= 3 && !dotted && !genExclude[Key(l.Path...)] && sectionDict {
			s.GenLeaves = append(s.GenLeaves, l)
		}
	}
	for _, k := range keys(defaults) {
		if k != "_config_version" {
			s.TopLevel = append(s.TopLevel, k)
		}
	}
	switch v := defaults["_config_version"].(type) {
	case int:
		s.Latest = v
	case float64:
		s.Latest = int(v)
	}
	return s
}

// Default value at a path, Missing when the defaults lack it
func (s *Schema) Default(path []string) any { return Get(s.Defaults, path) }

// Value a generated config holds for a leaf with the given default
func ValueFor(def any, g *RNG, long bool, env map[string]string, seed int64) any {
	roll := g.Random()
	if long && roll < 0.35 {
		return Choice(g, []func() string{
			func() string {
				return strings.Repeat("A", g.RandInt(74, 85)) + "D:\\CentBrowserPortable " + strings.Repeat("B", 40)
			},
			func() string {
				words := make([]string, 40)
				for i := range words {
					words[i] = Choice(g, Words)
				}
				return strings.Join(words, " ") + " \\scripts\\monitor-off-v3.ps1 end"
			},
			func() string {
				return strings.Repeat("x", 150) + " " + "C:\\Users\\me\\.icebath\\scripts\\" + strings.Repeat("y", 90)
			},
		})()
	}
	if roll < 0.08 {
		return nil
	}
	if roll < 0.18 {
		switch def.(type) {
		case bool, int, float64, string, nil:
			return def
		}
		return nil
	}
	switch def.(type) {
	case bool:
		return g.Random() < 0.5
	case int:
		return g.RandInt(0, 100000)
	case float64:
		return Round(g.Uniform(0, 1000), 3)
	}
	kind := g.Random()
	switch {
	case kind < 0.35:
		return Choice(g, Tricky)
	case kind < 0.55:
		return Choice(g, Unicode) + " " + Choice(g, Words)
	case kind < 0.65:
		name := fmt.Sprintf("C18_REF_%d_%d", seed, len(env))
		env[name] = fmt.Sprintf("secret-%d-%d", g.RandRange(100000000), seed)
		return "${" + name + "}"
	}
	return fmt.Sprintf("%s-%d", Choice(g, Words), g.RandRange(1000000))
}

// Ordered mapping so generated text keeps insertion order like a Python dict
type node struct {
	keys []string
	vals map[string]any
}

func newNode() *node { return &node{vals: map[string]any{}} }

func (n *node) set(k string, v any) {
	if _, ok := n.vals[k]; !ok {
		n.keys = append(n.keys, k)
	}
	n.vals[k] = v
}

func (n *node) get(path []string) any {
	var cur any = n
	for _, p := range path {
		m, ok := cur.(*node)
		if !ok {
			return Missing
		}
		v, has := m.vals[p]
		if !has {
			return Missing
		}
		cur = v
	}
	return cur
}

func (n *node) setPath(path []string, v any) {
	cur := n
	for _, p := range path[:len(path)-1] {
		next, ok := cur.vals[p].(*node)
		if !ok {
			next = newNode()
			cur.set(p, next)
		}
		cur = next
	}
	cur.set(path[len(path)-1], v)
}

// Ordered leaf paths, an empty mapping counted as a leaf
func (n *node) leaves(pre []string, out *[][]string) {
	for _, k := range n.keys {
		p := append(slices.Clone(pre), k)
		if child, ok := n.vals[k].(*node); ok && len(child.keys) > 0 {
			child.leaves(p, out)
			continue
		}
		*out = append(*out, p)
	}
}

func (n *node) plain() map[string]any {
	out := make(map[string]any, len(n.keys))
	for _, k := range n.keys {
		if child, ok := n.vals[k].(*node); ok {
			out[k] = child.plain()
			continue
		}
		out[k] = n.vals[k]
	}
	return out
}

func (n *node) yamlNode() *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, k := range n.keys {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, valueNode(n.vals[k]))
	}
	return m
}

func valueNode(v any) *yaml.Node {
	switch x := v.(type) {
	case *node:
		return x.yamlNode()
	case []any:
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range x {
			seq.Content = append(seq.Content, valueNode(e))
		}
		return seq
	}
	var n yaml.Node
	n.Encode(v)
	return &n
}

// Generated config text, its parse oracle, the line of every leaf and the env its references need
type Case struct {
	Seed      int64
	Text      string
	Tree      map[string]any
	LeafLines map[string]int
	LeafOrder [][]string
	Env       map[string]string
}

// Line index of a leaf the text spells, empty when the path is absent
func (c *Case) LinesOf(path []string) map[int]bool {
	if i, ok := c.LeafLines[Key(path...)]; ok {
		return map[int]bool{i: true}
	}
	return map[int]bool{}
}

// Whether the text spells a line for the path
func (c *Case) HasLine(path []string) bool {
	_, ok := c.LeafLines[Key(path...)]
	return ok
}

// Generator knobs, zero values meaning the Python defaults
type GenOptions struct {
	Long         bool
	NSections    [2]int
	ExcludeTop   []string
	NoNullLeaves bool
}

// Hand written config of 5 to 11 sections with tricky scalars, comments, refs, a dotted provider and a list
func (s *Schema) GenCase(seed int64, o GenOptions) (*Case, error) {
	g := NewRNG(seed)
	if o.NSections == [2]int{} {
		o.NSections = [2]int{5, 11}
	}
	exclude := map[string]bool{}
	for _, k := range o.ExcludeTop {
		exclude[k] = true
	}
	env := map[string]string{}
	bySection := map[string][]Leaf{}
	for _, l := range s.GenLeaves {
		if !exclude[l.Path[0]] {
			bySection[l.Path[0]] = append(bySection[l.Path[0]], l)
		}
	}
	names := make([]string, 0, len(bySection))
	for k := range bySection {
		names = append(names, k)
	}
	sort.Strings(names)
	sections := Sample(g, names, g.RandInt(o.NSections[0], o.NSections[1]))
	tree := newNode()
	for _, sec := range sections {
		leaves := bySection[sec]
		for _, l := range Sample(g, leaves, min(len(leaves), g.RandInt(1, 5))) {
			if len(l.Path) == 3 {
				if _, ok := tree.get(l.Path[:2]).(*node); !ok && tree.get(l.Path[:2]) != Missing {
					continue
				}
			}
			if len(l.Path) == 2 {
				if _, ok := tree.get(l.Path).(*node); ok {
					continue
				}
			}
			v := ValueFor(l.Default, g, o.Long, env, seed)
			if v == nil && o.NoNullLeaves {
				v = Choice(g, Words)
			}
			tree.setPath(l.Path, v)
		}
	}
	firstLeaf := func() []string {
		var all [][]string
		tree.leaves(nil, &all)
		for _, p := range all {
			if p[0] == sections[0] {
				return p
			}
		}
		return nil
	}
	if o.Long {
		tree.setPath(firstLeaf(), strings.Repeat("A", 76)+"D:\\CentBrowserPortable "+strings.Repeat("B", 60))
	}
	if !o.NoNullLeaves {
		hasNull := false
		for _, v := range Leaves(tree.plain()) {
			hasNull = hasNull || v == nil
		}
		if !hasNull {
			tree.setPath(firstLeaf(), nil)
		}
	}
	if !exclude["providers"] {
		prov := newNode()
		prov.set("base_url", fmt.Sprintf("http://127.0.0.1:9/v%d", seed))
		prov.set("api_mode", "chat_completions")
		providers := newNode()
		providers.set(fmt.Sprintf("acme.v%d", seed%7), prov)
		tree.set("providers", providers)
	}
	if !exclude["c18_custom_root"] {
		tree.set("c18_custom_root", Choice(g, Unicode))
	}
	if !exclude["skills"] && g.Random() < 0.6 {
		if tree.get([]string{"skills"}) == Missing {
			tree.set("skills", newNode())
		}
		if skills, ok := tree.vals["skills"].(*node); ok {
			skills.set("external_dirs", []any{"/opt/c18/" + Choice(g, Words), Choice(g, Unicode)})
		}
	}
	tree.set("_config_version", s.Latest)

	lines := []string{fmt.Sprintf("# C18 generated config (seed=%d) — comments must survive every write", seed)}
	leafLines := map[string]int{}
	var order [][]string
	var emit func(n *node, depth int, pre []string)
	emit = func(n *node, depth int, pre []string) {
		for _, k := range n.keys {
			ind := strings.Repeat("  ", depth)
			key := k
			if !PlainOK.MatchString(k) {
				key = JSONString(k)
			}
			if depth == 0 && g.Random() < 0.3 {
				lines = append(lines, fmt.Sprintf("# section %s: hand-written note ü", k))
			}
			p := append(slices.Clone(pre), k)
			switch v := n.vals[k].(type) {
			case *node:
				lines = append(lines, ind+key+":")
				emit(v, depth+1, p)
			case []any:
				lines = append(lines, ind+key+":")
				for _, item := range v {
					lines = append(lines, ind+"  - "+RenderScalar(item, g))
				}
			default:
				note := ""
				if g.Random() < 0.25 && k != "_config_version" {
					note = "  # inline note"
				}
				leafLines[Key(p...)] = len(lines)
				order = append(order, p)
				lines = append(lines, ind+key+": "+RenderScalar(v, g)+note)
			}
		}
	}
	emit(tree, 0, nil)
	text := strings.Join(lines, "\n") + "\n"
	plain := tree.plain()
	parsed, err := s.Load([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("generator self-check failed for seed %d: %v\n%s", seed, err, text)
	}
	if !Equal(parsed, plain) {
		return nil, fmt.Errorf("generator self-check failed for seed %d: changed %v\n%s", seed, Paths(ChangedPaths(plain, parsed)), text)
	}
	return &Case{Seed: seed, Text: text, Tree: plain, LeafLines: leafLines, LeafOrder: order, Env: env}, nil
}

var configVersionLine = regexp.MustCompile(`(?m)^_config_version:`)

// Text with a newline terminated block inserted right before the root _config_version line
func BeforeVersion(text, block string) (string, error) {
	loc := configVersionLine.FindStringIndex(text)
	if loc == nil {
		return "", fmt.Errorf("no root _config_version line in:\n%s", text)
	}
	return text[:loc[0]] + block + text[loc[0]:], nil
}

var stampLine = regexp.MustCompile(`(?m)^_config_version: \d+$`)

// Text with every root version stamp rewritten to the given version
func Restamp(text string, version int) string {
	return stampLine.ReplaceAllString(text, fmt.Sprintf("_config_version: %d", version))
}

// CLI spelling of a path, literal dots escaped
func CLIKey(path []string) string {
	out := make([]string, len(path))
	for i, seg := range path {
		out[i] = strings.ReplaceAll(seg, ".", `\.`)
	}
	return strings.Join(out, ".")
}

// CLI string for a new value of a documented leaf and the value it must persist as
func SetValueFor(def, current any, g *RNG) (string, any) {
	switch def.(type) {
	case bool:
		v := true
		if c, ok := current.(bool); ok {
			v = !c
		} else {
			v = !def.(bool)
		}
		return strconv.FormatBool(v), v
	case int:
		v := g.RandInt(1, 1000000)
		for Equal(v, current) {
			v++
		}
		return strconv.Itoa(v), v
	case float64:
		v := Round(g.Uniform(1, 999), 2) + 0.25
		return FloatRepr(v), v
	}
	v := fmt.Sprintf("c18-%s-%d", Choice(g, Words), g.RandRange(1000000))
	return v, v
}
