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
	CallID      string
	Line        string
	Description string
	Dir         string
	Started     time.Time
	Finished    time.Time
	Running     bool
	ExitCode    int
	TimedOut    bool
	// Why the job could not run at all, empty when the command started
	Error string
	// Bytes of output produced so far
	Bytes int64
}

// What changed about a background job
type JobEvent struct {
	Kind JobEventKind
	Job  JobInfo
	// Output chunk on output events
	Data []byte
}

type JobEventKind string

const (
	JobStarted  JobEventKind = "started"
	JobOutput   JobEventKind = "output"
	JobFinished JobEventKind = "finished"
)

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
	tap   func([]byte)
}

func (s *stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.buf = append(s.buf, p...)
	if over := len(s.buf) - s.max; over > 0 {
		s.start += int64(over)
		s.buf = append([]byte(nil), s.buf[over:]...)
	}
	tap := s.tap
	s.mu.Unlock()
	if tap != nil {
		tap(append([]byte(nil), p...))
	}
	return len(p), nil
}

// Total bytes written so far
func (s *stream) size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.start + int64(len(s.buf))
}

// Returns the bytes from an absolute offset, the next offset and how many bytes were lost before them
func (s *stream) since(from int64) (data []byte, next int64, dropped int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if from < s.start {
		dropped = s.start - from
		from = s.start
	}
	end := s.start + int64(len(s.buf))
	if from > end {
		from = end
	}
	data = append([]byte(nil), s.buf[from-s.start:]...)
	return data, end, dropped
}

// Table of background jobs keyed by id
type Jobs struct {
	mu      sync.Mutex
	next    int
	jobs    map[string]*job
	order   []string
	max     int
	keep    int
	onEvent func(JobEvent)
}

func newJobs(maxOutput, keep int, onEvent func(JobEvent)) *Jobs {
	return &Jobs{jobs: map[string]*job{}, max: maxOutput, keep: keep, onEvent: onEvent}
}

func (j *Jobs) emit(kind JobEventKind, jb *job, data []byte) {
	if j.onEvent == nil {
		return
	}
	j.onEvent(JobEvent{Kind: kind, Job: j.info(jb), Data: data})
}

// Snapshot of a job with its live byte count
func (j *Jobs) info(jb *job) JobInfo {
	j.mu.Lock()
	info := jb.JobInfo
	j.mu.Unlock()
	info.Bytes = jb.out.size()
	return info
}

// Launches the command in its own goroutine and returns the job at once
func (j *Jobs) start(runner Runner, cmd Command) *job {
	j.mu.Lock()
	j.next++
	id := fmt.Sprintf("job-%d", j.next)
	ctx, cancel := context.WithCancel(context.Background())
	out := &stream{max: j.max}
	jb := &job{JobInfo: JobInfo{ID: id, CallID: cmd.ID, Line: cmd.Line, Description: cmd.Description, Dir: cmd.Dir, Started: time.Now(), Running: true}, out: out, cancel: cancel, done: make(chan struct{})}
	out.tap = func(p []byte) { j.emit(JobOutput, jb, p) }
	j.jobs[id] = jb
	j.order = append(j.order, id)
	j.prune()
	j.mu.Unlock()
	j.emit(JobStarted, jb, nil)
	cmd.ID = id
	cmd.Stdout, cmd.Stderr = out, out
	go func() {
		res, err := runner.Run(ctx, cmd)
		j.mu.Lock()
		jb.Running = false
		jb.Finished = time.Now()
		jb.ExitCode, jb.TimedOut, jb.err = res.ExitCode, res.TimedOut, err
		if err != nil {
			jb.ExitCode = -1
			jb.Error = err.Error()
		}
		j.mu.Unlock()
		close(jb.done)
		j.emit(JobFinished, jb, nil)
	}()
	return jb
}

// Forgets the oldest finished jobs beyond the retention count
func (j *Jobs) prune() {
	finished := 0
	for _, id := range j.order {
		if jb := j.jobs[id]; jb != nil && !jb.Running {
			finished++
		}
	}
	for i := 0; i < len(j.order) && finished > j.keep; {
		id := j.order[i]
		if jb := j.jobs[id]; jb != nil && !jb.Running {
			delete(j.jobs, id)
			j.order = append(j.order[:i], j.order[i+1:]...)
			finished--
			continue
		}
		i++
	}
}

func (j *Jobs) get(id string) (*job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	jb, ok := j.jobs[id]
	return jb, ok
}

func (j *Jobs) list() []JobInfo {
	j.mu.Lock()
	jobs := make([]*job, 0, len(j.jobs))
	for _, jb := range j.jobs {
		jobs = append(jobs, jb)
	}
	j.mu.Unlock()
	out := make([]JobInfo, 0, len(jobs))
	for _, jb := range jobs {
		out = append(out, j.info(jb))
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Started.Equal(out[b].Started) {
			return out[a].ID < out[b].ID
		}
		return out[a].Started.Before(out[b].Started)
	})
	return out
}

func (j *Jobs) close() {
	j.mu.Lock()
	jobs := j.jobs
	j.jobs = map[string]*job{}
	j.order = nil
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
	return tool.Text(sb.String()).With(o.jobs.info(jb)), nil
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
		return tool.Textf("%s had already finished", in.ID).With(k.jobs.info(jb)), nil
	default:
	}
	if err := k.KillJob(ctx, in.ID); err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Textf("Killed %s", in.ID).With(k.jobs.info(jb)), nil
}
