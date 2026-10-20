package files

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

func data(t *testing.T, tl tool.Tool, args string) (tool.Result, string) {
	t.Helper()
	res, err := tl.Run(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, p := range res.Content {
		if tx, ok := p.(protocol.Text); ok {
			text += tx.Text
		}
	}
	return res, text
}

func TestRelativeRootAndDirDefault(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "f.txt"), "x\n")
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	out, isErr := run(t, Read(Options{Root: "."}), `{"file_path":"f.txt"}`)
	if isErr {
		t.Fatalf("relative root must resolve: %s", out)
	}
	other := t.TempDir()
	put(t, filepath.Join(other, "g.txt"), "y\n")
	if out, isErr := run(t, Read(Options{Root: other}), `{"file_path":"g.txt"}`); isErr {
		t.Fatalf("Dir must default to Root: %s", out)
	}
}

func TestGitIgnoreIsHonored(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, ".gitignore"), "# build output\nout/\n*.log\n!keep.log\n/top.txt\nsub/inner.txt\n")
	put(t, filepath.Join(dir, "out", "a.go"), "foo")
	put(t, filepath.Join(dir, "x.log"), "foo")
	put(t, filepath.Join(dir, "keep.log"), "foo")
	put(t, filepath.Join(dir, "top.txt"), "foo")
	put(t, filepath.Join(dir, "sub", "top.txt"), "foo")
	put(t, filepath.Join(dir, "sub", "inner.txt"), "foo")
	put(t, filepath.Join(dir, "sub", ".gitignore"), "nested.txt\n")
	put(t, filepath.Join(dir, "sub", "nested.txt"), "foo")
	put(t, filepath.Join(dir, "sub", "ok.txt"), "foo")
	put(t, filepath.Join(dir, "node_modules", "m.js"), "foo")
	out, _ := run(t, Grep(Options{Dir: dir}), `{"pattern":"foo"}`)
	for _, hidden := range []string{"out", "x.log", "top.txt\n", "inner.txt", "nested.txt", "node_modules"} {
		if strings.Contains(out+"\n", hidden) && hidden != "top.txt\n" {
			t.Fatalf("%s must be ignored:\n%s", hidden, out)
		}
	}
	if strings.Contains(out, filepath.Join(dir, "top.txt")) {
		t.Fatalf("anchored rule must hide the top level file:\n%s", out)
	}
	for _, shown := range []string{"keep.log", filepath.Join("sub", "top.txt"), filepath.Join("sub", "ok.txt")} {
		if !strings.Contains(out, shown) {
			t.Fatalf("%s must be listed:\n%s", shown, out)
		}
	}
	out, _ = run(t, Grep(Options{Dir: dir, NoGitIgnore: true}), `{"pattern":"foo"}`)
	if !strings.Contains(out, "x.log") || !strings.Contains(out, "inner.txt") || strings.Contains(out, "node_modules") {
		t.Fatalf("NoGitIgnore must walk ignored files but still skip default names:\n%s", out)
	}
	if r, ok := parseIgnoreLine("  "); ok || r.pattern != "" {
		t.Fatal("blank line")
	}
	if r, ok := parseIgnoreLine("\\#literal"); !ok || r.pattern != "#literal" {
		t.Fatalf("%+v", r)
	}
	if r, ok := parseIgnoreLine("a\\ b "); !ok || r.pattern != "a b" {
		t.Fatalf("%+v", r)
	}
}

func TestGlobBracesDirectoriesAndData(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "a.go"), "")
	put(t, filepath.Join(dir, "b.ts"), "")
	put(t, filepath.Join(dir, "c.txt"), "")
	put(t, filepath.Join(dir, "src", "d.go"), "")
	g := Glob(Options{Dir: dir})
	out, _ := run(t, g, `{"pattern":"*.{go,ts}"}`)
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.ts") || strings.Contains(out, "c.txt") {
		t.Fatalf("braces %q", out)
	}
	out, _ = run(t, g, `{"pattern":"s*"}`)
	if out != filepath.Join(dir, "src")+string(filepath.Separator) {
		t.Fatalf("directories must be listed with a slash: %q", out)
	}
	if got := expandBraces("x{a,{b,c}}y"); strings.Join(got, " ") != "xay xby xcy" {
		t.Fatalf("%v", got)
	}
	if got := expandBraces("x{a"); len(got) != 1 || got[0] != "x{a" {
		t.Fatalf("%v", got)
	}
	res, _ := data(t, g, `{"pattern":"**/*.go"}`)
	gd := res.Data.(GlobData)
	if gd.Total != 2 || len(gd.Matches) != 2 || gd.Pattern != "**/*.go" || gd.Dir != dir {
		t.Fatalf("%+v", gd)
	}
}

func TestReadSniffsImagesAndReportsData(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "fake.png"), "just text\n")
	out, isErr := run(t, Read(Options{Dir: dir}), `{"file_path":"fake.png"}`)
	if isErr || !strings.Contains(out, "just text") {
		t.Fatalf("text named like an image must read as text: %v %q", isErr, out)
	}
	put(t, filepath.Join(dir, "noext"), "GIF89a\x00\x00")
	if out, isErr = run(t, Read(Options{Dir: dir}), `{"file_path":"noext"}`); isErr || out != "image/gif" {
		t.Fatalf("image without extension must be sniffed: %v %q", isErr, out)
	}
	put(t, filepath.Join(dir, "t.txt"), "a\nb\nc\n")
	res, _ := data(t, Read(Options{Dir: dir, MaxLines: 2}), `{"file_path":"t.txt"}`)
	rd := res.Data.(ReadData)
	if rd.Total != 3 || len(rd.Lines) != 2 || rd.Offset != 1 || rd.Bytes != 6 {
		t.Fatalf("%+v", rd)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if res, _ := Read(Options{Dir: dir}).Run(cancelled, json.RawMessage(`{"file_path":"t.txt"}`)); !res.IsError {
		t.Fatal("cancelled read")
	}
	if res, _ := Write(Options{Dir: dir}).Run(cancelled, json.RawMessage(`{"file_path":"t.txt","content":"x"}`)); !res.IsError {
		t.Fatal("cancelled write")
	}
	if res, _ := Edit(Options{Dir: dir}).Run(cancelled, json.RawMessage(`{"file_path":"t.txt","old_string":"a","new_string":"b"}`)); !res.IsError {
		t.Fatal("cancelled edit")
	}
}

func TestWriteAndEditReportChangesAndFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	put(t, target, "old\n")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	res, _ := data(t, Write(Options{Dir: dir}), `{"file_path":"link.txt","content":"new\n"}`)
	ch := res.Data.(Change)
	if ch.Old != "old\n" || ch.New != "new\n" || ch.Created || ch.Replaced != 1 {
		t.Fatalf("%+v", ch)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("write must keep the symlink")
	}
	if got, _ := os.ReadFile(target); string(got) != "new\n" {
		t.Fatalf("target %q", got)
	}
	res, _ = data(t, Edit(Options{Dir: dir}), `{"file_path":"real.txt","old_string":"new","new_string":"edited"}`)
	ch = res.Data.(Change)
	if ch.Old != "new\n" || ch.New != "edited\n" || ch.Replaced != 1 {
		t.Fatalf("%+v", ch)
	}
	out, _ := run(t, Edit(Options{Dir: dir}), `{"file_path":"real.txt","old_string":"edited","new_string":"last"}`)
	if strings.Contains(out, "\n     2\t") {
		t.Fatalf("a file ending in a newline must not show a phantom line:\n%q", out)
	}
	res, _ = data(t, Write(Options{Dir: dir}), `{"file_path":"fresh.txt","content":"x"}`)
	if ch := res.Data.(Change); !ch.Created || ch.Old != "" {
		t.Fatalf("%+v", ch)
	}
}

func TestGrepHeadLimitCountsMatchesAndMultilineAnchors(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "f.txt"), "l1\nmatch one\nl3\nmatch two\nl5\n")
	put(t, filepath.Join(dir, "g.go"), "func a() {}\nx\nfunc b() {}\n")
	g := Grep(Options{Dir: dir})
	f := filepath.Join(dir, "f.txt")
	out, _ := run(t, g, `{"pattern":"match","output_mode":"content","-C":1,"head_limit":1,"path":"f.txt"}`)
	if out != f+"-1-l1\n"+f+":2:match one\n"+f+"-3-l3\n[showing 1 of 2, raise head_limit or narrow the search]" {
		t.Fatalf("%q", out)
	}
	out, _ = run(t, g, `{"pattern":"^func","multiline":true,"output_mode":"count","path":"g.go"}`)
	if out != filepath.Join(dir, "g.go")+":2" {
		t.Fatalf("multiline anchors must match at line starts: %q", out)
	}
	res, _ := data(t, g, `{"pattern":"match","output_mode":"content","path":"f.txt"}`)
	gd := res.Data.(GrepData)
	if gd.Total != 2 || len(gd.Files) != 1 || gd.Files[0].Count != 2 || len(gd.Files[0].Hits) != 2 || gd.Files[0].Hits[1].Line != 4 {
		t.Fatalf("%+v", gd)
	}
	out, _ = run(t, Grep(Options{Dir: dir, MaxFileBytes: 4}), `{"pattern":"x","multiline":true}`)
	if !strings.Contains(out, "over the 4 byte multiline limit") {
		t.Fatalf("%q", out)
	}
	out, _ = run(t, g, `{"pattern":"match","glob":"*.{txt,md}","output_mode":"count"}`)
	if out != f+":2" {
		t.Fatalf("brace glob filter %q", out)
	}
	out, _ = run(t, g, `{"pattern":"match","glob":"f/*.txt","path":"f.txt","output_mode":"count"}`)
	if out != "No matches found" {
		t.Fatalf("slash glob against a file target %q", out)
	}
	out, _ = run(t, g, `{"pattern":"match","glob":"f.txt","path":"f.txt","output_mode":"count"}`)
	if out != f+":2" {
		t.Fatalf("name glob against a file target %q", out)
	}
}
