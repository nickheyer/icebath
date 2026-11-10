package pyzip

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// One recorded result, the bytes of a success or the exception class and text
type pyOutcome struct {
	SHA    string  `json:"sha,omitempty"`
	Size   int     `json:"size,omitempty"`
	Dir    bool    `json:"dir,omitempty"`
	Class  string  `json:"class,omitempty"`
	Msg    string  `json:"msg,omitempty"`
	Result *string `json:"result,omitempty"`
}

// Everything CPython 3.14.8 recorded for one archive
type streamExpect struct {
	IgnoreInvalidNames bool                 `json:"ignore_invalid_names"`
	Namelist           []string             `json:"namelist"`
	Read               map[string]pyOutcome `json:"read"`
	Testzip            pyOutcome            `json:"testzip"`
	Extractall         pyOutcome            `json:"extractall"`
	Extracted          map[string]pyOutcome `json:"extracted"`
}

// Archive recorded with a legacy Zstandard v0.5 frame, checked by its own test
const legacyZstdCase = "zstd-v05magic-first.zip"

func loadStreamExpect(t *testing.T) map[string]streamExpect {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "stream", "expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expect map[string]streamExpect
	if err := json.Unmarshal(raw, &expect); err != nil {
		t.Fatal(err)
	}
	return expect
}

func dataOutcome(data []byte) pyOutcome {
	sum := sha256.Sum256(data)
	return pyOutcome{SHA: hex.EncodeToString(sum[:]), Size: len(data)}
}

func errOutcome(err error) pyOutcome { return pyOutcome{Class: pyClass(err), Msg: err.Error()} }

// Files and directories an extraction left, keyed like os.path.relpath with directories ending in a slash
func extractedTree(t *testing.T, root string) map[string]pyOutcome {
	t.Helper()
	tree := map[string]pyOutcome{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			tree[rel+"/"] = pyOutcome{Dir: true}
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		tree[rel] = dataOutcome(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// Checks read, testzip and extractall of one archive against its CPython record
func checkStreamArchive(t *testing.T, name string, want streamExpect) {
	path := filepath.Join("testdata", "stream", name)
	zr, err := OpenReader(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer zr.Close()
	zr.IgnoreInvalidNames = want.IgnoreInvalidNames
	if got := zr.Namelist(); !reflect.DeepEqual(got, want.Namelist) {
		t.Errorf("namelist %q, want %q", got, want.Namelist)
	}
	for _, member := range want.Namelist {
		data, err := zr.Read(member)
		got := dataOutcome(data)
		if err != nil {
			got = errOutcome(err)
		}
		if !reflect.DeepEqual(got, want.Read[member]) {
			t.Errorf("read %s: %+v, want %+v", member, got, want.Read[member])
		}
	}
	bad, found, err := zr.TestZip()
	gotTest := pyOutcome{}
	switch {
	case err != nil:
		gotTest = errOutcome(err)
	case found:
		gotTest.Result = &bad
	}
	if !reflect.DeepEqual(gotTest, want.Testzip) {
		t.Errorf("testzip %+v, want %+v", gotTest, want.Testzip)
	}
	dest := filepath.Join(t.TempDir(), name)
	gotExtract := pyOutcome{}
	if err := zr.ExtractAll(&dest, nil); err != nil {
		gotExtract = errOutcome(err)
	}
	if !reflect.DeepEqual(gotExtract, want.Extractall) {
		t.Errorf("extractall %+v, want %+v", gotExtract, want.Extractall)
	}
	if want.Extracted == nil {
		want.Extracted = map[string]pyOutcome{}
	}
	tree := map[string]pyOutcome{}
	if _, err := os.Stat(dest); err == nil {
		tree = extractedTree(t, dest)
	}
	if !reflect.DeepEqual(tree, want.Extracted) {
		t.Errorf("extracted %+v, want %+v", tree, want.Extracted)
	}
}

// Every archive CPython 3.14.8 recorded reads, tests and extracts with the same bytes or exception
func TestStreamMembersMatchCPython(t *testing.T) {
	expect := loadStreamExpect(t)
	names := make([]string, 0, len(expect))
	for name := range expect {
		if name != legacyZstdCase {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) { checkStreamArchive(t, name, expect[name]) })
	}
}

// One decompress call of a recorded decompressor sequence
type decompressStep struct {
	Feed       int    `json:"feed"`
	MaxLength  int    `json:"max_length"`
	Out        string `json:"out"`
	EOF        bool   `json:"eof"`
	NeedsInput bool   `json:"needs_input"`
	Class      string `json:"class"`
	Msg        string `json:"msg"`
}

// bz2.BZ2Decompressor and compression.zstd.ZstdDecompressor return the same output, flags and errors call by call
func TestDecompressorStepsMatchCPython(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "stream", "decompressor.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]struct {
		Method string           `json:"method"`
		Input  string           `json:"input"`
		Steps  []decompressStep `json:"steps"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := cases[name]
		t.Run(name, func(t *testing.T) {
			input, err := hex.DecodeString(c.Input)
			if err != nil {
				t.Fatal(err)
			}
			var d pushDecompressor = newBZ2Decompressor()
			if c.Method == "zstd" {
				d = newZstdDecompressor()
			}
			defer d.release()
			pos := 0
			for i, want := range c.Steps {
				end := min(pos+want.Feed, len(input))
				out, err := d.decompress(input[pos:end], want.MaxLength)
				pos = end
				got := decompressStep{Feed: want.Feed, MaxLength: want.MaxLength}
				if err != nil {
					got.Class, got.Msg = pyClass(err), err.Error()
				} else {
					got.Out, got.EOF, got.NeedsInput = hex.EncodeToString(out), d.isEOF(), d.wantsInput()
				}
				if got != want {
					t.Fatalf("step %d: %+v, want %+v", i, got, want)
				}
			}
		})
	}
}

// Block sizes grow 32 KiB, 64 KiB, 256 KiB and on, the last block cut at max_length
func TestOutputBufferGrowsLikeBlocksOutputBuffer(t *testing.T) {
	sizes := func(maxLength, blocks int) []int {
		b := newOutputBuffer(maxLength)
		got := []int{len(b.data)}
		for len(got) < blocks && (maxLength < 0 || len(b.data) < maxLength) {
			b.grow()
			got = append(got, len(b.data))
		}
		return got
	}
	if got := sizes(-1, 5); !reflect.DeepEqual(got, []int{32 << 10, 96 << 10, 352 << 10, 352<<10 + 1<<20, 352<<10 + 5<<20}) {
		t.Fatalf("unbounded %v", got)
	}
	if got := sizes(100<<10, 5); !reflect.DeepEqual(got, []int{32 << 10, 96 << 10, 100 << 10}) {
		t.Fatalf("bounded %v", got)
	}
	if got := sizes(4096, 5); !reflect.DeepEqual(got, []int{4096}) {
		t.Fatalf("small %v", got)
	}
}

// Illegal characters become underscores and parts lose trailing dots and spaces
func TestSanitizeWindowsName(t *testing.T) {
	cases := map[string]string{`a:b<c>d|e"f?g*h`: "a_b_c_d_e_f_g_h", `dir. \file .`: `dir\file`, `x\\y`: `x\y`, ` . `: ""}
	for in, want := range cases {
		if got := sanitizeWindowsName(in, `\`); got != want {
			t.Fatalf("%q: %q want %q", in, got, want)
		}
	}
}

// Members of other methods copy through shutil.copyfileobj chunks and reads after close raise ValueError
func TestMemberWriteToAndClosedRead(t *testing.T) {
	zr, err := OpenReader(filepath.Join("testdata", "stream", "zstd-second-first.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	f, err := zr.Open("d/m.bin")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := f.Read(make([]byte, 4)); err == nil || err.Error() != "read from closed file." || pyClass(err) != "ValueError" {
		t.Fatalf("closed read %v", err)
	}
	s, err := zr.Open("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var sink sinkWriter
	if n, err := s.(*memberFile).WriteTo(&sink); err != nil || n != 22 || string(sink) != "leading stored member\n" {
		t.Fatalf("stored WriteTo %d %v %q", n, err, sink)
	}
	s.Close()
	if _, err := s.Read(make([]byte, 4)); err == nil || err.Error() != "read from closed file." || pyClass(err) != "ValueError" {
		t.Fatalf("closed stored read %v", err)
	}
}

type sinkWriter []byte

func (w *sinkWriter) Write(p []byte) (int, error) {
	*w = append(*w, p...)
	return len(p), nil
}
