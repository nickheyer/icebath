package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/internal/profile"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	linesv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lines/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lines/v1/linesv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Most characters of a running line's output a chat holds, its end
const held = 64 << 10

// Rows of output a line shows in a chat, its last ones, the rest counted above them
const shownRows = 40

// Runs the lines a person types in a chat after a !, in their own shell in the chat's folder
type lines struct{}

func (lines) Views() []toolsapp.View { return nil }

func (lines) Lines(conn *connect.Client) []toolsapp.Liner {
	service := linesv1connect.NewLineServiceClient(conn)
	return []toolsapp.Liner{{
		Mark: "!",
		Hint: func(env *cards.Env) string { return "! runs a line in " + filepath.Base(env.Shell().Path) },
		Run: func(ctx context.Context, run toolsapp.Run, told func(toolsapp.Ran)) func() {
			return start(ctx, service, run, told)
		},
		Kept: func(ctx context.Context, session, profile string) ([]toolsapp.Ran, error) {
			return kept(ctx, service, session, profile)
		},
		Shown: shown,
	}}
}

// Starts the line on the server, telling what it printed as it comes and how it ended; the stop it answers ends the call, which stops it
func start(ctx context.Context, service linesv1connect.LineServiceClient, run toolsapp.Run, told func(toolsapp.Ran)) func() {
	ctx, stop := context.WithCancel(ctx)
	ran := toolsapp.Ran{Line: run.Line}
	go func() {
		defer stop()
		stream, err := service.Run(ctx, &linesv1.RunRequest{SessionId: run.Session, Profile: run.Profile, Line: run.Line, Folder: run.Folder,
			Shell: run.Shell})
		if err != nil {
			ran.Done, ran.Error = true, "The line was not run. "+strs.Full(tui.Reason(err))
			told(ran)
			return
		}
		defer stream.Close()
		for {
			event, err := stream.Receive()
			switch {
			case errors.Is(err, io.EOF):
				ran.Done, ran.Stopped = true, ctx.Err() != nil
				told(ran)
				return
			case err != nil && ctx.Err() != nil:
				ran.Done, ran.Stopped = true, true
				told(ran)
				return
			case err != nil:
				ran.Done, ran.Error = true, "The line stopped before it ended. "+strs.Full(tui.Reason(err))
				told(ran)
				return
			}
			switch kind := event.GetKind().(type) {
			case *linesv1.RunEvent_Output:
				ran.Output += kind.Output
				if len(ran.Output) > held {
					ran.Output, ran.Cut = ran.Output[len(ran.Output)-held:], true
				}
			case *linesv1.RunEvent_Done:
				ran = ranOf(kind.Done)
				ran.Done = true
			}
			told(ran)
			if ran.Done {
				return
			}
		}
	}()
	return stop
}

// The lines run in the session before, oldest first
func kept(ctx context.Context, service linesv1connect.LineServiceClient, session, profile string) ([]toolsapp.Ran, error) {
	response, err := service.List(ctx, &linesv1.ListRequest{SessionId: session, Profile: profile})
	if err != nil {
		return nil, err
	}
	var out []toolsapp.Ran
	for _, line := range response.GetLines() {
		ran := ranOf(line)
		ran.Done = true
		out = append(out, ran)
	}
	return out, nil
}

func ranOf(line *linesv1.Line) toolsapp.Ran {
	return toolsapp.Ran{Time: line.GetTime().AsTime(), Line: line.GetCommand(), Output: line.GetOutput(), Cut: line.GetCut(), Code: int(line.GetCode()),
		Stopped: line.GetStopped(), Error: line.GetError(), Folder: line.GetFolder()}
}

// What a chat shows of a line: the line under the mark of the shell, then what it printed, its last rows while it printed more, and how
// it ended when it did not end well or printed nothing
func shown(ran toolsapp.Ran, width int, t theme.Theme, stop string) *toolsapp.Shown {
	head := render.Cool("! ", t) + ran.Line
	// Where it ran, as a shell's prompt says it, once the server said where
	if ran.Folder != "" {
		head += render.Dim("  in "+profile.Tilde(ran.Folder), t)
	}
	var rows []string
	for _, line := range strings.Split(strings.TrimRight(ansi.Strip(ran.Output), "\n"), "\n") {
		if ran.Output == "" {
			break
		}
		line = strings.ReplaceAll(line, "\t", "    ")
		rows = append(rows, strings.Split(ansi.Hardwrap(line, max(width-2, 8), true), "\n")...)
	}
	before := 0
	if len(rows) > shownRows {
		before, rows = len(rows)-shownRows, rows[len(rows)-shownRows:]
	}
	if before > 0 || ran.Cut {
		rows = append([]string{render.Dim(fmt.Sprintf("%d lines before these are not shown.", before), t)}, rows...)
		if before == 0 {
			rows[0] = render.Dim("What came before these lines is not shown.", t)
		}
	}
	switch {
	case ran.Error != "":
		rows = append(rows, render.Bad(ran.Error, t))
	case !ran.Done:
		rows = append(rows, render.Dim("Running… "+stop+" stops it.", t))
	case ran.Stopped:
		rows = append(rows, render.Warm("You stopped it.", t))
	case ran.Code != 0:
		rows = append(rows, render.Warm(fmt.Sprintf("It ended with exit code %d.", ran.Code), t))
	case len(rows) == 0:
		rows = append(rows, render.Dim("It printed nothing.", t))
	}
	return &toolsapp.Shown{Head: head, Folded: fmt.Sprintf("%s  %s", head, render.Dim(render.Count(len(rows), "line"), t)), Rows: rows, Open: true}
}
