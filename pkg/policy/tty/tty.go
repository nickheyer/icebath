// Terminal prompt that asks a person to allow or refuse a tool call
package tty

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
)

const prompt = "Allow? [y]es, [n]o, [a]lways, ne[v]er: "

// Asker reading one line answers from a terminal
type Asker struct {
	// Renders the call above the prompt, the tool name and pretty arguments when nil
	Format func(call protocol.ToolCall) string
	out    io.Writer
	wmu    sync.Mutex
	lines  chan string
	errs   chan error
	err    error
	stale  bool
}

// Starts reading answers from in and writes prompts to out
func New(in io.Reader, out io.Writer) *Asker {
	a := &Asker{out: out, lines: make(chan string), errs: make(chan error, 1)}
	go a.read(in)
	return a
}

func (a *Asker) read(in io.Reader) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		a.lines <- sc.Text()
	}
	if err := sc.Err(); err != nil {
		a.errs <- err
	} else {
		a.errs <- io.EOF
	}
	close(a.lines)
}

// Shows the call and keeps prompting until a recognised answer arrives, discarding answers typed to a cancelled prompt
func (a *Asker) Ask(ctx context.Context, call protocol.ToolCall) (policy.Verdict, error) {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	if a.stale {
		a.drain()
		a.stale = false
	}
	fmt.Fprintln(a.out, a.format(call))
	for {
		fmt.Fprint(a.out, prompt)
		var line string
		select {
		case <-ctx.Done():
			fmt.Fprintln(a.out)
			a.stale = true
			return policy.No, ctx.Err()
		case l, ok := <-a.lines:
			if !ok {
				return policy.No, a.failure()
			}
			line = l
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return policy.Yes, nil
		case "n", "no", "":
			return policy.No, nil
		case "a", "always":
			return policy.Always, nil
		case "v", "never":
			return policy.Never, nil
		}
		fmt.Fprintln(a.out, "unrecognised answer")
	}
}

// Throws away lines that arrived since a prompt was cancelled, which answered nothing
func (a *Asker) drain() {
	for {
		select {
		case _, ok := <-a.lines:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// Reports why input ended, the same error on every ask after the first
func (a *Asker) failure() error {
	if a.err == nil {
		a.err = <-a.errs
	}
	return a.err
}

func (a *Asker) format(call protocol.ToolCall) string {
	if a.Format != nil {
		return a.Format(call)
	}
	var args bytes.Buffer
	if err := json.Indent(&args, call.Args, "  ", "  "); err != nil {
		args.Reset()
		args.Write(call.Args)
	}
	return fmt.Sprintf("\n%s wants to run with arguments:\n  %s", call.Name, args.String())
}
