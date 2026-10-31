package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
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
	// Kind the session runs as, none when empty
	Kind string
	// Workflow run with the prompt as its argument instead of a plain run
	Workflow string
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
	// Number of the approval an approval line asks for and an approval_done line answers
	Approval int `json:"approval,omitempty"`
	// Endpoint an attempt line hit
	URL string `json:"url,omitempty"`
	// Id the provider gave the response of a turn_end, run_end or run_done line
	ResponseID string `json:"response_id,omitempty"`
	// Neutral stop reason of that response and the wire value it came from
	Stop    string `json:"stop,omitempty"`
	RawStop string `json:"raw_stop,omitempty"`
	// Count a progress line reports done and the count it works toward
	Done  int `json:"done,omitempty"`
	Total int `json:"total,omitempty"`
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
		if ev.Response != nil {
			l.ResponseID, l.Stop, l.RawStop = ev.Response.ID, string(ev.Response.StopReason), ev.Response.RawStopReason
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
		return Line{Type: "progress", Time: time.Now(), Run: v.Run, Kind: v.Progress.Stream, Delta: v.Progress.Text, Done: v.Progress.Done, Total: v.Progress.Total, Call: &protocol.ToolCall{ID: v.Progress.CallID, Name: v.Progress.Tool}}, true
	case harness.ApprovalMsg:
		c := v.Pending.Request.Call
		return Line{Type: "approval", Time: v.Pending.Asked, Run: v.Pending.Run, Agent: v.Pending.Agent, Approval: v.Pending.ID, Call: &c, Text: v.Pending.Request.Reason, Kind: v.Pending.Request.Key}, true
	case harness.ApprovalDoneMsg:
		l := Line{Type: "approval_done", Time: time.Now(), Run: v.Run, Approval: v.ID, Verdict: v.Verdict.String()}
		if v.Err != nil {
			l.Error = v.Err.Error()
		}
		return l, true
	case harness.RunDoneMsg:
		l := Line{Type: "run_done", Time: time.Now(), Run: v.Run, Text: v.Response.Text(), Duration: float64(v.Duration) / float64(time.Millisecond), ResponseID: v.Response.ID, Stop: string(v.Response.StopReason), RawStop: v.Response.RawStopReason}
		if v.Err != nil {
			l.Error = v.Err.Error()
		}
		u := v.Response.Usage
		l.Usage = &u
		return l, true
	case harness.AttemptMsg:
		l := Line{Type: "attempt", Time: v.Attempt.Started, Status: v.Attempt.Status, Duration: float64(v.Attempt.Duration) / float64(time.Millisecond), Text: v.Attempt.RequestID, URL: v.Attempt.URL}
		if v.Attempt.Err != nil {
			l.Error = v.Attempt.Err.Error()
		}
		return l, true
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
	case harness.NoticeMsg:
		return Line{Type: "notice", Time: time.Now(), Kind: levelName(v.Level), Text: v.Text, Agent: v.Flow}, true
	case harness.OpenedMsg:
		return Line{Type: "session_opened", Time: time.Now(), Text: v.Live.Session.ID(), Agent: v.Flow}, true
	case harness.NoteMsg:
		return Line{Type: "note", Time: time.Now(), Kind: levelName(v.Level), Text: v.Text, Agent: v.Flow, Run: v.Session}, true
	case harness.FlowDoneMsg:
		l := Line{Type: "workflow_done", Time: time.Now(), Text: v.Name, Agent: v.ID, Duration: float64(v.Duration) / float64(time.Millisecond)}
		if v.Err != nil {
			l.Error = v.Err.Error()
		}
		return l, true
	}
	return Line{}, false
}

// Name of a notice level as the print lines carry it
func levelName(l harness.Level) string {
	switch l {
	case harness.Success:
		return "success"
	case harness.Warn:
		return "warn"
	case harness.Error:
		return "error"
	}
	return "info"
}

// Writer of the text stream, assistant text as it arrives and one line per tool
type textWriter struct {
	out  io.Writer
	err  io.Writer
	open bool
}

// Theme whose marks the text output borrows
var printTheme = theme.Blackwall()

func (w *textWriter) write(msg any) {
	t := printTheme
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
			fmt.Fprintf(w.err, "%s %s %s\n", t.Glyphs.Cursor, ev.Call.Name, render.Truncate(strings.Join(strings.Fields(string(ev.Call.Args)), " "), 100, t))
		case agent.EventToolEnd:
			if ev.Err != nil {
				fmt.Fprintf(w.err, "%s %s refused: %v\n", t.Glyphs.Error, ev.Call.Name, ev.Err)
			} else if ev.Result.IsError {
				fmt.Fprintf(w.err, "%s %s failed: %s\n", t.Glyphs.Error, ev.Call.Name, render.Truncate(protocol.Response{Content: ev.Result.Content}.Text(), 200, t))
			}
		case agent.EventSubagentStart:
			w.newline()
			fmt.Fprintf(w.err, "%s %s started\n", t.Glyphs.Child, ev.Call.Name)
		case agent.EventNote, agent.EventHook:
			w.newline()
			fmt.Fprintf(w.err, "%s %s\n", t.Glyphs.Note, ev.Text)
		case agent.EventTurnEnd:
			w.newline()
			if ev.Response != nil {
				if text := ev.Response.StopText(); text != "" {
					fmt.Fprintf(w.err, "%s %s\n", t.Glyphs.Note, text)
				}
			}
		}
	case harness.ApprovalMsg:
		w.newline()
		fmt.Fprintf(w.err, "%s approval for %s %s\n", t.Glyphs.Flag, v.Pending.Request.Call.Name, render.Truncate(string(v.Pending.Request.Call.Args), 120, t))
	case harness.FactMsg:
		w.newline()
		fmt.Fprintf(w.err, "%s %s %s: %s\n", t.Glyphs.Record, v.Change.Op, v.Change.Fact.ID, v.Change.Fact.Title)
	case harness.NoticeMsg:
		w.newline()
		fmt.Fprintf(w.err, "%s %s\n", t.Glyphs.Note, v.Text)
	case harness.NoteMsg:
		w.newline()
		fmt.Fprintf(w.err, "%s %s\n", t.Glyphs.Note, v.Text)
	case harness.FlowDoneMsg:
		w.newline()
		if v.Err != nil {
			fmt.Fprintf(w.err, "%s workflow %s failed: %v\n", t.Glyphs.Error, v.ID, v.Err)
			return
		}
		fmt.Fprintf(w.err, "%s workflow %s done in %s\n", t.Glyphs.Success, v.ID, render.Elapsed(v.Duration))
	}
}

func (w *textWriter) newline() {
	if w.open {
		io.WriteString(w.out, "\n")
		w.open = false
	}
}

// Runs a workflow to its end with the prompt as its argument and returns the exit code
func runWorkflow(ctx context.Context, app *harness.App, name, arg string, errw io.Writer, print bool) int {
	w, ok := harness.LookupWorkflow(name)
	if !ok {
		var names []string
		for _, have := range harness.Workflows() {
			names = append(names, have.Name)
		}
		fmt.Fprintf(errw, "icebath: no workflow %q, linked ones are %s\n", name, strings.Join(names, ", "))
		return 2
	}
	f, err := app.Flows.Start(ctx, w, app.Current(), arg)
	if err != nil {
		fmt.Fprintln(errw, "icebath:", err)
		return 2
	}
	select {
	case <-f.Done():
	case <-ctx.Done():
		app.Flows.Shutdown()
		fmt.Fprintln(errw, "icebath: cancelled")
		return 130
	}
	if f.Err() != nil {
		if !print {
			fmt.Fprintln(errw, "icebath:", f.Err())
		}
		return 1
	}
	return 0
}

// Runs one prompt, mission or workflow to completion without a terminal and returns the exit code
func Headless(ctx context.Context, opts HeadlessOptions) int {
	out, errw, t := opts.Out, opts.Err, printTheme
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
	if opts.Kind != "" {
		if err := app.SetKindOn(app.Current(), opts.Kind); err != nil {
			fmt.Fprintln(errw, "icebath:", err)
			return 2
		}
	}
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
	if opts.Workflow != "" {
		return runWorkflow(ctx, app, opts.Workflow, prompt, errw, opts.Print)
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
		app.Session.SetTitle(render.Summary(prompt, 60, t))
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
		fmt.Fprintf(errw, "session %s %s %s in %s out %s %s\n", app.Session.ID(), t.Glyphs.Sep, render.Tokens(usage.Input), render.Tokens(usage.Output), t.Glyphs.Sep, render.Elapsed(run.Elapsed(run.Finished())))
	}
	if run.Err() != nil {
		return 1
	}
	return 0
}
