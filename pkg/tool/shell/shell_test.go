package shell

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
	return res.Content[0].(protocol.Text).Text, res.IsError
}

func newShell(t *testing.T, o Options) *Shell {
	t.Helper()
	if o.Dir == "" {
		o.Dir = t.TempDir()
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestNewValidatesDir(t *testing.T) {
	if _, err := New(Options{Dir: "/definitely/missing"}); err == nil {
		t.Fatal("missing dir accepted")
	}
	f := filepath.Join(t.TempDir(), "f")
	os.WriteFile(f, nil, 0o644)
	if _, err := New(Options{Dir: f}); err == nil {
		t.Fatal("file accepted as dir")
	}
	s := newShell(t, Options{})
	if len(s.Tools()) != 3 || s.Tools()[0].Definition().Name != BashName {
		t.Fatalf("%+v", s.Tools())
	}
	var r tool.Registry
	if err := r.Add(s.Tools()...); err != nil {
		t.Fatal(err)
	}
	if ro := r.ReadOnly(); len(ro) != 1 || ro[0] != OutputName {
		t.Fatalf("%v", ro)
	}
	if err := s.SetDir(f); err == nil {
		t.Fatal("file accepted by SetDir")
	}
}

func TestBashOutputExitAndStderr(t *testing.T) {
	s := newShell(t, Options{Env: []string{"ICEBATH_TEST=yes"}})
	b := s.Bash()
	out, isErr := run(t, b, `{"command":"echo hi; echo $ICEBATH_TEST"}`)
	if isErr || out != "hi\nyes" {
		t.Fatalf("%v %q", isErr, out)
	}
	out, isErr = run(t, b, `{"command":"echo out; echo err >&2; exit 3"}`)
	if !isErr || out != "out\n[stderr]\nerr\n[exit code 3]" {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, b, `{"command":"true"}`); isErr || out != "(no output)" {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, b, `{"command":"cat"}`); isErr || out != "(no output)" {
		t.Fatalf("stdin must be closed: %v %q", isErr, out)
	}
	if out, isErr = run(t, b, `{"command":"kill -9 $$"}`); !isErr || !strings.Contains(out, "[exit code 137]") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, b, `{"command":"(echo FAIL; exit 1) | tail -1"}`); isErr || out != "FAIL\n[pipeline exit codes 1 0, an earlier command failed]" {
		t.Fatalf("pipeline statuses %v %q", isErr, out)
	}
	if out, isErr = run(t, b, `{"command":"echo ok | cat"}`); isErr || out != "ok" {
		t.Fatalf("clean pipeline %v %q", isErr, out)
	}
	if !strings.Contains(b.Definition().Description, "every stage is listed") {
		t.Fatal("bash promises the stage codes it reads back")
	}
	for args, want := range map[string]string{`{}`: "required", `{"command":"   "}`: "required", `{"command":1}`: "cannot unmarshal"} {
		if out, isErr := run(t, b, args); !isErr || !strings.Contains(out, want) {
			t.Fatalf("%s: %v %q", args, isErr, out)
		}
	}
}

func TestWorkingDirectoryPersists(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	s := newShell(t, Options{Dir: dir})
	b := s.Bash()
	if out, _ := run(t, b, `{"command":"pwd"}`); out != dir {
		t.Fatalf("%q", out)
	}
	if out, _ := run(t, b, `{"command":"cd sub && pwd"}`); out != sub {
		t.Fatalf("%q", out)
	}
	if out, _ := run(t, b, `{"command":"pwd"}`); out != sub || s.Dir() != sub {
		t.Fatalf("%q %q", out, s.Dir())
	}
	if err := s.SetDir(dir); err != nil {
		t.Fatal(err)
	}
	if out, _ := run(t, b, `{"command":"pwd"}`); out != dir {
		t.Fatalf("%q", out)
	}
	if out, isErr := run(t, b, `{"command":"cd sub; exit 2"}`); !isErr || !strings.HasSuffix(out, "[exit code 2]") || s.Dir() != sub {
		t.Fatalf("directory must move even on failure: %v %q %q", isErr, out, s.Dir())
	}
}

func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	s := newShell(t, Options{MaxTimeout: 300 * time.Millisecond})
	marker := filepath.Join(t.TempDir(), "alive")
	start := time.Now()
	out, isErr := run(t, s.Bash(), fmt.Sprintf(`{"command":"echo started; (sleep 5; touch %s) & sleep 5","timeout":60000}`, marker))
	if !isErr || !strings.HasPrefix(out, "started") || !strings.Contains(out, "[timed out after 300ms]") {
		t.Fatalf("%v %q", isErr, out)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout not applied")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("grandchild survived the kill")
	}
}

func TestOutputIsCappedKeepingBothEnds(t *testing.T) {
	s := newShell(t, Options{MaxOutput: 40})
	out, isErr := run(t, s.Bash(), `{"command":"printf 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'; printf 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'"}`)
	if isErr || !strings.HasPrefix(out, strings.Repeat("A", 20)+"\n[20 bytes omitted]\n") || !strings.HasSuffix(out, strings.Repeat("B", 20)) {
		t.Fatalf("%v %q", isErr, out)
	}
	c := newCapture(10)
	for _, chunk := range []string{"abc", "defgh", "ijklmno", "p"} {
		c.Write([]byte(chunk))
	}
	if c.String() != "abcde\n[6 bytes omitted]\nlmnop" {
		t.Fatalf("%q", c.String())
	}
}

func TestBackgroundJobs(t *testing.T) {
	s := newShell(t, Options{})
	b, o, k := s.Bash(), s.Output(), s.Kill()
	out, isErr := run(t, b, `{"command":"echo one; sleep 0.2; echo two; echo three; exit 4","run_in_background":true,"description":"counts"}`)
	if isErr || !strings.HasPrefix(out, "Started background job job-1") {
		t.Fatalf("%v %q", isErr, out)
	}
	time.Sleep(80 * time.Millisecond)
	if out, isErr = run(t, o, `{"bash_id":"job-1"}`); isErr || out != "one\n[running]" {
		t.Fatalf("%v %q", isErr, out)
	}
	deadline := time.Now().Add(3 * time.Second)
	var gathered []string
	for {
		out, _ = run(t, o, `{"bash_id":"job-1","filter":"t"}`)
		body, status, _ := strings.Cut(out, "\n[")
		if body != "(no new output)" {
			gathered = append(gathered, body)
		}
		if status == "finished with exit code 4]" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never finished: %q", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Join(gathered, "\n") != "two\nthree" {
		t.Fatalf("%q", gathered)
	}
	if out, _ = run(t, o, `{"bash_id":"job-1"}`); out != "(no new output)\n[finished with exit code 4]" {
		t.Fatalf("%q", out)
	}
	if out, isErr = run(t, k, `{"shell_id":"job-1"}`); isErr || out != "job-1 had already finished" {
		t.Fatalf("%v %q", isErr, out)
	}
	run(t, b, `{"command":"while true; do echo tick; sleep 0.05; done","run_in_background":true}`)
	time.Sleep(120 * time.Millisecond)
	if out, isErr = run(t, k, `{"shell_id":"job-2"}`); isErr || out != "Killed job-2" {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, _ = run(t, o, `{"bash_id":"job-2"}`); !strings.HasPrefix(out, "tick") || !strings.HasSuffix(out, "[finished with exit code 137]") {
		t.Fatalf("%q", out)
	}
	jobs := s.Jobs()
	if len(jobs) != 2 || jobs[0].ID != "job-1" || jobs[0].Description != "counts" || jobs[0].Running || jobs[0].ExitCode != 4 || jobs[1].Running {
		t.Fatalf("%+v", jobs)
	}
	for _, c := range []struct {
		tl   tool.Tool
		args string
	}{{o, `{"bash_id":"nope"}`}, {k, `{"shell_id":"nope"}`}, {o, `{"bash_id":"job-1","filter":"("}`}, {o, `{"bash_id":1}`}, {k, `{"shell_id":1}`}} {
		if out, isErr := run(t, c.tl, c.args); !isErr {
			t.Fatalf("%s: %q", c.args, out)
		}
	}
	run(t, b, `{"command":"sleep 30","run_in_background":true}`)
	s.Close()
	if len(s.Jobs()) != 0 {
		t.Fatal("close must forget jobs")
	}
}

func TestJobOutputRingReportsLoss(t *testing.T) {
	st := &stream{max: 5}
	st.Write([]byte("abcdefg"))
	data, next, dropped := st.since(0)
	if string(data) != "cdefg" || next != 7 || dropped != 2 {
		t.Fatalf("%q %d %d", data, next, dropped)
	}
	st.Write([]byte("hi"))
	if data, next, dropped = st.since(next); string(data) != "hi" || next != 9 || dropped != 0 {
		t.Fatalf("%q %d %d", data, next, dropped)
	}
	s := newShell(t, Options{MaxJobOutput: 8})
	run(t, s.Bash(), `{"command":"printf '0123456789'","run_in_background":true}`)
	deadline := time.Now().Add(2 * time.Second)
	for {
		out, _ := run(t, s.Output(), `{"bash_id":"job-1"}`)
		if strings.Contains(out, "finished") {
			if out != "[2 bytes were lost before this output]\n23456789\n[finished with exit code 0]" {
				t.Fatalf("%q", out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLocalRunnerFailsToStartUnknownShell(t *testing.T) {
	_, err := Local{Shell: "/no/such/shell"}.Run(context.Background(), Command{Line: "true"})
	if err == nil {
		t.Fatal("missing shell accepted")
	}
	out, err := Local{}.Run(context.Background(), Command{Line: "exit 7"})
	if err != nil || out.ExitCode != 7 || out.Dir == "" {
		t.Fatalf("%v %+v", err, out)
	}
}
