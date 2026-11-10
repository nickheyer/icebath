package config

import (
	"bytes"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Mapping with the given key value pairs in insertion order, the way a Python dict literal keeps them
func yamlDict(pairs ...any) map[string]any {
	m := map[string]any{}
	var keys []string
	for i := 0; i+1 < len(pairs); i += 2 {
		k := pairs[i].(string)
		m[k] = pairs[i+1]
		keys = append(keys, k)
	}
	SetMappingKeys(m, keys)
	return m
}

type yamlLoader struct {
	name string
	load func(any) (any, error)
}

// The shared reader and fast_safe_load, which hands every stream to it
func yamlLoaders() []yamlLoader {
	return []yamlLoader{
		{"safe_load", SafeLoad},
		{"fast_safe_load", func(stream any) (any, error) {
			data, err := readYAMLStream(stream)
			if err != nil {
				return nil, err
			}
			return FastSafeLoad(data)
		}},
	}
}

func TestSafeLoadAcceptsExistingConfigBooleanSpellings(t *testing.T) {
	document := "flags: [on, off, yes, no, true, false]\nquoted: ['off', 'yes']\n"
	expected := map[string]any{"flags": []any{true, false, true, false, true, false}, "quoted": []any{"off", "yes"}}
	for _, l := range yamlLoaders() {
		t.Run(l.name, func(t *testing.T) {
			for _, stream := range []any{document, []byte(document), strings.NewReader(document), bytes.NewReader([]byte(document))} {
				got, err := l.load(stream)
				if err != nil || !reflect.DeepEqual(got, expected) {
					t.Fatalf("%T: %#v %v", stream, got, err)
				}
			}
			if got, err := l.load(""); got != nil || err != nil {
				t.Fatalf("empty: %#v %v", got, err)
			}
		})
	}
}

func TestSafeLoadRejectsPythonObjectConstruction(t *testing.T) {
	for _, l := range yamlLoaders() {
		t.Run(l.name, func(t *testing.T) {
			_, err := l.load("!!python/object/apply:builtins.str ['must not construct']")
			if !IsYAMLError(err) || !strings.Contains(err.Error(), "could not determine a constructor for the tag 'tag:yaml.org,2002:python/object/apply:builtins.str'") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestSafeLoadHasTheCParserWhereItsWheelShips(t *testing.T) {
	got, err := SafeLoad("[{url: http://h}]")
	if err != nil || !reflect.DeepEqual(got, []any{map[string]any{"url": "http://h"}}) {
		t.Fatalf("safe_load = %#v %v", got, err)
	}
	fast, err := FastSafeLoad([]byte("[{url: http://h}]"))
	if err != nil || !reflect.DeepEqual(fast, got) {
		t.Fatalf("fast_safe_load = %#v %v", fast, err)
	}
}

func TestSafeDumpPreservesDataAndReadableBlockLayout(t *testing.T) {
	data := yamlDict("z", []any{yamlDict("mode", "off", "choice", "y", "label", "こんにちは 🦀")}, "a", "yes")
	opts := DefaultSafeDumpOptions()
	opts.SortKeys = false
	raw, err := SafeDumpWith(data, opts)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if loaded, err := SafeLoad(text); err != nil || !reflect.DeepEqual(loaded, data) {
		t.Fatalf("reload = %#v %v", loaded, err)
	}
	if !strings.HasPrefix(text, "z:\n  - ") || !strings.Contains(text, "こんにちは 🦀") {
		t.Fatalf("layout:\n%s", text)
	}
	var stream strings.Builder
	if err := SafeDumpTo(&stream, data, opts); err != nil || stream.String() != text {
		t.Fatalf("stream = %q %v", stream.String(), err)
	}
	if _, err := SafeDumpWith(map[string]any{"object": struct{}{}}, DefaultSafeDumpOptions()); !IsYAMLError(err) {
		t.Fatalf("object dump err = %v", err)
	}
}

func TestSafeDumpHonorsTheOptionsUsedByCallers(t *testing.T) {
	data := yamlDict("zebra", yamlDict("zed", 1, "alpha", 2), "alpha", "hé")
	for _, sortKeys := range []bool{false, true} {
		opts := DefaultSafeDumpOptions()
		opts.SortKeys = sortKeys
		raw, err := SafeDumpWith(data, opts)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := SafeLoad(raw)
		if err != nil {
			t.Fatal(err)
		}
		m := loaded.(map[string]any)
		want, wantInner := []string{"zebra", "alpha"}, []string{"zed", "alpha"}
		if sortKeys {
			want, wantInner = []string{"alpha", "zebra"}, []string{"alpha", "zed"}
		}
		if got := MappingKeys(m); !reflect.DeepEqual(got, want) {
			t.Fatalf("sort=%v keys %v", sortKeys, got)
		}
		if got := MappingKeys(m["zebra"].(map[string]any)); !reflect.DeepEqual(got, wantInner) {
			t.Fatalf("sort=%v inner keys %v", sortKeys, got)
		}
	}
	opts := DefaultSafeDumpOptions()
	opts.AllowUnicode = false
	escaped, err := SafeDumpWith(data, opts)
	if err != nil || strings.Contains(string(escaped), "hé") {
		t.Fatalf("escaped = %q %v", escaped, err)
	}
	if loaded, err := SafeLoad(escaped); err != nil || !reflect.DeepEqual(loaded, data) {
		t.Fatalf("escaped reload = %#v %v", loaded, err)
	}
	opts = DefaultSafeDumpOptions()
	opts.DefaultFlowStyle, opts.Width = true, 100000
	flow, err := SafeDumpWith(data, opts)
	if err != nil || !strings.HasPrefix(string(flow), "{") || len(strings.Split(strings.TrimSuffix(string(flow), "\n"), "\n")) != 1 {
		t.Fatalf("flow = %q %v", flow, err)
	}
	if loaded, err := SafeLoad(flow); err != nil || !reflect.DeepEqual(loaded, data) {
		t.Fatalf("flow reload = %#v %v", loaded, err)
	}
}

func TestRoundtripPreservesCommentsQuotesAndScalarTypes(t *testing.T) {
	editor := NewRoundtripYAML()
	original := "# keep this\nname: \"hello 🦀\"  # note\nflag: off\n"
	doc, err := editor.Load(original)
	if err != nil {
		t.Fatal(err)
	}
	value, err := doc.Value()
	if err != nil || value.(map[string]any)["flag"] != false {
		t.Fatalf("flag = %#v %v", value, err)
	}
	if err := doc.Set("mode", "off"); err != nil {
		t.Fatal(err)
	}
	var stream strings.Builder
	if err := editor.Dump(doc, &stream); err != nil {
		t.Fatal(err)
	}
	text := stream.String()
	if !strings.HasPrefix(text, "# keep this\nname: \"hello 🦀\"  # note\n") {
		t.Fatalf("text:\n%s", text)
	}
	loaded, err := SafeLoad(text)
	if err != nil || !reflect.DeepEqual(loaded, map[string]any{"name": "hello 🦀", "flag": false, "mode": "off"}) {
		t.Fatalf("reload = %#v %v", loaded, err)
	}
}

func TestNativeYaml11ScalarsAndDuplicateKeyPolicy(t *testing.T) {
	loaders := append(yamlLoaders(), yamlLoader{"roundtrip_yaml", func(stream any) (any, error) {
		doc, err := NewRoundtripYAML().Load(stream)
		if err != nil {
			return nil, err
		}
		return doc.Value()
	}})
	for _, l := range loaders {
		t.Run(l.name, func(t *testing.T) {
			got, err := l.load("[y, n, Y, N, 'y', 'n']")
			if err != nil || !reflect.DeepEqual(got, []any{true, false, true, false, "y", "n"}) {
				t.Fatalf("scalars = %#v %v", got, err)
			}
			if _, err := l.load("model: first\nmodel: second\n"); !IsYAMLError(err) {
				t.Fatalf("duplicate err = %v", err)
			}
		})
	}
	merged, err := SafeLoad("defaults: &defaults {enabled: true}\nlocal: {<<: *defaults, enabled: false}\n")
	if err != nil || merged.(map[string]any)["local"].(map[string]any)["enabled"] != false {
		t.Fatalf("merged = %#v %v", merged, err)
	}
}

func TestParallelCallsDoNotShareParserOrEmitterState(t *testing.T) {
	results := make([]int, 32)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for index := range 32 {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			data := yamlDict("index", index, "words", []any{"yes", "no", "on", "off", "y", "n"})
			opts := DefaultSafeDumpOptions()
			opts.SortKeys = index%2 == 1
			text, err := SafeDumpWith(data, opts)
			if err != nil {
				t.Error(err)
				return
			}
			if loaded, err := SafeLoad(text); err != nil || !reflect.DeepEqual(loaded, data) {
				t.Errorf("safe_load %d = %#v %v", index, loaded, err)
			}
			doc, err := NewRoundtripYAML().Load(text)
			if err != nil {
				t.Error(err)
				return
			}
			if loaded, err := doc.Value(); err != nil || !reflect.DeepEqual(loaded, data) {
				t.Errorf("roundtrip %d = %#v %v", index, loaded, err)
			}
			results[index] = index
		}()
	}
	wg.Wait()
	for i, got := range results {
		if got != i {
			t.Fatalf("results %v", results)
		}
	}
	if _, err := SafeLoad("key: [unterminated"); !IsYAMLError(err) {
		t.Fatalf("unterminated err = %v", err)
	}
	if _, err := SafeDumpWith(struct{}{}, DefaultSafeDumpOptions()); !IsYAMLError(err) {
		t.Fatalf("object err = %v", err)
	}
	healthy, err := SafeDumpWith(map[string]any{"healthy": true}, DefaultSafeDumpOptions())
	if err != nil {
		t.Fatal(err)
	}
	if loaded, err := SafeLoad(healthy); err != nil || !reflect.DeepEqual(loaded, map[string]any{"healthy": true}) {
		t.Fatalf("healthy = %#v %v", loaded, err)
	}
}

func TestSafeLoadRefusesASecondDocumentAndUnknownTags(t *testing.T) {
	for doc, want := range map[string]string{
		"a: [1, 2]\n---\nb: 1\n": "yaml: line 2: but found another document",
		"a: 1\n---\n":            "yaml: line 2: but found another document",
		"!foo bar":               "yaml: line 1: could not determine a constructor for the tag '!foo'",
		"%TAG !e! tag:example.com,2000:\n---\na: !e!foo 1\n": "yaml: line 3: could not determine a constructor for the tag 'tag:example.com,2000:foo'",
	} {
		if _, err := SafeLoad(doc); !IsYAMLError(err) || err.Error() != want {
			t.Fatalf("%q err = %v", doc, err)
		}
	}
	for doc, want := range map[string]any{
		"---\na: 1\n...\n":    map[string]any{"a": 1},
		"# only comment\n":    nil,
		"---\n":               nil,
		"a: ! 12\n":           map[string]any{"a": 12},
		"a: !!str 1\n":        map[string]any{"a": "1"},
		"!!omap [a: 1, b: 2]": map[string]any{"a": 1, "b": 2},
		"!!pairs [a: 1]":      []any{[]any{"a", 1}},
	} {
		got, err := SafeLoad(doc)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q = %#v %v", doc, got, err)
		}
	}
	if _, err := SafeLoad("!!omap [a: 1, [b]]"); err == nil || !strings.Contains(err.Error(), "while constructing an ordered map expected a mapping of length 1, but found sequence") {
		t.Fatalf("omap err = %v", err)
	}
}

func TestSafeLoadHonorsTheYAMLVersionDirective(t *testing.T) {
	got, err := SafeLoad("%YAML 1.2\n---\n[012, 0o12, 0x1F, 1_000, 1:20, y, True, TRUE, Yes, .5, 1e3, 1.5e3, null, ~, 0b11, +12, -0o7, .inf]\n")
	want := []any{12, 10, 31, 1000, "1:20", "y", true, true, "Yes", 0.5, 1000.0, 1500.0, nil, nil, 3, 12, -7}
	if err != nil {
		t.Fatal(err)
	}
	list := got.([]any)
	if !reflect.DeepEqual(list[:len(want)], want) || list[len(want)].(float64) <= 1e308 {
		t.Fatalf("1.2 = %#v", got)
	}
	got, err = SafeLoad("%YAML 1.1\n---\na: on\n")
	if err != nil || !reflect.DeepEqual(got, map[string]any{"a": true}) {
		t.Fatalf("1.1 = %#v %v", got, err)
	}
	if _, err := SafeLoad("%YAML 2.0\n---\na: on\n"); !IsYAMLError(err) || err.Error() != "yaml: line 1: found incompatible YAML document (version 1.* is required)" {
		t.Fatalf("2.0 err = %v", err)
	}
	if _, err := SafeLoad("%YAML 1.3\n---\na: on\n"); err == nil || IsYAMLError(err) || err.Error() != "version minor part can only be 2 or 1, got (1, 3)" {
		t.Fatalf("1.3 err = %v", err)
	}
}

func TestRoundtripLoadRefusesDuplicatesAndQuotesCreatedStrings(t *testing.T) {
	editor := NewRoundtripYAML()
	for _, doc := range []string{"a: 1\na: 2\n", "1: a\ntrue: b\n", "a: 1\n---\nb: 2\n"} {
		if _, err := editor.Load(doc); !IsYAMLError(err) {
			t.Fatalf("%q err = %v", doc, err)
		}
	}
	doc, err := editor.Load("a: 1\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]any{{"n", "x"}, {"1", "y"}, {"m", "line1\nline2"}, {"q", "123"}, {"o", yamlDict("z", 1, "a", []any{1, "off"})}, {"e", "it's"}} {
		if err := doc.Set(kv[0].(string), kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	if err := editor.Dump(doc, &out); err != nil {
		t.Fatal(err)
	}
	want := "a: 1\n'n': x\n'1': 'y'\nm: \"line1\\nline2\"\nq: '123'\no:\n  z: 1\n  a:\n    - 1\n    - 'off'\ne: it's\n"
	if out.String() != want {
		t.Fatalf("dump = %q", out.String())
	}
}

func TestSafeDumpMatchesTheRuamelEmitter(t *testing.T) {
	shared := map[string]any{"k": "v"}
	list := []any{1, 2}
	opt := func(sortKeys, flow, unicode bool, width int) SafeDumpOptions {
		return SafeDumpOptions{SortKeys: sortKeys, DefaultFlowStyle: flow, AllowUnicode: unicode, Width: width}
	}
	cases := []struct {
		data any
		opts SafeDumpOptions
		want string
	}{
		{yamlDict("zebra", yamlDict("zed", 1, "alpha", 2), "alpha", "hé"), opt(true, false, true, 80), "alpha: hé\nzebra:\n  alpha: 2\n  zed: 1\n"},
		{yamlDict("zebra", yamlDict("zed", 1, "alpha", 2), "alpha", "hé"), opt(true, false, false, 80), "alpha: \"h\\xE9\"\nzebra:\n  alpha: 2\n  zed: 1\n"},
		{yamlDict("zebra", yamlDict("zed", 1, "alpha", 2), "alpha", "hé"), opt(true, true, true, 80), "{alpha: hé, zebra: {alpha: 2, zed: 1}}\n"},
		{yamlDict("k", []any{"a", yamlDict("b", 1), []any{}, map[string]any{}, []any{1, []any{2, 3}}, yamlDict("x", "y", "z", []any{nil, true, 1.5})}, "s", "a b: c", "q", "it's", "e", "", "u", "🦀 crab"),
			opt(false, true, true, 20), "{k: [a, b: 1, [], {},\n   [1, [2, 3]], {x: 'y',\nz: [null, true, 1.5]}],\ns: 'a b: c', q: it's,\ne: '', u: 🦀 crab}\n"},
		{yamlDict("list", []any{strings.Repeat("x", 30), strings.Repeat("y", 30), strings.Repeat("z", 30), strings.Repeat("w", 10)}), opt(false, true, true, 80),
			"{list: [xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx, yyyyyyyyyyyyyyyyyyyyyyyyyyyyyy, zzzzzzzzzzzzzzzzzzzzzzzzzzzzzz,\nwwwwwwwwww]}\n"},
		{yamlDict("u", "é🦀\u2028x"), opt(false, false, false, 80), "u: \"\\xE9\\U0001F980\\Lx\"\n"},
		{yamlDict("a", strings.Repeat("word ", 30)), opt(true, false, true, 3), "a: 'word word word word word word word word word word word word word word word word\n  word word word word word word word word word word word word word word '\n"},
		{yamlDict("a", strings.Repeat("word ", 30)), opt(true, false, true, 40), "a: 'word word word word word word word word\n  word word word word word word word word\n  word word word word word word word word\n  word word word word word word '\n"},
		{[]any{1, "off", yamlDict("a", "y")}, opt(true, true, true, 80), "[1, 'off', a: 'y']\n"},
		{"scalar", opt(true, false, true, 80), "scalar\n...\n"},
		{"off", opt(true, false, true, 80), "'off'\n"},
		{map[string]any{}, opt(true, true, true, 80), "{}\n"},
		{map[string]any{}, opt(true, false, true, 80), "{}\n"},
		{[]any{}, opt(true, false, true, 80), "[]\n"},
		{yamlDict("a,b", "c[d]", "x", "-a", "y", "?", "z", ":x", "w", "a#b", "v", "a #b", "q", "k:v"), opt(false, true, true, 80), "{'a,b': 'c[d]', x: -a, 'y': '?', z: :x, w: a#b, v: 'a #b', q: k:v}\n"},
		{yamlDict("a", shared, "b", shared, "c", list, "d", list), opt(false, false, true, 80), "a: &id001\n  k: v\nb: *id001\nc: &id002\n  - 1\n  - 2\nd: *id002\n"},
		{yamlDict("a", shared, "b", shared, "c", list, "d", list), opt(false, true, true, 80), "{a: &id001 {k: v}, b: *id001, c: &id002 [1, 2], d: *id002}\n"},
		{yamlDict("k", []any{[]any{1, 2}, []any{3}}, "m", []any{yamlDict("a", 1, "b", []any{2})}, "e", []any{[]any{}, map[string]any{}}), opt(false, false, true, 80),
			"k:\n  -   - 1\n      - 2\n  -   - 3\nm:\n  - a: 1\n    b:\n      - 2\ne:\n  - []\n  - {}\n"},
		{yamlDict(strings.Repeat("x", 130), 1, "n", nil), opt(false, false, true, 80), "? " + strings.Repeat("x", 130) + "\n: 1\n'n': null\n"},
		{yamlDict("m", "line1\nline2", "s", "it's", "t", " lead", "f", 1.0, "g", 1e20, "i", -7), opt(false, false, true, 80), "m: \"line1\\nline2\"\ns: it's\nt: ' lead'\nf: 1.0\ng: 1e+20\ni: -7\n"},
		{yamlDict("p", strings.Repeat(`D:\Cent `, 20)), opt(true, false, true, 80), "p: 'D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent\n  D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent D:\\Cent '\n"},
		{map[int]string{2: "b", 1: "a"}, opt(true, false, true, 80), "1: a\n2: b\n"},
		{yamlDict("p", strings.Repeat(`D:\Cent it's `, 12)), opt(true, false, true, 80),
			"p: \"D:\\\\Cent it's D:\\\\Cent it's D:\\\\Cent it's D:\\\\Cent it's D:\\\\Cent it's D:\\\\Cent\n  it's D:\\\\Cent it's D:\\\\Cent it's D:\\\\Cent it's D:\\\\Cent it's D:\\\\Cent it's D:\\\\\\\n  Cent it's \"\n"},
		{yamlDict("p", strings.Repeat("a\tb  ", 30)), opt(true, false, true, 80),
			"p: \"a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb \\\n  \\ a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb  a\\tb \\\n  \\ a\\tb  a\\tb  a\\tb  a\\tb  \"\n"},
		{yamlDict("p", strings.Repeat("x'y z\n", 3)+"w"), opt(true, false, true, 80), "p: \"x'y z\\nx'y z\\nx'y z\\nw\"\n"},
		{yamlDict("n", yamlDict("d", strings.Repeat("it's a long ", 10))), opt(true, true, true, 80),
			"{'n': {d: \"it's a long it's a long it's a long it's a long it's a long it's a long\nit's a long it's a long it's a long it's a long \"}}\n"},
	}
	for i, c := range cases {
		got, err := SafeDumpWith(c.data, c.opts)
		if err != nil || string(got) != c.want {
			t.Errorf("case %d = %q %v\nwant %q", i, got, err, c.want)
		}
	}
}
