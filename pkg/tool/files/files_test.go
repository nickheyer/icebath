package files

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

func run(t *testing.T, tl tool.Tool, args string) (string, bool) {
	t.Helper()
	res, err := tl.Run(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%+v", res)
	}
	if img, ok := res.Content[0].(protocol.Image); ok {
		return img.MediaType, res.IsError
	}
	return res.Content[0].(protocol.Text).Text, res.IsError
}

func put(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAllToolsAreRegistrable(t *testing.T) {
	var r tool.Registry
	if err := r.Add(All(Options{})...); err != nil {
		t.Fatal(err)
	}
	ro := r.ReadOnly()
	if strings.Join(ro, ",") != "Glob,Grep,Read" {
		t.Fatalf("%v", ro)
	}
	for _, name := range []string{WriteName, EditName} {
		tl, _ := r.Lookup(name)
		if !tool.Describe(tl).Destructive {
			t.Fatalf("%s must be destructive", name)
		}
	}
}

func TestResolveAndRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	put(t, filepath.Join(outside, "secret"), "s")
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	o := Options{Dir: dir, Root: dir}
	if p, err := o.resolve("a/b.txt"); err != nil || p != filepath.Join(dir, "a", "b.txt") {
		t.Fatalf("%v %s", err, p)
	}
	if _, err := o.resolve("link/secret"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("symlink escape accepted: %v", err)
	}
	if _, err := o.resolve("../escape"); err == nil {
		t.Fatal("dotdot escape accepted")
	}
	if _, err := o.resolve(filepath.Join(outside, "secret")); err == nil {
		t.Fatal("absolute escape accepted")
	}
	if _, err := o.resolve("link/missing/deeper"); err == nil {
		t.Fatal("escape through a missing tail accepted")
	}
	if _, err := o.resolve(""); err == nil {
		t.Fatal("empty path accepted")
	}
	home, _ := os.UserHomeDir()
	if p, err := (Options{}).resolve("~/x"); err != nil || p != filepath.Join(home, "x") {
		t.Fatalf("%v %s", err, p)
	}
	if _, err := (Options{Root: filepath.Join(dir, "nope")}).resolve("a"); err == nil {
		t.Fatal("missing root accepted")
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	o := Options{Dir: dir, MaxLines: 3, MaxLineBytes: 10}
	r := Read(o)
	put(t, filepath.Join(dir, "a.txt"), "one\ntwo\nthree\nfour\nfive")
	out, isErr := run(t, r, `{"file_path":"a.txt"}`)
	if isErr || out != "     1\tone\n     2\ttwo\n     3\tthree\n\n[lines 1-3 of 5, continue with offset=4]" {
		t.Fatalf("%v %q", isErr, out)
	}
	out, _ = run(t, r, `{"file_path":"a.txt","offset":4,"limit":5}`)
	if out != "     4\tfour\n     5\tfive" {
		t.Fatalf("%q", out)
	}
	if out, isErr := run(t, r, `{"file_path":"a.txt","offset":9}`); !isErr || !strings.Contains(out, "past the end") {
		t.Fatalf("%v %q", isErr, out)
	}
	put(t, filepath.Join(dir, "long.txt"), "abcdefghijklmnop\r\n")
	if out, _ = run(t, r, `{"file_path":"long.txt"}`); out != "     1\tabcdefghij [+6 bytes]" {
		t.Fatalf("%q", out)
	}
	put(t, filepath.Join(dir, "empty.txt"), "")
	if out, isErr = run(t, r, `{"file_path":"empty.txt"}`); isErr || !strings.HasSuffix(out, "is empty") {
		t.Fatalf("%v %q", isErr, out)
	}
	put(t, filepath.Join(dir, "bin"), "ab\x00cd")
	if out, isErr = run(t, r, `{"file_path":"bin"}`); !isErr || !strings.Contains(out, "binary") {
		t.Fatalf("%v %q", isErr, out)
	}
	put(t, filepath.Join(dir, "pic.PNG"), "\x89PNG")
	if out, isErr = run(t, r, `{"file_path":"pic.PNG"}`); isErr || out != "image/png" {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, Read(Options{Dir: dir, MaxFileBytes: 2}), `{"file_path":"pic.PNG"}`); !isErr || !strings.Contains(out, "image limit") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, r, `{"file_path":"."}`); !isErr || !strings.Contains(out, "directory") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, r, `{"file_path":"missing"}`); !isErr || !strings.Contains(out, "does not exist") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, r, `{"file_path":1}`); !isErr {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, r, `{}`); !isErr || !strings.Contains(out, "required") {
		t.Fatalf("%v %q", isErr, out)
	}
	put(t, filepath.Join(dir, "utf.txt"), "héllo wörld")
	if out, _ = run(t, Read(Options{Dir: dir, MaxLineBytes: 2}), `{"file_path":"utf.txt"}`); !strings.HasPrefix(out, "     1\th [+") {
		t.Fatalf("rune boundary %q", out)
	}
}

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	w := Write(Options{Dir: dir})
	out, isErr := run(t, w, `{"file_path":"deep/nested/f.txt","content":"hello"}`)
	if isErr || !strings.HasPrefix(out, "Created") || !strings.HasSuffix(out, "(5 bytes)") {
		t.Fatalf("%v %q", isErr, out)
	}
	p := filepath.Join(dir, "deep", "nested", "f.txt")
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, isErr = run(t, w, `{"file_path":"deep/nested/f.txt","content":"hi"}`); isErr || !strings.HasPrefix(out, "Updated") {
		t.Fatalf("%v %q", isErr, out)
	}
	st, _ := os.Stat(p)
	if data, _ := os.ReadFile(p); string(data) != "hi" || st.Mode().Perm() != 0o600 {
		t.Fatalf("%q %v", data, st.Mode())
	}
	if out, isErr = run(t, w, `{"file_path":"deep","content":"x"}`); !isErr || !strings.Contains(out, "directory") {
		t.Fatalf("%v %q", isErr, out)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "deep", "nested")); len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
	if _, isErr = run(t, Write(Options{Dir: dir, Root: dir}), `{"file_path":"../out.txt","content":"x"}`); !isErr {
		t.Fatal("escape accepted")
	}
}

func TestEdit(t *testing.T) {
	dir := t.TempDir()
	e := Edit(Options{Dir: dir})
	p := filepath.Join(dir, "f.go")
	put(t, p, "a\nfoo\nb\nfoo\nc\n")
	os.Chmod(p, 0o640)
	cases := map[string]string{
		`{"file_path":"f.go","old_string":"foo","new_string":"foo"}`: "identical",
		`{"file_path":"f.go","old_string":"","new_string":"x"}`:      "empty",
		`{"file_path":"f.go","old_string":"zzz","new_string":"x"}`:   "not found",
		`{"file_path":"f.go","old_string":"foo","new_string":"bar"}`: "occurs 2 times",
		`{"file_path":"nope.go","old_string":"a","new_string":"b"}`:  "does not exist",
		`{"file_path":".","old_string":"a","new_string":"b"}`:        "directory",
	}
	for args, want := range cases {
		if out, isErr := run(t, e, args); !isErr || !strings.Contains(out, want) {
			t.Fatalf("%s: %v %q", args, isErr, out)
		}
	}
	out, isErr := run(t, e, `{"file_path":"f.go","old_string":"a\nfoo","new_string":"a\nbaz\nqux"}`)
	if isErr || !strings.HasPrefix(out, "Replaced 1 occurrence(s)") || !strings.Contains(out, "     2\tbaz\n     3\tqux\n     4\tb\n") {
		t.Fatalf("%v %q", isErr, out)
	}
	out, isErr = run(t, e, `{"file_path":"f.go","old_string":"foo","new_string":"bar","replace_all":true}`)
	if isErr || !strings.HasPrefix(out, "Replaced 1 occurrence(s)") {
		t.Fatalf("%v %q", isErr, out)
	}
	put(t, p, "x foo y foo z")
	if out, isErr = run(t, e, `{"file_path":"f.go","old_string":"foo","new_string":"bar","replace_all":true}`); isErr || !strings.HasPrefix(out, "Replaced 2 occurrence(s)") {
		t.Fatalf("%v %q", isErr, out)
	}
	data, _ := os.ReadFile(p)
	st, _ := os.Stat(p)
	if string(data) != "x bar y bar z" || st.Mode().Perm() != 0o640 {
		t.Fatalf("%q %v", data, st.Mode())
	}
	put(t, filepath.Join(dir, "bin"), "a\x00foo")
	if out, isErr = run(t, e, `{"file_path":"bin","old_string":"foo","new_string":"bar"}`); !isErr || !strings.Contains(out, "binary") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, Edit(Options{Dir: dir, MaxFileBytes: 3}), `{"file_path":"f.go","old_string":"bar","new_string":"baz"}`); !isErr || !strings.Contains(out, "edit limit") {
		t.Fatalf("%v %q", isErr, out)
	}
}

func TestGlob(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "a.go"), "")
	put(t, filepath.Join(dir, "src", "b.go"), "")
	put(t, filepath.Join(dir, "src", "deep", "c.go"), "")
	put(t, filepath.Join(dir, "src", "d.txt"), "")
	put(t, filepath.Join(dir, ".git", "e.go"), "")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "a.go"), old, old)
	g := Glob(Options{Dir: dir})
	out, isErr := run(t, g, `{"pattern":"**/*.go"}`)
	if isErr {
		t.Fatal(out)
	}
	got := strings.Split(out, "\n")
	if len(got) != 3 || got[2] != filepath.Join(dir, "a.go") || strings.Contains(out, ".git") {
		t.Fatalf("%q", out)
	}
	if out, _ = run(t, g, `{"pattern":"*.go","path":"src"}`); out != filepath.Join(dir, "src", "b.go") {
		t.Fatalf("%q", out)
	}
	if out, _ = run(t, g, fmt.Sprintf(`{"pattern":%q}`, filepath.Join(dir, "src", "**", "*.go"))); strings.Count(out, "\n") != 1 {
		t.Fatalf("absolute pattern %q", out)
	}
	if out, _ = run(t, g, `{"pattern":"*.rs"}`); out != "No files found" {
		t.Fatalf("%q", out)
	}
	if out, _ = run(t, Glob(Options{Dir: dir, MaxResults: 2}), `{"pattern":"**/*.go"}`); !strings.HasSuffix(out, "[showing 2 of 3 matches, narrow the pattern]") {
		t.Fatalf("%q", out)
	}
	if out, _ = run(t, Glob(Options{Dir: dir, Ignore: []string{}}), `{"pattern":"**/*.go"}`); !strings.Contains(out, ".git") {
		t.Fatalf("explicit empty ignore must walk .git: %q", out)
	}
	for args, want := range map[string]string{`{}`: "required", `{"pattern":"*","path":"a.go"}`: "not a directory", `{"pattern":"*","path":"nope"}`: "not a directory"} {
		if out, isErr := run(t, g, args); !isErr || !strings.Contains(out, want) {
			t.Fatalf("%s: %v %q", args, isErr, out)
		}
	}
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "a.go"), "package a\n\nfunc Foo() {}\nfunc bar() {}\nfunc Foo2() {}\n")
	put(t, filepath.Join(dir, "sub", "b.go"), "// foo\nfunc Baz() {}\n")
	put(t, filepath.Join(dir, "sub", "c.txt"), "foo\nfoo\n")
	put(t, filepath.Join(dir, "bin"), "foo\x00")
	put(t, filepath.Join(dir, ".git", "x.go"), "foo")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "a.go"), old, old)
	g := Grep(Options{Dir: dir})
	out, isErr := run(t, g, `{"pattern":"foo"}`)
	if isErr || out != filepath.Join(dir, "sub", "c.txt")+"\n"+filepath.Join(dir, "sub", "b.go") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, _ = run(t, g, `{"pattern":"foo","-i":true,"glob":"*.go"}`); out != filepath.Join(dir, "sub", "b.go")+"\n"+filepath.Join(dir, "a.go") {
		t.Fatalf("%q", out)
	}
	if out, _ = run(t, g, `{"pattern":"foo","glob":"sub/*.txt","output_mode":"count"}`); out != filepath.Join(dir, "sub", "c.txt")+":2" {
		t.Fatalf("%q", out)
	}
	out, _ = run(t, g, `{"pattern":"^func Foo","output_mode":"content","-A":1}`)
	a := filepath.Join(dir, "a.go")
	if out != a+":3:func Foo() {}\n"+a+"-4-func bar() {}\n"+a+":5:func Foo2() {}" {
		t.Fatalf("%q", out)
	}
	out, _ = run(t, g, `{"pattern":"bar","output_mode":"content","-C":1,"-n":false}`)
	if out != a+"-func Foo() {}\n"+a+":func bar() {}\n"+a+"-func Foo2() {}" {
		t.Fatalf("%q", out)
	}
	out, _ = run(t, g, `{"pattern":"^package|Foo2","output_mode":"content"}`)
	if out != a+":1:package a\n--\n"+a+":5:func Foo2() {}" {
		t.Fatalf("separator %q", out)
	}
	out, _ = run(t, g, `{"pattern":"foo\\nfunc","multiline":true,"output_mode":"content","path":"sub/b.go"}`)
	if out != filepath.Join(dir, "sub", "b.go")+":1:// foo\n"+filepath.Join(dir, "sub", "b.go")+":2:func Baz() {}" {
		t.Fatalf("multiline %q", out)
	}
	if out, _ = run(t, g, `{"pattern":"Foo","output_mode":"content","head_limit":1}`); out != a+":3:func Foo() {}\n[showing 1 of 2, raise head_limit or narrow the search]" {
		t.Fatalf("%q", out)
	}
	if out, _ = run(t, Grep(Options{Dir: dir, MaxOutput: 30}), `{"pattern":"Foo","output_mode":"content"}`); !strings.HasSuffix(out, "[output cut at 30 bytes, narrow the search]") || strings.Count(out, "\n") != 1 {
		t.Fatalf("%q", out)
	}
	if out, _ = run(t, g, `{"pattern":"nothing-here"}`); out != "No matches found" {
		t.Fatalf("%q", out)
	}
	if os.Getuid() != 0 {
		put(t, filepath.Join(dir, "locked"), "foo")
		os.Chmod(filepath.Join(dir, "locked"), 0)
		if out, _ = run(t, g, `{"pattern":"foo","output_mode":"count"}`); !strings.HasSuffix(out, "[1 files could not be read]") {
			t.Fatalf("%q", out)
		}
		os.Chmod(filepath.Join(dir, "locked"), 0o644)
		os.Remove(filepath.Join(dir, "locked"))
	}
	for args, want := range map[string]string{`{}`: "required", `{"pattern":"("}`: "invalid pattern", `{"pattern":"x","output_mode":"weird"}`: "unknown output_mode", `{"pattern":"x","path":"nope"}`: "does not exist"} {
		if out, isErr := run(t, g, args); !isErr || !strings.Contains(out, want) {
			t.Fatalf("%s: %v %q", args, isErr, out)
		}
	}
}
