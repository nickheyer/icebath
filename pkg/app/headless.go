package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// What a scripted run needs beside the configuration
type HeadlessOptions struct {
	Config harness.Config
	Resume string
	Dir    string
	// Prompt to run, read from Input when empty
	Prompt string
	// Objective a mission is started with instead of a plain prompt
	Mission string
	// Writes every bridge message as one JSON object per line instead of text
	Print bool
	// Answers every approval yes, every approval is denied otherwise
	Yes bool
	Out io.Writer
	Err io.Writer
	In  io.Reader
}

// One bridge message as the print mode writes it
type Line struct {
	Type     string             `json:"type"`
	Time     time.Time          `json:"time"`
	Run      string             `json:"run,omitempty"`
	Seq      int64              `json:"seq,omitempty"`
	Agent    string             `json:"agent,omitempty"`
	Parent   string             `json:"parent,omitempty"`
	Depth    int                `json:"depth,omitempty"`
	Round    int                `json:"round,omitempty"`
	Event    string             `json:"event,omitempty"`
	Kind     string             `json:"kind,omitempty"`
	Index    int                `json:"index,omitempty"`
	Delta    string             `json:"delta,omitempty"`
	Text     string             `json:"text,omitempty"`
	Call     *protocol.ToolCall `json:"call,omitempty"`
	Result   json.RawMessage    `json:"result,omitempty"`
	IsError  bool               `json:"is_error,omitempty"`
	Error    string             `json:"error,omitempty"`
	Usage    *protocol.Usage    `json:"usage,omitempty"`
	Duration float64            `json:"duration_ms,omitempty"`
	Data     json.RawMessage    `json:"data,omitempty"`
	Verdict  string             `json:"verdict,omitempty"`
	Status   int                `json:"status,omitempty"`
	Path     string             `json:"path,omitempty"`
}

// Turns a bridge message into a print line, false for messages that are not written
func encode(msg any) (Line, bool) {
	switch v := msg.(type) {
	case harness.EventMsg:
		ev := v.Event
		l := Line{Type: "event", Time: ev.Time, Run: v.Run, Seq: ev.Seq, Agent: ev.Agent, Parent: ev.Parent, Depth: ev.Depth, Round: ev.Round, Event: string(ev.Type), Text: ev.Text}
		if ev.Type == agent.EventModel {
			l.Kind = string(ev.Model.Type)
			l.Index = ev.Model.Index
			l.Delta = ev.Model.Delta
		}
		if ev.Call.Name != "" || ev.Call.ID != "" {
			c := ev.Call
			l.Call = &c
		}
		if ev.Result.CallID != "" {
			l.Result, _ = json.Marshal(protocol.Response{Content: ev.Result.Content}.Text())
			l.IsError = ev.Result.IsError
		}
		if ev.Err != nil {
			l.Error = ev.Err.Error()
		}
		if ev.Usage != (protocol.Usage{}) {
			u := ev.Usage
			l.Usage = &u
		}
		if ev.Duration > 0 {
			l.Duration = float64(ev.Duration) / float64(time.Millisecond)
		}
		if len(ev.Parts) > 0 {
			l.Text = protocol.Response{Content: ev.Parts}.Text()
		}
		if ev.Data != nil {
			if raw, err := json.Marshal(ev.Data); err == nil {
				l.Data = raw
			}
		}
		return l, true
	case harness.ProgressMsg:
		return Line{Type: "progress", Time: time.Now(), Run: v.Run, Kind: v.Progress.Stream, Delta: v.Progress.Text, Call: &protocol.ToolCall{ID: v.Progress.CallID, Name: v.Progress.Tool}}, true
	case harness.ApprovalMsg:
		c := v.Pending.Request.Call
		return Line{Type: "approval", Time: v.Pending.Asked, Run: v.Pending.Run, Agent: v.Pending.Agent, Call: &c, Text: v.Pending.Request.Reason, Kind: v.Pending.Request.Key}, true
	case harness.ApprovalDoneMsg:
		l := Line{Type: "approval_done", Time: time.Now(), Run: v.Run, Verdict: v.Verdict.String()}
		if v.Err != nil {
			l.Error = v.Err.Error()
		}
		return l, true
	case harness.RunDoneMsg:
		l := Line{Type: "run_done", Time: time.Now(), Run: v.Run, Text: v.Response.Text(), Duration: float64(v.Duration) / float64(time.Millisecond)}
		if v.Err != nil {
			l.Error = v.Err.Error()
		}
		u := v.Response.Usage
		l.Usage = &u
		return l, true
	case harness.AttemptMsg:
		l := Line{Type: "attempt", Time: v.Attempt.Started, Status: v.Attempt.Status, Duration: float64(v.Attempt.Duration) / float64(time.Millisecond), Text: v.Attempt.RequestID}
		if v.Attempt.Err != nil {
			l.Error = v.Attempt.Err.Error()
		}
		return l, true
	case harness.FailoverMsg:
		return Line{Type: "failover", Time: time.Now(), Index: v.From, Error: v.Err.Error()}, true
	case harness.SavedMsg:
		l := Line{Type: "saved", Time: time.Now(), Text: v.Session, Path: v.Path}
		if v.Err != nil {
			l.Error = v.Err.Error()
		}
		return l, true
	case harness.FactMsg:
		raw, _ := json.Marshal(v.Change)
		return Line{Type: "fact", Time: v.Change.Fact.Time, Data: raw}, true
	case harness.MissionMsg:
		raw, _ := json.Marshal(mission.Change{Mission: v.Mission, Op: v.Op})
		return Line{Type: "mission", Time: time.Now(), Data: raw}, true
	case harness.JobMsg:
		raw, _ := json.Marshal(v.Event.Job)
		return Line{Type: "job", Time: time.Now(), Kind: string(v.Event.Kind), Delta: string(v.Event.Data), Data: raw}, true
	case harness.MCPMsg:
		l := Line{Type: "mcp", Time: time.Now(), Text: v.Server, Kind: string(v.State)}
		if v.Err != nil {
			l.Error = v.Err.Error()
		}
		return l, true
	case harness.HookOutputMsg:
		return Line{Type: "hook", Time: time.Now(), Run: v.Run, Text: v.Text}, true
	}
	return Line{}, false
}

// Writer of the text stream, assistant text as it arrives and one line per tool
type textWriter struct {
	out  io.Writer
	err  io.Writer
	open bool
}

func (w *textWriter) write(msg any) {
	switch v := msg.(type) {
	case harness.EventMsg:
		ev := v.Event
		if ev.Depth > 0 {
			return
		}
		switch ev.Type {
		case agent.EventModel:
			if ev.Model.Type == protocol.EventTextDelta {
				io.WriteString(w.out, ev.Model.Delta)
				w.open = true
			}
		case agent.EventToolStart:
			w.newline()
			fmt.Fprintf(w.err, "▸ %s %s\n", ev.Call.Name, render.Truncate(strings.Join(strings.Fields(string(ev.Call.Args)), " "), 100))
		case agent.EventToolEnd:
			if ev.Err != nil {
				fmt.Fprintf(w.err, "✗ %s refused: %v\n", ev.Call.Name, ev.Err)
			} else if ev.Result.IsError {
				fmt.Fprintf(w.err, "✗ %s failed: %s\n", ev.Call.Name, render.Truncate(protocol.Response{Content: ev.Result.Content}.Text(), 200))
			}
		case agent.EventSubagentStart:
			w.newline()
			fmt.Fprintf(w.err, "☍ %s started\n", ev.Call.Name)
		case agent.EventNote, agent.EventHook:
			w.newline()
			fmt.Fprintf(w.err, "· %s\n", ev.Text)
		case agent.EventTurnEnd:
			w.newline()
		}
	case harness.ApprovalMsg:
		w.newline()
		fmt.Fprintf(w.err, "⚑ approval for %s %s\n", v.Pending.Request.Call.Name, render.Truncate(string(v.Pending.Request.Call.Args), 120))
	case harness.FactMsg:
		w.newline()
		fmt.Fprintf(w.err, "✎ %s %s: %s\n", v.Change.Op, v.Change.Fact.ID, v.Change.Fact.Title)
	}
}

func (w *textWriter) newline() {
	if w.open {
		io.WriteString(w.out, "\n")
		w.open = false
	}
}

// Runs one prompt or mission to completion without a terminal and returns the exit code
func Headless(ctx context.Context, opts HeadlessOptions) int {
	out, errw := opts.Out, opts.Err
	enc := json.NewEncoder(out)
	tw := &textWriter{out: out, err: errw}
	var app *harness.App
	send := func(msg any) {
		if v, ok := msg.(harness.ApprovalMsg); ok {
			verdict := policy.No
			if opts.Yes {
				verdict = policy.Yes
			}
			defer func() {
				if app != nil {
					app.Approver.Answer(v.Pending.ID, verdict)
				}
			}()
		}
		if opts.Print {
			if l, ok := encode(msg); ok {
				enc.Encode(l)
			}
			return
		}
		tw.write(msg)
	}
	var err error
	app, err = harness.Build(ctx, opts.Config, send, harness.BuildOptions{Resume: opts.Resume, Dir: opts.Dir})
	if err != nil {
		fmt.Fprintln(errw, "icebath:", err)
		return 2
	}
	defer app.Close(context.Background())
	if err := app.SessionStart(ctx, "headless"); err != nil {
		fmt.Fprintln(errw, "icebath:", err)
		return 2
	}
	prompt := strings.TrimSpace(opts.Prompt)
	if prompt == "" && opts.Mission == "" && opts.In != nil {
		data, err := io.ReadAll(bufio.NewReader(opts.In))
		if err != nil {
			fmt.Fprintln(errw, "icebath:", err)
			return 2
		}
		prompt = strings.TrimSpace(string(data))
	}
	missionID := ""
	if opts.Mission != "" {
		ms, err := app.Missions.Create(opts.Mission)
		if err != nil {
			fmt.Fprintln(errw, "icebath:", err)
			return 2
		}
		missionID = ms.ID
		if prompt == "" {
			prompt = opts.Mission
		}
	}
	if prompt == "" {
		fmt.Fprintln(errw, "icebath: nothing to run, give a prompt or a mission")
		return 2
	}
	if app.Session.Title() == "" {
		app.Session.SetTitle(render.Summary(prompt, 60))
	}
	run, err := app.Run(ctx, missionID, []protocol.Part{protocol.Text{Text: prompt}})
	if err != nil {
		fmt.Fprintln(errw, "icebath:", err)
		return 2
	}
	finished, _ := app.Control.Done(run.ID)
	select {
	case <-finished:
	case <-ctx.Done():
		app.Control.Shutdown()
		fmt.Fprintln(errw, "icebath: cancelled")
		return 130
	}
	if !opts.Print {
		tw.newline()
		if run.Err() != nil {
			fmt.Fprintln(errw, "icebath:", run.Err())
		}
		usage := run.Response().Usage
		fmt.Fprintf(errw, "session %s · %s in %s out · %s\n", app.Session.ID(), render.Tokens(usage.Input), render.Tokens(usage.Output), render.Elapsed(run.Elapsed(run.Finished())))
		if changed := app.Workspace.Changed(); len(changed) > 0 {
			paths := make([]string, 0, len(changed))
			for _, c := range changed {
				paths = append(paths, shortPath(app.Workspace.Dir(), c.Path))
			}
			fmt.Fprintf(errw, "▣ %d changed: %s\n", len(paths), strings.Join(paths, ", "))
		}
	}
	if run.Err() != nil {
		return 1
	}
	return 0
}

// Path relative to the working directory when it lies inside it
func shortPath(dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}
