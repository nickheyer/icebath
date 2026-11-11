package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Adapter reporting the connector created auto thread of the latest send into a chat
type autoThreadInfoAdapter interface {
	AutoThreadInfoForChat(chatID string) any
}

// Adapter able to wait for the send that reports the connector created auto thread
type autoThreadWaitAdapter interface {
	WaitForAutoThreadInfo(ctx context.Context, chatID string, timeout float64) (any, error)
}

// Adapter renaming a Discord thread under the native or connector guard
type threadRenameAdapter interface {
	RenameThread(ctx context.Context, threadID, name string, opts gateway.ThreadRenameOptions) (bool, error)
}

// Adapter renaming a Telegram private chat forum topic
type dmTopicRenameAdapter interface {
	RenameDMTopic(ctx context.Context, chatID string, threadID *big.Int, title string) error
}

// Adapter exposing its Telegram bot client
type telegramBotAdapter interface {
	TelegramBot() any
}

// Bot client able to rename a forum topic
type forumTopicEditor interface {
	EditForumTopic(ctx context.Context, chatID, messageThreadID any, name string) error
}

// Whitespace runs collapsed to one space and trimmed, the default topic title when empty
func collapseTitle(ctx context.Context, title string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range title {
		if pystr.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	if cleaned := pystr.Strip(b.String()); cleaned != "" {
		return cleaned
	}
	return i18n.T(ctx, "gateway.topic.default_title", "", nil)
}

// Bot API safe forum topic name, at most 120 code points with an ellipsis past that
func sanitizeTelegramTopicTitle(ctx context.Context, title string) string {
	cleaned := collapseTitle(ctx, title)
	runes := []rune(cleaned)
	if len(runes) <= 120 {
		return cleaned
	}
	return pystr.Rstrip(string(runes[:117])) + "..."
}

// Discord safe thread title, at most 80 UTF-16 units with an ellipsis past that
func sanitizeDiscordThreadTitle(ctx context.Context, title string) string {
	cleaned := collapseTitle(ctx, title)
	if gateway.UTF16Len(cleaned) <= 80 {
		return cleaned
	}
	return pystr.Rstrip(gateway.PrefixWithinUTF16Limit(cleaned, 77)) + "..."
}

// Thread id and initial name pair the connector reported, nil when the value is not one
func asThreadInfo(info any) *[2]string {
	var items []any
	switch v := info.(type) {
	case [2]string:
		return &v
	case *[2]string:
		return v
	case []any:
		items = v
	default:
		return nil
	}
	if len(items) != 2 {
		return nil
	}
	id, okID := items[0].(string)
	name, okName := items[1].(string)
	if !okID || !okName {
		return nil
	}
	return &[2]string{id, name}
}

// Thread id and initial name when the relay connector auto threaded the reply, nil otherwise
func (r *GatewayRunner) relayAutoThreadInfo(ctx context.Context, source *gateway.SessionSource) *[2]string {
	if source.Platform != gateway.PlatformDiscord || source.ChatID == "" || !source.DeliveredViaUpstreamRelay {
		return nil
	}
	if source.ProspectiveThreadID != nil && *source.ProspectiveThreadID != "" {
		return &[2]string{*source.ProspectiveThreadID, ""}
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil
	}
	infoFn, ok := adapter.(autoThreadInfoAdapter)
	if !ok || adapter == nil {
		return nil
	}
	var info *[2]string
	suppressPanic(func() { info = asThreadInfo(infoFn.AutoThreadInfoForChat(source.ChatID)) })
	return info
}

// Relay auto thread info, waited out until the turn delivers with the turn inactivity limit as backstop
func (r *GatewayRunner) awaitRelayAutoThreadInfo(ctx context.Context, source *gateway.SessionSource) *[2]string {
	if known := r.relayAutoThreadInfo(ctx, source); known != nil {
		return known
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil
	}
	waitFn, ok := adapter.(autoThreadWaitAdapter)
	if !ok || adapter == nil || source.ChatID == "" {
		return nil
	}
	timeout := floatEnv("ICEBATH_AGENT_TIMEOUT", 1800)
	if timeout == 0 {
		timeout = 1800
	}
	var info *[2]string
	suppressPanic(func() {
		raw, werr := waitFn.WaitForAutoThreadInfo(ctx, source.ChatID, timeout)
		if werr == nil {
			info = asThreadInfo(raw)
		}
	})
	return info
}

// Whether err is the TypeError of a call with keywords the callee does not take
func isPyTypeError(err error) bool {
	var named interface{ PyTypeName() string }
	return errors.As(err, &named) && named.PyTypeName() == "TypeError"
}

// Semantic rename of a newly auto created Discord thread, the relay connector guarding its own name
func (r *GatewayRunner) renameDiscordAutoThreadForSessionTitle(ctx context.Context, source *gateway.SessionSource, sessionID, title string, relayInfo *[2]string) {
	if relayInfo == nil && !r.isDiscordAutoThreadLane(source) {
		if !r.isRelayDiscordChannelLane(source) {
			return
		}
		relayInfo = r.awaitRelayAutoThreadInfo(ctx, source)
		if relayInfo == nil {
			return
		}
	}
	var adapter gateway.PlatformAdapter
	if r.adapters != nil && r.adapters.Len() > 0 {
		adapter, _ = r.deliveryAdapterFor(ctx, source)
	}
	renamer, ok := adapter.(threadRenameAdapter)
	if !ok || adapter == nil {
		return
	}
	relay := relayInfo != nil
	targetThreadID := optText(source.ThreadID)
	if source.ThreadID == nil {
		targetThreadID = "None"
	}
	if relay {
		targetThreadID = relayInfo[0]
	}
	threadName := sanitizeDiscordThreadTitle(ctx, title)
	opts := gateway.ThreadRenameOptions{OnlyIfCurrentName: source.AutoThreadInitialName}
	lane := "native"
	if relay {
		var parent *string
		if source.ChatID != "" {
			chatID := source.ChatID
			parent = &chatID
		}
		opts = gateway.ThreadRenameOptions{PreferConnectorCreated: true, ParentChatID: parent}
		lane = "relay"
	}
	runLog().Info(fmt.Sprintf("discord auto-thread rename: thread=%s lane=%s new_title=%s", targetThreadID, lane, pyrepr.Repr(threadName)))
	renamed, err := renamer.RenameThread(ctx, targetThreadID, threadName, opts)
	switch {
	case err != nil && isPyTypeError(err):
		runLog().Warn(fmt.Sprintf("Discord semantic thread rename raised TypeError (adapter=%s)", pyTypeNameOf(adapter)), "exc_info", err)
	case err != nil:
		runLog().Debug("Failed to rename Discord auto-thread for generated session title", "exc_info", err)
	default:
		runLog().Info(fmt.Sprintf("discord auto-thread rename result: thread=%s applied=%s", targetThreadID, pyrepr.Repr(renamed)))
	}
}

// Class name of an adapter as type(adapter).__name__ reports it
func pyTypeNameOf(v any) string {
	name := fmt.Sprintf("%T", v)
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimPrefix(name, "*")
}

// Schedules a rename onto the gateway loop from the auto title thread, logging failures at debug
func (r *GatewayRunner) scheduleRenameFromTitleThread(source *gateway.SessionSource, rename func(ctx context.Context, copied *gateway.SessionSource) error, label string) {
	loop := r.gatewayLoop
	if loop == nil {
		return
	}
	copied := source
	suppressPanic(func() { copied = gateway.ReplaceSource(source, nil) })
	err := loop.CallSoonThreadsafe(func() {
		task := spawnGatewayTask(context.Background(), label, func(taskCtx context.Context) (any, error) {
			return nil, rename(taskCtx, copied)
		})
		task.addDoneCallback(func(fut *gatewayFuture) {
			if fut.cancelled() {
				return
			}
			if _, ferr := fut.wait(context.Background()); ferr != nil {
				runLog().Debug(label+" failed", "exc_info", ferr)
			}
		})
	})
	if err != nil && !errors.Is(err, gateway.ErrLoopClosed) {
		runLog().Debug(fmt.Sprintf("%s failed to schedule: %s", label, err))
	}
}

// Schedules the Discord auto thread rename from the auto title thread
func (r *GatewayRunner) scheduleDiscordSemanticThreadRename(ctx context.Context, source *gateway.SessionSource, sessionID, title string) {
	if title == "" {
		return
	}
	var relayInfo *[2]string
	if !r.isDiscordAutoThreadLane(source) {
		relayInfo = r.relayAutoThreadInfo(ctx, source)
		if relayInfo == nil && !r.isRelayDiscordChannelLane(source) {
			return
		}
	}
	r.scheduleRenameFromTitleThread(source, func(taskCtx context.Context, copied *gateway.SessionSource) error {
		r.renameDiscordAutoThreadForSessionTitle(taskCtx, copied, sessionID, title, relayInfo)
		return nil
	}, "Discord semantic thread rename")
}

// Schedules a Telegram topic rename from the auto title thread
func (r *GatewayRunner) scheduleTelegramTopicTitleRename(ctx context.Context, source *gateway.SessionSource, sessionID, title string) {
	if title == "" || !r.isTelegramTopicLane(ctx, source) || r.telegramTopicAutoRenameDisabled(source) {
		return
	}
	r.scheduleRenameFromTitleThread(source, func(taskCtx context.Context, copied *gateway.SessionSource) error {
		r.renameTelegramTopicForSessionTitle(taskCtx, copied, sessionID, title)
		return nil
	}, "Telegram topic title rename")
}

// Whether the platform extra disable_topic_auto_rename turned topic auto rename off
func (r *GatewayRunner) telegramTopicAutoRenameDisabled(source *gateway.SessionSource) bool {
	if r.config == nil || len(r.config.Platforms) == 0 {
		return false
	}
	platformCfg := r.config.Platforms[source.Platform]
	if platformCfg == nil {
		return false
	}
	return isTruthyValue(platformCfg.Extra["disable_topic_auto_rename"], false)
}

// Whether a DM topic info answer names an operator declared topic
func isOperatorTopic(info any) bool {
	switch v := info.(type) {
	case map[string]any:
		return true
	case *pyjson.Object:
		return v != nil
	}
	return false
}

// Renames a Telegram DM topic after the session was auto titled, operator topics and rebound topics left alone
func (r *GatewayRunner) renameTelegramTopicForSessionTitle(ctx context.Context, source *gateway.SessionSource, sessionID, title string) {
	if !r.isTelegramTopicLane(ctx, source) || source.ChatID == "" || optText(source.ThreadID) == "" {
		return
	}
	if r.telegramTopicAutoRenameDisabled(source) {
		return
	}
	threadID := *source.ThreadID
	adapter, _ := r.deliveryAdapterFor(ctx, source)
	if lookup, ok := adapter.(dmTopicInfoAdapter); ok && adapter != nil {
		info, err := lookup.GetDMTopicInfo(source.ChatID, threadID)
		if err != nil {
			info = nil
		}
		if isOperatorTopic(info) {
			return
		}
	}
	if db := r.sessionDB(ctx); db != nil {
		binding, err := db.DB().GetTelegramTopicBinding(ctx, source.ChatID, threadID, telegramTopicProfileName(source))
		if err != nil {
			runLog().Debug("Failed to verify Telegram topic binding before rename", "exc_info", err)
			return
		}
		if binding != nil && binding.Len() > 0 && pyrepr.Str(orEmpty(binding.GetOr("session_id", nil))) != sessionID {
			return
		}
	}
	if adapter == nil {
		return
	}
	topicName := sanitizeTelegramTopicTitle(ctx, title)
	if err := renameTelegramTopic(ctx, adapter, source.ChatID, threadID, topicName); err != nil {
		runLog().Debug("Failed to rename Telegram topic for auto-generated title", "exc_info", err)
	}
}

// Value when truthy, the empty string otherwise
func orEmpty(v any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return ""
}

// Renames the topic through the adapter, else through its bot client with integer ids then raw ids
func renameTelegramTopic(ctx context.Context, adapter gateway.PlatformAdapter, chatID, threadID, name string) error {
	if renamer, ok := adapter.(dmTopicRenameAdapter); ok {
		tid, err := pyint.Of(threadID)
		if err != nil {
			return err
		}
		return renamer.RenameDMTopic(ctx, chatID, tid, name)
	}
	holder, ok := adapter.(telegramBotAdapter)
	if !ok {
		return nil
	}
	editor, ok := holder.TelegramBot().(forumTopicEditor)
	if !ok {
		return nil
	}
	chatInt, chatErr := pyint.Of(chatID)
	threadInt, threadErr := pyint.Of(threadID)
	if chatErr == nil && threadErr == nil {
		err := editor.EditForumTopic(ctx, chatInt, threadInt, name)
		if err == nil || !isPyTypeOrValueError(err) {
			return err
		}
	}
	return editor.EditForumTopic(ctx, chatID, threadID, name)
}

// Whether err is a TypeError or ValueError
func isPyTypeOrValueError(err error) bool {
	var named interface{ PyTypeName() string }
	if !errors.As(err, &named) {
		return false
	}
	return named.PyTypeName() == "TypeError" || named.PyTypeName() == "ValueError"
}
