package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	fileOpsReadSentinelRe  = regexp.MustCompile(`__ICEBATH_RF_[0-9a-f]{32}__`)
	fileOpsWriteSentinelRe = regexp.MustCompile(`__ICEBATH_WF_[0-9a-f]{32}__`)
	fileOpsBytesSentinelRe = regexp.MustCompile(`__ICEBATH_RB_[0-9a-f]+__`)
)

// Options of one compound read probe reply mirroring the file_ops_fakes helper
type compoundReadReply struct {
	size             int
	sample           []byte
	content          string
	totalLines       int
	noTrailingNL     bool
	sampleRC, readRC int
}

// Stdout of the compound read probe for a regular file exactly as the shell prints it
func compoundReadOutput(sentinel string, r compoundReadReply) string {
	sampleSeg := ""
	if len(r.sample) > 0 {
		sampleSeg = base64.StdEncoding.EncodeToString(r.sample) + "\n"
	}
	trailing := 1
	if r.noTrailingNL {
		trailing = 0
	}
	return fmt.Sprintf("%d\n%s\n%s%s\n%s%s\n%d\n%s\n%d\n%s\n%d %d\n",
		r.size, sentinel, sampleSeg, sentinel, r.content, sentinel, r.totalLines, sentinel, trailing, sentinel, r.sampleRC, r.readRC)
}

// Stdout of the compound write probe for an existing file with its BOM head and body
func compoundWriteProbeOutput(sentinel string, head3 []byte, body string) string {
	headSeg := ""
	if len(head3) > 0 {
		headSeg = base64.StdEncoding.EncodeToString(head3) + "\n"
	}
	return headSeg + sentinel + "\n" + body
}

// Fenced base64 reply of a byte exact read with the wc size when the command asks for it
func fencedBase64Reply(command string, payload []byte, rc int) string {
	sentinel := fileOpsBytesSentinelRe.FindString(command)
	body := ""
	if rc == 0 {
		body = base64.StdEncoding.EncodeToString(payload)
	}
	size := ""
	if strings.Contains(command, "wc -c <") {
		size = strconv.Itoa(len(payload)) + "\n" + sentinel + "\n"
	}
	return sentinel + "\n" + body + "\n" + sentinel + "\n" + size + strconv.Itoa(rc) + "\n"
}

// Scripted env off the host filesystem answering each command through the given closure
func mockFileOpsEnv(answer func(command string, opts ExecOptions) *ExecResult) *fakeTermEnv {
	if answer == nil {
		answer = func(string, ExecOptions) *ExecResult { return &ExecResult{} }
	}
	return &fakeTermEnv{envType: "fake", cwd: "/tmp/test", execute: func(_ context.Context, command string, opts ExecOptions) (*ExecResult, error) {
		return answer(command, opts), nil
	}}
}

// Runs a command through bash in the requested cwd with stdin piped or embedded as a heredoc
func bashExecute(heredoc bool, intercept func(command string) (*ExecResult, bool)) func(context.Context, string, ExecOptions) (*ExecResult, error) {
	return func(_ context.Context, command string, opts ExecOptions) (*ExecResult, error) {
		if intercept != nil {
			if r, ok := intercept(command); ok {
				return r, nil
			}
		}
		stdin := opts.Stdin
		if stdin != nil {
			raw, err := pystr.SurrogateEscapeEncode(*stdin)
			if err != nil {
				return &ExecResult{Output: "[stdin write failed: " + err.Error() + "]", StdinError: err.Error()}, nil
			}
			encoded := string(raw)
			stdin = &encoded
		}
		if heredoc && stdin != nil && *stdin != "" {
			command = embedStdinHeredoc(command, *stdin)
			stdin = nil
		}
		cmd := exec.Command("bash", "-c", command)
		cmd.Dir = opts.Cwd
		if stdin != nil {
			cmd.Stdin = strings.NewReader(*stdin)
		}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		rc := 0
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return nil, err
			}
			rc = exitErr.ExitCode()
		}
		d := &utf8Decoder{}
		return &ExecResult{Output: d.decode(out.Bytes()) + d.finish(), ReturnCode: rc}, nil
	}
}

// Real bash env, local when it shares the host filesystem, the subprocess double otherwise
func bashFileOpsEnv(cwd string, host bool) *fakeTermEnv {
	envType := "fake"
	if host {
		envType = "local"
	}
	return &fakeTermEnv{envType: envType, cwd: cwd, execute: bashExecute(false, nil)}
}

// Every command the env saw in order
func envCommands(f *fakeTermEnv) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.command
	}
	return out
}

// Forgets the recorded commands
func resetEnvCommands(f *fakeTermEnv) {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

// Writes bytes to a file under dir and returns its path
func writeTestFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// File backend over a host local bash env with the native lane on or pinned to the shell
func hostFileOps(t *testing.T, native bool) (*shellFileOps, *fakeTermEnv, string) {
	t.Helper()
	dir := t.TempDir()
	if native {
		t.Setenv("ICEBATH_NATIVE_FILE_READ", "1")
	} else {
		t.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
	}
	env := bashFileOpsEnv(dir, true)
	return newShellFileOps(remoteTestEnv(t), env, dir), env, dir
}

// JSON of a read result for whole result comparisons
func readResultJSON(t *testing.T, r *ReadResult) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFileOpsReadResultOmitsDefaults(t *testing.T) {
	d := readResultJSON(t, &ReadResult{})
	if strings.Contains(d, `"error"`) || strings.Contains(d, `"similar_files"`) {
		t.Fatalf("defaults leaked into %s", d)
	}
}

func TestFileOpsSearchResultTruncatedMarksLowerBound(t *testing.T) {
	b, _ := json.Marshal((&SearchResult{TotalCount: 100, Truncated: true}).toDict(false))
	if !strings.Contains(string(b), `"truncated":true`) || !strings.Contains(string(b), `"total_count_is_lower_bound":true`) {
		t.Fatalf("got %s", b)
	}
	b, _ = json.Marshal((&SearchResult{TotalCount: 100}).toDict(false))
	if strings.Contains(string(b), "total_count_is_lower_bound") {
		t.Fatalf("got %s", b)
	}
}

// Path grouped matches the way ripgrep emits them, padding the remainder onto the last path
func densifyMatches(n int, paths ...string) []SearchMatch {
	if len(paths) == 0 {
		paths = []string{"a.py"}
	}
	per := max(1, n/len(paths))
	var out []SearchMatch
	ln := 0
	for _, p := range paths {
		for i := 0; i < per; i++ {
			ln++
			out = append(out, SearchMatch{Path: p, LineNumber: ln, Content: fmt.Sprintf("line content %d", ln)})
		}
	}
	for len(out) < n {
		ln++
		out = append(out, SearchMatch{Path: paths[len(paths)-1], LineNumber: ln, Content: fmt.Sprintf("line content %d", ln)})
	}
	return out
}

func TestFileOpsSearchResultDensify(t *testing.T) {
	d := (&SearchResult{Matches: densifyMatches(10), TotalCount: 10}).toDict(false)
	if _, ok := d.Get("matches"); !ok {
		t.Fatal("verbose matches missing by default")
	}
	if _, ok := d.Get("matches_text"); ok {
		t.Fatal("densified without opt in")
	}
	d = (&SearchResult{Matches: densifyMatches(4), TotalCount: 4}).toDict(true)
	if _, ok := d.Get("matches_text"); ok {
		t.Fatal("densified below the threshold")
	}
	var spaced []SearchMatch
	for i := 0; i < 6; i++ {
		spaced = append(spaced, SearchMatch{Path: "my dir/a b.py", LineNumber: i + 1, Content: fmt.Sprintf("x%d", i)})
	}
	text, _ := (&SearchResult{Matches: spaced, TotalCount: 6}).toDict(true).Get("matches_text")
	if first := strings.Split(text.(string), "\n")[0]; !strings.Contains(first, "my dir/a b.py") {
		t.Fatalf("header %q", first)
	}
	got := jsonCompact((&SearchResult{Matches: densifyMatches(6, "a.py", "b.py"), TotalCount: 6}).toDict(true))
	want := `{"total_count":6,"matches_format":"path-grouped: each file path on its own line, followed by indented '<line>: <content>' rows for matches in that file",` +
		`"matches_text":"a.py\n  1: line content 1\n  2: line content 2\n  3: line content 3\nb.py\n  4: line content 4\n  5: line content 5\n  6: line content 6"}`
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func TestFileOpsSearchResultKeyOrderAndCounts(t *testing.T) {
	r := &SearchResult{TotalCount: 3, Files: []string{"z", "a"}, LimitReason: "cap", Warning: "w", Error: "e"}
	r.setCount("z.py", 1)
	r.setCount("a.py", 1)
	r.updateCounts([]SearchCount{{"z.py", 2}, {"m.py", 5}})
	got, _ := json.Marshal(r.toDict(false))
	want := `{"total_count":3,"files":["z","a"],"counts":{"z.py":2,"a.py":1,"m.py":5},"limit_reason":"cap","warning":"w","error":"e"}`
	if string(got) != want {
		t.Fatalf("got %s", got)
	}
	rows, _ := json.Marshal((&SearchResult{Matches: []SearchMatch{{Path: "p", LineNumber: 2, Content: "c"}}}).toDict(true))
	if string(rows) != `{"total_count":0,"matches":[{"path":"p","line":2,"content":"c"}]}` {
		t.Fatalf("got %s", rows)
	}
}

func TestFileOpsLintResultToDict(t *testing.T) {
	if d := (&LintResult{Skipped: true, Message: "m"}).toDict(); d.Vals["status"] != "skipped" || d.Vals["message"] != "m" || strings.Join(d.Keys, ",") != "status,message" {
		t.Fatalf("skipped %v", d.Vals)
	}
	if d := newLintResult().toDict(); d.Vals["status"] != "ok" || d.Vals["output"] != "" || strings.Join(d.Keys, ",") != "status,output" {
		t.Fatalf("ok %v", d.Vals)
	}
	if d := (&LintResult{Output: "bad", Message: "n"}).toDict(); strings.Join(d.Keys, ",") != "status,output,message" || d.Vals["status"] != "error" || d.Vals["output"] != "bad" || d.Vals["message"] != "n" {
		t.Fatalf("error %v", d.Vals)
	}
	if n, over := searchLineNumber("007"); n != 7 || over != "" {
		t.Fatalf("leading zeros %d %q", n, over)
	}
	if n, over := searchLineNumber("123456789012345678901234"); n != 0 || over != "123456789012345678901234" {
		t.Fatalf("overflow %d %q", n, over)
	}
	if rows, _ := json.Marshal((&SearchResult{Matches: []SearchMatch{newSearchMatch("p", "123456789012345678901234", "c")}}).toDict(false)); !strings.Contains(string(rows), `"line":123456789012345678901234`) {
		t.Fatalf("overflow json %s", rows)
	}
}

func TestFileOpsTextHelpers(t *testing.T) {
	if n := countConflictBlocks("1|<<<<<<< HEAD\n2|a\n3|=======\n4|b\n5|>>>>>>> branch\n"); n != 1 {
		t.Fatalf("conflicts %d", n)
	}
	if n := countConflictBlocks("1|<<<<<<< lone marker\n"); n != 0 {
		t.Fatalf("lone marker %d", n)
	}
	if got := detectLineEnding("a\r\nb\n"); got != "\r\n" {
		t.Fatalf("crlf %q", got)
	}
	if got := detectLineEnding("solo"); got != "" {
		t.Fatalf("single line %q", got)
	}
	if got := normalizeLineEndings("a\r\nb\rc\n", "\r\n"); got != "a\r\nb\r\nc\r\n" {
		t.Fatalf("normalize %q", got)
	}
	if got := normalizeLineEndings("a\r\nb", "x"); got != "a\r\nb" {
		t.Fatalf("unknown target %q", got)
	}
	if got, had := stripBOM("\xef\xbb\xbfhi\xef\xbb\xbf"); got != "hi\xef\xbb\xbf" || !had {
		t.Fatalf("bom %q %v", got, had)
	}
	leak := "keep\n'\x07__ICEBATH_FENCE_ab12__\x07'\n\x1b]0;title\x07\nx\x07y\n"
	if got := stripTerminalFenceLeaks(leak); got != "keep\nxy\n" {
		t.Fatalf("fence %q", got)
	}
}

func TestFileOpsNormalizeReadPaginationClamps(t *testing.T) {
	env := remoteTestEnv(t)
	cases := []struct {
		offset, limit any
		o, l          int
	}{{0, 0, 1, 1}, {-10, -5, 1, 1}, {"bad", "bad", 1, 2000}, {2, 999999, 2, 2000}, {nil, nil, 1, 2000}, {" 7 ", 3.9, 7, 3}}
	for _, c := range cases {
		if o, l := normalizeReadPagination(env, c.offset, c.limit); o != c.o || l != c.l {
			t.Fatalf("%v %v gave %d %d", c.offset, c.limit, o, l)
		}
	}
	if o, l := normalizeSearchPagination(-3, 0); o != 0 || l != 1 {
		t.Fatalf("search %d %d", o, l)
	}
}

func TestFileOpsEscapeShellArgWindowsDrivePath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive path rewrite happens on Windows only")
	}
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(nil), "")
	if got := ops.escapeShellArg("C:/Users/alice/notes.txt", true); got != "'/c/Users/alice/notes.txt'" {
		t.Fatalf("got %s", got)
	}
}

func TestFileOpsEscapeNativeToolArgWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native tool arguments differ on Windows only")
	}
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(nil), "")
	for in, want := range map[string]string{`C:\Users\alice\project`: "'C:/Users/alice/project'", "/c/Users/alice/project": "'C:/Users/alice/project'", "/tmp/workdir": "'/tmp/workdir'"} {
		if got := ops.escapeNativeToolArg(in); got != want {
			t.Fatalf("%s gave %s", in, got)
		}
	}
}

func TestFileOpsEscapeNativeToolArgPosixIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix quoting only")
	}
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(nil), "")
	if got := ops.escapeNativeToolArg("/tmp/it's"); got != `'/tmp/it'"'"'s'` {
		t.Fatalf("got %s", got)
	}
	if got := ops.escapeShellArg("/tmp/it's", true); got != `'/tmp/it'"'"'s'` {
		t.Fatalf("got %s", got)
	}
}

func TestFileOpsIsLikelyBinary(t *testing.T) {
	for p, want := range map[string]bool{"photo.png": true, "data.db": true, "code.py": false, "readme.md": false} {
		if got := isLikelyBinary(p, ""); got != want {
			t.Fatalf("%s gave %v", p, got)
		}
	}
	if !isLikelyBinary("archive.tar.gz", "hello") {
		t.Fatal("binary extension must short circuit")
	}
	if isLikelyBinary("unknown.xyz", "Hello, world!\nThis is a normal text file.\n") {
		t.Fatal("plain text flagged")
	}
	if !isLikelyBinary("data.xyz", strings.Repeat("\x00", 301)+strings.Repeat("a", 699)) {
		t.Fatal("just above threshold not flagged")
	}
	if isLikelyBinary("file.txt", strings.Repeat("\t", 400)+strings.Repeat("\n", 300)+strings.Repeat("\r", 200)+strings.Repeat("a", 100)) {
		t.Fatal("whitespace counted as non printable")
	}
	if isLikelyBinary("file.xyz", strings.Repeat("\x00", 200)+strings.Repeat("a", 800)+strings.Repeat("\x00", 1000)) {
		t.Fatal("analysed past the first 1000 characters")
	}
	if !isLikelyBinary("notes.txt", "caf\xef\xbf\xbd r\xef\xbf\xbdsum\xef\xbf\xbd\n") {
		t.Fatal("replacement characters not flagged")
	}
	if isLikelyBinary("notes.txt", "café résumé\nsecond\n") {
		t.Fatal("utf8 text flagged")
	}
}

func TestFileOpsByteLayerBinaryDetection(t *testing.T) {
	cases := []struct {
		name   string
		sample []byte
		binary bool
	}{
		{"cjk cut", append(bytes.Repeat([]byte("a"), 999), []byte("中")...)[:1000], false},
		{"emoji cut", append(bytes.Repeat([]byte("x"), 998), []byte("🎉")...)[:1000], false},
		{"bom", []byte("\xef\xbb\xbfhello"), false},
		{"real replacement char", []byte("log: \xef\xbf\xbd bad byte\n"), false},
		{"nul", []byte("MZ\x00\x01text"), true},
		{"latin1", []byte("caf\xe9 au lait, plus padding"), true},
		{"empty", []byte{}, false},
		{"garbage prefix", append(append([]byte("\xff\xfe"), bytes.Repeat([]byte("a"), 10)...), 0xe4), true},
	}
	for _, c := range cases {
		if got := isLikelyBinaryBytes(c.sample); got != c.binary {
			t.Fatalf("%s gave %v", c.name, got)
		}
	}
}

func TestFileOpsSampleFileBytesTransport(t *testing.T) {
	payload := []byte(strings.Repeat("汉字", 400))[:1000]
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(func(command string, _ ExecOptions) *ExecResult {
		return &ExecResult{Output: fencedBase64Reply(command, payload, 0)}
	}), "")
	if got, ok := ops.sampleFileBytes("/tmp/x.txt", 1000); !ok || !bytes.Equal(got, payload) {
		t.Fatalf("sample %v %d", ok, len(got))
	}
	ops = newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(func(string, ExecOptions) *ExecResult {
		return &ExecResult{Output: "not base64 at all!!"}
	}), "")
	if _, ok := ops.sampleFileBytes("/tmp/x.txt", 1000); ok {
		t.Fatal("unfenced output accepted")
	}
	ops = newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(func(string, ExecOptions) *ExecResult {
		return &ExecResult{ReturnCode: 127}
	}), "")
	if _, ok := ops.sampleFileBytes("/tmp/x.txt", 1000); ok {
		t.Fatal("failed exec accepted")
	}
}

// Scripted env answering the compound read probe with the given bytes as one line
func compoundReadDispatch(data []byte) func(string, ExecOptions) *ExecResult {
	return func(command string, _ ExecOptions) *ExecResult {
		if m := fileOpsReadSentinelRe.FindString(command); m != "" {
			sample := data
			if len(sample) > 1000 {
				sample = sample[:1000]
			}
			return &ExecResult{Output: compoundReadOutput(m, compoundReadReply{size: len(data), sample: sample, content: decodeUTF8Replace(data), totalLines: 1})}
		}
		return &ExecResult{}
	}
}

func TestFileOpsReadFileCJKOverMockedTransport(t *testing.T) {
	content := []byte(strings.Repeat("汉字测试", 300))
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(compoundReadDispatch(content)), "")
	r := ops.readFile("/tmp/notes-中文.txt", 1, 2000)
	if r.IsBinary || r.Error != "" || !strings.Contains(r.Content, "汉字测试") {
		t.Fatalf("cjk read %+v", r)
	}
	elf := append([]byte("\x7fELF\x00\x00binarybinary"), bytes.Repeat([]byte{0}, 100)...)
	ops = newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(compoundReadDispatch(elf)), "")
	if r := ops.readFile("/tmp/a.out", 1, 2000); !r.IsBinary {
		t.Fatalf("nul binary read %+v", r)
	}
}

func TestFileOpsReadFileStripsLeakedFenceMarkers(t *testing.T) {
	leaked := "'\x07__ICEBATH_FENCE_a9f7b3__\x1b]0;cat '/tmp/test/a.py' 2> /dev/null\x07\n" +
		"print('ok')\n" +
		"__ICEBATH_FENCE_a9f7b3__\x07'\n"
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(func(command string, _ ExecOptions) *ExecResult {
		if m := fileOpsReadSentinelRe.FindString(command); m != "" {
			return &ExecResult{Output: compoundReadOutput(m, compoundReadReply{size: 12, sample: []byte("print('ok')\n"), content: leaked, totalLines: 1})}
		}
		return &ExecResult{}
	}), "")
	r := ops.readFile("/tmp/test/a.py", 1, 2000)
	if r.Error != "" || strings.Contains(r.Content, "ICEBATH_FENCE") || strings.Contains(r.Content, "\x1b]") ||
		strings.Contains(r.Content, "\x07") || !strings.Contains(r.Content, "1|print('ok')") {
		t.Fatalf("content %q error %q", r.Content, r.Error)
	}
}

func TestFileOpsAddLineNumbers(t *testing.T) {
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(nil), "")
	cases := []struct {
		in    string
		start int
		want  string
	}{
		{"line1\nline2\nline3\n", 1, "1|line1\n2|line2\n3|line3"},
		{"line1\nline2\nline3", 1, "1|line1\n2|line2\n3|line3"},
		{"a\n\n", 1, "1|a\n2|"},
		{"def f():\n    return 1\n", 10, "10|def f():\n11|    return 1"},
	}
	for _, c := range cases {
		if got := ops.addLineNumbers(c.in, c.start); got != c.want {
			t.Fatalf("%q gave %q", c.in, got)
		}
	}
	long := ops.addLineNumbers(strings.Repeat("é", 2001), 1)
	if long != "1|"+strings.Repeat("é", 2000)+"... [truncated]" {
		t.Fatal("character clamp not applied by characters")
	}
}

func TestFileOpsWriteFileDeniedPath(t *testing.T) {
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(nil), "")
	r := ops.WriteFile("~/.ssh/authorized_keys", "evil key", nil)
	if r.Error == "" || !strings.Contains(strings.ToLower(r.Error), "denied") {
		t.Fatalf("got %+v", r)
	}
}

func TestFileOpsMoveFileFailure(t *testing.T) {
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(func(string, ExecOptions) *ExecResult {
		return &ExecResult{Output: "No such file or directory", ReturnCode: 1}
	}), "")
	r := ops.MoveFile("/tmp/nonexistent.txt", "/tmp/dest.txt")
	if r.Error == "" || !strings.Contains(r.Error, "Failed to move") {
		t.Fatalf("got %+v", r)
	}
}

func TestFileOpsPatchReplaceFailsWhenNotPersisted(t *testing.T) {
	files := map[string]string{"/tmp/test/a.py": "hello world\n"}
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(func(command string, _ ExecOptions) *ExecResult {
		if strings.Contains(command, "base64 < ") {
			for p, c := range files {
				if strings.Contains(command, p) {
					return &ExecResult{Output: fencedBase64Reply(command, []byte(c), 0)}
				}
			}
			return &ExecResult{ReturnCode: 1}
		}
		if strings.HasPrefix(command, "if [ -f ") || strings.HasPrefix(command, "wc -c") {
			for p, c := range files {
				if strings.Contains(command, p) {
					return &ExecResult{Output: strconv.Itoa(len(c))}
				}
			}
			return &ExecResult{Output: "0"}
		}
		return &ExecResult{}
	}), "")
	r := ops.patchReplace("/tmp/test/a.py", "hello", "hi", false, nil)
	if r.Error == "" || !strings.Contains(strings.ToLower(r.Error), "verification failed") || !strings.Contains(strings.ToLower(r.Error), "did not persist") {
		t.Fatalf("got success=%v diff=%q error=%q", r.Success, r.Diff, r.Error)
	}
}

func TestFileOpsPatchReplaceFailsWhenVerifyReadErrors(t *testing.T) {
	reads := 0
	content := "hello world\n"
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(func(command string, opts ExecOptions) *ExecResult {
		if opts.Stdin != nil {
			content = *opts.Stdin
			return &ExecResult{}
		}
		if strings.Contains(command, "base64 < ") {
			reads++
			if reads == 1 {
				return &ExecResult{Output: fencedBase64Reply(command, []byte(content), 0)}
			}
			return &ExecResult{ReturnCode: 1}
		}
		if strings.HasPrefix(command, "if [ -f ") || strings.HasPrefix(command, "wc -c") {
			return &ExecResult{Output: strconv.Itoa(len(content))}
		}
		return &ExecResult{}
	}), "")
	r := ops.patchReplace("/tmp/test/a.py", "hello", "hi", false, nil)
	if r.Error == "" || !strings.Contains(strings.ToLower(r.Error), "could not re-read") {
		t.Fatalf("got %+v", r)
	}
}

func TestFileOpsAtomicWriteOverwritePreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix modes")
	}
	for _, host := range []bool{false, true} {
		dir := t.TempDir()
		ops := newShellFileOps(remoteTestEnv(t), bashFileOpsEnv(dir, host), dir)
		dest := writeTestFile(t, dir, "existing.sh", []byte("#!/bin/sh\n"))
		os.Chmod(dest, 0o755)
		if r := ops.WriteFile(dest, "#!/bin/sh\necho updated\n", nil); r.Error != "" {
			t.Fatalf("host=%v %s", host, r.Error)
		}
		data, _ := os.ReadFile(dest)
		st, _ := os.Stat(dest)
		if string(data) != "#!/bin/sh\necho updated\n" || st.Mode().Perm() != 0o755 {
			t.Fatalf("host=%v mode %04o content %q", host, st.Mode().Perm(), data)
		}
	}
}

func TestFileOpsAtomicWriteThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix symlinks")
	}
	for _, host := range []bool{false, true} {
		dir := t.TempDir()
		ops := newShellFileOps(remoteTestEnv(t), bashFileOpsEnv(dir, host), dir)
		real := writeTestFile(t, dir, "real.txt", []byte("original\n"))
		link := filepath.Join(dir, "link.txt")
		os.Symlink(real, link)
		if r := ops.WriteFile(link, "newcontent\n", nil); r.Error != "" {
			t.Fatalf("host=%v %s", host, r.Error)
		}
		if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("host=%v symlink replaced by a plain file", host)
		}
		if data, _ := os.ReadFile(real); string(data) != "newcontent\n" {
			t.Fatalf("host=%v target %q", host, data)
		}
		target := filepath.Join(dir, "target.txt")
		broken := filepath.Join(dir, "broken.lnk")
		os.Symlink(target, broken)
		if r := ops.WriteFile(broken, "data\n", nil); r.Error != "" {
			t.Fatalf("host=%v broken link %s", host, r.Error)
		}
		if data, err := os.ReadFile(target); err != nil || string(data) != "data\n" {
			t.Fatalf("host=%v broken link target %q %v", host, data, err)
		}
		if st, _ := os.Lstat(broken); st.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("host=%v broken link replaced", host)
		}
	}
}

func TestFileOpsEnvNotReadyIsUnavailableNotMissing(t *testing.T) {
	dead := func() *shellFileOps {
		return newShellFileOps(remoteTestEnv(t), &fakeTermEnv{envType: "fake", cwd: "/workspace", execute: func(context.Context, string, ExecOptions) (*ExecResult, error) {
			return &ExecResult{Output: "Error: container is not running", ReturnCode: 125}, nil
		}}, "")
	}
	for name, r := range map[string]*ReadResult{
		"read_file":       dead().readFile("state/notes.md", 1, 2000),
		"read_file_raw":   dead().ReadFileRaw("state/notes.md"),
		"read_file_bytes": dead().readFileBytes("state/notes.md", 0),
	} {
		if r.Error == "" || strings.Contains(r.Error, "File not found") || !strings.Contains(strings.ToLower(r.Error), "environment unavailable") {
			t.Fatalf("%s gave %q", name, r.Error)
		}
	}
	healthy := func() *shellFileOps {
		return newShellFileOps(remoteTestEnv(t), &fakeTermEnv{envType: "fake", cwd: "/workspace", execute: func(_ context.Context, command string, _ ExecOptions) (*ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "if [ -f"):
				return &ExecResult{Output: "__icebath_missing__\n"}, nil
			case strings.HasPrefix(command, "test -e"):
				return &ExecResult{Output: "not_found\n"}, nil
			case strings.HasPrefix(command, "echo "):
				return &ExecResult{Output: strings.TrimSpace(command[5:]) + "\n"}, nil
			}
			return &ExecResult{ReturnCode: 1}, nil
		}}, "")
	}
	for name, r := range map[string]*ReadResult{
		"read_file":       healthy().readFile("state/notes.md", 1, 2000),
		"read_file_raw":   healthy().ReadFileRaw("state/notes.md"),
		"read_file_bytes": healthy().readFileBytes("state/notes.md", 0),
	} {
		if !strings.Contains(r.Error, "File not found") {
			t.Fatalf("%s gave %q", name, r.Error)
		}
	}
}

func TestFileOpsExecMapsStdinErrorToFailure(t *testing.T) {
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(func(string, ExecOptions) *ExecResult {
		return &ExecResult{Output: "child output\n[stdin write failed: boom]", StdinError: "boom"}
	}), "/tmp")
	in := "data"
	r := ops.execStdin("echo hi", "/tmp", 0, &in)
	if r.ExitCode != 1 || !strings.Contains(r.Stdout, "boom") {
		t.Fatalf("got %+v", r)
	}
}

func TestFileOpsExecNamesMissingWorkingDirectory(t *testing.T) {
	env := mockFileOpsEnv(func(string, ExecOptions) *ExecResult {
		return &ExecResult{Output: "bash: line 1: cd: /nope: No such file or directory\n", ReturnCode: 126}
	})
	env.envType = "docker"
	ops := newShellFileOps(remoteTestEnv(t), env, "/nope")
	env.cwd = ""
	r := ops.exec("true", "", 0)
	want := "working directory '/nope' does not exist on the active terminal backend (check terminal.cwd or the session cwd; " +
		"for container backends use a path inside the container, e.g. /workspace). bash: line 1: cd: /nope: No such file or directory"
	if r.CwdError != want || r.Stdout != want || r.ExitCode != 126 {
		t.Fatalf("got %+v", r)
	}
}

func TestFileOpsExecutionErrorPanicsLikeARaisedException(t *testing.T) {
	ops := newShellFileOps(remoteTestEnv(t), &fakeTermEnv{envType: "fake", execute: func(context.Context, string, ExecOptions) (*ExecResult, error) {
		return nil, errors.New("transport down")
	}}, "/w")
	defer func() {
		if r := recover(); fmt.Sprint(r) != "transport down" {
			t.Fatalf("recovered %v", r)
		}
	}()
	ops.exec("true", "", 0)
	t.Fatal("no panic")
}

func TestFileOpsJSONWriteRefusesOnlyIntroducedConstants(t *testing.T) {
	for _, token := range []string{"NaN", "Infinity", "-Infinity"} {
		ops, _, dir := hostFileOps(t, true)
		clean := writeTestFile(t, dir, "clean.json", []byte("{\"value\": 1}\n"))
		r := ops.WriteFile(clean, "{\"value\": "+token+"}\n", nil)
		if r.Error == "" || !strings.Contains(r.Error, token) {
			t.Fatalf("%s not refused %+v", token, r)
		}
		if data, _ := os.ReadFile(clean); string(data) != "{\"value\": 1}\n" {
			t.Fatalf("%s modified the file", token)
		}
		quoted := "{\"value\": [\"" + token + "\", 1.5, null]}\n"
		if r := ops.WriteFile(clean, quoted, nil); r.Error != "" {
			t.Fatalf("%s quoted refused %s", token, r.Error)
		}
		legacy := writeTestFile(t, dir, "legacy.json", []byte("{\"value\": "+token+", \"n\": 1}\n"))
		edited := "{\"value\": " + token + ", \"n\": 2}\n"
		if r := ops.WriteFile(legacy, edited, nil); r.Error != "" {
			t.Fatalf("%s legacy edit refused %s", token, r.Error)
		}
		if data, _ := os.ReadFile(legacy); string(data) != edited {
			t.Fatalf("%s legacy content %q", token, data)
		}
	}
}

func TestFileOpsNoTrailingNewlineTotalLines(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, _, dir := hostFileOps(t, native)
		target := writeTestFile(t, dir, "no_trailing.txt", []byte("line1\nline2\nline3"))
		first := ops.readFile(target, 1, 2)
		if !first.Truncated || !strings.Contains(first.Hint, "of 3 lines") {
			t.Fatalf("native=%v first %+v", native, first)
		}
		last := ops.readFile(target, 3, 2000)
		if last.Error != "" || last.Content != "3|line3" || last.TotalLines != 3 {
			t.Fatalf("native=%v last %+v", native, last)
		}
		terminated := writeTestFile(t, dir, "terminated.txt", []byte("a\nb\nc\n"))
		if n := ops.readFile(terminated, 1, 2000).TotalLines; n != 3 {
			t.Fatalf("native=%v terminated %d", native, n)
		}
		empty := writeTestFile(t, dir, "empty.txt", nil)
		if n := ops.readFile(empty, 1, 2000).TotalLines; n != 0 {
			t.Fatalf("native=%v empty %d", native, n)
		}
	}
	ops, _, dir := hostFileOps(t, true)
	target := writeTestFile(t, dir, "no_trailing.txt", []byte("line1\nline2\nline3"))
	if r := ops.readFileNative(target, 1, 2000); r.Error != "" || r.TotalLines != 3 {
		t.Fatalf("native direct %+v", r)
	}
	f := false
	if r := ops.assembleReadResult("a\nb\nc\n", 1, 2000, 2, 5, &f); r.TotalLines != 3 {
		t.Fatalf("proved %d", r.TotalLines)
	}
	if r := ops.assembleReadResult("a\nb\nc\n", 1, 2000, 2, 5, nil); r.TotalLines != 2 {
		t.Fatalf("unknown %d", r.TotalLines)
	}
}

func TestFileOpsDeleteFile(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, _, dir := hostFileOps(t, native)
		target := writeTestFile(t, dir, "doomed.txt", []byte("bye"))
		if r := ops.DeleteFile(target); r.Error != "" {
			t.Fatalf("native=%v %s", native, r.Error)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("native=%v file survived", native)
		}
		sub := filepath.Join(dir, "dir")
		os.Mkdir(sub, 0o755)
		writeTestFile(t, sub, "keep.txt", []byte("still here"))
		r := ops.DeleteFile(sub)
		if !strings.Contains(r.Error, "is a directory") {
			t.Fatalf("native=%v dir %q", native, r.Error)
		}
		if _, err := os.Stat(sub); err != nil {
			t.Fatalf("native=%v dir removed", native)
		}
		if r := ops.DeleteFile(filepath.Join(dir, "never.txt")); r.Error != "" {
			t.Fatalf("native=%v missing file %q", native, r.Error)
		}
	}
}

func TestFileOpsDeleteRefusalTextMatchesSnippet(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ops, _, dir := hostFileOps(t, true)
	ro := filepath.Join(dir, "ro")
	os.Mkdir(ro, 0o755)
	f := writeTestFile(t, ro, "f", []byte("x"))
	os.Chmod(ro, 0o555)
	defer os.Chmod(ro, 0o755)
	rel, _ := filepath.Rel(dir, f)
	native := ops.DeleteFile("./" + strings.ReplaceAll(rel, "/", "//"))
	t.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
	shell := ops.DeleteFile("./" + strings.ReplaceAll(rel, "/", "//"))
	want := "Failed to delete ./ro//f: [Errno 13] Permission denied: 'ro/f'"
	if native.Error != want {
		t.Fatalf("native %q", native.Error)
	}
	if hostPythonInterpreter() != "" && shell.Error != want {
		t.Fatalf("shell %q", shell.Error)
	}
}

func TestFileOpsDeleteAndMoveGuardTheEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix symlinks")
	}
	for _, op := range []string{"delete", "delete_trailing_slash", "delete_dot", "move"} {
		ops, _, dir := hostFileOps(t, true)
		home := filepath.Join(dir, "home")
		outside := writeTestFile(t, dir, "outside.txt", []byte("keep"))
		t.Setenv("HOME", home)
		os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
		entry := filepath.Join(home, ".ssh", "link")
		os.Symlink(outside, entry)
		moved := filepath.Join(home, "moved.txt")
		var r *WriteResult
		if op == "move" {
			r = ops.MoveFile(entry, moved)
		} else {
			r = ops.DeleteFile(entry + map[string]string{"delete_trailing_slash": "/", "delete_dot": "/."}[op])
		}
		if !strings.Contains(r.Error, "protected") {
			t.Fatalf("%s gave %q", op, r.Error)
		}
		if st, err := os.Lstat(entry); err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s removed the link", op)
		}
		if _, err := os.Lstat(moved); err == nil {
			t.Fatalf("%s moved the link", op)
		}
		for _, native := range []string{"1", "0"} {
			os.Setenv("ICEBATH_NATIVE_FILE_READ", native)
			plain := writeTestFile(t, home, "plain.txt", []byte("x"))
			os.Remove(moved)
			if op == "move" {
				r = ops.MoveFile(plain, moved)
			} else {
				r = ops.DeleteFile(plain + map[string]string{"delete_trailing_slash": "/", "delete_dot": "/."}[op])
			}
			if r.Error != "" {
				t.Errorf("%s native=%s plain file %q", op, native, r.Error)
				continue
			}
			if _, err := os.Lstat(plain); err == nil {
				t.Errorf("%s native=%s plain file survived", op, native)
			}
		}
		if data, _ := os.ReadFile(outside); string(data) != "keep" {
			t.Fatalf("%s touched the link target", op)
		}
	}
}

func TestFileOpsReadFileOneRoundTrip(t *testing.T) {
	ops, env, dir := hostFileOps(t, false)
	check := func(name string, wantCalls int) []string {
		t.Helper()
		calls := envCommands(env)
		if wantCalls >= 0 && len(calls) != wantCalls {
			t.Fatalf("%s made %d calls %q", name, len(calls), calls)
		}
		resetEnvCommands(env)
		return calls
	}
	r := ops.readFile(writeTestFile(t, dir, "a.txt", []byte("one\ntwo\nthree\n")), 1, 2000)
	calls := check("text", 1)
	if !strings.Contains(calls[0], "__ICEBATH_RF_") || r.Error != "" || r.Content != "1|one\n2|two\n3|three" || r.TotalLines != 3 || r.FileSize != 14 || r.Truncated {
		t.Fatalf("text %+v", r)
	}
	r = ops.readFile(writeTestFile(t, dir, "b.txt", []byte("a\nb")), 1, 2000)
	check("no trailing newline", 1)
	if r.Content != "1|a\n2|b" || r.TotalLines != 2 {
		t.Fatalf("no trailing %+v", r)
	}
	var ten []byte
	for i := 1; i <= 10; i++ {
		ten = append(ten, []byte(fmt.Sprintf("l%d\n", i))...)
	}
	r = ops.readFile(writeTestFile(t, dir, "c.txt", ten), 3, 2)
	check("window", 1)
	if r.Content != "3|l3\n4|l4" || !r.Truncated || r.TotalLines != 10 || !strings.Contains(r.Hint, "offset=5") {
		t.Fatalf("window %+v", r)
	}
	r = ops.readFile(writeTestFile(t, dir, "d.txt", ten[:15]), 50, 2000)
	check("past eof", 1)
	if r.Content != "" || r.Error != "" || !strings.Contains(r.Hint, "beyond the end") || !strings.Contains(r.Hint, "5") {
		t.Fatalf("past eof %+v", r)
	}
	r = ops.readFile(writeTestFile(t, dir, "e.txt", nil), 1, 2000)
	check("empty", 1)
	if r.Error != "" || r.Content != "" || r.TotalLines != 0 || !strings.Contains(r.Hint, "empty") {
		t.Fatalf("empty %+v", r)
	}
	r = ops.readFile(writeTestFile(t, dir, "f.txt", []byte("\xef\xbb\xbfhello\n")), 1, 2000)
	check("bom", 1)
	if r.Content != "1|hello" {
		t.Fatalf("bom %q", r.Content)
	}
	r = ops.readFile(writeTestFile(t, dir, "g.txt", []byte("x\r\ny\r\n")), 1, 2000)
	check("crlf", -1)
	if r.Content != "1|x\r\n2|y\r" {
		t.Fatalf("crlf %q", r.Content)
	}
	r = ops.readFile(writeTestFile(t, dir, "L.txt", append(bytes.Repeat([]byte("a"), 9000), []byte("\nshort\n")...)), 1, 2000)
	check("long line", 1)
	parts := strings.Split(r.Content, "\n")
	if len(parts) != 2 || !strings.HasSuffix(parts[0], "... [truncated]") || len(parts[0]) >= 9000 || parts[1] != "2|short" {
		t.Fatalf("long line %q", r.Content[:40])
	}
	writeTestFile(t, dir, "rel.txt", []byte("here\n"))
	if r := ops.readFile("rel.txt", 1, 2000); r.Error != "" || r.Content != "1|here" {
		t.Fatalf("relative %+v", r)
	}
	resetEnvCommands(env)
	lookalike := "__ICEBATH_RF_" + strings.Repeat("ab", 16) + "__"
	r = ops.readFile(writeTestFile(t, dir, "s.txt", []byte("x\n"+lookalike+"\ny\n")), 1, 2000)
	if r.Error != "" || r.TotalLines != 3 || r.Content != "1|x\n2|"+lookalike+"\n3|y" {
		t.Fatalf("lookalike %+v", r)
	}
}

func TestFileOpsReadFileNonTextPaths(t *testing.T) {
	ops, env, dir := hostFileOps(t, false)
	writeTestFile(t, dir, "notes.txt", []byte("x\n"))
	r := ops.readFile(filepath.Join(dir, "note.txt"), 1, 2000)
	calls := envCommands(env)
	if !strings.Contains(calls[0], "__ICEBATH_RF_") || !strings.Contains(r.Error, "File not found") {
		t.Fatalf("missing %+v", r)
	}
	found := false
	for _, s := range r.SimilarFiles {
		found = found || strings.HasSuffix(s, "notes.txt")
	}
	if !found {
		t.Fatalf("similar %v", r.SimilarFiles)
	}
	writeTestFile(t, dir, "it\xe2\x80\x99s.txt", []byte("accent\n"))
	r = ops.readFile(filepath.Join(dir, "it's.txt"), 1, 2000)
	if r.Error != "" || r.Content != "1|accent" || !strings.Contains(r.Hint, "unicode-equivalent") {
		t.Fatalf("variant %+v", r)
	}
	resetEnvCommands(env)
	r = ops.readFile(dir, 1, 2000)
	if n := len(envCommands(env)); n != 1 || !strings.Contains(r.Error, "not a regular file") {
		t.Fatalf("directory %d %+v", n, r)
	}
	resetEnvCommands(env)
	r = ops.readFile(writeTestFile(t, dir, "blob", append([]byte("\x00\x01\x02"), bytes.Repeat([]byte{0}, 50)...)), 1, 2000)
	calls = envCommands(env)
	if !strings.Contains(calls[0], "__ICEBATH_RF_") || !r.IsBinary || r.Error == "" {
		t.Fatalf("binary %+v", r)
	}
	for _, c := range calls[1:] {
		if strings.Contains(c, "head -c 1000") {
			t.Fatalf("second sample %q", c)
		}
	}
	resetEnvCommands(env)
	r = ops.readFile(writeTestFile(t, dir, "p.png", []byte("\x89PNG\r\n")), 1, 2000)
	calls = envCommands(env)
	if len(calls) != 1 || strings.Contains(calls[0], "__ICEBATH_RF_") || !r.IsImage || r.FileSize != 6 {
		t.Fatalf("image %q %+v", calls, r)
	}
}

func TestFileOpsWriteFileRoundTrips(t *testing.T) {
	ops, env, dir := hostFileOps(t, false)
	calls := func() []string { c := envCommands(env); resetEnvCommands(env); return c }
	p := filepath.Join(dir, "new.txt")
	r := ops.WriteFile(p, "line one\nline two\n", nil)
	if c := calls(); r.Error != "" || r.Verified == nil || !*r.Verified || len(c) != 3 {
		t.Fatalf("new file %+v %q", r, c)
	}
	if data, _ := os.ReadFile(p); string(data) != "line one\nline two\n" {
		t.Fatalf("new content %q", data)
	}
	crlf := writeTestFile(t, dir, "crlf.txt", []byte("a\r\nb\r\n"))
	if r := ops.WriteFile(crlf, "x\ny\n", nil); r.Error != "" || len(calls()) != 3 {
		t.Fatalf("crlf %+v", r)
	}
	if data, _ := os.ReadFile(crlf); string(data) != "x\r\ny\r\n" {
		t.Fatalf("crlf content %q", data)
	}
	bom := writeTestFile(t, dir, "bom.txt", []byte("\xef\xbb\xbfold\n"))
	if r := ops.WriteFile(bom, "new\n", nil); r.Error != "" || len(calls()) != 3 {
		t.Fatalf("bom %+v", r)
	}
	if data, _ := os.ReadFile(bom); string(data) != "\xef\xbb\xbfnew\n" {
		t.Fatalf("bom content %q", data)
	}
	code := writeTestFile(t, dir, "code.py", []byte("x = 1\r\ny = 2\r\n"))
	if r := ops.WriteFile(code, "x = 1\ny = 3\n", nil); r.Error != "" {
		t.Fatalf("code %+v", r)
	}
	var probes []string
	for _, c := range calls() {
		if strings.Contains(c, "__ICEBATH_WF_") {
			probes = append(probes, c)
		}
		if strings.HasPrefix(c, "cat ") {
			t.Fatalf("separate cat %q", c)
		}
	}
	if len(probes) != 1 || !strings.Contains(probes[0], "cat ") {
		t.Fatalf("probes %q", probes)
	}
	if data, _ := os.ReadFile(code); string(data) != "x = 1\r\ny = 3\r\n" {
		t.Fatalf("code content %q", data)
	}
	deep := filepath.Join(dir, "deep", "er", "new.md")
	if r := ops.WriteFile(deep, "hi\n", nil); r.Error != "" || !r.DirsCreated || len(calls()) != 3 {
		t.Fatalf("deep %+v", r)
	}
	if data, _ := os.ReadFile(deep); string(data) != "hi\n" {
		t.Fatalf("deep content %q", data)
	}
}

func TestFileOpsWriteProbeGarbledFallsBackToSequential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
	env := &fakeTermEnv{envType: "local", cwd: dir, execute: bashExecute(false, func(command string) (*ExecResult, bool) {
		if strings.Contains(command, "__ICEBATH_WF_") {
			return &ExecResult{Output: "[Command timed out after 1s]\n", ReturnCode: 124}, true
		}
		return nil, false
	})}
	ops := newShellFileOps(remoteTestEnv(t), env, dir)
	p := writeTestFile(t, dir, "crlf.txt", []byte("a\r\nb\r\n"))
	if r := ops.WriteFile(p, "x\ny\n", nil); r.Error != "" {
		t.Fatalf("%+v", r)
	}
	if data, _ := os.ReadFile(p); string(data) != "x\r\ny\r\n" {
		t.Fatalf("content %q", data)
	}
}

func TestFileOpsReadProbeGarbledFallsBackToSequential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
	env := &fakeTermEnv{envType: "local", cwd: dir, execute: bashExecute(false, func(command string) (*ExecResult, bool) {
		if strings.Contains(command, "__ICEBATH_RF_") {
			return &ExecResult{Output: "[Command timed out after 1s]\n", ReturnCode: 124}, true
		}
		return nil, false
	})}
	ops := newShellFileOps(remoteTestEnv(t), env, dir)
	r := ops.readFile(writeTestFile(t, dir, "a.txt", []byte("one\ntwo\n")), 1, 2000)
	if r.Error != "" || r.Content != "1|one\n2|two" || r.TotalLines != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestFileOpsHeredocStdinBackendsAreByteExact(t *testing.T) {
	for _, content := range []string{"no trailing newline", "one trailing newline\n", "blank tail\n\n\n", "q ' \" ) ( $(x) `y` ${z} \\\ncontinued\\"} {
		dir := t.TempDir()
		t.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
		env := &fakeTermEnv{envType: "local", cwd: dir, execute: bashExecute(true, nil)}
		ops := newShellFileOps(remoteTestEnv(t), env, dir)
		p := writeTestFile(t, dir, "existing.txt", []byte("keep me\n"))
		r := ops.WriteFile(p, content, nil)
		if data, _ := os.ReadFile(p); string(data) != content || r.Error != "" || r.Verified == nil || !*r.Verified {
			t.Fatalf("%q gave %q %+v", content, data, r)
		}
	}
	dir := t.TempDir()
	env := &fakeTermEnv{envType: "local", cwd: dir, execute: bashExecute(true, nil)}
	ops := newShellFileOps(remoteTestEnv(t), env, dir)
	p := writeTestFile(t, dir, "notes.txt", []byte("line one\nline two\nimportant data"))
	r := ops.patchReplace(p, "line two", "line 2", false, nil)
	if data, _ := os.ReadFile(p); string(data) != "line one\nline 2\nimportant data" || !r.Success {
		t.Fatalf("patch %q %+v", data, r)
	}
}

func TestFileOpsNativeRead(t *testing.T) {
	ops, env, dir := hostFileOps(t, true)
	r := ops.readFile(writeTestFile(t, dir, "a.txt", []byte("one\ntwo\n")), 1, 2000)
	if c := envCommands(env); len(c) != 0 || r.Error != "" || r.Content != "1|one\n2|two" || r.TotalLines != 2 || r.FileSize != 8 {
		t.Fatalf("native %q %+v", c, r)
	}
	if _, ok := readResultSnapshot(r); !ok {
		t.Fatal("native read attached no snapshot")
	}
	t.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
	resetEnvCommands(env)
	ops.readFile(filepath.Join(dir, "a.txt"), 1, 2000)
	if c := envCommands(env); len(c) != 1 || !strings.Contains(c[0], "__ICEBATH_RF_") {
		t.Fatalf("kill switch %q", c)
	}
	if newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(nil), "").nativeReadEnabled() {
		t.Fatal("non local env took the native path")
	}
}

func TestFileOpsTildeAndInjectionThroughNativeLane(t *testing.T) {
	ops, env, dir := hostFileOps(t, true)
	t.Setenv("HOME", dir)
	r := ops.readFile("~/icebath-no-such-file-7f3a.txt", 1, 2000)
	if c := envCommands(env); len(c) == 0 || c[0] != "echo $HOME" || !strings.Contains(r.Error, "File not found") {
		t.Fatalf("tilde %q %+v", c, r)
	}
	resetEnvCommands(env)
	marker := filepath.Join(dir, "pwned")
	r = ops.readFile("~; echo PWNED > "+marker, 1, 2000)
	if _, err := os.Stat(marker); r.Error == "" || err == nil {
		t.Fatalf("injection ran %+v", r)
	}
	for _, c := range envCommands(env) {
		if c != "echo $HOME" && !strings.HasPrefix(c, "ls -1 '~; echo PWNED") {
			t.Fatalf("unexpected command %q", c)
		}
	}
}

func TestFileOpsNativeReadParity(t *testing.T) {
	const chunk = 1 << 20
	const clamp = 8001
	var ten, five, q []byte
	for i := 1; i <= 10; i++ {
		ten = append(ten, []byte(fmt.Sprintf("l%d\n", i))...)
	}
	for i := 1; i <= 5; i++ {
		five = append(five, []byte(fmt.Sprintf("l%d\n", i))...)
	}
	q = append(bytes.Repeat([]byte("q\n"), chunk/2+3), []byte("target1\ntarget2\n")...)
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	rep := func(s string, n int) []byte { return bytes.Repeat([]byte(s), n) }
	cases := []struct {
		name          string
		data          []byte
		offset, limit int
	}{
		{"plain", []byte("one\ntwo\nthree\n"), 1, 2000},
		{"no_trailing_newline", []byte("a\nb"), 1, 2000},
		{"blank_tail", []byte("a\n\n"), 1, 2000},
		{"crlf", []byte("x\r\ny\r\n"), 1, 2000},
		{"lone_cr", []byte("a\rb\n"), 1, 2000},
		{"bom", []byte("\xef\xbb\xbfhello\n"), 1, 2000},
		{"empty", nil, 1, 2000},
		{"single_no_newline", []byte("solo"), 1, 2000},
		{"only_newline", []byte("\n"), 1, 2000},
		{"unicode", []byte("héllo wörld\n汉字\n"), 1, 2000},
		{"long_line", cat(rep("a", 9000), []byte("\nshort\n")), 1, 2000},
		{"multibyte_long_line", cat(rep("汉", 4000), []byte("\nx\n")), 1, 2000},
		{"multi_chunk_line", cat(rep("b", 3000000), []byte("\nz\n")), 1, 2000},
		{"newline_last_byte_of_chunk", cat(rep("a", chunk-1), []byte("\nsecond\n")), 1, 2000},
		{"newline_first_byte_of_next_chunk", cat(rep("a", chunk), []byte("\nsecond\n")), 1, 2000},
		{"line_spans_three_boundaries", cat([]byte("p\n"), rep("b", 3*chunk+5), []byte("\nz\n")), 1, 2000},
		{"clamp_fills_on_boundary", cat(rep("x", chunk-clamp-1), []byte("\n"), rep("c", 20000), []byte("\ntail\n")), 2, 3},
		{"clamp_fills_after_boundary", cat(rep("x", chunk-clamp), []byte("\n"), rep("c", 20000), []byte("\ntail\n")), 2, 3},
		{"eof_midline_on_boundary", cat([]byte("l1\n"), rep("d", 2*chunk-3)), 1, 2000},
		{"blank_lines_on_boundary", cat(rep("e", chunk-2), []byte("\n\n\n"), []byte("f\n")), 1, 2000},
		{"crlf_split_on_boundary", cat(rep("r", chunk-1), []byte("\r\n"), []byte("s\r\n")), 1, 2000},
		{"page_starts_in_second_chunk", q, chunk/2 + 3, 4},
		{"window", ten, 3, 2},
		{"window_reaches_eof", ten, 9, 5},
		{"past_eof", five, 50, 2000},
		{"nul_binary", rep("\x00\x01\x02", 20), 1, 2000},
		{"latin1_tail", []byte("caf\xe9\n"), 1, 2000},
		{"sentinel_lookalike", cat([]byte("x\n__ICEBATH_RF_"), rep("ab", 16), []byte("__\ny\n")), 1, 2000},
	}
	ops, env, dir := hostFileOps(t, true)
	for _, c := range cases {
		p := writeTestFile(t, dir, c.name+".txt", c.data)
		os.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
		viaShell := readResultJSON(t, ops.readFile(p, c.offset, c.limit))
		if calls := envCommands(env); len(calls) == 0 || !strings.Contains(calls[0], "__ICEBATH_RF_") {
			t.Fatalf("%s shell lane %q", c.name, calls)
		}
		resetEnvCommands(env)
		os.Setenv("ICEBATH_NATIVE_FILE_READ", "1")
		viaNative := readResultJSON(t, ops.readFile(p, c.offset, c.limit))
		if viaNative != viaShell {
			t.Fatalf("%s\nnative %s\nshell  %s", c.name, trimForLog(viaNative), trimForLog(viaShell))
		}
		for _, cmd := range envCommands(env) {
			if strings.Contains(cmd, "__ICEBATH_RF_") {
				t.Fatalf("%s native lane ran the read probe", c.name)
			}
		}
		resetEnvCommands(env)
	}
}

// Head and tail of a long JSON string for failure logs
func trimForLog(s string) string {
	if len(s) <= 400 {
		return s
	}
	return s[:200] + " ... " + s[len(s)-200:]
}

func TestFileOpsNativeReadSpecialPathsAgree(t *testing.T) {
	ops, env, dir := hostFileOps(t, true)
	real := writeTestFile(t, dir, "real.txt", []byte("target\n"))
	os.Symlink(real, filepath.Join(dir, "link.txt"))
	os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "dangling.txt"))
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	writeTestFile(t, dir, "pic.png", []byte("\x89PNG\r\n"))
	writeTestFile(t, dir, "notes.txt", []byte("n\n"))
	for _, p := range []string{filepath.Join(dir, "link.txt"), filepath.Join(dir, "dangling.txt"), filepath.Join(dir, "sub"), filepath.Join(dir, "pic.png"), filepath.Join(dir, "note.txt"), "real.txt"} {
		os.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
		viaShell := readResultJSON(t, ops.readFile(p, 1, 2000))
		os.Setenv("ICEBATH_NATIVE_FILE_READ", "1")
		resetEnvCommands(env)
		viaNative := readResultJSON(t, ops.readFile(p, 1, 2000))
		if viaNative != viaShell {
			t.Fatalf("%s\nnative %s\nshell  %s", p, viaNative, viaShell)
		}
		for _, cmd := range envCommands(env) {
			if strings.Contains(cmd, "__ICEBATH_RF_") {
				t.Fatalf("%s native lane ran the read probe", p)
			}
		}
	}
}

func TestFileOpsReadFileBinaryClassification(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, _, dir := hostFileOps(t, native)
		r := ops.readFile(writeTestFile(t, dir, "cjk.md", []byte(strings.Repeat("漢字テキスト", 200))), 1, 2000)
		if r.IsBinary || r.Error != "" || !strings.Contains(r.Content, "漢字テキスト") {
			t.Fatalf("native=%v cjk %+v", native, r.Error)
		}
		r = ops.readFile(writeTestFile(t, dir, "cyr.md", []byte("x"+strings.Repeat("Привет мир\n", 100))), 1, 2000)
		if r.IsBinary || r.Error != "" {
			t.Fatalf("native=%v cyrillic %+v", native, r.Error)
		}
		r = ops.readFile(writeTestFile(t, dir, "bom.txt", []byte("\xef\xbb\xbf"+strings.Repeat("Привет мир\n", 100))), 1, 2000)
		if r.IsBinary || r.Error != "" || !strings.Contains(r.Content, "Привет") {
			t.Fatalf("native=%v bom %+v", native, r.Error)
		}
		if r := ops.readFile(writeTestFile(t, dir, "empty.txt", nil), 1, 2000); r.IsBinary || r.Error != "" {
			t.Fatalf("native=%v empty %+v", native, r)
		}
		png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x80, 0x01, 0xff, 0x00}, 512)...)
		if r := ops.readFile(writeTestFile(t, dir, "blob.dat", png), 1, 2000); !r.IsBinary {
			t.Fatalf("native=%v png %+v", native, r)
		}
		elf := cat3([]byte("\x7fELF\x02\x01\x01\x00"), bytes.Repeat([]byte{0}, 64), []byte("code"))
		if r := ops.readFile(writeTestFile(t, dir, "a.out.dat", elf), 1, 2000); !r.IsBinary {
			t.Fatalf("native=%v elf %+v", native, r)
		}
		if r := ops.readFile(writeTestFile(t, dir, "nul.txt", append([]byte("hello\x00world"), bytes.Repeat([]byte("a"), 128)...)), 1, 2000); !r.IsBinary {
			t.Fatalf("native=%v nul %+v", native, r)
		}
	}
}

// Concatenation of three byte slices
func cat3(a, b, c []byte) []byte { return append(append(append([]byte{}, a...), b...), c...) }

// UTF-16 encoding of ASCII text in the requested byte order
func utf16ASCII(s string, bigEndian bool) []byte {
	var out []byte
	for _, c := range []byte(s) {
		if bigEndian {
			out = append(out, 0, c)
		} else {
			out = append(out, c, 0)
		}
	}
	return out
}

func TestFileOpsUTF16TranscodesToReadableText(t *testing.T) {
	for _, native := range []bool{true, false} {
		if !native && hostPythonInterpreter() == "" {
			continue
		}
		for _, be := range []bool{false, true} {
			ops, _, dir := hostFileOps(t, native)
			r := ops.readFile(writeTestFile(t, dir, "u16.txt", utf16ASCII(strings.Repeat("hello world\n", 50), be)), 1, 2000)
			if r.Error != "" || r.IsBinary || !strings.Contains(r.Content, "hello world") || !strings.Contains(strings.ToLower(r.Hint), "utf-16") {
				t.Fatalf("native=%v be=%v %+v", native, be, r)
			}
		}
	}
}

func TestFileOpsUTF16NativeMatchesSnippet(t *testing.T) {
	if hostPythonInterpreter() == "" {
		t.Skip("no host python for the backend snippet")
	}
	samples := map[string][]byte{
		"bom_le":      cat3([]byte{0xff, 0xfe}, utf16ASCII("a\r\nb\nc", false), []byte{0x00, 0xd8, 0x41}),
		"bom_be":      cat3([]byte{0xfe, 0xff}, utf16ASCII("x\ny\n", true), []byte{0xdc, 0x00}),
		"pairs_le":    cat3([]byte{0xff, 0xfe}, cat3(utf16ASCII("line\n", false), []byte{0x00, 0xd8, 0x00, 0xd8, 0x00, 0xdc}, nil), utf16ASCII("\ntail", false)),
		"window":      utf16ASCII("1\n2\n3\n4\n5\n6", false),
		"odd_tail":    append(utf16ASCII("abc\n", false), 'Z'),
		"replace_mid": cat3([]byte{0xfe, 0xff}, cat3(utf16ASCII("ok\n", true), []byte{0xdc, 0x00, 0x00, 0x41}, nil), utf16ASCII("\nend", true)),
		"not_utf16":   []byte("plain bytes \x00\x00 mixed \x00 x\x00"),
	}
	for name, data := range samples {
		for _, window := range [][2]int{{1, 2000}, {2, 2}, {5, 10}} {
			ops, _, dir := hostFileOps(t, true)
			p := writeTestFile(t, dir, name+".txt", data)
			nativePage, nativeTotal, nativeEnc, nok := ops.utf16PageNative(p, window[0], window[1])
			shellPage, shellTotal, shellEnc, sok := ops.utf16PageShell(p, window[0], window[1])
			if nok != sok || nativePage != shellPage || nativeTotal != shellTotal || nativeEnc != shellEnc {
				t.Fatalf("%s %v native %q %d %s shell %q %d %s", name, window, nativePage, nativeTotal, nativeEnc, shellPage, shellTotal, shellEnc)
			}
		}
	}
}

func TestFileOpsReadFileRawAndPatchOnBoundaryCutCJK(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, _, dir := hostFileOps(t, native)
		raw := ops.ReadFileRaw(writeTestFile(t, dir, "cjk.md", []byte(strings.Repeat("漢字テキスト", 200))))
		if raw.IsBinary || raw.Error != "" {
			t.Fatalf("native=%v raw %+v", native, raw.Error)
		}
		p := writeTestFile(t, dir, "doc.md", []byte(strings.Repeat("漢字テキスト", 200)+"\nEND-MARKER\n"))
		r := ops.patchReplace(p, "END-MARKER", "END-PATCHED", false, nil)
		if data, _ := os.ReadFile(p); !r.Success || !strings.Contains(string(data), "END-PATCHED") {
			t.Fatalf("native=%v patch %+v", native, r)
		}
	}
}

func TestFileOpsReadFileRawPreservesUndecodableBytesAndSha(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, _, dir := hostFileOps(t, native)
		body := "head\n" + strings.Repeat("a", 1000) + "\n\xff\xfe tail\n"
		data := []byte("\xef\xbb\xbf" + body)
		r := ops.ReadFileRaw(writeTestFile(t, dir, "mixed.txt", data))
		if r.Error != "" || r.Content != pystr.SurrogateEscapeDecode([]byte(body)) || r.ContentSha256 == nil || *r.ContentSha256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
			t.Fatalf("native=%v %+v", native, r)
		}
		if encoded, err := pystr.SurrogateEscapeEncode(r.Content); err != nil || string(encoded) != body {
			t.Fatalf("native=%v round trip %q %v", native, encoded, err)
		}
	}
}

func TestFileOpsWriteFileSurrogateEscapeRoundTrips(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, _, dir := hostFileOps(t, native)
		p := filepath.Join(dir, "surrogate.bin")
		r := ops.WriteFile(p, pystr.SurrogateEscapeDecode([]byte{0xff, 0x00, 0xfe}), nil)
		if r.Error != "" || r.BytesWritten != 3 || r.Verified == nil || !*r.Verified {
			t.Fatalf("native=%v %+v", native, r)
		}
		if data, _ := os.ReadFile(p); !bytes.Equal(data, []byte{0xff, 0x00, 0xfe}) {
			t.Fatalf("native=%v bytes %q", native, data)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, ".icebath-tmp*")); len(left) != 0 {
			t.Fatalf("native=%v temp left %v", native, left)
		}
		mixed := filepath.Join(dir, "mixed.bin")
		r = ops.WriteFile(mixed, "head\n"+pystr.SurrogateEscapeDecode([]byte{0xff})+"\ntail\n", nil)
		if data, _ := os.ReadFile(mixed); r.Error != "" || r.Verified == nil || !*r.Verified || string(data) != "head\n\xff\ntail\n" {
			t.Fatalf("native=%v mixed %+v %q", native, r, data)
		}
		normal := filepath.Join(dir, "normal.txt")
		r = ops.WriteFile(normal, "hello\nworld\n", nil)
		if data, _ := os.ReadFile(normal); r.Error != "" || r.Verified == nil || !*r.Verified || string(data) != "hello\nworld\n" {
			t.Fatalf("native=%v normal %+v", native, r)
		}
	}
}

func TestFileOpsFailClosedSyntaxGate(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, _, dir := hostFileOps(t, native)
		target := filepath.Join(dir, "config.json")
		r := ops.WriteFile(target, `{"a": 1,`, nil)
		if r.Error == "" || !strings.Contains(strings.ToLower(r.Error), "json") {
			t.Fatalf("native=%v invalid json %+v", native, r)
		}
		if _, err := os.Stat(target); err == nil {
			t.Fatalf("native=%v invalid json written", native)
		}
		want := "Refusing to write '" + target + "': candidate content fails .json syntax validation " +
			"(JSONDecodeError: Expecting property name enclosed in double quotes (line 1, column 9)). The file was NOT created or modified. Fix the content and retry."
		if r.Error != want {
			t.Fatalf("native=%v message %q", native, r.Error)
		}
		content := `{"a": 1, "b": [1, 2, 3]}`
		if r := ops.WriteFile(target, content, nil); r.Error != "" {
			t.Fatalf("native=%v valid json %+v", native, r)
		}
		if data, _ := os.ReadFile(target); string(data) != content {
			t.Fatalf("native=%v valid content %q", native, data)
		}
		broken := filepath.Join(dir, "broken.py")
		bad := "def foo(:\n    pass\n"
		r = ops.WriteFile(broken, bad, nil)
		if r.Error != "" || r.Lint == nil || r.Lint.Vals["status"] != "error" || !strings.Contains(fmt.Sprint(r.Lint.Vals["output"]), "SyntaxError") {
			t.Fatalf("native=%v python %+v", native, r)
		}
		if data, _ := os.ReadFile(broken); string(data) != bad {
			t.Fatalf("native=%v python content %q", native, data)
		}
	}
}

func TestFileOpsFailClosedSyntaxGateYAMLAndTOML(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, _, dir := hostFileOps(t, native)
		tagged := filepath.Join(dir, "template.yaml")
		content := "Resources:\n" +
			"  Bucket:\n" +
			"    Type: AWS::S3::Bucket\n" +
			"    Properties:\n" +
			"      BucketName: !Sub '${AWS::StackName}-bucket'\n"
		if r := ops.WriteFile(tagged, content, nil); r.Error != "" {
			t.Fatalf("native=%v tagged yaml %+v", native, r)
		}
		if data, _ := os.ReadFile(tagged); string(data) != content {
			t.Fatalf("native=%v tagged yaml content %q", native, data)
		}
		bad := filepath.Join(dir, "bad.yaml")
		r := ops.WriteFile(bad, "key: value\n  bad: indent\n", nil)
		want := "Refusing to write '" + bad + "': candidate content fails .yaml syntax validation " +
			"(YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 2, column 6). " +
			"The file was NOT created or modified. Fix the content and retry."
		if r.Error != want {
			t.Fatalf("native=%v yaml refusal %q", native, r.Error)
		}
		if _, err := os.Stat(bad); err == nil {
			t.Fatalf("native=%v invalid yaml written", native)
		}
		toml := filepath.Join(dir, "b.toml")
		r = ops.WriteFile(toml, "[section\nk = \"v\"", nil)
		if !strings.Contains(r.Error, "(TOMLDecodeError: Expected ']' at the end of a table declaration (at line 1, column 9))") {
			t.Fatalf("native=%v toml refusal %q", native, r.Error)
		}
		if _, err := os.Stat(toml); err == nil {
			t.Fatalf("native=%v invalid toml written", native)
		}
	}
}

func TestLintCheckLintTOMLAndYAMLInproc(t *testing.T) {
	ops := newShellFileOps(nil, mockFileOpsEnv(nil), "/")
	content := "[section\nk = \"v\""
	r := ops.checkLint("/tmp/b.toml", &content)
	if r.Success || r.Output != "TOMLDecodeError: Expected ']' at the end of a table declaration (at line 1, column 9)" {
		t.Fatalf("%+v", r)
	}
	flow := "a: [1, 2\nb: 3\n"
	r = ops.checkLint("/tmp/c.yml", &flow)
	want := "YAMLError: while parsing a flow sequence\n  in \"<unicode string>\", line 1, column 4\n" +
		"did not find expected ',' or ']'\n  in \"<unicode string>\", line 2, column 2"
	if r.Success || r.Output != want {
		t.Fatalf("%+v", r)
	}
	ok := "a: !vault |\n  secret\n"
	if r := ops.checkLint("/tmp/d.yaml", &ok); !r.Success || r.Skipped {
		t.Fatalf("%+v", r)
	}
}

func TestFileOpsPatchReplaceResultShape(t *testing.T) {
	ops, _, dir := hostFileOps(t, true)
	p := writeTestFile(t, dir, "notes.txt", []byte("alpha\r\nbeta\r\n"))
	r := ops.patchReplace(p, "beta", "gamma delta epsilon", false, nil)
	if !r.Success || r.Error != "" || len(r.FilesModified) != 1 || r.FilesModified[0] != p || len(r.Writes) != 1 {
		t.Fatalf("%+v", r)
	}
	if data, _ := os.ReadFile(p); string(data) != "alpha\r\ngamma delta epsilon\r\n" {
		t.Fatalf("content %q", data)
	}
	if !strings.Contains(r.Diff, "--- a/"+p) || !strings.Contains(r.Diff, "+gamma delta epsilon") {
		t.Fatalf("diff %q", r.Diff)
	}
	if !r.Writes[0].readKnown || !r.Writes[0].writtenKnown || r.Writes[0].path != p {
		t.Fatalf("writes %+v", r.Writes)
	}
	again := ops.patchReplace(p, "zzz missing", "gamma delta epsilon", false, nil)
	if again.Error != "" || !again.Success || !again.NoChange {
		t.Fatalf("already applied %+v", again)
	}
	miss := ops.patchReplace(p, "nowhere to be found", "replacement text", false, nil)
	if miss.Success || !strings.HasPrefix(miss.Error, "Could not find a match for old_string") {
		t.Fatalf("miss %+v", miss)
	}
}

func TestFileOpsPatchV4AThroughBackend(t *testing.T) {
	ops, _, dir := hostFileOps(t, true)
	p := writeTestFile(t, dir, "v.txt", []byte("one\ntwo\nthree\n"))
	probe := &editProbe{}
	patch := "*** Begin Patch\n*** Update File: " + p + "\n@@\n one\n-two\n+TWO\n three\n*** End Patch"
	r := ops.patchV4A(patch, probe)
	if !r.Success || r.Error != "" {
		t.Fatalf("%+v", r)
	}
	if data, _ := os.ReadFile(p); string(data) != "one\nTWO\nthree\n" {
		t.Fatalf("content %q", data)
	}
	if bad := ops.patchV4A("not a patch", probe); !strings.HasPrefix(bad.Error, "Failed to parse patch: ") && !strings.Contains(bad.Error, "validation failed") {
		t.Fatalf("bad patch %+v", bad)
	}
	if got := v4aProbe(probedShellFileOps{shellFileOps: ops, probe: probe}); got != probe {
		t.Fatal("probe not visible to the applier")
	}
}

func TestFileOpsReadFileBytes(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, _, dir := hostFileOps(t, native)
		data := []byte{0x00, 0xff, 0x10, 'a'}
		p := writeTestFile(t, dir, "b.bin", data)
		r := ops.readFileBytes(p, 0)
		if r.Error != "" || !r.IsBinary || r.FileSize != 4 || r.Base64Content != base64.StdEncoding.EncodeToString(data) {
			t.Fatalf("native=%v %+v", native, r)
		}
		big := ops.readFileBytes(p, 3)
		if big.Error != "File is too large (4 bytes, limit is 3)" || big.FileSize != 4 {
			t.Fatalf("native=%v limit %+v", native, big)
		}
		if missing := ops.readFileBytes(filepath.Join(dir, "nope.bin"), 0); !missing.NotFound || missing.Error != "File not found: "+filepath.Join(dir, "nope.bin") {
			t.Fatalf("native=%v missing %+v", native, missing)
		}
	}
}

func TestFileOpsDescribeBinaryFile(t *testing.T) {
	cases := map[string]struct {
		sample []byte
		size   int64
	}{
		"Binary file (PNG image data, 4.1 KB) \xe2\x80\x94 cannot display as text.":                       {[]byte("\x89PNG\r\n\x1a\nrest"), 4200},
		"Binary file (unknown binary, 12 bytes) \xe2\x80\x94 cannot display as text.":                     {[]byte{1, 2, 3}, 12},
		"Binary file (ISO media container (MP4/MOV family), 2.0 MB) \xe2\x80\x94 cannot display as text.": {[]byte("\x00\x00\x00\x18ftypmp42"), 2 * 1024 * 1024},
		"Binary file (unknown binary, 1.0 KB) \xe2\x80\x94 cannot display as text.":                       {[]byte("\x00\x00\x00\x18moov"), 1024},
	}
	for want, c := range cases {
		if got := describeBinaryFile(c.sample, c.size); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}

func TestFileOpsSuggestSimilarFilesScoring(t *testing.T) {
	ops, _, dir := hostFileOps(t, false)
	for _, n := range []string{"config.yml", "AGENTS.md", "configuration.txt", "unrelated.bin"} {
		writeTestFile(t, dir, n, []byte("x"))
	}
	r := ops.suggestSimilarFiles(filepath.Join(dir, "config.yaml"))
	if len(r.SimilarFiles) == 0 || r.SimilarFiles[0] != filepath.Join(dir, "config.yml") {
		t.Fatalf("config %v", r.SimilarFiles)
	}
	r = ops.suggestSimilarFiles(filepath.Join(dir, "AGENT.md"))
	if len(r.SimilarFiles) == 0 || r.SimilarFiles[0] != filepath.Join(dir, "AGENTS.md") {
		t.Fatalf("agent %v", r.SimilarFiles)
	}
	if !r.NotFound || r.Error != "File not found: "+filepath.Join(dir, "AGENT.md") {
		t.Fatalf("result %+v", r)
	}
}

func TestFileOpsPyLowerAndSplitext(t *testing.T) {
	if got := pyLower("ΟΔΟΣ ΑΣ"); got != "οδος ας" {
		t.Fatalf("final sigma %q", got)
	}
	if got := pyLower("\xc4\xb0x"); got != "i\xcc\x87x" {
		t.Fatalf("dotted capital %q", got)
	}
	for in, want := range map[string]string{"a/b.TXT": ".TXT", ".bashrc": "", "a/.b.c": ".c", "a.b/c": "", "x..": ".", "...": ""} {
		if got := pySplitextExt(in); got != want {
			t.Fatalf("%s gave %q", in, got)
		}
	}
}

func TestFileOpsPyRealpathFollowsDanglingLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix symlinks")
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.Symlink(filepath.Join(dir, "missing", "target.txt"), filepath.Join(dir, "dangling"))
	if got := pyRealpath(filepath.Join(dir, "dangling")); got != filepath.Join(dir, "missing", "target.txt") {
		t.Fatalf("dangling %q", got)
	}
	os.Symlink("loop", filepath.Join(dir, "loop"))
	if got := pyRealpath(filepath.Join(dir, "loop")); got != filepath.Join(dir, "loop") {
		t.Fatalf("loop %q", got)
	}
}

func TestFileOpsNativeAtomicWriteErrorText(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("posix permissions as non root")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	os.Mkdir(ro, 0o555)
	defer os.Chmod(ro, 0o755)
	target := filepath.Join(ro, "f.txt")
	msg := nativeAtomicWrite(target, []byte("x"))
	if !strings.HasPrefix(msg, "Permission denied: "+ro+"/.icebath-tmp.") {
		t.Fatalf("got %q", msg)
	}
	file := writeTestFile(t, dir, "plain", []byte("x"))
	if msg := nativeAtomicWrite(filepath.Join(file, "child"), []byte("x")); msg != "File exists: "+file {
		t.Fatalf("parent file %q", msg)
	}
}

// Real child copying its stdin to a file byte for byte
func surrogateCatProc(t *testing.T, out string, data *string) *ExecProcess {
	t.Helper()
	requireBash(t)
	p, err := StartExecProcess(exec.Command("bash", "-c", "cat > "+shQuote(out)), data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, done := p.Poll(); !done {
			p.Kill()
		}
	})
	return p
}

// Exit code within five seconds, killing a hung child so it cannot leak into the next test
func surrogateWaitOrKill(t *testing.T, p *ExecProcess) int {
	t.Helper()
	code, err := p.Wait(5)
	if err != nil {
		p.Kill()
		t.Fatalf("child did not exit: %v", err)
	}
	return code
}

func TestPipeStdinRoundTripsSurrogateEscapeBytes(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.bin")
	content := pystr.SurrogateEscapeDecode([]byte{0xff, 0x00, 0xfe})
	p := surrogateCatProc(t, out, &content)
	if code := surrogateWaitOrKill(t, p); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if data, _ := os.ReadFile(out); !bytes.Equal(data, []byte{0xff, 0x00, 0xfe}) {
		t.Fatalf("bytes %q", data)
	}
	if e := p.JoinStdin(5 * time.Second); e != "" {
		t.Fatalf("stdin error %q", e)
	}
}

func TestPipeStdinUnencodableSurrogateCapturesErrorAndClosesStdin(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.bin")
	content := "\xed\xa0\x80"
	p := surrogateCatProc(t, out, &content)
	if code := surrogateWaitOrKill(t, p); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if e := p.JoinStdin(5 * time.Second); e != "'utf-8' codec can't encode character '\\ud800' in position 0: surrogates not allowed" {
		t.Fatalf("stdin error %q", e)
	}
}

func TestPipeStdinNormalContentUnchanged(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.bin")
	content := "hello\nworld\n"
	p := surrogateCatProc(t, out, &content)
	if code := surrogateWaitOrKill(t, p); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if data, _ := os.ReadFile(out); string(data) != "hello\nworld\n" || p.JoinStdin(5*time.Second) != "" {
		t.Fatalf("bytes %q", data)
	}
}

func TestPipeStdinRawBytesPassThroughUntouched(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.bin")
	content := "\x00\x01\xfe"
	p := surrogateCatProc(t, out, &content)
	if code := surrogateWaitOrKill(t, p); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if data, _ := os.ReadFile(out); !bytes.Equal(data, []byte{0x00, 0x01, 0xfe}) || p.JoinStdin(5*time.Second) != "" {
		t.Fatalf("bytes %q", data)
	}
}

func TestPipeStdinMissingPipeRecordsError(t *testing.T) {
	p := &ExecProcess{}
	pipeStdin(p, nil, "data")
	if e := p.JoinStdin(5 * time.Second); e != "process stdin unavailable" {
		t.Fatalf("stdin error %q", e)
	}
}

// Exact refusal text of a write whose content holds a lone surrogate
func loneSurrogateRefusal(path, repr string) string {
	return "Refusing to write '" + path + "': content contains a lone surrogate character (" + repr +
		") that cannot be encoded as UTF-8. The file was NOT created or modified."
}

func TestFileOpsWriteFileRejectsUnencodableSurrogateBeforeWrite(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, env, dir := hostFileOps(t, native)
		for bad, repr := range map[string]string{"\xed\xa0\x80": `'\ud800'`, "\xed\xb1\xbf": `'\udc7f'`, "\xed\xb4\x80": `'\udd00'`} {
			p := filepath.Join(dir, "reject.bin")
			resetEnvCommands(env)
			r := ops.WriteFile(p, bad, nil)
			if r.Error != loneSurrogateRefusal(p, repr) || strings.Contains(r.Error, "timed out") || strings.Contains(r.Error, "codec can't encode") {
				t.Fatalf("native=%v %q: %+v", native, bad, r)
			}
			if _, err := os.Stat(p); err == nil {
				t.Fatalf("native=%v %q created the file", native, bad)
			}
			if cmds := envCommands(env); len(cmds) != 0 {
				t.Fatalf("native=%v %q ran %q before refusing", native, bad, cmds)
			}
		}
	}
}

func TestFileOpsRejectedWriteLeavesExistingTargetUnchanged(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, _, dir := hostFileOps(t, native)
		p := writeTestFile(t, dir, "keep.bin", []byte("precious original bytes"))
		r := ops.WriteFile(p, "\xed\xa0\x80", nil)
		if !strings.Contains(r.Error, "NOT created or modified") {
			t.Fatalf("native=%v %+v", native, r)
		}
		if data, _ := os.ReadFile(p); string(data) != "precious original bytes" {
			t.Fatalf("native=%v target changed %q", native, data)
		}
	}
}

func TestFileOpsPatchReplaceFunnelRejectsSurrogateNewString(t *testing.T) {
	for _, native := range []bool{true, false} {
		ops, _, dir := hostFileOps(t, native)
		p := writeTestFile(t, dir, "patchme.txt", []byte("old\n"))
		r := ops.patchReplace(p, "old", "new\xed\xb1\xbf", false, nil)
		if !strings.Contains(r.Error, "surrogate") || r.Error != "Failed to write changes: "+loneSurrogateRefusal(p, `'\udc7f'`) {
			t.Fatalf("native=%v %+v", native, r)
		}
		if data, _ := os.ReadFile(p); string(data) != "old\n" {
			t.Fatalf("native=%v target changed %q", native, data)
		}
	}
}

func TestExecuteSurfacesStdinErrorWithoutHanging(t *testing.T) {
	requireBash(t)
	l := NewLocalEnvironment(context.Background(), newTestToolEnv(t), t.TempDir(), 15, nil, true)
	defer l.Cleanup(false)
	bad := "\xed\xa0\x80"
	start := time.Now()
	res, err := l.Execute(context.Background(), "cat > /dev/null", ExecOptions{Stdin: &bad})
	elapsed := time.Since(start)
	if err != nil || res.ReturnCode != 0 || res.StdinError == "" || !strings.Contains(res.Output, "stdin write failed") {
		t.Fatalf("%+v %v", res, err)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("stdin failure path hung for %v", elapsed)
	}
	normal, err := l.Execute(context.Background(), "echo hi", ExecOptions{})
	if err != nil || normal.StdinError != "" || normal.ReturnCode != 0 || !strings.Contains(normal.Output, "hi") {
		t.Fatalf("%+v %v", normal, err)
	}
}

func TestFileOpsByteExactReadSurvivesXtraceShell(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	t.Setenv("ICEBATH_NATIVE_FILE_READ", "0")
	hook := writeTestFile(t, dir, "xtrace.sh", []byte("set -x\n"))
	original := []byte("HEADER\n\x1b]0;osc\x07\n" + strings.Repeat("line\n", 250) + "VERSION=1 \xff\n")
	target := writeTestFile(t, dir, "conf.txt", original)
	env := &fakeTermEnv{envType: "local", cwd: dir, execute: func(_ context.Context, command string, opts ExecOptions) (*ExecResult, error) {
		cmd := exec.Command("bash", "-c", command)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "BASH_ENV="+hook)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		rc := 0
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return nil, err
			}
			rc = exitErr.ExitCode()
		}
		d := &utf8Decoder{}
		return &ExecResult{Output: d.decode(out.Bytes()) + d.finish(), ReturnCode: rc}, nil
	}}
	ops := newShellFileOps(remoteTestEnv(t), env, dir)
	if sample, ok := ops.sampleFileBytes(target, 1000); !ok || !bytes.Equal(sample, original[:1000]) {
		t.Fatalf("sample %q %v", sample, ok)
	}
	raw := ops.ReadFileRaw(target)
	if encoded, err := pystr.SurrogateEscapeEncode(raw.Content); raw.Error != "" || err != nil || !bytes.Equal(encoded, original) {
		t.Fatalf("raw %+v %v", raw.Error, err)
	}
}

func TestFileToolsSurrogateEscapedByteRoundTripsThroughReadAndPatch(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	head := strings.Repeat("filler line\n", 100)
	original := []byte(head + "value a\x80b end\nother \xff tail\n")
	p := filepath.Join(ws, "mixed.txt")
	if err := os.WriteFile(p, original, 0o644); err != nil {
		t.Fatal(err)
	}
	read := ftDecode(t, readFileTool(context.Background(), env, p, 101, 2, "t1"))
	if read["content"] != "101|value a\ufffdb end\n102|other \ufffd tail" {
		t.Fatalf("read %v", read)
	}
	args, errText := ParseToolArguments(`{"path": ` + jsonCompact(p) + `, "old_string": "value a\udc80b end", "new_string": "value a\udc80c end"}`)
	if errText != "" {
		t.Fatal(errText)
	}
	raw := handlePatch(context.Background(), &Call{Name: "patch", Args: args, TaskID: "t1", Env: env})
	out := ftDecode(t, raw)
	if out["success"] != true {
		t.Fatalf("patch %s", raw)
	}
	diff, _ := out["diff"].(string)
	if !strings.Contains(diff, "-value a\xed\xb2\x80b end") || !strings.Contains(diff, "+value a\xed\xb2\x80c end") || !strings.Contains(raw, "\xed\xb2\x80") {
		t.Fatalf("diff %q", diff)
	}
	want := []byte(head + "value a\x80c end\nother \xff tail\n")
	if data, _ := os.ReadFile(p); !bytes.Equal(data, want) {
		t.Fatalf("disk %q", data)
	}
}
