package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	tools.RegisterInlineToolExecutor("read_terminal", guiExecutor(func(a *AIAgent, _ context.Context, args map[string]any, _ tools.InlineToolContext) (string, error) {
		read, err := paneReader("read_terminal_callback", a.readTerminalCallback)
		return tools.ReadTerminalTool(args["start_line"], args["count"], read), err
	}))
	tools.RegisterInlineToolExecutor("desktop_preview", guiExecutor(func(a *AIAgent, _ context.Context, args map[string]any, _ tools.InlineToolContext) (string, error) {
		if action, _ := args["action"].(string); strings.TrimSpace(action) != "read" {
			return tools.PreviewTool(a.env, args), nil
		}
		read, err := paneReader("read_preview_callback", a.readPreviewCallback)
		return tools.ReadPreviewTool(args["start"], args["count"], read), err
	}))
	tools.RegisterInlineToolExecutor("drive_preview", guiExecutor(func(a *AIAgent, _ context.Context, args map[string]any, _ tools.InlineToolContext) (string, error) {
		act, err := desktopRequest("drive_preview_callback", a.drivePreviewCallback)
		return tools.DrivePreviewTool(args, act), err
	}))
	tools.RegisterInlineToolExecutor("annotate_preview", guiExecutor(func(a *AIAgent, _ context.Context, args map[string]any, _ tools.InlineToolContext) (string, error) {
		act, err := desktopRequest("drive_preview_callback", a.drivePreviewCallback)
		return tools.AnnotatePreviewFromArgs(args, act), err
	}))
	tools.RegisterInlineToolExecutor("read_window_below", guiExecutor(func(a *AIAgent, _ context.Context, _ map[string]any, _ tools.InlineToolContext) (string, error) {
		read, err := paneReader("read_window_below_callback", a.readWindowBelowCallback)
		return tools.ReadWindowBelowTool(read), err
	}))
	tools.RegisterInlineToolExecutor("gui_tour", guiExecutor(func(a *AIAgent, _ context.Context, args map[string]any, _ tools.InlineToolContext) (string, error) {
		request, err := desktopRequest("tour_callback", a.tourCallback)
		return tools.TourTool(args, request), err
	}))
	tools.RegisterInlineToolExecutor("set_theme", guiExecutor(func(a *AIAgent, _ context.Context, args map[string]any, _ tools.InlineToolContext) (string, error) {
		request, err := desktopAnswer("theme_callback", a.themeCallback)
		return tools.SetThemeFromArgs(args, request), err
	}))
	tools.RegisterInlineToolExecutor("setup_beat", guiExecutor(func(a *AIAgent, ctx context.Context, args map[string]any, ictx tools.InlineToolContext) (string, error) {
		card, err := desktopAnswer("setup_beat_callback", a.setupBeatCallback)
		if err != nil {
			return "", err
		}
		endTurn := func() { a.InterruptWith("", InterruptOptions{ToolReason: reasonNewMessage}) }
		return tools.SetupBeatTool(tools.WithEnvHome(ctx, a.env), a.env, a.sessionDB, a.sessionID, ictx.ToolCallID, args, card, endTurn), nil
	}))
}

// Inline executor running a GUI callback tool against the agent that carries the callbacks
func guiExecutor(run func(a *AIAgent, ctx context.Context, args map[string]any, ictx tools.InlineToolContext) (string, error)) tools.InlineToolExecutor {
	return func(ctx context.Context, host tools.ExecutorHost, args map[string]any, ictx tools.InlineToolContext) (string, error) {
		h, ok := host.(agentExecutorHost)
		if !ok || h.AIAgent == nil {
			return "", fmt.Errorf("GUI callback tools run on an agent executor, not %T", host)
		}
		return run(h.AIAgent, ctx, args, ictx)
	}
}

// Raise of a GUI callback turned into the error the tool reports with its failure prefix
func recoverCallback(err *error) {
	if r := recover(); r != nil {
		if e, ok := r.(error); ok {
			*err = e
			return
		}
		*err = fmt.Errorf("%v", r)
	}
}

// Pane read over a start and count callback or a no argument one, nil when the agent carries none
func paneReader(name string, cb any) (tools.PaneReader, error) {
	switch fn := cb.(type) {
	case nil:
		return nil, nil
	case func(start, count *int) string:
		return func(window map[string]int) (raw string, err error) {
			defer recoverCallback(&err)
			var start, count *int
			if v, ok := window["start"]; ok {
				start = &v
			}
			if v, ok := window["count"]; ok {
				count = &v
			}
			return fn(start, count), nil
		}, nil
	case func() string:
		return func(map[string]int) (raw string, err error) {
			defer recoverCallback(&err)
			return fn(), nil
		}, nil
	}
	return nil, fmt.Errorf("%s is a %T, not a desktop read callback", name, cb)
}

// Raw reply request over a payload callback, nil when the agent carries none
func desktopRequest(name string, cb any) (tools.DesktopRequest, error) {
	switch fn := cb.(type) {
	case nil:
		return nil, nil
	case func(payload map[string]any) string:
		return func(payload map[string]any) (raw string, err error) {
			defer recoverCallback(&err)
			return fn(payload), nil
		}, nil
	}
	return nil, fmt.Errorf("%s is a %T, not a desktop request callback", name, cb)
}

// Decoded answer request over a payload callback, nil when the agent carries none
func desktopAnswer(name string, cb any) (tools.DesktopAnswer, error) {
	var call func(payload map[string]any) any
	switch fn := cb.(type) {
	case nil:
		return nil, nil
	case func(payload map[string]any) any:
		call = fn
	case func(payload map[string]any) *pyjson.Object:
		call = func(payload map[string]any) any {
			if answer := fn(payload); answer != nil {
				return answer
			}
			return nil
		}
	case func(payload map[string]any) map[string]any:
		call = func(payload map[string]any) any { return fn(payload) }
	default:
		return nil, fmt.Errorf("%s is a %T, not a desktop answer callback", name, cb)
	}
	return func(payload map[string]any) (answer map[string]any, err error) {
		defer recoverCallback(&err)
		answer, _ = pyjson.Plain(call(payload)).(map[string]any)
		return answer, nil
	}, nil
}
