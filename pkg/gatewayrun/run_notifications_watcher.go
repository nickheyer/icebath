package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	tools.ArmProcessWatcher = armCurrentProcessWatcher
}

// Arms a mid turn watcher on the gateway runner of this process, false outside a serving gateway
func armCurrentProcessWatcher(watcher map[string]any) bool {
	runner, ok := gateway.CurrentGatewayRunner().(*GatewayRunner)
	if !ok || runner == nil {
		return false
	}
	return runner.armProcessWatcher(watcher)
}

// Tool environment of the active profile scope for registry reads and output redaction
func watcherToolsEnv(ctx context.Context) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
}

// Last limit runes of process output through the transform seam and the secret redactors
func redactedOutputTail(ctx context.Context, fields tools.ProcessWatchFields, taskID string, limit int) string {
	if fields.OutputBuffer == "" {
		return ""
	}
	runes := []rune(fields.OutputBuffer)
	newOutput := tools.StripANSI(string(runes[max(0, len(runes)-limit):]))
	if newOutput == "" {
		return ""
	}
	env := watcherToolsEnv(ctx)
	newOutput = tools.TransformProcessOutput(env, newOutput, fields.Command, fields.ExitCode, taskID)
	newOutput = tools.RedactTerminalCommandOutput(env, newOutput, fields.Command, false)
	return redactGatewayUserFacingSecrets(ctx, newOutput)
}

// Whether the session that launched the watcher's process is still inside a turn on its adapter
func (r *GatewayRunner) launchingTurnActive(ctx context.Context, platformName string, watcher map[string]any) (bool, error) {
	sessionKey := pystr.Strip(truthyText(watcher["session_key"]))
	if sessionKey == "" {
		return false, nil
	}
	source, err := r.buildProcessEventSource(ctx, watcher)
	if err != nil {
		return false, err
	}
	adapter, err := r.resolveInjectionAdapter(ctx, platformName, source)
	if err != nil {
		return false, err
	}
	if adapter == nil || adapter.Base() == nil {
		return false, nil
	}
	return adapter.Base().HasActiveSession(sessionKey), nil
}

// Sends a watcher notice to the watcher's chat as non conversational output, a send failure only logged
func (r *GatewayRunner) sendWatcherMessage(ctx context.Context, platformName string, chatID, threadID any, messageText string, watcher map[string]any) error {
	source, err := r.buildProcessEventSource(ctx, watcher)
	if err != nil {
		return err
	}
	adapter, err := r.resolveInjectionAdapter(ctx, platformName, source)
	if err != nil {
		return err
	}
	chat := pyrepr.Str(pyrepr.Or(chatID, ""))
	if adapter == nil || !pyrepr.Truthy(chatID) {
		return nil
	}
	var sendMeta map[string]any
	if pyrepr.Truthy(threadID) {
		sendMeta = map[string]any{"thread_id": threadID}
	}
	_, sendErr := adapter.Send(ctx, chat, messageText, "", nonConversationalMetadata(sendMeta, gateway.Platform(platformName)))
	logSuppressed(slog.LevelError, sendErr, "Watcher delivery error: %v")
	return nil
}

// Synthetic completion event of an agent notify watcher, output snapped to a line boundary and redacted
func buildProcessCompletionEvent(ctx context.Context, watcher map[string]any, fields tools.ProcessWatchFields, sessionID string) map[string]any {
	env := watcherToolsEnv(ctx)
	raw := ""
	if fields.OutputBuffer != "" {
		raw = tools.StripANSI(fields.OutputBuffer)
	}
	if raw != "" {
		raw = tools.TransformProcessOutput(env, raw, fields.Command, fields.ExitCode, fields.TaskID)
	}
	raw = tools.RedactTerminalCommandOutput(env, raw, fields.Command, false)
	const limit = 2000
	out := raw
	if utf8.RuneCountInString(raw) > limit {
		runes := []rune(raw)
		tail := string(runes[len(runes)-limit:])
		if nl := strings.Index(tail, "\n"); nl != -1 {
			tail = tail[nl+1:]
		}
		out = fmt.Sprintf("[… output truncated — showing last %d chars]\n%s", utf8.RuneCountInString(tail), tail)
	}
	evt := map[string]any{"type": "completion", "session_id": sessionID}
	for _, field := range watcherRouteFields {
		if v, ok := watcher[field]; ok {
			evt[field] = v
		} else {
			evt[field] = ""
		}
	}
	var messageID any
	if id := pystr.Strip(truthyText(watcher["message_id"])); id != "" {
		messageID = id
	}
	var exitCode any
	if fields.ExitCode != nil {
		exitCode = *fields.ExitCode
	}
	parent := pyrepr.Or(watcher["parent_session_id"], pyrepr.Or(fields.ParentSessionID, ""))
	evt["owner_task_id"] = fields.OwnerTaskID
	evt["task_id"] = fields.TaskID
	evt["message_id"] = messageID
	evt["started_at"] = fields.StartedAt
	evt["command"] = redactGatewayUserFacingSecrets(ctx, fields.Command)
	evt["exit_code"] = exitCode
	evt["completion_reason"] = fields.CompletionReason
	evt["termination_source"] = fields.TerminationSource
	evt["output"] = redactGatewayUserFacingSecrets(ctx, out)
	evt["parent_session_id"] = parent
	return evt
}

// Seconds since the process started, never negative
func processDuration(fields tools.ProcessWatchFields) *float64 {
	elapsed := max(0.0, float64(time.Now().UnixNano())/1e9-fields.StartedAt)
	return &elapsed
}

// Completion message, the concise status line with the bounded output tail under it in the raw output modes
func formatProcessFinalMessage(ctx context.Context, sessionID string, fields tools.ProcessWatchFields, notifyMode string) string {
	newOutput := redactedOutputTail(ctx, fields, fields.TaskID, 1000)
	duration := processDuration(fields)
	command := redactGatewayUserFacingSecrets(ctx, fields.Command)
	if notifyMode == "concise" {
		return formatConciseProcessNotification(ctx, sessionID, command, fields.ExitCode, newOutput, duration)
	}
	header := formatConciseProcessNotification(ctx, sessionID, command, fields.ExitCode, "", duration)
	if stripped := pystr.Strip(newOutput); stripped != "" {
		return i18n.T(ctx, "gateway.background.final_output", "", map[string]any{"header": header, "output": stripped})
	}
	return header
}

// Still running notice with the recent output tail of the all mode
func formatProcessRunningMessage(ctx context.Context, fields tools.ProcessWatchFields) string {
	newOutput := redactedOutputTail(ctx, fields, fields.TaskID, 500)
	shortCmd := shortenCommandForDisplay(redactGatewayUserFacingSecrets(ctx, fields.Command), 80)
	header := i18n.T(ctx, "gateway.background.still_running", "", nil)
	if shortCmd != "" {
		header += " — `" + shortCmd + "`"
	}
	if stripped := pystr.Strip(newOutput); stripped != "" {
		return i18n.T(ctx, "gateway.background.recent_output", "", map[string]any{"header": header, "output": stripped})
	}
	return header
}

// Starts a watcher registered mid turn on the serving loop in an empty context, false while not serving
func (r *GatewayRunner) armProcessWatcher(watcher map[string]any) bool {
	loop := r.gatewayLoop
	if !r.running || loop == nil {
		return false
	}
	err := loop.CallSoonThreadsafe(func() {
		spawnGatewayTask(context.Background(), "process-watcher", func(taskCtx context.Context) (any, error) {
			return nil, r.runProcessWatcher(taskCtx, watcher)
		})
	})
	if err != nil {
		runLog().Debug("Live process watcher arming failed", "error", err)
		return false
	}
	return true
}

// Watches the process inside the owning profile's scope from start to finish
func (r *GatewayRunner) runProcessWatcher(ctx context.Context, watcher map[string]any) error {
	scoped, err := r.completionEventScope(ctx, watcher)
	if err != nil {
		return err
	}
	return r.watchProcess(scoped, watcher)
}

// Whether the notify mode surfaces the final message for the exit code
func finalMessageSurfaces(notifyMode string, exitCode *int) bool {
	failed := exitCode != nil && *exitCode != 0
	return notifyMode == "concise" || notifyMode == "all" || notifyMode == "result" || (notifyMode == "error" && failed)
}

// Polls a background process and pushes updates by the notification mode until it exits
func (r *GatewayRunner) watchProcess(ctx context.Context, watcher map[string]any) error {
	registry := tools.DefaultProcessRegistry()
	sessionID := pyrepr.Str(watcher["session_id"])
	interval, err := pyint.Float(watcher["check_interval"])
	if err != nil {
		return err
	}
	platformName := pyrepr.Str(pyrepr.Or(watcher["platform"], ""))
	chatID, ok := watcher["chat_id"]
	if !ok {
		chatID = ""
	}
	threadID, ok := watcher["thread_id"]
	if !ok {
		threadID = ""
	}
	agentNotify := pyrepr.Truthy(watcher["notify_on_complete"])
	notifyMode := loadBackgroundNotificationsMode(ctx)
	runLog().Debug(fmt.Sprintf("Process watcher started: %s (every %ss, notify=%s, agent_notify=%s)",
		sessionID, pyrepr.Str(watcher["check_interval"]), notifyMode, pyrepr.Str(agentNotify)))
	silent := notifyMode == "off" && !agentNotify
	lastOutputLen := 0
	completionAttempts := 0
	retryInterval := time.Duration(interval * float64(time.Second))
	env := watcherToolsEnv(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryInterval):
		}
		session := registry.Get(env, sessionID)
		if session == nil {
			break
		}
		fields := session.WatchFields()
		cfg, loadErr := config.Load(home.Dir(ctx), nil)
		if loadErr != nil {
			cfg = config.Defaults()
		}
		if !tools.ShouldSurfaceNotification(cfg, map[string]any{"owner_task_id": fields.OwnerTaskID, "task_id": fields.TaskID}) {
			if fields.Exited {
				break
			}
			continue
		}
		if silent {
			if fields.Exited {
				break
			}
			continue
		}
		currentOutputLen := utf8.RuneCountInString(fields.OutputBuffer)
		hasNewOutput := currentOutputLen > lastOutputLen
		lastOutputLen = currentOutputLen
		if !fields.Exited {
			if hasNewOutput && notifyMode == "all" && !agentNotify {
				if err := r.sendWatcherMessage(ctx, platformName, chatID, threadID, formatProcessRunningMessage(ctx, fields), watcher); err != nil {
					return err
				}
			}
			continue
		}
		if agentNotify && !registry.IsCompletionConsumed(sessionID) {
			completionEvt := buildProcessCompletionEvent(ctx, watcher, fields, sessionID)
			synthText := tools.FormatProcessNotification(completionEvt)
			if synthText == "" {
				break
			}
			turnBusy, err := r.launchingTurnActive(ctx, platformName, watcher)
			if err != nil {
				return err
			}
			delivered, err := r.enqueueProcessCompletionNotification(ctx, synthText, completionEvt)
			if err != nil {
				return err
			}
			if delivered == nil || *delivered {
				if turnBusy && finalMessageSurfaces(notifyMode, fields.ExitCode) {
					messageText := formatProcessFinalMessage(ctx, sessionID, fields, "concise")
					if err := r.sendWatcherMessage(ctx, platformName, chatID, threadID, messageText, watcher); err != nil {
						return err
					}
				}
				break
			}
			completionAttempts++
			if completionAttempts < maxCompletionInjectionAttempts {
				retryInterval = provider.JitteredBackoff(completionAttempts, provider.BackoffOptions{
					BaseDelay: time.Duration(max(interval, 1) * 2 * float64(time.Second)),
					MaxDelay:  completionInjectionRetryMaxDelay * time.Second, JitterRatio: 0})
				continue
			}
			if _, hasIdentity := completionDeliveryIdentity(completionEvt); r.sessionAuthority != nil && hasIdentity {
				committed := false
				scoped, scopeErr := r.completionEventScope(ctx, completionEvt)
				if scopeErr == nil {
					receipt, admissionErr := gateway.CompletionAdmission(scoped, r, completionEvt)
					if admissionErr != nil {
						runLog().Debug("Process watcher: admission recheck failed for "+sessionID, "error", admissionErr)
					} else {
						committed = receipt != nil
					}
				} else {
					runLog().Debug("Process watcher: admission recheck failed for "+sessionID, "error", scopeErr)
				}
				if committed {
					break
				}
			}
			runLog().Warn(fmt.Sprintf("Process watcher: completion for %s was refused %d times; falling back to the notify_mode=%s notice",
				sessionID, completionAttempts, notifyMode))
		}
		if registry.IsCompletionConsumed(sessionID) {
			runLog().Debug(fmt.Sprintf("Process watcher: completion for %s already consumed via wait/log — skipping raw notification (#65379)", sessionID))
			break
		}
		if finalMessageSurfaces(notifyMode, fields.ExitCode) {
			messageText := formatProcessFinalMessage(ctx, sessionID, fields, notifyMode)
			diagnostic := fields.ExitCode != nil && *fields.ExitCode != 0
			if _, err := config.PresentNotification(ctx, func(sendCtx context.Context) error {
				return r.sendWatcherMessage(sendCtx, platformName, chatID, threadID, messageText, watcher)
			}, platformName, diagnostic, nil); err != nil {
				return err
			}
		}
		break
	}
	suffix := ""
	if silent {
		suffix = " (silent)"
	}
	runLog().Debug(fmt.Sprintf("Process watcher ended%s: %s", suffix, sessionID))
	return nil
}
