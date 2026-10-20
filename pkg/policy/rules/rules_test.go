package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
)

func call(name, args string) protocol.ToolCall {
	return protocol.ToolCall{ID: "1", Name: name, Args: json.RawMessage(args)}
}

func bash(line string) protocol.ToolCall {
	raw, _ := json.Marshal(map[string]string{"command": line})
	return call("Bash", string(raw))
}

func texts(subjects []Subject) string {
	var out []string
	for _, s := range subjects {
		t := s.Text
		if s.Unsafe {
			t += "!"
		}
		if s.Exact {
			t = "=" + t
		}
		out = append(out, t)
	}
	return strings.Join(out, "|")
}

func TestSplit(t *testing.T) {
	cases := map[string]string{
		"git status":                     "git status",
		"git status && ls -la":           "git status|ls -la",
		"a; b || c | d & e":              "a|b|c|d|e",
		"echo 'a && b' && echo \"c; d\"": "echo 'a && b'|echo \"c; d\"",
		"echo a\\;b; c":                  "echo a\\;b|c",
		"cat f 2>&1 | wc -l":             "cat f 2>&1|wc -l",
		"cmd &> out; next":               "cmd &> out|next",
		"(cd x && make)":                 "cd x|make",
		"{ a; b; }":                      "a|b",
		"echo $(whoami)":                 "echo $(whoami)!",
		"echo `date` && ls":              "echo `date`!|ls",
		"echo \"$(x)\"":                  "echo \"$(x)\"!",
		"diff <(a) b":                    "diff <(a) b!",
		"line1\nline2":                   "line1|line2",
		"a |& b":                         "a|b",
		"":                               "",
		"   ":                            "",
		"echo \"unterminated":            "echo \"unterminated",
		"echo 'it''s'":                   "echo 'it''s'",
	}
	for in, want := range cases {
		if got := texts(Split(in)); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestParseAndLoad(t *testing.T) {
	for in, want := range map[string]Rule{
		"Bash":          {Tool: "Bash"},
		"Bash(git *)":   {Tool: "Bash", Pattern: "git *", HasPattern: true},
		" Read(~/x) ":   {Tool: "Read", Pattern: "~/x", HasPattern: true},
		"mcp__a__*":     {Tool: "mcp__a__*"},
		"Bash(a (b) c)": {Tool: "Bash", Pattern: "a (b) c", HasPattern: true},
		"Bash()":        {Tool: "Bash", Pattern: "", HasPattern: true},
	} {
		got, err := Parse(in)
		if err != nil || got != want || got.String() != strings.TrimSpace(in) {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "Bash(", "(x)", "Bash)", "Bash x", "Bash(x) y"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	nested, err := Load(strings.NewReader(`{"permissions":{"allow":["Read"],"deny":["Bash(rm *)"],"ask":["Write"]},"other":1}`))
	if err != nil || len(nested.Allow) != 1 || nested.Deny[0] != "Bash(rm *)" || nested.Ask[0] != "Write" {
		t.Fatalf("%+v %v", nested, err)
	}
	flat, err := Load(strings.NewReader(`{"allow":["Glob"]}`))
	if err != nil || flat.Allow[0] != "Glob" || flat.Deny != nil {
		t.Fatalf("%+v %v", flat, err)
	}
	if _, err := Load(strings.NewReader(`{`)); err == nil {
		t.Fatal("bad json accepted")
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"permissions":{"ask":["Bash"]}}`), 0o644)
	if s, err := LoadFile(path); err != nil || s.Ask[0] != "Bash" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := LoadFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := New(Set{Allow: []string{"Bash("}}); err == nil {
		t.Fatal("malformed rule accepted")
	}
	p, _ := New()
	if err := p.Add("maybe", "Bash"); err == nil {
		t.Fatal("unknown decision accepted")
	}
}

func TestCommandRules(t *testing.T) {
	p, err := New(Set{
		Allow: []string{"Bash(git *)", "Bash(ls)", "Bash(npm run test:*)", "Bash(cat f 2>&1)", "Glob"},
		Deny:  []string{"Bash(rm -rf *)", "Bash(git push*)"},
		Ask:   []string{"Bash(sudo *)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]policy.Decision{
		"git status":                  policy.Allow,
		"git log src/x | head":        policy.Ask,
		"git status && ls":            policy.Allow,
		"ls -la":                      policy.Ask,
		"ls":                          policy.Allow,
		"npm run test":                policy.Allow,
		"npm run test -- --watch":     policy.Allow,
		"npm run tests":               policy.Ask,
		"git status && rm -rf /":      policy.Deny,
		"git push origin main":        policy.Deny,
		"git pushes":                  policy.Deny,
		"sudo ls && git status":       policy.Ask,
		"git status && sudo rm -rf /": policy.Ask,
		"ls; rm -rf build":            policy.Deny,
		"git $(evil)":                 policy.Ask,
		"git status; echo `x`":        policy.Ask,
		"cat f 2>&1":                  policy.Allow,
		"":                            policy.Ask,
	}
	for line, want := range cases {
		if got := p.Decide(bash(line)); got != want {
			t.Errorf("%q: got %s want %s", line, got, want)
		}
	}
	if p.Decide(call("Glob", `{"pattern":"*"}`)) != policy.Allow || p.Decide(call("Grep", `{"pattern":"*"}`)) != policy.Ask {
		t.Fatal("tool wide allow")
	}
	if p.Decide(call("Bash", `{}`)) != policy.Ask || p.Decide(call("Bash", `not json`)) != policy.Ask {
		t.Fatal("missing command must fall to the default")
	}
	strict := &Policy{Default: policy.Deny}
	if strict.Decide(bash("anything")) != policy.Deny {
		t.Fatal("default")
	}
}

func TestExactWholeLineRules(t *testing.T) {
	p, _ := New(Set{Allow: []string{"Bash(make clean && make)"}})
	if p.Decide(bash("make clean && make")) != policy.Allow || p.Decide(bash("make clean && make install")) != policy.Ask || p.Decide(bash("make clean")) != policy.Ask {
		t.Fatal("exact whole line")
	}
	wild, _ := New(Set{Allow: []string{"Bash(make clean && *)"}})
	if wild.Decide(bash("make clean && make")) != policy.Ask {
		t.Fatal("wildcards must not match whole lines")
	}
	key := p.Key(bash("git status && ls"))
	if key != "Bash(git status && ls)" {
		t.Fatal(key)
	}
	if err := p.Add(policy.Allow, key); err != nil {
		t.Fatal(err)
	}
	if p.Decide(bash("git status && ls")) != policy.Allow || p.Decide(bash("git status")) != policy.Ask {
		t.Fatal("remembered key must allow exactly that line")
	}
	if p.Key(bash("git status")) != "Bash(git status)" || p.Key(call("Bash", `{}`)) != "Bash" {
		t.Fatal(p.Key(bash("git status")))
	}
}

func TestPathRules(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	p, err := New(Set{
		Allow: []string{"Read(src/**)", "Read(~/.config/app/*)", "Edit(/abs/dir)", "Write(notes.md)"},
		Deny:  []string{"Read(~/.ssh/**)", "Read(**/*.pem)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.Dir, p.Home = dir, home
	cases := map[string]policy.Decision{
		`{"file_path":"src/a.go"}`:                   policy.Allow,
		`{"file_path":"src/deep/b.go"}`:              policy.Allow,
		`{"file_path":"` + dir + `/src/c.go"}`:       policy.Allow,
		`{"file_path":"src"}`:                        policy.Allow,
		`{"file_path":"other/a.go"}`:                 policy.Ask,
		`{"file_path":"src/../other"}`:               policy.Ask,
		`{"file_path":"~/.config/app/x"}`:            policy.Allow,
		`{"file_path":"` + home + `/.config/app/x"}`: policy.Allow,
		`{"file_path":"~/.config/app/x/y"}`:          policy.Ask,
		`{"file_path":"~/.ssh/id_rsa"}`:              policy.Deny,
		`{"file_path":"src/key.pem"}`:                policy.Deny,
		`{"file_path":"/etc/key.pem"}`:               policy.Deny,
	}
	for args, want := range cases {
		if got := p.Decide(call("Read", args)); got != want {
			t.Errorf("%s: got %s want %s", args, got, want)
		}
	}
	if p.Decide(call("Edit", `{"file_path":"/abs/dir/deep/file"}`)) != policy.Allow || p.Decide(call("Edit", `{"file_path":"/abs/dir"}`)) != policy.Allow || p.Decide(call("Edit", `{"file_path":"/abs/dirty"}`)) != policy.Ask {
		t.Fatal("directory rules cover everything below")
	}
	if p.Decide(call("Write", `{"file_path":"notes.md"}`)) != policy.Allow || p.Decide(call("Write", `{"file_path":"`+dir+`/notes.md"}`)) != policy.Allow {
		t.Fatal("relative pattern")
	}
	if p.Decide(call("Glob", `{"pattern":"*"}`)) != policy.Ask {
		t.Fatal("glob without path is the working directory")
	}
	g, _ := New(Set{Allow: []string{"Glob(.)", "Grep(" + dir + ")"}})
	g.Dir = dir
	if g.Decide(call("Glob", `{"pattern":"*"}`)) != policy.Allow || g.Decide(call("Grep", `{"pattern":"x","path":"sub"}`)) != policy.Allow {
		t.Fatal("working directory rules")
	}
	if key := p.Key(call("Read", `{"file_path":"src/a.go"}`)); key != "Read("+filepath.ToSlash(dir)+"/src/a.go)" {
		t.Fatal(key)
	}
}

func TestDomainAndPlainRules(t *testing.T) {
	p, err := New(Set{
		Allow: []string{"WebFetch(domain:example.com)", "WebFetch(https://docs.*)", "mcp__gh__*", "mcp__db__query(select *)"},
		Deny:  []string{"WebFetch(domain:evil.com)", "mcp__db__query(*drop*)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		call protocol.ToolCall
		want policy.Decision
	}{
		{call("WebFetch", `{"url":"https://example.com/x"}`), policy.Allow},
		{call("WebFetch", `{"url":"https://api.Example.com/x"}`), policy.Allow},
		{call("WebFetch", `{"url":"https://notexample.com/x"}`), policy.Ask},
		{call("WebFetch", `{"url":"https://docs.other.org/a"}`), policy.Allow},
		{call("WebFetch", `{"url":"https://evil.com"}`), policy.Deny},
		{call("WebFetch", `{"url":"https://sub.evil.com"}`), policy.Deny},
		{call("WebFetch", `{"url":"::bad"}`), policy.Ask},
		{call("mcp__gh__search", `{"q":"anything"}`), policy.Allow},
		{call("mcp__db__query", `{"sql":"select * from t"}`), policy.Allow},
		{call("mcp__db__query", `{"sql":"select 1; drop table"}`), policy.Deny},
		{call("mcp__db__query", `{"sql":"delete from t"}`), policy.Ask},
		{call("mcp__db__query", `{"sql":"select 1","db":"x"}`), policy.Ask},
		{call("mcp__db__query", `{}`), policy.Ask},
	}
	for _, c := range cases {
		if got := p.Decide(c.call); got != c.want {
			t.Errorf("%s %s: got %s want %s", c.call.Name, c.call.Args, got, c.want)
		}
	}
	if k := p.Key(call("WebFetch", `{"url":"https://API.example.com/x"}`)); k != "WebFetch(domain:api.example.com)" {
		t.Fatal(k)
	}
	if k := p.Key(call("mcp__db__query", `{"sql":"select 1","db":"x"}`)); k != "mcp__db__query" {
		t.Fatal(k)
	}
	if k := p.Key(call("mcp__db__query", `{"sql":"select 1"}`)); k != "mcp__db__query(select 1)" {
		t.Fatal(k)
	}
	custom := &Policy{Subjects: map[string]Extractor{"Bash": Strings("command")}}
	custom.Add(policy.Allow, "Bash(git *)")
	if custom.Decide(bash("git status && rm -rf /")) != policy.Allow {
		t.Fatal("custom extractor must replace shell splitting")
	}
	if texts(AllStrings(json.RawMessage(`{"b":"2","a":"1","n":3}`))) != "1|2" || AllStrings(json.RawMessage(`[]`)) != nil {
		t.Fatal("all strings")
	}
	if len(p.Rules(policy.Allow)) != 4 || p.Rules(policy.Deny)[1] != "mcp__db__query(*drop*)" || p.Rules(policy.Ask) != nil {
		t.Fatalf("%v", p.Rules(policy.Allow))
	}
	if texts(p.SubjectsOf(bash("a && b"))) != "a|b|=a && b" {
		t.Fatal(texts(p.SubjectsOf(bash("a && b"))))
	}
}
