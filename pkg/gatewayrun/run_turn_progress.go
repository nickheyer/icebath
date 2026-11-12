package gatewayrun

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Progress queue item asking the sender to bump the repeat counter of the last line
type progressDedup struct {
	msg   string
	count int
}

// Progress queue item asking the sender to start a fresh progress bubble
type progressReset struct{}

// Closing code fence ending the text, so a repeat counter goes on its own line
var closingFenceRe = regexp.MustCompile("(?:^|\n)[ \t]*```[ \t]*\r?$")

// Line with its repeat counter, placed after a closing code fence on its own line
func appendProgressRepeatCount(message string, occurrences int) string {
	suffix := fmt.Sprintf("(×%d)", occurrences)
	body := strings.TrimRight(message, "\r\n")
	if closingFenceRe.MatchString(body) {
		lineEnd := "\n"
		if strings.HasSuffix(message, "\r\n") {
			lineEnd = "\r\n"
		}
		return body + lineEnd + suffix
	}
	return message + " " + suffix
}

// Whether the turn's viewers read the canonical event stream, a local route or a detached conversation
func localViewerTurn(t *TurnRunner) bool {
	if t.approvalOwner == nil || t.ctx.source == nil {
		return false
	}
	return t.ctx.source.Platform == gateway.PlatformLocal || gateway.DetachedRouteOf(t.ctx.source) != nil
}

// Status kind a viewer reads, auto compaction progress retagged as compacting
func viewerStatusKind(kind string, text any) string {
	if kind == "lifecycle" && agent.IsCompactionProgressStatus(text) {
		return "compacting"
	}
	return kind
}

// Hops a coroutine from the agent goroutine onto the gateway loop, nil with a logged reason when the loop refuses
func (t *TurnRunner) schedule(fn func(ctx context.Context) (any, error), logMessage string) *concurrentFuture {
	return t.scheduleOn(t.ctx.loopForStep, fn, logMessage)
}

// Hops a coroutine onto the given loop, nil with a logged reason when the loop is missing or refuses
func (t *TurnRunner) scheduleOn(loop gateway.EventLoop, fn func(ctx context.Context) (any, error), logMessage string) *concurrentFuture {
	if loop == nil {
		runLog().Debug(fmt.Sprintf("%s: loop is None", logMessage))
		return nil
	}
	fut := newConcurrentFuture()
	taskCtx := context.WithoutCancel(t.turnCtx)
	err := loop.CallSoonThreadsafe(func() {
		fut.setRunningOrNotifyCancel()
		task := spawnGatewayTask(taskCtx, "turn-callback", fn)
		task.addDoneCallback(func(done *gatewayFuture) {
			result, taskErr := done.wait(context.Background())
			fut.set(result, taskErr)
		})
	})
	if err != nil {
		runLog().Debug(fmt.Sprintf("%s: %s", logMessage, err))
		return nil
	}
	return fut
}

// Whether the user stopped the turn's agent
func (t *TurnRunner) agentInterrupted() bool {
	a := t.ctx.currentAgent()
	return a != nil && a.IsInterrupted()
}

// Drops every queued progress item
func (t *TurnRunner) drainProgressQueue() {
	q := t.ctx.progressQueue
	if q == nil {
		return
	}
	for !q.empty() {
		q.getNowait()
	}
}

// Remembers a delivered progress or status message id for end of turn cleanup
func (t *TurnRunner) trackProgressResult(result any) {
	sent, ok := result.(gateway.SendResult)
	if t.ctx.cleanupProgress && ok && sent.Success && pyrepr.Truthy(sent.MessageID) {
		t.ctx.holderMu.Lock()
		t.ctx.cleanupMsgIDs = append(t.ctx.cleanupMsgIDs, pyrepr.Str(sent.MessageID))
		t.ctx.holderMu.Unlock()
	}
}

// Tracks the message id a settled send future delivered, a failed send tracking nothing
func (t *TurnRunner) trackFutureCleanupID(fut *concurrentFuture) {
	result, err := fut.outcome()
	if err != nil {
		return
	}
	t.trackProgressResult(result)
}

// Renders one clean failure notice for a subagent that ended in a failure status
func (t *TurnRunner) progressSubagentNotice(preview any, kwargs map[string]any) {
	tc := t.ctx
	status := kwargs["status"]
	defer func() {
		if r := recover(); r != nil {
			runLog().Debug(fmt.Sprintf("subagent failure notice failed: %v", r))
		}
	}()
	if !agent.IsSubagentFailureStatus(status) || !tc.runStillCurrent() {
		return
	}
	errText := kwargs["summary"]
	if !pyrepr.Truthy(errText) {
		errText = preview
	}
	goal := ""
	if g, ok := kwargs["goal"].(string); ok {
		goal = g
	}
	line := agent.FormatSubagentFailureLine(goal, pyrepr.Str(status), errText, kwargs["duration_seconds"], kwargs["failure_reason"])
	_, err := config.RenderNotification(t.turnCtx, func() error {
		t.schedule(func(ctx context.Context) (any, error) {
			return nil, t.runner.deliverPlatformNotice(ctx, tc.source, line)
		}, "subagent failure notice scheduling error")
		return nil
	}, string(tc.source.Platform), false, tc.userConfig)
	if err != nil {
		runLog().Debug(fmt.Sprintf("subagent failure notice failed: %s", err))
	}
}

// Stashes the live status phrase on the adapter for its typing refresh to render
func (t *TurnRunner) progressLiveStatus(eventType string, toolName string, args map[string]any) {
	tc := t.ctx
	adapter := tc.liveStatusAdapter
	if adapter == nil || tc.liveStatusMode == "off" || toolName == "_thinking" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			runLog().Debug(fmt.Sprintf("live status update failed: %v", r))
		}
	}()
	if eventType == "tool.started" && toolName != "" && tc.runStillCurrent() {
		var phraseArgs map[string]any
		if tc.liveStatusMode == "full" {
			phraseArgs = args
		}
		adapter.Base().SetStatusText(tc.source.ChatID, tools.BuildStatusPhrase(t.toolsEnv(), toolName, phraseArgs, 49))
	} else if eventType == "tool.completed" {
		adapter.Base().SetStatusText(tc.source.ChatID, "")
	}
}

// Queues the one time verbose hint the first time a tool runs past the long tool threshold in all mode
func (t *TurnRunner) progressOnboardingHint(kwargs map[string]any) {
	tc := t.ctx
	duration := 0.0
	switch d := kwargs["duration"].(type) {
	case float64:
		duration = d
	case int:
		duration = float64(d)
	}
	if duration < tc.longToolThresholdS || tc.progressMode != "all" {
		return
	}
	cfg := gateway.LoadGatewayConfigMap(t.turnCtx, "")
	gate, _ := config.CfgGet(cfg, "display", "tool_progress_command")
	if !isTruthyValue(gate, false) || tools.IsSeen(cfg, tools.OnboardingToolProgressFlag) {
		return
	}
	tc.longToolHintFired = true
	tc.progressQueue.put(tools.ToolProgressHintGateway())
	tools.MarkSeen(t.turnCtx, filepath.Join(home.Dir(t.turnCtx), "config.yaml"), tools.OnboardingToolProgressFlag)
}

// Truthy reading of a config value with the shared truthy string set, default for None
func isTruthyValue(value any, def bool) bool {
	if value == nil {
		return def
	}
	if s, ok := value.(string); ok {
		switch pystr.Lower(pystr.Strip(s)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyrepr.Truthy(value)
}

// One line preview budget of the all and new modes, 40 when unset
func previewCap() int {
	if pl := tools.GetToolPreviewMaxLen(); pl > 0 {
		return pl
	}
	return 40
}

// Full and short fenced blocks for a terminal command on a code block platform, empty otherwise
func (t *TurnRunner) progressTerminalBlocks(adapter gateway.PlatformAdapter, toolName string, args map[string]any, emoji string) (*string, *string) {
	command, _ := args["command"].(string)
	if adapter == nil || !adapter.Base().Traits.SupportsCodeBlocks || toolName != "terminal" || args == nil || pystr.Strip(command) == "" {
		return nil, nil
	}
	cmdFull := pystr.Rstrip(command)
	header := ""
	if !t.ctx.lastWasTerminalBlock {
		header = i18n.T(t.turnCtx, "gateway.progress.tool_head", "", map[string]any{"emoji": emoji, "tool": toolName}) + "\n"
	}
	limit := previewCap()
	lines := pystr.Splitlines(cmdFull, false)
	cmdShort := cmdFull
	if len(lines) > 0 {
		cmdShort = lines[0]
	}
	if pystr.Len(cmdShort) > limit {
		cmdShort = pystr.Slice(cmdShort, 0, limit-3) + "..."
	} else if len(lines) > 1 {
		cmdShort += " ..."
	}
	full := header + "```\n" + cmdFull + "\n```"
	short := header + "```\n" + cmdShort + "\n```"
	return &full, &short
}

// Progress line for a started tool, nil in verbose mode where the line is queued directly
func (t *TurnRunner) progressBuildMessage(toolName string, preview string, args map[string]any) *string {
	tc := t.ctx
	emoji := tools.GetToolEmoji(t.toolsEnv(), toolName, "⚙️")
	adapter, err := t.runner.deliveryAdapterFor(t.turnCtx, tc.source)
	if err != nil {
		adapter = nil
	}
	codeFull, codeShort := t.progressTerminalBlocks(adapter, toolName, args, emoji)
	verbose := tc.progressMode == "verbose"
	code := codeShort
	if verbose {
		code = codeFull
	}
	tc.lastWasTerminalBlock = code != nil
	if verbose {
		var line string
		switch {
		case code != nil:
			line = *code
		case len(args) > 0:
			supports := adapter != nil && adapter.Base().Traits.SupportsCodeBlocks
			ordered := pyjson.NewObject()
			keys := make([]any, 0, len(args))
			for _, k := range slices.Sorted(maps.Keys(args)) {
				keys = append(keys, k)
				ordered.Set(k, args[k])
			}
			argsStr := tools.VerboseToolArgsText(ordered, tools.GetToolPreviewMaxLen(), supports)
			line = i18n.T(t.turnCtx, "gateway.progress.tool_verbose", "", map[string]any{"emoji": emoji, "tool": toolName,
				"keys": pyrepr.Repr(keys), "args": argsStr})
		case preview != "":
			line = i18n.T(t.turnCtx, "gateway.progress.tool_preview", "", map[string]any{"emoji": emoji, "tool": toolName, "preview": preview})
		default:
			line = i18n.T(t.turnCtx, "gateway.progress.tool_pending", "", map[string]any{"emoji": emoji, "tool": toolName})
		}
		tc.progressQueue.put(line)
		return nil
	}
	if code != nil {
		return code
	}
	if preview == "" {
		line := i18n.T(t.turnCtx, "gateway.progress.tool_pending", "", map[string]any{"emoji": emoji, "tool": toolName})
		return &line
	}
	prepared := tools.PrepareToolPreview(t.toolsEnv(), toolName, args, preview, previewCap())
	if adapter != nil {
		preview = adapter.FormatToolPreview(prepared)
	} else {
		preview = prepared.Text
	}
	verb := tools.GetToolVerb(toolName)
	var line string
	switch {
	case verb == "":
		line = i18n.T(t.turnCtx, "gateway.progress.tool_preview", "", map[string]any{"emoji": emoji, "tool": toolName, "preview": preview})
	case tools.VerbDropsPreview(toolName):
		line = emoji + " " + verb
	default:
		line = emoji + " " + verb + tools.ToolVerbConnector(toolName) + preview
	}
	return &line
}

// Dedups consecutive identical lines then routes to the native stream bubble or the progress queue
func (t *TurnRunner) progressEmit(msg string) {
	tc := t.ctx
	sc := tc.streamConsumer()
	native := sc != nil && sc.AcceptsToolProgress()
	if tc.lastProgressMsg != nil && msg == *tc.lastProgressMsg {
		tc.repeatCount++
		if native {
			sc.OnToolProgress(appendProgressRepeatCount(msg, tc.repeatCount+1))
		} else {
			tc.progressQueue.put(progressDedup{msg: msg, count: tc.repeatCount})
		}
		return
	}
	last := msg
	tc.lastProgressMsg, tc.repeatCount = &last, 0
	if native {
		sc.OnToolProgress(msg)
	} else {
		tc.progressQueue.put(msg)
	}
}

// Tools env of the turn, its session env or the active home, read through the policy scope while the turn runs in it
func (t *TurnRunner) toolsEnv() *tools.Env {
	env := t.ctx.sessionEnv
	if env == nil {
		env = &tools.Env{Home: home.Dir(t.turnCtx), TerminalScope: tools.GetTerminalScope(t.turnCtx)}
	}
	if scope := t.policyScope.Load(); scope != nil && env.TerminalScope != scope {
		scoped := *env
		scoped.TerminalScope = scope
		return &scoped
	}
	return env
}
