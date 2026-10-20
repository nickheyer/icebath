// Terminal prompt that asks a person to allow or refuse a tool call
package tty

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/policy"
)

const prompt = "Allow? [y]es, [n]o, [a]lways, ne[v]er: "

// Error every ask returns once the asker was closed
var ErrClosed = errors.New("tty: asker closed")

// Asker reading one line answers from a terminal
type Asker struct {
	// Renders the request above the prompt, the tool name and pretty arguments when nil
	Format func(req policy.Request) string
	in     io.Reader
	out    io.Writer
	wmu    sync.Mutex
	lines  chan string
	errs   chan error
	err    error
	stale  bool
	closed chan struct{}
	once   sync.Once
}

// Starts reading answers from in and writes prompts to out
func New(in io.Reader, out io.Writer) *Asker {
	a := &Asker{in: in, out: out, lines: make(chan string), errs: make(chan error, 1), closed: make(chan struct{})}
	go a.read(in)
	return a
}

func (a *Asker) read(in io.Reader) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		select {
		case a.lines <- sc.Text():
		case <-a.closed:
			return
		}
	}
	if err := sc.Err(); err != nil {
		a.errs <- err
	} else {
		a.errs <- io.EOF
	}
	close(a.lines)
}

// Stops answering, closing the input when it can be closed, so a UI may take over the terminal
func (a *Asker) Close() error {
	var err error
	a.once.Do(func() {
		close(a.closed)
		if c, ok := a.in.(io.Closer); ok {
			err = c.Close()
		}
	})
	return err
}

// Shows the request and keeps prompting until a recognised answer arrives, discarding answers typed to a cancelled prompt
func (a *Asker) Ask(ctx context.Context, req policy.Request) (policy.Verdict, error) {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	select {
	case <-a.closed:
		return policy.No, ErrClosed
	default:
	}
	if a.stale {
		a.drain()
		a.stale = false
	}
	fmt.Fprintln(a.out, a.format(req))
	for {
		fmt.Fprint(a.out, prompt)
		var line string
		select {
		case <-ctx.Done():
			fmt.Fprintln(a.out)
			a.stale = true
			return policy.No, ctx.Err()
		case <-a.closed:
			fmt.Fprintln(a.out)
			return policy.No, ErrClosed
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

// Default rendering with the agent, the gate reason, any review, the arguments and what always remembers
func (a *Asker) format(req policy.Request) string {
	if a.Format != nil {
		return a.Format(req)
	}
	var args bytes.Buffer
	if err := json.Indent(&args, req.Call.Args, "  ", "  "); err != nil {
		args.Reset()
		args.Write(req.Call.Args)
	}
	var sb strings.Builder
	sb.WriteString("\n")
	if req.Agent != "" {
		fmt.Fprintf(&sb, "[%s] ", req.Agent)
	}
	fmt.Fprintf(&sb, "%s wants to run with arguments:\n  %s", req.Call.Name, args.String())
	if req.Tool != nil && req.Tool.Description != "" {
		fmt.Fprintf(&sb, "\n  tool: %s", firstLine(req.Tool.Description))
	}
	if req.Reason != "" {
		fmt.Fprintf(&sb, "\n  asked because: %s", req.Reason)
	}
	if req.Review != nil {
		fmt.Fprintf(&sb, "\n  %s said %s: %s", req.Review.By, req.Review.Decision, req.Review.Reason)
	}
	if req.Key != "" {
		fmt.Fprintf(&sb, "\n  always or never remembers: %s", req.Key)
	}
	return sb.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
