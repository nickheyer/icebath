package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Age past which a completion notice stops waiting for its platform adapter to connect
const updateNotifyMaxAdapterWaitSeconds = 3600.0

// Chat notice that a failed update left the previous version running
func updateFailedNotice(ctx context.Context) string {
	return i18n.T(ctx, "gateway.update.failed_notice", "", nil)
}

// Last limit characters of an update log, prefixed with an ellipsis when cut
func updateOutputTail(output string, limit int) string {
	runes := []rune(output)
	if len(runes) <= limit {
		return output
	}
	return "…" + string(runes[len(runes)-limit:])
}

// Marker files the gateway update command and its watcher exchange under the home
type updatePaths struct {
	pending, claimed, output, exitCode, prompt, response string
}

// Whether a path exists, stat errors reading as absent
func updatePathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Whether a pending or claimed update marker is on disk
func (p updatePaths) anyPending() bool {
	return updatePathExists(p.pending) || updatePathExists(p.claimed)
}

// Removes every marker file, missing ones ignored
func (p updatePaths) unlinkAll() {
	for _, path := range []string{p.pending, p.claimed, p.output, p.exitCode, p.prompt, p.response} {
		_ = os.Remove(path)
	}
}

// Resolved delivery target of update watcher messages
type updateTarget struct {
	adapter    gateway.PlatformAdapter
	chatID     string
	sessionKey string
	metadata   map[string]any
	platform   gateway.Platform
}

// Thread metadata of the target marked non conversational for its platform
func (t *updateTarget) sendMetadata() map[string]any {
	return nonConversationalMetadata(t.metadata, t.platform)
}

// Sends text to the target chat
func (t *updateTarget) send(ctx context.Context, text string) (gateway.SendResult, error) {
	return t.adapter.Send(ctx, t.chatID, text, "", t.sendMetadata())
}

// Starts the update progress watcher unless one is still running
func (r *GatewayRunner) scheduleUpdateNotificationWatch(ctx context.Context) {
	if existing := r.updateNotificationTask; existing != nil && !existing.isDone() {
		return
	}
	r.updateNotificationTask = spawnGatewayTask(ctx, "update_notification_watch", func(ctx context.Context) (any, error) {
		return nil, r.watchUpdateProgress(ctx, 2*time.Second, 4*time.Second, 1800*time.Second)
	})
}

// Marker paths under the process home
func (r *GatewayRunner) updatePaths(ctx context.Context) updatePaths {
	h := home.Dir(ctx)
	return updatePaths{
		pending: filepath.Join(h, ".update_pending.json"), claimed: filepath.Join(h, ".update_pending.claimed.json"),
		output: filepath.Join(h, ".update_output.txt"), exitCode: filepath.Join(h, ".update_exit_code"),
		prompt: filepath.Join(h, ".update_prompt.json"), response: filepath.Join(h, ".update_response"),
	}
}

// Owning profile of a restart or update marker, nil for the default profile
func markerProfile(data *pyjson.Object) *string {
	if profile := pystr.Strip(truthyText(rowField(data, "profile"))); profile != "" {
		return &profile
	}
	parts := strings.Split(truthyText(rowField(data, "session_key")), ":")
	if len(parts) >= 5 && parts[0] == "agent" && parts[1] != "main" && parts[1] != "" {
		profile := state.ProfileFromSessionKeyNamespace(parts[1])
		return &profile
	}
	return nil
}

// Seconds since the marker timestamp, false when it carries no parseable stamp
func markerAgeSeconds(data *pyjson.Object) (float64, bool) {
	raw := pystr.Strip(truthyText(rowField(data, "timestamp")))
	if raw == "" {
		return 0, false
	}
	stamped, aware, err := pydatetime.ParseISOFormat(raw)
	if err != nil {
		return 0, false
	}
	if aware {
		return time.Since(stamped).Seconds(), true
	}
	now := time.Now()
	local := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), time.UTC)
	return local.Sub(stamped).Seconds(), true
}

// Marker JSON object of a utf-8-sig file
func readMarkerObject(path string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	object, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
	}
	return object, nil
}

// Optional string form of a marker value, nil for an absent or null one
func markerText(data *pyjson.Object, key string) *string {
	v := rowField(data, key)
	if v == nil {
		return nil
	}
	text := pyrepr.Str(v)
	return &text
}

// Adapter, chat and session of the update requester from the claimed or pending marker
func (r *GatewayRunner) resolveUpdateTarget(ctx context.Context, paths updatePaths) *updateTarget {
	for _, path := range []string{paths.claimed, paths.pending} {
		if !updatePathExists(path) {
			continue
		}
		pending, err := readMarkerObject(path)
		if err != nil {
			continue
		}
		platformStr, chatID := rowField(pending, "platform"), rowField(pending, "chat_id")
		if !pyrepr.Truthy(platformStr) || !pyrepr.Truthy(chatID) {
			continue
		}
		platform, err := gateway.ParsePlatform(ctx, platformStr)
		if err != nil {
			continue
		}
		adapter := r.authorizationAdapter(ctx, platform, markerProfile(pending))
		if adapter == nil {
			return nil
		}
		chat := pyrepr.Str(chatID)
		metadata := r.pendingMarkerMetadata(platform, chat, pending, adapter)
		sessionKey := truthyText(rowField(pending, "session_key"))
		if sessionKey == "" {
			sessionKey = pyrepr.Str(platformStr) + ":" + chat
		}
		return &updateTarget{adapter: adapter, chatID: chat, sessionKey: sessionKey, metadata: metadata, platform: platform}
	}
	return nil
}

// Thread metadata of a persisted update or restart marker
func (r *GatewayRunner) pendingMarkerMetadata(platform gateway.Platform, chatID string, data *pyjson.Object, adapter gateway.PlatformAdapter) map[string]any {
	chatType := ""
	if v := markerText(data, "chat_type"); v != nil {
		chatType = *v
	}
	return r.threadMetadataForTarget(platform, chatID, markerText(data, "thread_id"), chatType, markerText(data, "message_id"), adapter)
}

// Sleeps for d, the context error when cancelled first
func updateSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Waits for the exit code with no resolvable chat, then sends the final notice
func (r *GatewayRunner) watchUpdateCompletionOnly(ctx context.Context, paths updatePaths, deadline time.Time, pollInterval time.Duration) error {
	inboundLog().Warn("Update watcher: cannot resolve adapter/chat_id, falling back to completion-only")
	for paths.anyPending() && time.Now().Before(deadline) {
		if updatePathExists(paths.exitCode) && r.sendUpdateNotification(ctx) {
			return nil
		}
		if err := updateSleep(ctx, pollInterval); err != nil {
			return err
		}
	}
	if paths.anyPending() && !updatePathExists(paths.exitCode) {
		if err := os.WriteFile(paths.exitCode, []byte("124"), 0o644); err != nil {
			return err
		}
		r.sendUpdateNotification(ctx)
	}
	return nil
}

// Exit code the update process wrote, one for an empty file
func updateExitCode(paths updatePaths) (*big.Int, error) {
	raw, err := os.ReadFile(paths.exitCode)
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	text = pystr.Strip(text)
	if text == "" {
		text = "1"
	}
	code, ok := pyint.Parse(text)
	if !ok {
		return nil, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(text))
	}
	return code, nil
}

// Update log text past offset with invalid UTF-8 replaced, and the new offset
func readUpdateOutputSince(path string, offset int) (string, int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", offset
	}
	if len(data) <= offset {
		return "", len(data)
	}
	return pystr.DecodeUTF8Replace(data[offset:]), len(data)
}

// Sends buffered update output as fenced chunks that fit message limits
func (r *GatewayRunner) sendUpdateOutput(ctx context.Context, target *updateTarget, text string) {
	clean := []rune(pystr.Strip(tools.StripANSI(text)))
	if len(clean) == 0 {
		return
	}
	const maxChunk = 3500
	for i := 0; i < len(clean); i += maxChunk {
		end := min(i+maxChunk, len(clean))
		_, err := target.send(ctx, "```\n"+string(clean[i:end])+"\n```")
		logSuppressed(slog.LevelDebug, err, "Update stream send failed: %s")
	}
}

// Forwards an update prompt as native buttons when the adapter has them, else as text
func (r *GatewayRunner) forwardUpdatePrompt(ctx context.Context, target *updateTarget, promptText, def string) error {
	sentButtons := false
	if sender, ok := target.adapter.(gateway.UpdatePromptSender); ok {
		_, err := sender.SendUpdatePrompt(ctx, target.chatID, promptText, def, target.sessionKey, target.sendMetadata())
		logSuppressed(slog.LevelDebug, err, "Button-based update prompt failed: %s")
		sentButtons = err == nil
	}
	if !sentButtons {
		hint := ""
		if def != "" {
			hint = i18n.T(ctx, "gateway.update.prompt_default", "", map[string]any{"default": def})
		}
		prefix := target.adapter.Base().Traits.TypedCommandPrefix
		if _, err := target.send(ctx, i18n.T(ctx, "gateway.update.prompt", "", map[string]any{"prompt": promptText, "default_hint": hint, "prefix": prefix})); err != nil {
			return err
		}
	}
	r.updatePromptPending().Set(target.sessionKey, true)
	inboundLog().Info(fmt.Sprintf("Forwarded update prompt to %s: %s", target.sessionKey, pystr.Slice(promptText, 0, 80)))
	return nil
}

// Removes the markers and clears the session's pending prompt flag
func (r *GatewayRunner) clearUpdateMarkers(paths updatePaths, sessionKey string) {
	paths.unlinkAll()
	r.withPeekedSessionState(sessionKey, func(s *SessionState) { s.persistent.updatePromptPending = false })
}

// Streams update output, forwards prompts and sends the final notice until the exit code or the timeout
func (r *GatewayRunner) watchUpdateProgress(ctx context.Context, pollInterval, streamInterval, timeout time.Duration) error {
	paths := r.updatePaths(ctx)
	deadline := time.Now().Add(timeout)
	target := r.resolveUpdateTarget(ctx, paths)
	if target == nil {
		return r.watchUpdateCompletionOnly(ctx, paths, deadline, pollInterval)
	}
	sessionKey := target.sessionKey
	bytesSent := 0
	lastStream := time.Now()
	buffer := ""
	flush := func() {
		text := buffer
		buffer = ""
		if pystr.Strip(text) != "" {
			lastStream = time.Now()
			r.sendUpdateOutput(ctx, target, text)
		}
	}
	readNew := func() {
		if updatePathExists(paths.output) {
			var chunk string
			chunk, bytesSent = readUpdateOutputSince(paths.output, bytesSent)
			buffer += chunk
		}
	}
	for time.Now().Before(deadline) {
		if updatePathExists(paths.exitCode) {
			readNew()
			flush()
			if err := r.sendUpdateFinished(ctx, paths, target); err != nil {
				logSuppressed(slog.LevelWarn, err, "Update final notification failed: %s")
			}
			r.clearUpdateMarkers(paths, sessionKey)
			return nil
		}
		readNew()
		if pystr.Strip(buffer) != "" && time.Since(lastStream) >= streamInterval {
			flush()
		}
		promptPending := false
		if sessionKey != "" {
			promptPending, _ = r.updatePromptPending().Get(sessionKey)
		}
		if updatePathExists(paths.prompt) && sessionKey != "" && !promptPending {
			if err := r.forwardPendingUpdatePrompt(ctx, paths, target, flush); err != nil {
				return err
			}
		}
		if err := updateSleep(ctx, pollInterval); err != nil {
			return err
		}
	}
	if !updatePathExists(paths.exitCode) {
		inboundLog().Warn(fmt.Sprintf("Update watcher timed out after %.0fs", timeout.Seconds()))
		if err := os.WriteFile(paths.exitCode, []byte("124"), 0o644); err != nil {
			return err
		}
		flush()
		_, _ = target.send(ctx, i18n.T(ctx, "gateway.update.timed_out", "", nil))
		r.clearUpdateMarkers(paths, sessionKey)
	}
	return nil
}

// Sends the finished or failed notice for the written exit code
func (r *GatewayRunner) sendUpdateFinished(ctx context.Context, paths updatePaths, target *updateTarget) error {
	code, err := updateExitCode(paths)
	if err != nil {
		return err
	}
	notice := updateFailedNotice(ctx)
	if code.Sign() == 0 {
		notice = i18n.T(ctx, "gateway.update.finished", "", nil)
	}
	if _, err := target.send(ctx, notice); err != nil {
		return err
	}
	inboundLog().Info(fmt.Sprintf("Update finished (exit=%s), notified %s", code.String(), target.sessionKey))
	return nil
}

// Forwards the prompt marker after flushing output, a decode or read failure logged
func (r *GatewayRunner) forwardPendingUpdatePrompt(ctx context.Context, paths updatePaths, target *updateTarget, flush func()) error {
	raw, err := os.ReadFile(paths.prompt)
	var data any
	if err == nil {
		var text string
		if text, err = pystr.DecodeUTF8Sig(raw); err == nil {
			data, err = pyjson.Loads(text)
		}
	}
	if err != nil {
		var decodeErr *pyjson.DecodeError
		var pathErr *fs.PathError
		if errors.As(err, &decodeErr) || errors.As(err, &pathErr) {
			inboundLog().Debug(fmt.Sprintf("Failed to read update prompt: %s", err))
			return nil
		}
		return err
	}
	object, ok := data.(*pyjson.Object)
	if !ok {
		return fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(data))
	}
	promptText := object.GetOr("prompt", "")
	if !pyrepr.Truthy(promptText) {
		return nil
	}
	flush()
	return r.forwardUpdatePrompt(ctx, target, pyrepr.Str(promptText), pyrepr.Str(object.GetOr("default", "")))
}

// Sends the post update notice once the exit code exists, false while the update still runs
func (r *GatewayRunner) sendUpdateNotification(ctx context.Context) bool {
	paths := r.updatePaths(ctx)
	if !paths.anyPending() {
		return false
	}
	cleanup := true
	active := paths.claimed
	deferred := func(reason string) (bool, error) {
		inboundLog().Info(reason)
		cleanup = false
		active = paths.pending
		return false, os.Rename(paths.claimed, paths.pending)
	}
	defer func() {
		if cleanup {
			for _, p := range []string{active, paths.claimed, paths.output, paths.exitCode} {
				_ = os.Remove(p)
			}
		}
	}()
	result, err := r.deliverUpdateNotification(ctx, paths, deferred)
	if err != nil {
		inboundLog().Warn(fmt.Sprintf("Post-update notification failed: %s", err))
		return true
	}
	return result
}

// Claims the marker and sends the notice, the result and the error the caller logs
func (r *GatewayRunner) deliverUpdateNotification(ctx context.Context, paths updatePaths, deferred func(string) (bool, error)) (bool, error) {
	if updatePathExists(paths.pending) {
		if err := os.Rename(paths.pending, paths.claimed); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return true, err
			}
			if !updatePathExists(paths.claimed) {
				return true, nil
			}
		}
	} else if !updatePathExists(paths.claimed) {
		return true, nil
	}
	pending, err := readMarkerObject(paths.claimed)
	if err != nil {
		return true, err
	}
	platformStr, chatID := rowField(pending, "platform"), rowField(pending, "chat_id")
	if !updatePathExists(paths.exitCode) {
		return deferred("Update notification deferred: update still running")
	}
	code, err := updateExitCode(paths)
	if err != nil {
		return true, err
	}
	output := ""
	if updatePathExists(paths.output) {
		raw, err := os.ReadFile(paths.output)
		if err != nil {
			return true, err
		}
		output = pystr.DecodeUTF8Replace(raw)
	}
	platform, err := gateway.ParsePlatform(ctx, platformStr)
	if err != nil {
		return true, err
	}
	adapter := r.authorizationAdapter(ctx, platform, markerProfile(pending))
	if pyrepr.Truthy(chatID) && adapter == nil {
		if age, ok := markerAgeSeconds(pending); ok && age > updateNotifyMaxAdapterWaitSeconds {
			inboundLog().Warn(fmt.Sprintf("Post-update notification for %s:%s dropped after %.1fh: %s adapter never connected",
				pyrepr.Str(platformStr), pyrepr.Str(chatID), age/3600.0, pyrepr.Str(platformStr)))
			r.clearUpdateMarkers(paths, truthyText(rowField(pending, "session_key")))
			return true, nil
		}
		return deferred(fmt.Sprintf("Update notification deferred: %s adapter not connected yet", pyrepr.Str(platformStr)))
	}
	if pyrepr.Truthy(chatID) {
		chat := pyrepr.Str(chatID)
		metadata := r.pendingMarkerMetadata(platform, chat, pending, adapter)
		output = pystr.Strip(tools.StripANSI(output))
		var msg string
		if code.Sign() == 0 {
			msg = i18n.T(ctx, "gateway.update.finished_success", "", nil)
			if output != "" {
				msg = msg + "\n\n```\n" + updateOutputTail(output, 3500) + "\n```"
			}
		} else {
			msg = updateFailedNotice(ctx)
			if output != "" {
				msg = i18n.T(ctx, "gateway.update.last_lines", "", map[string]any{"msg": msg, "tail": updateOutputTail(output, 800)})
			}
		}
		if _, err := adapter.Send(ctx, chat, msg, "", nonConversationalMetadata(metadata, platform)); err != nil {
			return true, err
		}
		inboundLog().Info(fmt.Sprintf("Sent post-update notification to %s:%s (exit=%s)", pyrepr.Str(platformStr), chat, code.String()))
	}
	return true, nil
}
