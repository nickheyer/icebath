package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Snapshot of one background job
type JobInfo struct {
	ID          string
	Line        string
	Description string
	Started     time.Time
	Running     bool
	ExitCode    int
	TimedOut    bool
}

type job struct {
	JobInfo
	out    *stream
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	read   int64
}

// Output ring that keeps the newest bytes and the absolute offset of the oldest one kept
type stream struct {
	mu    sync.Mutex
	buf   []byte
	start int64
	max   int
}

func (s *stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	if over := len(s.buf) - s.max; over > 0 {
		s.start += int64(over)
		s.buf = append([]byte(nil), s.buf[over:]...)
	}
	return len(p), nil
}

// Returns the bytes from an absolute offset, the next offset and how many bytes were lost before them
func (s *stream) since(from int64) (data []byte, next int64, dropped int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if from < s.start {
		dropped = s.start - from
		from = s.start
	}
	data = append([]byte(nil), s.buf[from-s.start:]...)
	return data, s.start + int64(len(s.buf)), dropped
}

// Table of background jobs keyed by id
type Jobs struct {
	mu   sync.Mutex
	next int
	jobs map[string]*job
	max  int
}

func newJobs(maxOutput int) *Jobs {
	return &Jobs{jobs: map[string]*job{}, max: maxOutput}
}

// Launches the command in its own goroutine and returns the job at once
func (j *Jobs) start(runner Runner, cmd Command, description string) *job {
	j.mu.Lock()
	j.next++
	id := fmt.Sprintf("job-%d", j.next)
	ctx, cancel := context.WithCancel(context.Background())
	out := &stream{max: j.max}
	jb := &job{JobInfo: JobInfo{ID: id, Line: cmd.Line, Description: description, Started: time.Now(), Running: true}, out: out, cancel: cancel, done: make(chan struct{})}
	j.jobs[id] = jb
	j.mu.Unlock()
	cmd.Stdout, cmd.Stderr = out, out
	go func() {
		res, err := runner.Run(ctx, cmd)
		j.mu.Lock()
		jb.Running = false
		jb.ExitCode, jb.TimedOut, jb.err = res.ExitCode, res.TimedOut, err
		if err != nil {
			jb.ExitCode = -1
		}
		j.mu.Unlock()
		close(jb.done)
	}()
	return jb
}

func (j *Jobs) get(id string) (*job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	jb, ok := j.jobs[id]
	return jb, ok
}

func (j *Jobs) list() []JobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]JobInfo, 0, len(j.jobs))
	for _, jb := range j.jobs {
		out = append(out, jb.JobInfo)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Started.Before(out[b].Started) })
	return out
}

func (j *Jobs) close() {
	j.mu.Lock()
	jobs := j.jobs
	j.jobs = map[string]*job{}
	j.mu.Unlock()
	for _, jb := range jobs {
		jb.cancel()
		<-jb.done
	}
}

const outputSchema = `{"type":"object","properties":{
"bash_id":{"type":"string","description":"Id returned when the job was started"},
"filter":{"type":"string","description":"Regular expression, only lines matching it are returned"}},
"required":["bash_id"],"additionalProperties":false}`

type output struct{ *Shell }

// Tool that returns the output a background job produced since the previous call
func (s *Shell) Output() tool.Tool { return output{s} }

func (output) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        OutputName,
		Description: "Returns new output from a background job started by Bash along with whether it is still running and its exit code once finished. Each call returns only what arrived since the previous call.",
		Parameters:  json.RawMessage(outputSchema),
	}
}

func (output) Annotations() tool.Annotations {
	return tool.Annotations{ReadOnly: true}
}

func (o output) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in struct {
		ID     string `json:"bash_id"`
		Filter string `json:"filter"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	jb, ok := o.jobs.get(in.ID)
	if !ok {
		return tool.Errorf("no background job %q", in.ID), nil
	}
	var filter *regexp.Regexp
	if in.Filter != "" {
		re, err := regexp.Compile(in.Filter)
		if err != nil {
			return tool.Errorf("invalid filter: %v", err), nil
		}
		filter = re
	}
	o.jobs.mu.Lock()
	data, next, dropped := jb.out.since(jb.read)
	jb.read = next
	info, err := jb.JobInfo, jb.err
	o.jobs.mu.Unlock()
	text := string(data)
	if filter != nil {
		var kept []string
		for _, l := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
			if filter.MatchString(l) {
				kept = append(kept, l)
			}
		}
		text = strings.Join(kept, "\n")
	}
	var sb strings.Builder
	if dropped > 0 {
		fmt.Fprintf(&sb, "[%d bytes were lost before this output]\n", dropped)
	}
	sb.WriteString(strings.TrimRight(text, "\n"))
	if sb.Len() == 0 {
		sb.WriteString("(no new output)")
	}
	switch {
	case info.Running:
		sb.WriteString("\n[running]")
	case err != nil:
		fmt.Fprintf(&sb, "\n[failed to run: %v]", err)
	case info.TimedOut:
		sb.WriteString("\n[killed by timeout]")
	default:
		fmt.Fprintf(&sb, "\n[finished with exit code %d]", info.ExitCode)
	}
	return tool.Text(sb.String()), nil
}

const killSchema = `{"type":"object","properties":{
"shell_id":{"type":"string","description":"Id of the background job to stop"}},
"required":["shell_id"],"additionalProperties":false}`

type kill struct{ *Shell }

// Tool that stops a running background job
func (s *Shell) Kill() tool.Tool { return kill{s} }

func (kill) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        KillName,
		Description: "Stops a background job started by Bash, killing its whole process group.",
		Parameters:  json.RawMessage(killSchema),
	}
}

func (kill) Annotations() tool.Annotations {
	return tool.Annotations{Destructive: true, Idempotent: true}
}

func (k kill) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in struct {
		ID string `json:"shell_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	jb, ok := k.jobs.get(in.ID)
	if !ok {
		return tool.Errorf("no background job %q", in.ID), nil
	}
	select {
	case <-jb.done:
		return tool.Textf("%s had already finished", in.ID), nil
	default:
	}
	jb.cancel()
	select {
	case <-jb.done:
	case <-ctx.Done():
		return tool.Error(ctx.Err().Error()), nil
	}
	return tool.Textf("Killed %s", in.ID), nil
}
