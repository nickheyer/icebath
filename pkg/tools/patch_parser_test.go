package tools

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Map backed file backend mirroring the hermes test double
type fakePatchOps struct {
	files    map[string]string
	failRead map[string]bool
	panicOn  string
}

func newFakePatchOps(files map[string]string, failRead ...string) *fakePatchOps {
	f := &fakePatchOps{files: map[string]string{}, failRead: map[string]bool{}}
	for k, v := range files {
		f.files[k] = v
	}
	for _, p := range failRead {
		f.failRead[p] = true
	}
	return f
}

func (f *fakePatchOps) ReadFileRaw(path string) *ReadResult {
	if f.failRead[path] {
		return &ReadResult{Error: "transport down"}
	}
	c, ok := f.files[path]
	if !ok {
		return &ReadResult{Error: "File not found: " + path, NotFound: true}
	}
	return &ReadResult{Content: c}
}

func (f *fakePatchOps) WriteFile(path, content string, pre *string) *WriteResult {
	if path == f.panicOn {
		panic("disk on fire")
	}
	f.files[path] = content
	return &WriteResult{BytesWritten: len(content)}
}

func (f *fakePatchOps) DeleteFile(path string) *WriteResult {
	if _, ok := f.files[path]; !ok {
		return &WriteResult{Error: "delete failed: " + path}
	}
	delete(f.files, path)
	return &WriteResult{}
}

func (f *fakePatchOps) MoveFile(src, dst string) *WriteResult {
	c, ok := f.files[src]
	if !ok {
		return &WriteResult{Error: "move failed: " + src}
	}
	delete(f.files, src)
	f.files[dst] = c
	return &WriteResult{}
}

func patchResultMap(t *testing.T, r *PatchResult) map[string]any {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPatchParserGolden(t *testing.T) {
	g := loadFileEditGolden(t)
	for _, c := range g.Patch {
		ops, parseErr := parseV4APatch(c.Patch)
		if parseErr != c.ParseError {
			t.Errorf("%s parse error: got %q want %q", c.Name, parseErr, c.ParseError)
			continue
		}
		if len(ops) != len(c.Ops) {
			t.Errorf("%s: got %d ops want %d", c.Name, len(ops), len(c.Ops))
			continue
		}
		for i, op := range ops {
			w := c.Ops[i]
			if op.kind != w.Kind || op.filePath != w.Path || op.newPath != w.NewPath || len(op.hunks) != len(w.Hunks) {
				t.Errorf("%s op %d: got %+v want %+v", c.Name, i, op, w)
				continue
			}
			for k, h := range op.hunks {
				wh := w.Hunks[k]
				if h.contextHint != wh.Hint || len(h.lines) != len(wh.Lines) {
					t.Errorf("%s op %d hunk %d: got %+v want %+v", c.Name, i, k, h, wh)
					continue
				}
				for j, l := range h.lines {
					if string(l.prefix) != wh.Lines[j][0] || l.content != wh.Lines[j][1] {
						t.Errorf("%s op %d hunk %d line %d: got %q%q want %v", c.Name, i, k, j, l.prefix, l.content, wh.Lines[j])
					}
				}
			}
		}
		if parseErr != "" {
			continue
		}
		fake := newFakePatchOps(c.Files, c.FailRead...)
		got := patchResultMap(t, applyV4AOperations(ops, fake))
		if !reflect.DeepEqual(got, c.Result) {
			t.Errorf("%s result:\n got  %#v\n want %#v", c.Name, got, c.Result)
		}
		if !reflect.DeepEqual(fake.files, c.After) && !(len(fake.files) == 0 && len(c.After) == 0) {
			t.Errorf("%s files:\n got  %#v\n want %#v", c.Name, fake.files, c.After)
		}
	}
}

func TestPatchParserParseShapes(t *testing.T) {
	ops, errText := parseV4APatch("*** Begin Patch\n*** Update File: a.go\n@@ func main() @@\n ctx\n-old\n+new\n*** Add File: b.go\n+x\n*** Delete File: c.go\n*** Move File: d.go -> e.go\n*** End Patch\n+*** End Patch")
	if errText != "" || len(ops) != 4 {
		t.Fatalf("parse: %q %d", errText, len(ops))
	}
	if ops[0].kind != v4aUpdate || ops[0].hunks[0].contextHint != "func main()" || len(ops[0].hunks[0].lines) != 3 {
		t.Fatalf("update op %+v", ops[0])
	}
	if ops[1].kind != v4aAdd || ops[2].kind != v4aDelete || ops[3].kind != v4aMove || ops[3].newPath != "e.go" {
		t.Fatalf("ops %+v %+v %+v", ops[1], ops[2], ops[3])
	}
	ops, errText = parseV4APatch("")
	if errText != "" || ops == nil || len(ops) != 0 {
		t.Fatalf("empty patch must be an empty list without error, got %v %q", ops, errText)
	}
	_, errText = parseV4APatch("*** Update File: x\n*** Move File: a -> \n")
	want := "Parse error: UPDATE 'x': no hunks found; MOVE 'a': missing destination path (expected 'src -> dst')"
	if errText != want {
		t.Fatalf("parse errors:\n got  %q\n want %q", errText, want)
	}
}

func TestPatchParserValidateBeforeApply(t *testing.T) {
	fake := newFakePatchOps(map[string]string{"a.txt": "one\n", "b.txt": "two\n"})
	ops, _ := parseV4APatch("*** Begin Patch\n*** Update File: a.txt\n-one\n+ONE\n*** Update File: b.txt\n-zzz\n+X\n*** End Patch")
	res := applyV4AOperations(ops, fake)
	if res.Success || !strings.HasPrefix(res.Error, "Patch validation failed (no files were modified):\n  • b.txt: hunk 1 (no hint) not found — Could not find a match for old_string in the file") {
		t.Fatalf("validation error text: %q", res.Error)
	}
	if fake.files["a.txt"] != "one\n" {
		t.Fatal("a failed validation must not modify any file")
	}
}

func TestPatchParserApplyPhaseFailure(t *testing.T) {
	fake := newFakePatchOps(map[string]string{"a.txt": "one\n"})
	fake.panicOn = "a.txt"
	ops, _ := parseV4APatch("*** Begin Patch\n*** Update File: a.txt\n-one\n+ONE\n*** Add File: n.txt\n+n\n*** End Patch")
	res := applyV4AOperations(ops, fake)
	want := "Apply phase failed (state may be inconsistent — run `git diff` to assess):\n  • Error processing a.txt: disk on fire"
	if res.Success || res.Error != want {
		t.Fatalf("apply failure:\n got  %q\n want %q", res.Error, want)
	}
	if len(res.FilesCreated) != 1 || res.FilesCreated[0] != "n.txt" {
		t.Fatalf("later ops still apply, got %+v", res)
	}
	deleteFake := newFakePatchOps(map[string]string{"d.txt": "x"})
	ops, _ = parseV4APatch("*** Begin Patch\n*** Delete File: d.txt\n*** End Patch")
	delete(deleteFake.files, "d.txt")
	direct := applyV4AAddOrDeleteDirect(ops[0], deleteFake)
	if direct.ok || direct.payload != "Cannot delete d.txt: file not found" {
		t.Fatalf("delete race: %+v", direct)
	}
}

func applyV4AAddOrDeleteDirect(op *v4aOperation, f v4aFileOps) v4aApplyResult {
	return v4aApplyDispatch[op.kind].handler(op, f)
}

func TestPatchParserWritesCarryShas(t *testing.T) {
	fake := &shaPatchOps{fakePatchOps: newFakePatchOps(map[string]string{"a.txt": "x\n"})}
	ops, _ := parseV4APatch("*** Begin Patch\n*** Update File: a.txt\n-x\n+y\n*** Add File: b.txt\n+b\n*** End Patch")
	res := applyV4AOperations(ops, fake)
	if !res.Success || len(res.Writes) != 2 {
		t.Fatalf("writes: %+v", res)
	}
	if w := res.Writes[0]; w.path != "a.txt" || !w.readKnown || w.readSha != "read-a.txt" || !w.writtenKnown || w.writtenSha != "wrote-a.txt" {
		t.Fatalf("update write record %+v", w)
	}
	if w := res.Writes[1]; w.path != "b.txt" || !w.readKnown || w.readSha != "" {
		t.Fatalf("add write record must vouch an empty read sha, got %+v", w)
	}
}

// File backend that reports content hashes for reads and writes
type shaPatchOps struct{ *fakePatchOps }

func (s *shaPatchOps) ReadFileRaw(path string) *ReadResult {
	r := s.fakePatchOps.ReadFileRaw(path)
	if r.Error == "" {
		r.ContentSha256 = shaRef("read-" + path)
	}
	return r
}

func (s *shaPatchOps) WriteFile(path, content string, pre *string) *WriteResult {
	w := s.fakePatchOps.WriteFile(path, content, pre)
	w.ContentSha256 = shaRef("wrote-" + path)
	return w
}

func TestPatchParserHelpers(t *testing.T) {
	if countOccurrences("aaaa", "aa") != 3 || countOccurrences("abc", "") != 4 {
		t.Fatal("countOccurrences must count overlaps")
	}
	if got := pyReprStr("it's"); got != `"it's"` {
		t.Fatalf("pyReprStr = %s", got)
	}
	if got := pyReprStr("a\tb\x01 "); got != `'a\tb\x01\xa0'` {
		t.Fatalf("pyReprStr = %s", got)
	}
	lines := pySplitLinesKeepEnds("a\r\nb\rc d")
	if len(lines) != 4 || lines[0] != "a\r\n" || lines[1] != "b\r" || lines[2] != "c " || lines[3] != "d" {
		t.Fatalf("pySplitLinesKeepEnds = %q", lines)
	}
	if got := runeOffsetBack("héllo", 6, 2); got != 4 {
		t.Fatalf("runeOffsetBack = %d", got)
	}
}
