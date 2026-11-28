package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
)

func setup(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	// The administrator's folder is one of the test's own, empty until a test fills it, and no package manager keeps the settings
	t.Setenv(config.ManagedDirEnv, filepath.Join(dir, "managed"))
	t.Setenv(config.ManagedSystemEnv, "")
	cfg := config.New(dir)
	cfg.Register(config.Sections{{Key: "app", Global: true, Title: "App", Settings: []config.Setting{
		{Key: "app.skin", Title: "Skin", Default: "dark"},
	}}})
	cfg.Register(config.Sections{{Key: "agent", Title: "Agent", Settings: []config.Setting{
		{Key: "agent.model", Title: "Model", Default: "", Env: []string{"ICEBATH_TEST_MODEL"}},
		{Key: "agent.max_turns", Title: "Max", Default: 0},
	}}})
	cfg.Register(config.Sections{{Key: "agent.approvals", Parent: "agent", Title: "Approvals", Settings: []config.Setting{
		{Key: "agent.approvals.mode", Title: "Mode", Default: "manual"},
	}}})
	value := []config.Setting{{Title: "Value", Default: ""}}
	cfg.Register(config.Sections{{Key: "net", Title: "Net", Settings: []config.Setting{
		{Key: "net.headers", Title: "Headers", Default: map[string]any{}, Entry: &config.Entry{Noun: "header", IgnoreCase: true, Fields: value}},
		{Key: "net.env", Title: "Variables", Default: map[string]any{}, Entry: &config.Entry{Noun: "variable", Fields: value}},
		{Key: "net.options", Title: "Options", Default: map[string]any{}},
		{Key: "net.servers", Title: "Servers", Default: map[string]any{}, Entry: &config.Entry{Noun: "server", Fields: []config.Setting{
			{Key: "command", Title: "Command", Default: ""},
			{Key: "env", Title: "Variables", Default: map[string]any{}, Entry: &config.Entry{Noun: "variable", Fields: value}},
		}}},
	}}})
	if err := cfg.Load(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Use("default"); err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

func TestScopes(t *testing.T) {
	cfg, dir := setup(t)
	if err := cfg.Set("app.skin", "light"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("agent.max_turns", 5); err != nil {
		t.Fatal(err)
	}
	shared, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	own, _ := os.ReadFile(filepath.Join(dir, "profiles", "default", "config.yaml"))
	if !strings.Contains(string(shared), "skin: light") || strings.Contains(string(shared), "max_turns") {
		t.Fatalf("shared file holds %q", shared)
	}
	if !strings.Contains(string(own), "max_turns: 5") || strings.Contains(string(own), "skin") {
		t.Fatalf("profile file holds %q", own)
	}
	if err := cfg.Use("work"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetInt("agent.max_turns"); got != 0 {
		t.Fatalf("another profile sees max_turns %d", got)
	}
	if got := cfg.GetString("app.skin"); got != "light" {
		t.Fatalf("another profile sees skin %q", got)
	}
}

func TestEnvironmentDefaultsAndRemoval(t *testing.T) {
	cfg, _ := setup(t)
	if got := cfg.GetString("agent.approvals.mode"); got != "manual" {
		t.Fatalf("default is %q", got)
	}
	if err := cfg.Set("agent.model", "a"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_TEST_MODEL", "b")
	if got := cfg.GetString("agent.model"); got != "b" {
		t.Fatalf("environment gives %q", got)
	}
	view := cfg.View("agent")
	if err := view.Remove(); err != nil {
		t.Fatal(err)
	}
	if view.Exists() {
		t.Fatal("agent is still kept after its removal")
	}
}

func TestKeysFollowTheTree(t *testing.T) {
	for name, section := range map[string]config.Section{
		"dotted top":       {Key: "a.b"},
		"not under parent": {Key: "other.b", Parent: "agent"},
		"two names below":  {Key: "agent.b.c", Parent: "agent"},
		"unknown parent":   {Key: "nothing.b", Parent: "nothing"},
		"global child":     {Key: "agent.b", Parent: "agent", Global: true},
		"setting outside":  {Key: "agent.b", Parent: "agent", Settings: []config.Setting{{Key: "agent.c"}}},
		"twice":            {Key: "agent.approvals", Parent: "agent"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _ := setup(t)
			defer func() {
				if recover() == nil {
					t.Fatal("registered without a panic")
				}
			}()
			cfg.Register(config.Sections{section})
		})
	}
}

type moving struct{}

// One section for each table under items, made from what is stored only
func (moving) Sections(values *config.Config) []config.Section {
	var sections []config.Section
	for _, name := range values.View("items").Names() {
		sections = append(sections, config.Section{Key: "items." + name, Parent: "items", Title: name, Settings: []config.Setting{
			{Key: "items." + name + ".size", Title: "Size", Default: 3},
		}})
	}
	return sections
}

func TestSectionsThatComeAndGo(t *testing.T) {
	cfg, _ := setup(t)
	cfg.Register(config.Sections{{Key: "items", Title: "Items"}})
	cfg.Register(moving{})
	if _, _, ok := cfg.Setting("items.one.size"); ok {
		t.Fatal("setting of an item that isn't there")
	}
	if err := cfg.View("items").View("one").Set("name", "One"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetInt("items.one.size"); got != 3 {
		t.Fatalf("default of an item is %d", got)
	}
	if _, ok := cfg.Section("items.one"); !ok {
		t.Fatal("no section for the item")
	}
}

// One section for each table under things, with a section of its accounts and one for each account, as the providers have
type things struct{}

func (things) Sections(values *config.Config) []config.Section {
	var sections []config.Section
	all := values.View("things")
	for _, name := range all.Names() {
		thing := all.View(name)
		accounts := thing.View("accounts")
		sections = append(sections,
			config.Section{Key: thing.Prefix(), Parent: "things", Title: name, Settings: []config.Setting{
				{Key: thing.Key("kind"), Title: "Kind", Default: ""},
				{Key: thing.Key("account"), Title: "Account", Default: ""},
				{Key: thing.Key("headers"), Title: "Headers", Default: map[string]any{},
					Entry: &config.Entry{Noun: "header", IgnoreCase: true, Fields: []config.Setting{{Title: "Value", Default: ""}}}},
			}},
			config.Section{Key: accounts.Prefix(), Parent: thing.Prefix(), Title: "Accounts"})
		for _, account := range accounts.Names() {
			fields := accounts.View(account)
			sections = append(sections, config.Section{Key: fields.Prefix(), Parent: accounts.Prefix(), Title: account, Settings: []config.Setting{
				{Key: fields.Key("api_key"), Title: "API key", Default: "", Secret: true},
				{Key: fields.Key("priority"), Title: "Order", Default: 0},
			}})
		}
	}
	return sections
}

func TestThingsWrittenWhole(t *testing.T) {
	cfg, dir := setup(t)
	cfg.Register(config.Sections{{Key: "things", Title: "Things"}})
	cfg.Register(things{})
	if err := cfg.View("things").Set("acme", map[string]any{"kind": "acme"}); err != nil {
		t.Fatal(err)
	}
	thing := cfg.View("things").View("acme").Within()
	kept, _ := thing.Stored("").(map[string]any)
	if kept["kind"] != "acme" {
		t.Fatalf("the thing holds %v", kept)
	}
	kept["accounts"] = map[string]any{"work": map[string]any{"api_key": "work-key"}}
	kept["account"] = "work"
	if err := thing.Replace(kept); err != nil {
		t.Fatal(err)
	}
	file, _ := os.ReadFile(filepath.Join(dir, "profiles", "default", "config.yaml"))
	if names := thing.View("accounts").Names(); len(names) != 1 || names[0] != "work" {
		t.Fatalf("accounts are %v, the file holds:\n%s", names, file)
	}
	if got := cfg.GetString("things.acme.accounts.work.api_key"); got != "work-key" {
		t.Fatalf("the key of the account is %q, the file holds:\n%s", got, file)
	}
	if got := cfg.GetString("things.acme.account"); got != "work" {
		t.Fatalf("the account in use is %q, the file holds:\n%s", got, file)
	}
}

func TestProfilesAtOnce(t *testing.T) {
	cfg, _ := setup(t)
	cfg.Register(config.Sections{{Key: "items", Title: "Items"}})
	cfg.Register(moving{})
	work := cfg.In("work")
	if err := work.Set("agent.max_turns", 7); err != nil {
		t.Fatal(err)
	}
	if err := work.View("items").View("two").Set("name", "Two"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetInt("agent.max_turns"); got != 0 {
		t.Fatalf("the profile in use sees %d", got)
	}
	if got := work.GetInt("agent.max_turns"); got != 7 {
		t.Fatalf("work sees %d", got)
	}
	if _, ok := cfg.Section("items.two"); ok {
		t.Fatal("the profile in use has the item of work")
	}
	if got := work.GetInt("items.two.size"); got != 3 {
		t.Fatalf("the item of work has size %d", got)
	}
	if err := cfg.Use("work"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetInt("agent.max_turns"); got != 7 || cfg.In("default").GetInt("agent.max_turns") != 0 {
		t.Fatal("switching the profile in use mixed up the profiles")
	}
}

// Text of the profile's settings file
func own(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "profiles", "default", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Writes the profile's settings file by hand
func handWrite(t *testing.T, dir, text string) {
	t.Helper()
	path := filepath.Join(dir, "profiles", "default", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNamesKeepTheirCase(t *testing.T) {
	cfg, dir := setup(t)
	if err := cfg.Set("net.headers.X-Test", "a"); err != nil {
		t.Fatal(err)
	}
	headers, _ := cfg.Get("net.headers").(map[string]any)
	if headers["X-Test"] != "a" || !strings.Contains(own(t, dir), "X-Test: a") {
		t.Fatalf("headers read %v, the file holds:\n%s", headers, own(t, dir))
	}
	// A header is found in any case, and a header written in another case is the same header
	if got := cfg.Get("net.headers.x-test"); got != "a" {
		t.Fatalf("x-test reads %v", got)
	}
	if err := cfg.Set("net.headers.x-test", "b"); err != nil {
		t.Fatal(err)
	}
	headers, _ = cfg.Get("net.headers").(map[string]any)
	if len(headers) != 1 || headers["x-test"] != "b" || strings.Contains(own(t, dir), "X-Test") {
		t.Fatalf("one header written in two cases reads %v, the file holds:\n%s", headers, own(t, dir))
	}
	// Variables are told apart by case
	for name, value := range map[string]string{"http_proxy": "lower", "HTTP_PROXY": "upper", "GOFLAGS": "-mod=mod"} {
		if err := cfg.Set("net.env."+name, value); err != nil {
			t.Fatal(err)
		}
	}
	env, _ := cfg.Get("net.env").(map[string]any)
	if len(env) != 3 || env["http_proxy"] != "lower" || env["HTTP_PROXY"] != "upper" || env["GOFLAGS"] != "-mod=mod" {
		t.Fatalf("variables read %v", env)
	}
	if cfg.Get("net.env.goflags") != nil || cfg.Get("net.env.http_proxy") != "lower" {
		t.Fatal("a variable answered to a name in another case")
	}
	for _, line := range []string{"http_proxy: lower", "HTTP_PROXY: upper", "GOFLAGS: -mod=mod"} {
		if !strings.Contains(own(t, dir), line) {
			t.Fatalf("the file lacks %q:\n%s", line, own(t, dir))
		}
	}
	// A group of values comes back name for name
	options := map[string]any{"usePlaceholders": true, "inlayHints": map[string]any{"parameterNames": "all"}}
	if err := cfg.Set("net.options", options); err != nil {
		t.Fatal(err)
	}
	got, _ := cfg.Get("net.options").(map[string]any)
	hints, _ := got["inlayHints"].(map[string]any)
	if got["usePlaceholders"] != true || hints["parameterNames"] != "all" || !strings.Contains(own(t, dir), "usePlaceholders: true") {
		t.Fatalf("options read %v, the file holds:\n%s", got, own(t, dir))
	}
	// The declared fields of an entry answer in any case, its name and the names of a table inside it only exactly
	if err := cfg.Set("net.servers.MyServer.COMMAND", "run"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("net.servers.MyServer.env.camelCase", "x"); err != nil {
		t.Fatal(err)
	}
	servers, _ := cfg.Get("net.servers").(map[string]any)
	server, _ := servers["MyServer"].(map[string]any)
	if server["command"] != "run" || cfg.Get("net.servers.myserver.command") != nil || cfg.Get("net.servers.MyServer.env.camelcase") != nil {
		t.Fatalf("servers read %v", servers)
	}
	if key := cfg.Key("NET.Servers.MyServer.Command"); key != "net.servers.MyServer.command" {
		t.Fatalf("the key is spelled %q", key)
	}
}

func TestDeclaredKeysInAnyCase(t *testing.T) {
	cfg, dir := setup(t)
	handWrite(t, dir, "# My settings\nAgent:\n  # How long it runs\n  Max_Turns: 5\n  Mine:\n    CamelName: 1\nNet:\n  Headers:\n    X-Mine: v\n")
	if got := cfg.GetInt("agent.max_turns"); got != 5 {
		t.Fatalf("a key written in another case reads %d", got)
	}
	if got := cfg.GetInt("AGENT.Max_Turns"); got != 5 {
		t.Fatalf("a key typed in another case reads %d", got)
	}
	if got := cfg.Get("net.headers.X-Mine"); got != "v" {
		t.Fatalf("a header under keys written in another case reads %v", got)
	}
	agent, _ := cfg.Stored("agent").(map[string]any)
	if agent["max_turns"] != 5 {
		t.Fatalf("the section reads %v", agent)
	}
	if err := cfg.Set("agent.model", "m"); err != nil {
		t.Fatal(err)
	}
	text := own(t, dir)
	for _, line := range []string{"# My settings", "# How long it runs", "agent:", "max_turns: 5", "model: m", "Mine:", "CamelName: 1", "net:",
		"headers:", "X-Mine: v"} {
		if !strings.Contains(text, line) {
			t.Fatalf("the file lacks %q:\n%s", line, text)
		}
	}
	for _, gone := range []string{"Agent:", "Max_Turns", "Net:", "Headers:"} {
		if strings.Contains(text, gone) {
			t.Fatalf("the file still spells %q so:\n%s", gone, text)
		}
	}
	if strings.Index(text, "max_turns") > strings.Index(text, "model") {
		t.Fatalf("the order of the file changed:\n%s", text)
	}
}

func TestSpellingsMergeIntoOne(t *testing.T) {
	cfg, dir := setup(t)
	handWrite(t, dir, "Agent:\n  max_turns: 1\nagent:\n  max_turns: 2\n  model: kept\n")
	if got := cfg.GetInt("agent.max_turns"); got != 2 {
		t.Fatalf("the key spelled as declared should win, it reads %d", got)
	}
	if err := cfg.Set("agent.approvals.mode", "auto"); err != nil {
		t.Fatal(err)
	}
	text := own(t, dir)
	if strings.Count(text, "agent:") != 1 || strings.Contains(text, "Agent:") || !strings.Contains(text, "max_turns: 2") ||
		!strings.Contains(text, "model: kept") || !strings.Contains(text, "mode: auto") {
		t.Fatalf("the file holds:\n%s", text)
	}
}

func TestCommentsOnlyFileKept(t *testing.T) {
	cfg, dir := setup(t)
	handWrite(t, dir, "# every setting at its default\n# agent:\n#   max_turns: 0\n")
	if err := cfg.Set("agent.max_turns", 3); err != nil {
		t.Fatal(err)
	}
	text := own(t, dir)
	if !strings.Contains(text, "# every setting at its default") || !strings.Contains(text, "max_turns: 3") || cfg.GetInt("agent.max_turns") != 3 {
		t.Fatalf("the file holds:\n%s", text)
	}
}

func TestAdministratorPins(t *testing.T) {
	cfg, dir := setup(t)
	managed := filepath.Join(dir, "managed")
	pins := "Agent:\n  Max_Turns: 9\nnet:\n  env:\n    HTTP_PROXY: pinned\n  headers:\n    X-Pinned: p\n"
	if err := os.MkdirAll(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managed, "config.yaml"), []byte(pins), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetInt("agent.max_turns"); got != 9 {
		t.Fatalf("the pin reads %d", got)
	}
	if cfg.Lock("agent.max_turns") == nil || cfg.Set("agent.max_turns", 3) == nil {
		t.Fatal("a pinned key is not locked")
	}
	if cfg.Lock("net.env.HTTP_PROXY") == nil || cfg.Lock("net.env.http_proxy") != nil {
		t.Fatal("a pinned variable locks another of the same letters, or not itself")
	}
	if err := cfg.Set("net.env.http_proxy", "mine"); err != nil {
		t.Fatal(err)
	}
	// A header is the pinned one in any case
	if cfg.Set("net.headers.x-pinned", "mine") == nil {
		t.Fatal("a pinned header written in another case is not locked")
	}
	if err := cfg.Set("net.headers.X-Other", "mine"); err != nil {
		t.Fatal(err)
	}
	env, _ := cfg.Get("net.env").(map[string]any)
	headers, _ := cfg.Get("net.headers").(map[string]any)
	if env["HTTP_PROXY"] != "pinned" || env["http_proxy"] != "mine" || len(headers) != 2 || headers["X-Pinned"] != "p" || headers["X-Other"] != "mine" {
		t.Fatalf("variables read %v and headers %v", env, headers)
	}
	if keys := cfg.ManagedKeys(); !slices.Equal(keys, []string{"agent.max_turns", "net.env.HTTP_PROXY", "net.headers.X-Pinned"}) {
		t.Fatalf("the pinned keys read %v", keys)
	}
}

func TestNamesThatCantBeKeys(t *testing.T) {
	cfg, _ := setup(t)
	for _, name := range []string{"", " ", "a.b"} {
		if config.KeyName(name) == nil {
			t.Fatalf("%q is taken as a name", name)
		}
	}
	if config.KeyName("X-Test") != nil {
		t.Fatal("X-Test is refused")
	}
	if cfg.Set("net.headers.", "x") == nil || cfg.Set("net..headers", "x") == nil {
		t.Fatal("a key with an empty part is written")
	}
}

func TestGuardsGetTheKeyAsWritten(t *testing.T) {
	cfg, _ := setup(t)
	var handed []string
	cfg.Guard("net.env", config.Guard{Check: func(_ *config.Config, key string, _ any) error {
		handed = append(handed, key)
		return nil
	}})
	if err := cfg.Change("NET.Env.http_Proxy", "x"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Refused("net.ENV.GOFLAGS", "y"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(handed, []string{"net.env.http_Proxy", "net.env.GOFLAGS"}) {
		t.Fatalf("the guard was handed %v", handed)
	}
}

func TestThingsAnswerInAnyCase(t *testing.T) {
	cfg, dir := setup(t)
	cfg.Register(config.Sections{{Key: "things", Title: "Things"}})
	cfg.Register(things{})
	handWrite(t, dir, "things:\n  acme:\n    Kind: rocket\n    Headers:\n      X-Mine: v\n")
	if got := cfg.GetString("things.ACME.kind"); got != "rocket" {
		t.Fatalf("a thing typed in another case reads %q", got)
	}
	thing, _ := cfg.Stored("things.acme").(map[string]any)
	if thing["kind"] != "rocket" {
		t.Fatalf("the thing reads %v", thing)
	}
	if got := cfg.Get("things.acme.headers.x-mine"); got != "v" {
		t.Fatalf("a header of the thing reads %v", got)
	}
	if err := cfg.Set("things.acme.account", "work"); err != nil {
		t.Fatal(err)
	}
	text := own(t, dir)
	if !strings.Contains(text, "kind: rocket") || !strings.Contains(text, "headers:") || !strings.Contains(text, "X-Mine: v") {
		t.Fatalf("the file holds:\n%s", text)
	}
	// A write made within a thing makes nothing once the thing is gone
	within := cfg.View("things").View("acme").Within()
	if err := cfg.View("things").View("acme").Remove(); err != nil {
		t.Fatal(err)
	}
	if err := within.Set("kind", "again"); err != nil {
		t.Fatal(err)
	}
	if cfg.View("things").View("acme").Exists() {
		t.Fatalf("a write within the removed thing made it again:\n%s", own(t, dir))
	}
}

func TestOrderAsWritten(t *testing.T) {
	cfg, dir := setup(t)
	handWrite(t, dir, "net:\n  servers:\n    zeta:\n      command: z\n    Alpha:\n      command: a\n    mid:\n      command: m\n")
	if got := cfg.Order("net.servers"); !slices.Equal(got, []string{"zeta", "Alpha", "mid"}) {
		t.Fatalf("the servers are in the order %v", got)
	}
	if err := cfg.Set("net.servers.beta.command", "b"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Order("NET.Servers"); !slices.Equal(got, []string{"zeta", "Alpha", "mid", "beta"}) {
		t.Fatalf("after a write the servers are in the order %v, the file holds:\n%s", got, own(t, dir))
	}
}

func TestMergedNamesKept(t *testing.T) {
	cfg, dir := setup(t)
	// The value of first spells the name of the key after it
	handWrite(t, dir, "net:\n  base: &base\n    first: second\n    second: kept\n  options:\n    <<: *base\n")
	if err := cfg.Set("net.options.extra", "x"); err != nil {
		t.Fatal(err)
	}
	options, _ := cfg.Get("net.options").(map[string]any)
	if options["first"] != "second" || options["second"] != "kept" || options["extra"] != "x" {
		t.Fatalf("the options are %v, the file holds:\n%s", options, own(t, dir))
	}
}

func TestAliasesOutliveTheirAnchors(t *testing.T) {
	for name, tc := range map[string]struct {
		file  string
		key   string
		value any
		reads map[string]any
	}{
		"removed": {"net:\n  options:\n    one: &shared\n      a: b\n    two: *shared\n", "net.options.one", nil,
			map[string]any{"net.options.one": nil, "net.options.two.a": "b"}},
		"dropped with the table above it": {"net:\n  options:\n    one:\n      deep: &shared\n        a: b\n    two: *shared\n", "net.options.one",
			map[string]any{"other": "c"}, map[string]any{"net.options.one.other": "c", "net.options.one.deep": nil, "net.options.two.a": "b"}},
		"text changed": {"net:\n  options:\n    one: &shared before\n    two: *shared\n", "net.options.one", "after",
			map[string]any{"net.options.one": "after", "net.options.two": "after"}},
		"merged table made text": {"net:\n  options:\n    one: &shared\n      a: b\n    two:\n      <<: *shared\n      c: d\n", "net.options.one", "text",
			map[string]any{"net.options.one": "text", "net.options.two.a": nil, "net.options.two.c": "d"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, dir := setup(t)
			handWrite(t, dir, tc.file)
			if err := cfg.Set(tc.key, tc.value); err != nil {
				t.Fatal(err)
			}
			if err := cfg.Writable(tc.key); err != nil {
				t.Fatalf("the file no longer reads: %v\n%s", err, own(t, dir))
			}
			for key, want := range tc.reads {
				if got := cfg.Get(key); got != want {
					t.Fatalf("%s reads %v, not %v, the file holds:\n%s", key, got, want, own(t, dir))
				}
			}
		})
	}
}

func TestMergesWrittenAsTheyWere(t *testing.T) {
	cfg, dir := setup(t)
	handWrite(t, dir, "net:\n  base: &base\n    a: b\n  options:\n    <<: *base\n")
	if err := cfg.Set("agent.max_turns", 3); err != nil {
		t.Fatal(err)
	}
	options, _ := cfg.Get("net.options").(map[string]any)
	if text := own(t, dir); !strings.Contains(text, "    <<: *base\n") || options["a"] != "b" {
		t.Fatalf("the options are %v, the file holds:\n%s", options, text)
	}
}

func TestPartsOfTables(t *testing.T) {
	cfg, _ := setup(t)
	for key, want := range map[string]string{
		"net.env.HTTP_PROXY":         "net.env.HTTP_PROXY",
		"net.servers.one.Command":    "net.servers.one.command",
		"net.servers.one.env.TOKEN":  "net.servers.one.env.TOKEN",
		"net.servers.one":            "",
		"net.servers.one.other":      "",
		"net.servers.one.command.x":  "",
		"net.env.HTTP_PROXY.further": "",
		"net.options.any":            "",
	} {
		table, path, under := cfg.Under(key)
		if !under {
			t.Fatalf("%s is under no table", key)
		}
		part, ok := table.Part(path)
		if ok != (want != "") || part.Key != want {
			t.Fatalf("the part at %s is %q, declared %v, not %q", key, part.Key, ok, want)
		}
	}
}

func TestNamesWithDots(t *testing.T) {
	cfg, dir := setup(t)
	// A dotted key splits the name, so it is no way to keep a thing named with a dot
	if err := cfg.SetNamed("net.options", map[string]any{"limit": 3}, "acme", "model-4.1"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetNamed("net.options", "kept", "acme", "other"); err != nil {
		t.Fatal(err)
	}
	options, _ := cfg.Get("net.options").(map[string]any)
	acme, _ := options["acme"].(map[string]any)
	if entry, _ := acme["model-4.1"].(map[string]any); entry["limit"] != 3 || acme["other"] != "kept" || len(acme) != 2 {
		t.Fatalf("the options are %v, the file holds:\n%s", options, own(t, dir))
	}
	if err := cfg.SetNamed("net.options", nil, "acme", "model-4.1"); err != nil {
		t.Fatal(err)
	}
	acme, _ = cfg.Get("net.options.acme").(map[string]any)
	if len(acme) != 1 || acme["other"] != "kept" {
		t.Fatalf("after the removal acme holds %v, the file holds:\n%s", acme, own(t, dir))
	}
}

func TestTakenOutWithItsAliases(t *testing.T) {
	text := "# mine\nagent:\n    model: &kept-elsewhere secret\n    max_turns: 3\nother: *kept-elsewhere\nlist:\n    - *kept-elsewhere\n    - stays\n"
	got, err := config.Without([]byte(text), func(settings *yaml.Node) {
		agent := settings.Content[1]
		agent.Content = agent.Content[2:]
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# mine", "    max_turns: 3", "    - stays"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("%q is gone from:\n%s", want, got)
		}
	}
	for _, gone := range []string{"secret", "other", "kept-elsewhere"} {
		if strings.Contains(string(got), gone) {
			t.Fatalf("%q is still in:\n%s", gone, got)
		}
	}
	if err := config.Problem(got); err != nil {
		t.Fatalf("the text no longer reads: %v\n%s", err, got)
	}
}
