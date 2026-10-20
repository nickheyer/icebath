package shell

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

func TestForegroundStreamsProgressAndReportsData(t *testing.T) {
	s := newShell(t, Options{})
	var mu sync.Mutex
	var seen []tool.Progress
	ctx := tool.WithReporter(context.Background(), func(p tool.Progress) {
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
	})
	ctx = tool.WithCall(ctx, protocol.ToolCall{ID: "call-9", Name: BashName})
	res, err := s.Bash().Run(ctx, json.RawMessage(`{"command":"echo out; echo err >&2","description":"says things"}`))
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	mu.Lock()
	defer mu.Unlock()
	streams := map[string]string{}
	for _, p := range seen {
		streams[p.Stream] += p.Text
		if p.CallID != "call-9" || p.Tool != BashName {
			t.Fatalf("progress must name the call: %+v", p)
		}
	}
	if streams["stdout"] != "out\n" || streams["stderr"] != "err\n" {
		t.Fatalf("%+v", streams)
	}
	data := res.Data.(Run)
	if data.Command == "" || data.Description != "says things" || data.Duration <= 0 || data.Stdout != "out\n" || data.Stderr != "err\n" || data.Dir == "" {
		t.Fatalf("%+v", data)
	}
}

func TestJobTailWaitKillAndEvents(t *testing.T) {
	var mu sync.Mutex
	var kinds []JobEventKind
	var output []byte
	s := newShell(t, Options{OnJob: func(e JobEvent) {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, e.Kind)
		output = append(output, e.Data...)
	}})
	ctx := tool.WithCall(context.Background(), protocol.ToolCall{ID: "c1", Name: BashName})
	res, _ := s.Bash().Run(ctx, json.RawMessage(`{"command":"echo a; sleep 0.1; echo b","run_in_background":true,"description":"ab"}`))
	info := res.Data.(JobInfo)
	if info.ID != "job-1" || info.CallID != "c1" || info.Description != "ab" || !info.Running {
		t.Fatalf("%+v", info)
	}
	done, ok := s.Wait("job-1")
	if !ok {
		t.Fatal("wait handle")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("job never finished")
	}
	data, next, dropped, ok := s.Tail("job-1", 0)
	if !ok || string(data) != "a\nb\n" || next != 4 || dropped != 0 {
		t.Fatalf("%q %d %d %v", data, next, dropped, ok)
	}
	if data, _, _, _ = s.Tail("job-1", 2); string(data) != "b\n" {
		t.Fatalf("tail from offset %q", data)
	}
	if out, _ := run(t, s.Output(), `{"bash_id":"job-1"}`); out != "a\nb\n[finished with exit code 0]" {
		t.Fatalf("tail must not consume the model cursor: %q", out)
	}
	job, ok := s.Job("job-1")
	if !ok || job.Running || job.Bytes != 4 || job.Finished.IsZero() {
		t.Fatalf("%+v", job)
	}
	mu.Lock()
	if kinds[0] != JobStarted || kinds[len(kinds)-1] != JobFinished || string(output) != "a\nb\n" {
		t.Fatalf("%v %q", kinds, output)
	}
	mu.Unlock()
	run(t, s.Bash(), `{"command":"sleep 30","run_in_background":true}`)
	if err := s.KillJob(context.Background(), "job-2"); err != nil {
		t.Fatal(err)
	}
	if job, _ := s.Job("job-2"); job.Running || job.ExitCode != 137 {
		t.Fatalf("%+v", job)
	}
	if err := s.KillJob(context.Background(), "nope"); err == nil {
		t.Fatal("unknown job killed")
	}
	if _, ok := s.Wait("nope"); ok {
		t.Fatal("unknown wait")
	}
	if _, _, _, ok := s.Tail("nope", 0); ok {
		t.Fatal("unknown tail")
	}
	if _, ok := s.Job("nope"); ok {
		t.Fatal("unknown job")
	}
}

func TestBackgroundTimeoutIsHonored(t *testing.T) {
	s := newShell(t, Options{})
	run(t, s.Bash(), `{"command":"sleep 5","run_in_background":true,"timeout":100}`)
	done, _ := s.Wait("job-1")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("background timeout ignored")
	}
	if out, _ := run(t, s.Output(), `{"bash_id":"job-1"}`); !strings.HasSuffix(out, "[killed by timeout]") {
		t.Fatalf("%q", out)
	}
	if job, _ := s.Job("job-1"); !job.TimedOut {
		t.Fatalf("%+v", job)
	}
}

func TestFinishedJobsArePruned(t *testing.T) {
	s := newShell(t, Options{MaxJobs: 2})
	for range 4 {
		run(t, s.Bash(), `{"command":"true","run_in_background":true}`)
		for _, j := range s.Jobs() {
			if j.Running {
				done, _ := s.Wait(j.ID)
				<-done
			}
		}
	}
	run(t, s.Bash(), `{"command":"true","run_in_background":true}`)
	jobs := s.Jobs()
	if len(jobs) > 3 {
		t.Fatalf("%d jobs kept", len(jobs))
	}
	if _, ok := s.Job("job-1"); ok {
		t.Fatal("oldest finished job must be forgotten")
	}
	if _, ok := s.Job("job-5"); !ok {
		t.Fatal("newest job must be kept")
	}
}

func TestFailedStartIsReported(t *testing.T) {
	s := newShell(t, Options{Runner: Local{Shell: "/no/such/shell"}})
	out, isErr := run(t, s.Bash(), `{"command":"true"}`)
	if !isErr || out == "" {
		t.Fatalf("%v %q", isErr, out)
	}
	run(t, s.Bash(), `{"command":"true","run_in_background":true}`)
	done, _ := s.Wait("job-1")
	<-done
	if out, _ := run(t, s.Output(), `{"bash_id":"job-1"}`); !strings.Contains(out, "[failed to run:") {
		t.Fatalf("%q", out)
	}
	if job, _ := s.Job("job-1"); job.Error == "" || job.ExitCode != -1 {
		t.Fatalf("%+v", job)
	}
}

func TestStreamSinceBeyondEnd(t *testing.T) {
	st := &stream{max: 10}
	st.Write([]byte("abc"))
	data, next, dropped := st.since(50)
	if len(data) != 0 || next != 3 || dropped != 0 {
		t.Fatalf("%q %d %d", data, next, dropped)
	}
}
