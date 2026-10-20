package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

const bashSchema = `{"type":"object","properties":{
"command":{"type":"string","description":"Command line to run"},
"timeout":{"type":"integer","minimum":1,"description":"Milliseconds the command may run before it is killed"},
"description":{"type":"string","description":"Short plain words saying what the command does, shown to the user"},
"run_in_background":{"type":"boolean","description":"Start the command as a background job and return its id at once"}},
"required":["command"],"additionalProperties":false}`

type bash struct{ *Shell }

// Tool that runs a command line in the shell, foreground or as a background job
func (s *Shell) Bash() tool.Tool { return bash{s} }

func (b bash) Definition() protocol.Tool {
	return protocol.Tool{
		Name: BashName,
		Description: fmt.Sprintf("Runs a command line in a shell and returns its output, exit code and whether it timed out. The working directory persists between calls. Stdin is closed. Commands run for %s by default and at most %s. Output is cut to %d bytes keeping the start and the end. With run_in_background the call returns a job id for BashOutput and KillShell.",
			b.opts.timeout(), b.opts.maxTimeout(), b.opts.maxOutput()),
		Parameters: json.RawMessage(bashSchema),
	}
}

func (bash) Annotations() tool.Annotations {
	return tool.Annotations{Destructive: true, OpenWorld: true}
}

func (b bash) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Command     string `json:"command"`
		Timeout     int    `json:"timeout"`
		Description string `json:"description"`
		Background  bool   `json:"run_in_background"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if strings.TrimSpace(in.Command) == "" {
		return tool.Error("command is required"), nil
	}
	timeout := b.opts.timeout()
	if in.Timeout > 0 {
		timeout = min(time.Duration(in.Timeout)*time.Millisecond, b.opts.maxTimeout())
	}
	id := ""
	if call, ok := tool.CallFrom(ctx); ok {
		id = call.ID
	}
	if in.Background {
		if in.Timeout == 0 {
			timeout = b.opts.maxTimeout()
		}
		job := b.jobs.start(b.runner, Command{ID: id, Description: in.Description, Line: in.Command, Dir: b.Dir(), Env: b.opts.Env, Timeout: timeout})
		return tool.Textf("Started background job %s, read it with BashOutput and stop it with KillShell", job.ID).With(b.jobs.info(job)), nil
	}
	stdout, stderr := newCapture(b.opts.maxOutput()), newCapture(b.opts.maxOutput())
	cmd := Command{
		ID:          id,
		Description: in.Description,
		Line:        in.Command,
		Dir:         b.Dir(),
		Env:         b.opts.Env,
		Timeout:     timeout,
		Stdout:      io.MultiWriter(stdout, reporting{ctx, "stdout"}),
		Stderr:      io.MultiWriter(stderr, reporting{ctx, "stderr"}),
	}
	out, err := b.runner.Run(ctx, cmd)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if out.Dir != "" {
		b.mu.Lock()
		b.dir = out.Dir
		b.mu.Unlock()
	}
	res := render(stdout.String(), stderr.String(), out, timeout)
	return res.With(Run{Command: in.Command, Description: in.Description, Dir: out.Dir, ExitCode: out.ExitCode, TimedOut: out.TimedOut, Duration: out.Duration, Stdout: stdout.String(), Stderr: stderr.String()}), nil
}

// Formats stdout, stderr and the outcome, marking failures as error results
func render(stdout, stderr string, out Output, timeout time.Duration) tool.Result {
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(stdout, "\n"))
	if stderr = strings.TrimRight(stderr, "\n"); stderr != "" {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("[stderr]\n")
		sb.WriteString(stderr)
	}
	if sb.Len() == 0 {
		sb.WriteString("(no output)")
	}
	if out.TimedOut {
		fmt.Fprintf(&sb, "\n[timed out after %s]", timeout)
	} else if out.ExitCode != 0 {
		fmt.Fprintf(&sb, "\n[exit code %d]", out.ExitCode)
	}
	res := tool.Text(sb.String())
	res.IsError = out.TimedOut || out.ExitCode != 0
	return res
}
