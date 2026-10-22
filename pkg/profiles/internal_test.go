package profiles

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"

	_ "github.com/nickheyer/icebath/pkg/protocol/ollama-chat"
)

func TestSources(t *testing.T) {
	ctx := context.Background()
	if v, err := (Source{}).Resolve(ctx); v != "" || err != nil {
		t.Fatal(v, err)
	}
	t.Setenv("PROFILES_TEST_KEY", "fromenv")
	if v, _ := Env("PROFILES_TEST_KEY").Resolve(ctx); v != "fromenv" {
		t.Fatal(v)
	}
	if v, _ := Static("lit").Resolve(ctx); v != "lit" {
		t.Fatal(v)
	}
	path := filepath.Join(t.TempDir(), "key")
	os.WriteFile(path, []byte("  filekey\n"), 0o600)
	if v, err := File(path).Resolve(ctx); v != "filekey" || err != nil {
		t.Fatal(v, err)
	}
	if _, err := File(path + ".missing").Resolve(ctx); err == nil {
		t.Fatal("missing file accepted")
	}
	if v, err := Exec("printf '  execkey '").Resolve(ctx); v != "execkey" || err != nil {
		t.Fatal(v, err)
	}
	if _, err := Exec("echo nope >&2; exit 3").Resolve(ctx); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("%v", err)
	}
	if _, err := (Source{Kind: "vault", Value: "x"}).Resolve(ctx); err == nil {
		t.Fatal("unknown kind accepted")
	}
	for in, want := range map[string]Source{`"env:X"`: Env("X"), `{"kind":"file","value":"/p"}`: File("/p"), `"exec:op read x:y"`: Exec("op read x:y")} {
		var s Source
		if err := json.Unmarshal([]byte(in), &s); err != nil || s != want {
			t.Fatalf("%s: %+v %v", in, s, err)
		}
	}
	var s Source
	if err := json.Unmarshal([]byte(`"nocolon"`), &s); err == nil {
		t.Fatal("bare string accepted")
	}
	if err := json.Unmarshal([]byte(`[1]`), &s); err == nil {
		t.Fatal("array accepted")
	}
	if Static("k").Describe() != "static key" || Env("X").Describe() != "env X" || (Source{}).Describe() != "no key source" {
		t.Fatal("describe")
	}
}

func TestValidateBuildAndOpen(t *testing.T) {
	ctx := context.Background()
	p := Profile{Name: "q", Protocol: protocol.OllamaChat, BaseURL: "http://example", Key: Env("PROFILES_TEST_KEY"), Auth: AuthQuery, AuthName: "key", Header: http.Header{"X-A": {"1"}}, Query: url.Values{"v": {"2"}}, Models: []string{"m1", "m2"}}
	if _, err := p.Build(""); err == nil {
		t.Fatal("missing credential accepted")
	}
	built, err := p.Build("k")
	if err != nil || built.Auth != (provider.Query{Name: "key", Value: "k"}) || built.Retry != provider.DefaultRetry || built.Protocol.ID() != protocol.OllamaChat || built.Header.Get("X-A") != "1" || built.Query.Get("v") != "2" {
		t.Fatalf("%v %+v", err, built)
	}
	built.Header.Set("X-A", "changed")
	if p.Header.Get("X-A") != "1" {
		t.Fatal("build must copy headers")
	}
	if p.Model() != "m1" || (Profile{}).Model() != "" {
		t.Fatal("model")
	}
	t.Setenv("PROFILES_TEST_KEY", "")
	if _, err := p.Open(ctx); err == nil || !strings.Contains(err.Error(), "env PROFILES_TEST_KEY") {
		t.Fatalf("unset env: %v", err)
	}
	t.Setenv("PROFILES_TEST_KEY", "fromenv")
	built, err = p.Open(ctx)
	if err != nil || built.Auth != (provider.Query{Name: "key", Value: "fromenv"}) {
		t.Fatalf("%v %+v", err, built)
	}
	bearer := Profile{Name: "b", Protocol: protocol.OllamaChat, BaseURL: "http://example", Auth: AuthBearer}
	if _, err := bearer.Open(ctx); err == nil || !strings.Contains(err.Error(), "no key source") {
		t.Fatalf("missing source: %v", err)
	}
	if built, err := bearer.Build("tok"); err != nil || built.Auth != provider.Bearer("tok") {
		t.Fatalf("%v %+v", err, built)
	}
	header := Profile{Name: "h", Protocol: protocol.OllamaChat, BaseURL: "http://example", Auth: AuthHeader, AuthName: "x-api-key"}
	if built, err := header.Build("k"); err != nil || built.Auth != (provider.Header{Name: "x-api-key", Value: "k"}) {
		t.Fatalf("%v %+v", err, built)
	}
	none := Profile{Name: "n", Protocol: protocol.OllamaChat, BaseURL: "http://example", Auth: AuthNone}
	if built, err := none.Open(ctx); err != nil || built.Auth != (provider.None{}) {
		t.Fatalf("%v %+v", err, built)
	}
	failing := Profile{Name: "f", Protocol: protocol.OllamaChat, BaseURL: "http://example", Auth: AuthBearer, Key: Exec("exit 1")}
	if _, err := failing.Open(ctx); err == nil || !strings.Contains(err.Error(), "profile f") {
		t.Fatalf("%v", err)
	}
	for name, bad := range map[string]Profile{
		"unknown protocol": {Name: "x", Protocol: "nope", BaseURL: "http://e", Auth: AuthBearer},
		"unknown auth":     {Name: "x", Protocol: protocol.OllamaChat, BaseURL: "http://e", Auth: "magic"},
		"header no name":   {Name: "x", Protocol: protocol.OllamaChat, BaseURL: "http://e", Auth: AuthHeader},
		"query no name":    {Name: "x", Protocol: protocol.OllamaChat, BaseURL: "http://e", Auth: AuthQuery},
		"bad name":         {Name: "bad name", Protocol: protocol.OllamaChat, BaseURL: "http://e"},
		"empty name":       {Protocol: protocol.OllamaChat, BaseURL: "http://e"},
		"no protocol":      {Name: "x", BaseURL: "http://e"},
		"bad url":          {Name: "x", Protocol: protocol.OllamaChat, BaseURL: "example"},
		"bad source":       {Name: "x", Protocol: protocol.OllamaChat, BaseURL: "http://e", Key: Source{Kind: "vault", Value: "x"}},
		"empty source":     {Name: "x", Protocol: protocol.OllamaChat, BaseURL: "http://e", Key: Source{Kind: SourceEnv}},
	} {
		if _, err := bad.Build("k"); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

type stubAuth struct{ who string }

func (s stubAuth) Apply(r *http.Request) { r.Header.Set("X-Who", s.who) }

func TestRegistries(t *testing.T) {
	RegisterAuth("stub", func(p Profile, key string) (provider.Auth, error) {
		return stubAuth{who: p.Options["who"] + key}, nil
	})
	for _, kind := range []AuthKind{AuthBearer, "", "stub"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%q registered twice or as built in", kind)
				}
			}()
			RegisterAuth(kind, nil)
		}()
	}
	custom := Profile{Name: "custom", Protocol: protocol.OllamaChat, BaseURL: "http://example", Auth: "stub", Options: map[string]string{"who": "me-"}, Key: Static("k")}
	built, err := custom.Open(context.Background())
	if err != nil || built.Auth != (stubAuth{who: "me-k"}) {
		t.Fatalf("%v %+v", err, built)
	}
	if built, err := custom.Build(""); err != nil || built.Auth != (stubAuth{who: "me-"}) {
		t.Fatalf("registered kinds decide about missing keys themselves: %v %+v", err, built)
	}
	Register(Profile{Name: "zz-test", Protocol: protocol.OllamaChat, BaseURL: "http://example"})
	Register(Profile{Name: "aa-test", Protocol: protocol.OllamaChat, BaseURL: "http://example"})
	if p, ok := Lookup("zz-test"); !ok || p.BaseURL != "http://example" {
		t.Fatal("lookup")
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("unknown profile resolved")
	}
	defaults := Defaults()
	if len(defaults) < 2 || defaults[0].Name != "aa-test" || defaults[len(defaults)-1].Name != "zz-test" {
		t.Fatalf("%+v", defaults)
	}
	for name, bad := range map[string]Profile{"dup": {Name: "zz-test", Protocol: protocol.OllamaChat, BaseURL: "http://e"}, "invalid": {Name: "x"}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s registered", name)
				}
			}()
			Register(bad)
		}()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/list" || r.URL.Query().Get("page") != "2" || r.Header.Get("X-Who") != "me-k" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"models":[{"id":"a"}]}`))
	}))
	defer srv.Close()
	RegisterVendor(Vendor{Name: "stub", Models: func(ctx context.Context, p *provider.Provider) ([]Model, error) {
		var doc struct{ Models []Model }
		if err := GetJSON(ctx, p, "/list", url.Values{"page": {"2"}}, &doc); err != nil {
			return nil, err
		}
		return doc.Models, nil
	}})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("duplicate vendor registered")
			}
		}()
		RegisterVendor(Vendor{Name: "stub", Models: func(context.Context, *provider.Provider) ([]Model, error) { return nil, nil }})
	}()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("empty vendor registered")
			}
		}()
		RegisterVendor(Vendor{})
	}()
	if v := Vendors(); len(v) < 1 || v[len(v)-1] != "stub" {
		t.Fatalf("%v", v)
	}
	custom.Vendor, custom.BaseURL = "stub", srv.URL
	prov, _ := custom.Build("k")
	models, err := custom.ListModels(context.Background(), prov)
	if err != nil || len(models) != 1 || models[0].ID != "a" {
		t.Fatalf("%v %+v", err, models)
	}
	custom.Vendor = "ghost"
	if _, err := custom.ListModels(context.Background(), prov); err == nil {
		t.Fatal("unknown vendor listed models")
	}
	bad := &provider.Provider{Name: "x", Protocol: prov.Protocol, BaseURL: srv.URL, Auth: provider.None{}}
	if err := GetJSON(context.Background(), bad, "/list", nil, &struct{}{}); err == nil {
		t.Fatal("http error swallowed")
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not json")) }))
	defer srv2.Close()
	bad.BaseURL = srv2.URL
	if err := GetJSON(context.Background(), bad, "/x", nil, &struct{}{}); err == nil || !strings.Contains(err.Error(), "GET /x") {
		t.Fatalf("%v", err)
	}
}

func TestLoadMergeAndFind(t *testing.T) {
	doc := `{"profiles":[
	 {"name":"work","vendor":"stub","protocol":"ollama-chat","base_url":"http://work","auth":"bearer","key":"env:WORK_KEY","models":["m1"],"extra":{"temperature":0.1},"header":{"X-Org":["o"]},"options":{"region":"eu"}},
	 {"name":"home","protocol":"ollama-chat","base_url":"http://home","key":{"kind":"file","value":"/tmp/k"}}
	]}`
	list, err := Load(strings.NewReader(doc))
	if err != nil || len(list) != 2 || list[0].Key != Env("WORK_KEY") || list[0].Model() != "m1" || string(list[0].Extra["temperature"]) != "0.1" || list[0].Header.Get("X-Org") != "o" || list[0].Options["region"] != "eu" || list[1].Key != File("/tmp/k") {
		t.Fatalf("%v %+v", err, list)
	}
	flat, err := Load(strings.NewReader(`[{"name":"only","protocol":"ollama-chat","base_url":"http://x"}]`))
	if err != nil || len(flat) != 1 {
		t.Fatalf("%v %+v", err, flat)
	}
	for name, bad := range map[string]string{
		"json":    `{`,
		"invalid": `[{"name":"x"}]`,
		"dup":     `[{"name":"x","protocol":"ollama-chat","base_url":"http://x"},{"name":"x","protocol":"ollama-chat","base_url":"http://y"}]`,
		"source":  `[{"name":"x","protocol":"ollama-chat","base_url":"http://x","key":"bare"}]`,
		"empty":   ``,
	} {
		if _, err := Load(strings.NewReader(bad)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := Load(strings.NewReader(`[{"name":"x","protocol":"ollama-chat","base_url":"http://x","key":"bare"}]`)); err == nil || !strings.Contains(err.Error(), "kind:value") {
		t.Fatalf("a list must report its own decoding error: %v", err)
	}
	if _, err := Load(strings.NewReader(`{"profiles":[{"name":"x","protocol":"ollama-chat","base_url":"http://x","key":"bare"}]}`)); err == nil || !strings.Contains(err.Error(), "kind:value") {
		t.Fatalf("an object must report its own decoding error: %v", err)
	}
	built, err := list[0].Build("k")
	if err != nil || string(built.Extra["temperature"]) != "0.1" {
		t.Fatalf("a built provider must carry the profile extras: %v %+v", err, built)
	}
	built.Extra["temperature"] = json.RawMessage(`0.9`)
	if string(list[0].Extra["temperature"]) != "0.1" {
		t.Fatal("the provider must own a copy of the extras")
	}
	path := filepath.Join(t.TempDir(), "profiles.json")
	os.WriteFile(path, []byte(doc), 0o600)
	if list, err := LoadFile(path); err != nil || len(list) != 2 {
		t.Fatalf("%v %+v", err, list)
	}
	if _, err := LoadFile(path + ".missing"); err == nil {
		t.Fatal("missing file accepted")
	}
	merged := Merge([]Profile{{Name: "b", BaseURL: "old"}, {Name: "a"}}, []Profile{{Name: "b", BaseURL: "new"}, {Name: "c"}})
	if len(merged) != 3 || merged[0].Name != "a" || merged[1].BaseURL != "new" || merged[2].Name != "c" {
		t.Fatalf("%+v", merged)
	}
	Register(Profile{Name: "find-test", Protocol: protocol.OllamaChat, BaseURL: "http://builtin"})
	if p, ok := Find("find-test", list); !ok || p.BaseURL != "http://builtin" {
		t.Fatal("built in")
	}
	if p, ok := Find("find-test", []Profile{{Name: "find-test", BaseURL: "http://over"}}); !ok || p.BaseURL != "http://over" {
		t.Fatal("override")
	}
	if p, ok := Find("work", list); !ok || p.BaseURL != "http://work" {
		t.Fatal("loaded")
	}
	if _, ok := Find("ghost", list); ok {
		t.Fatal("ghost found")
	}
}

func TestReadEarlierFormatAndDefault(t *testing.T) {
	earlier := `{"live":"idk","profiles":{"idk":{"API_KEY":"k-1","BASE_URL":"http://192.168.1.27:8484","MODEL":"freddie"},"bare":{"BASE_URL":"http://host/v1/"}}}`
	doc, err := Read(strings.NewReader(earlier))
	if err != nil || doc.Default != "idk" || len(doc.Profiles) != 2 {
		t.Fatalf("%v %+v", err, doc)
	}
	bare, idk := doc.Profiles[0], doc.Profiles[1]
	if idk.Name != "idk" || idk.Vendor != "openai" || idk.Protocol != protocol.OpenAICompletions || idk.BaseURL != "http://192.168.1.27:8484/v1" || idk.Auth != AuthBearer || idk.Key != Static("k-1") || idk.Model() != "freddie" {
		t.Fatalf("%+v", idk)
	}
	if bare.Name != "bare" || bare.BaseURL != "http://host/v1" || bare.Auth != AuthNone || bare.Key != (Source{}) || bare.Model() != "" {
		t.Fatalf("%+v", bare)
	}
	prov, err := idk.Open(context.Background())
	if err != nil || prov.BaseURL != "http://192.168.1.27:8484/v1" {
		t.Fatalf("an earlier entry must open as is: %v %+v", err, prov)
	}
	if doc, err := Read(strings.NewReader(`{"default":"home","profiles":[{"name":"home","protocol":"ollama-chat","base_url":"http://home"}]}`)); err != nil || doc.Default != "home" || len(doc.Profiles) != 1 {
		t.Fatalf("%v %+v", err, doc)
	}
	if doc, err := Read(strings.NewReader(`{"live":"home","profiles":[{"name":"home","protocol":"ollama-chat","base_url":"http://home"}]}`)); err != nil || doc.Default != "home" {
		t.Fatalf("live must name the default beside a list too: %v %+v", err, doc)
	}
	if doc, err := Read(strings.NewReader(`{"default":"anthropic"}`)); err != nil || doc.Default != "anthropic" || len(doc.Profiles) != 0 {
		t.Fatalf("a default alone must be accepted: %v %+v", err, doc)
	}
	if doc, err := Read(strings.NewReader(`[{"name":"only","protocol":"ollama-chat","base_url":"http://x"}]`)); err != nil || doc.Default != "" {
		t.Fatalf("%v %+v", err, doc)
	}
	for name, bad := range map[string]string{
		"no url":     `{"profiles":{"x":{"API_KEY":"k"}}}`,
		"bad name":   `{"profiles":{"bad name":{"BASE_URL":"http://x"}}}`,
		"not object": `{"profiles":{"x":"http://x"}}`,
		"not json":   `{"profiles":{"x":{"BASE_URL":}}}`,
	} {
		if _, err := Read(strings.NewReader(bad)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if doc, err := Read(strings.NewReader(`{"profiles":{"x":{"API_KEY":"k","BASE_URL":"http://x","PROTOCOL":"p"}}}`)); err != nil || len(doc.Profiles) != 1 || doc.Profiles[0].Name != "x" {
		t.Fatalf("an unknown earlier field must be ignored: %v %+v", err, doc)
	}
}

func TestTemplatesFillWhatAProfileLeavesEmpty(t *testing.T) {
	Register(Profile{Name: "tpl-test", Vendor: "v", Protocol: "p", BaseURL: "http://tpl", Auth: AuthHeader, AuthName: "x-key", Key: Env("TPL_KEY"), Models: []string{"m1"}, BaseURLEnv: "TPL_URL", Options: map[string]string{"o": "1"}})
	p, err := Profile{Name: "mine", Template: "tpl-test", Models: []string{"m2"}}.Expand()
	if err != nil || p.Vendor != "v" || p.Protocol != "p" || p.BaseURL != "http://tpl" || p.Auth != AuthHeader || p.AuthName != "x-key" || p.Key.Value != "TPL_KEY" || p.Model() != "m2" || p.BaseURLEnv != "TPL_URL" || p.Options["o"] != "1" || p.Template != "tpl-test" {
		t.Fatalf("%+v %v", p, err)
	}
	own, _ := Profile{Name: "own", Template: "tpl-test", Auth: AuthBearer, Key: Static("k")}.Expand()
	if own.Auth != AuthBearer || own.AuthName != "" || own.Key.Value != "k" {
		t.Fatalf("a profile keeps the auth and key it names %+v", own)
	}
	if _, err := (Profile{Name: "x", Template: "nowhere"}).Expand(); err == nil {
		t.Fatal("an unknown template accepted")
	}
	bare, _ := Profile{Name: "bare", Template: "tpl-test", Key: Source{Kind: SourceNone}}.Expand()
	if bare.Key.Kind != SourceNone || bare.Key.Describe() != "no key source" || bare.Validate() != nil {
		t.Fatalf("an explicit none keeps the template key out %+v", bare.Key)
	}
	if k, err := bare.Key.Resolve(context.Background()); k != "" || err != nil {
		t.Fatal("none yields no credential")
	}
	if _, err := bare.Open(context.Background()); err == nil || !strings.Contains(err.Error(), "no key source is set") {
		t.Fatalf("a profile that needs a key says none is set %v", err)
	}
	if p, _ := (Profile{Name: "plain"}).Expand(); p.Name != "plain" || p.BaseURL != "" {
		t.Fatal("a profile without a template stays as it is")
	}
	doc, err := Read(strings.NewReader(`{"default":"mine","profiles":[{"name":"mine","template":"tpl-test"},{"name":"off","template":"tpl-test","disabled":true}]}`))
	if err != nil || doc.Profiles[0].BaseURL != "" || !doc.Profiles[1].Disabled {
		t.Fatalf("a document keeps profiles as written %+v %v", doc, err)
	}
	list, _ := doc.Expanded()
	if list[0].BaseURL != "http://tpl" {
		t.Fatal("expanded profiles carry the template")
	}
	if _, err := Read(strings.NewReader(`[{"name":"x","template":"nowhere"}]`)); err == nil {
		t.Fatal("reading checks templates")
	}
	if !ValidName("a.b-c_1") || ValidName("-a") || ValidName("") {
		t.Fatal("names")
	}
	kinds := AuthKinds()
	if len(kinds) < 4 || kinds[0] != AuthNone || kinds[3] != AuthQuery {
		t.Fatalf("generic auth kinds lead %v", kinds)
	}
}

func TestDocumentsEditAndWrite(t *testing.T) {
	var d Document
	if err := d.Put("", Profile{Name: "a", Protocol: "p", BaseURL: "http://a"}); err != nil {
		t.Fatal(err)
	}
	d.Put("", Profile{Name: "b", Protocol: "p", BaseURL: "http://b"})
	d.Put("", Profile{Name: "c", Protocol: "p", BaseURL: "http://c"})
	d.Default = "a"
	if d.Put("", Profile{Name: "b"}) == nil || d.Put("ghost", Profile{Name: "g"}) == nil {
		t.Fatal("duplicates and missing names are refused")
	}
	if err := d.Put("a", Profile{Name: "z", Protocol: "p", BaseURL: "http://z"}); err != nil || d.Profiles[0].Name != "z" || d.Default != "z" {
		t.Fatalf("a rename keeps its place and the default follows %+v", d)
	}
	d.Move("c", -5)
	d.Move("z", 1)
	if d.Profiles[0].Name != "c" || d.Profiles[1].Name != "b" || d.Profiles[2].Name != "z" {
		t.Fatalf("moves hold at the ends %+v", d.Profiles)
	}
	if d.Move("ghost", 1) == nil || d.Remove("ghost") == nil {
		t.Fatal("missing profiles are reported")
	}
	d.Remove("z")
	if d.Default != "" || len(d.Profiles) != 2 || d.Index("b") != 1 {
		t.Fatal("removing the default clears it")
	}
	path := filepath.Join(t.TempDir(), "deep", "profiles.json")
	if err := WriteFile(path, d); err != nil {
		t.Fatal(err)
	}
	back, err := ReadFile(path)
	if err != nil || len(back.Profiles) != 2 || back.Profiles[0].Name != "c" {
		t.Fatalf("written documents read back %+v %v", back, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatal("profile files are readable by their owner alone")
	}
	d.Profiles = append(d.Profiles, Profile{Name: "c", Protocol: "p", BaseURL: "http://c"})
	if WriteFile(path, d) == nil {
		t.Fatal("an invalid document is not written")
	}
}
