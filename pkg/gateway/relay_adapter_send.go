package gateway

import (
	"context"
	"fmt"
	"maps"
	"math"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Optional reply target, nil for an empty one
func relayOptReply(replyTo string) *string {
	if replyTo == "" {
		return nil
	}
	return &replyTo
}

// Wire value of an optional reply target
func relayWireReply(replyTo *string) any {
	if replyTo == nil {
		return nil
	}
	return *replyTo
}

// Stops the revocation monitor, asks the connector to buffer, then closes the transport within the remaining budget
func (a *RelayAdapter) Disconnect(ctx context.Context) error {
	started := time.Now()
	budget := relayEnvDisconnectBudget()
	a.mu.Lock()
	monitor := a.revocationMonitor
	a.mu.Unlock()
	if monitor != nil {
		monitor.cancel()
		timer := time.NewTimer(secondsDuration(relayRevocationMonitorTeardownTimeoutS))
		select {
		case <-monitor.done:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		a.mu.Lock()
		a.revocationMonitor = nil
		a.mu.Unlock()
	}
	if a.transport == nil {
		return ctx.Err()
	}
	if idler, ok := a.transport.(relayIdler); ok {
		idler.GoIdle(ctx, relayGoIdleOnDisconnectTimeoutS)
	}
	remaining := math.Max(0, budget-time.Since(started).Seconds())
	if err := a.transport.Disconnect(context.WithoutCancel(ctx), &remaining); err != nil {
		relayAdapterLog().Debug("relay transport disconnect failed during drain", "exc_info", err)
	}
	return ctx.Err()
}

// Quiesces the relay for a scale to zero suspend with the reconnect path left armed
func (a *RelayAdapter) GoDormant(ctx context.Context) (bool, error) {
	dormant, ok := a.transport.(relayDormantTransport)
	if !ok {
		return false, nil
	}
	return dormant.GoDormant(ctx, 10.0), nil
}

// Parks the transport's reconnect supervisor, false when it did not take
func (a *RelayAdapter) HoldRedial() bool {
	holder, ok := a.transport.(relayRedialHolder)
	if !ok {
		return false
	}
	return relayToggleRedial("hold_redial", holder.HoldRedial)
}

// Undoes a hold when the suspend did not land, false when it did not take
func (a *RelayAdapter) ReleaseRedial() bool {
	releaser, ok := a.transport.(relayRedialReleaser)
	if !ok {
		return false
	}
	return relayToggleRedial("release_redial", releaser.ReleaseRedial)
}

// Runs a redial toggle, a panicking one reporting false
func relayToggleRedial(name string, toggle func()) (took bool) {
	defer func() {
		if r := recover(); r != nil {
			relayAdapterLog().Debug(fmt.Sprintf("relay %s failed", name), "exc_info", fmt.Errorf("%v", r))
			took = false
		}
	}()
	toggle()
	return true
}

// Sends to an advertised logical platform, refusing one the transport does not front
func (a *RelayAdapter) SendForPlatform(ctx context.Context, platform Platform, chatID, content string, metadata map[string]any) (SendResult, error) {
	return a.sendForPlatform(ctx, string(platform), chatID, content, nil, metadata)
}

func (a *RelayAdapter) sendForPlatform(ctx context.Context, platform, chatID, content string, replyTo *string, metadata map[string]any) (SendResult, error) {
	if !a.FrontsPlatform(Platform(platform)) {
		return SendResult{Success: false, Error: "relay does not front platform " + platform}, nil
	}
	sfpMetadata := maps.Clone(metadata)
	if sfpMetadata == nil {
		sfpMetadata = map[string]any{}
	}
	interim := pyrepr.Truthy(sfpMetadata["_interim_send"])
	delete(sfpMetadata, "_interim_send")
	seal, err := a.absorbIntoOpenDraft(ctx, chatID, content, sfpMetadata, interim)
	if err != nil {
		return SendResult{}, err
	}
	if seal != nil {
		return *seal, nil
	}
	if a.transport == nil {
		return SendResult{Success: false, Error: "no transport"}, nil
	}
	a.stampSlackUnfurl(platform, sfpMetadata)
	result, err := a.transport.SendOutbound(ctx, map[string]any{"op": "send", "chat_id": chatID, "content": content,
		"reply_to": relayWireReply(replyTo), "metadata": a.withScope(chatID, a.withFormatHintsForPlatform(platform, sfpMetadata))}, platform)
	if err != nil {
		return SendResult{}, err
	}
	if !pyrepr.Truthy(result["success"]) && IsEgressDecline(result) {
		LogDecline("send", chatID, result)
	}
	sr := relaySendResult(result)
	sr.RawResponse = result
	return sr, nil
}

// Block formatting hints of a destination that advertises them and has the operator knobs on
func (a *RelayAdapter) formatHints(descriptor *CapabilityDescriptor, platform string) map[string]any {
	if descriptor == nil || !descriptor.SupportsBlockFormatting {
		return nil
	}
	knobs := a.relayPlatformExtra(pystr.Lower(platform))
	hints := map[string]any{}
	for _, knob := range []string{"rich_blocks", "markdown_blocks"} {
		if relayCoerceFlag(knobs[knob], false) {
			hints[knob] = true
		}
	}
	if len(hints) == 0 {
		return nil
	}
	return hints
}

// Metadata with the destination's format hints set unless already present
func (a *RelayAdapter) stampFormatHints(descriptor *CapabilityDescriptor, platform string, metadata map[string]any) map[string]any {
	hints := a.formatHints(descriptor, platform)
	if hints == nil {
		return metadata
	}
	merged := maps.Clone(metadata)
	if merged == nil {
		merged = map[string]any{}
	}
	if _, has := merged["format_hints"]; !has {
		merged["format_hints"] = hints
	}
	return merged
}

// Format hints for a chat addressed send, the chat's inbound platform else the primary
func (a *RelayAdapter) withFormatHintsForChat(chatID string, metadata map[string]any) map[string]any {
	d := a.descriptorForChat(chatID)
	return a.stampFormatHints(&d, a.chatPlatform(chatID), metadata)
}

// Format hints for an explicit platform send, the primary descriptor only when it is that platform
func (a *RelayAdapter) withFormatHintsForPlatform(platform string, metadata map[string]any) map[string]any {
	descriptor := a.negotiatedDescriptor(platform)
	if primary := a.Descriptor(); descriptor == nil && primary.Platform == platform {
		descriptor = &primary
	}
	return a.stampFormatHints(descriptor, platform, metadata)
}

// Sends text, sealing an open stream with a turn final and recording any connector created auto thread
func (a *RelayAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (SendResult, error) {
	sendMetadata := maps.Clone(metadata)
	if sendMetadata == nil {
		sendMetadata = map[string]any{}
	}
	explicitPlatform := sendMetadata["_relay_logical_platform"]
	delete(sendMetadata, "_relay_logical_platform")
	interim := pyrepr.Truthy(sendMetadata["_interim_send"])
	delete(sendMetadata, "_interim_send")
	seal, err := a.absorbIntoOpenDraft(ctx, chatID, content, sendMetadata, interim)
	if err != nil {
		return SendResult{}, err
	}
	if seal != nil {
		return *seal, nil
	}
	if pyrepr.Truthy(explicitPlatform) {
		var md map[string]any
		if len(sendMetadata) > 0 {
			md = sendMetadata
		}
		return a.sendForPlatform(ctx, pyrepr.Str(explicitPlatform), chatID, content, relayOptReply(replyTo), md)
	}
	if a.transport == nil {
		return SendResult{Success: false, Error: "no transport"}, nil
	}
	effectiveReplyTo := a.prepareSlackEgress(chatID, relayOptReply(replyTo), sendMetadata)
	result, err := a.outbound(ctx, chatID, map[string]any{"op": "send", "chat_id": chatID, "content": content,
		"reply_to": relayWireReply(effectiveReplyTo), "metadata": a.textMetadata(chatID, sendMetadata)})
	if err != nil {
		return SendResult{}, err
	}
	if thread, name := result["thread_id"], result["auto_thread_name"]; pyrepr.Truthy(thread) && pyrepr.Truthy(name) {
		a.mu.Lock()
		a.autoThreadByChat.set(chatID, [2]string{pyrepr.Str(thread), pyrepr.Str(name)})
		a.autoThreadByChat.evict(256)
		a.mu.Unlock()
	}
	a.mu.Lock()
	waiter := a.autoThreadWaiters[chatID]
	a.mu.Unlock()
	if waiter != nil {
		waiter.set()
	}
	return relaySendResult(result), nil
}

// Thread id and initial name of the auto thread the connector made for the latest send, nil when none
func (a *RelayAdapter) AutoThreadInfoForChat(chatID string) any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if info, ok := a.autoThreadByChat.get(chatID); ok {
		return info
	}
	return nil
}

// Auto thread info after the next send into the chat, nil when the timeout passes first
func (a *RelayAdapter) WaitForAutoThreadInfo(ctx context.Context, chatID string, timeout float64) (any, error) {
	if info := a.AutoThreadInfoForChat(chatID); info != nil {
		return info, nil
	}
	a.mu.Lock()
	waiter := a.autoThreadWaiters[chatID]
	if waiter == nil {
		waiter = newRelayEvent()
		a.autoThreadWaiters[chatID] = waiter
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.autoThreadWaiters[chatID] == waiter {
			delete(a.autoThreadWaiters, chatID)
		}
		a.mu.Unlock()
	}()
	select {
	case <-waiter.waitChan():
		return a.AutoThreadInfoForChat(chatID), nil
	default:
	}
	timer := time.NewTimer(secondsDuration(timeout))
	defer timer.Stop()
	select {
	case <-waiter.waitChan():
	case <-timer.C:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return a.AutoThreadInfoForChat(chatID), nil
}

// Reply target with the synthetic Slack DM anchor dropped in flat mode, never adding one
func (a *RelayAdapter) resolveReplyToForSend(chatID string, replyTo *string, metadata map[string]any) *string {
	if replyTo == nil {
		return nil
	}
	a.mu.Lock()
	platform, chatType := a.platformByChat[chatID], a.chatTypeByChat[chatID]
	a.mu.Unlock()
	if platform != relaySlack || chatType != "dm" || pyrepr.Truthy(metadata["thread_id"]) || pyrepr.Truthy(metadata["thread_ts"]) {
		return replyTo
	}
	if a.effectiveReplyInThread() {
		return replyTo
	}
	return nil
}

// Slack thread anchor of one frame, its mirror key stripped when dropped and promoted to thread id when kept
func (a *RelayAdapter) applySlackThreadAnchor(chatID string, replyTo *string, metadata map[string]any, mirrorKey string) *string {
	effective := a.resolveReplyToForSend(chatID, replyTo, metadata)
	if effective == nil && replyTo != nil {
		delete(metadata, mirrorKey)
	}
	a.mu.Lock()
	platform := a.platformByChat[chatID]
	a.mu.Unlock()
	if effective != nil && platform == relaySlack && !(pyrepr.Truthy(metadata["thread_id"]) || pyrepr.Truthy(metadata["thread_ts"])) {
		metadata["thread_id"] = *effective
	}
	return effective
}

// Slack thread anchor and unfurl hints of a text or media frame, the metadata mutated in place
func (a *RelayAdapter) prepareSlackEgress(chatID string, replyTo *string, metadata map[string]any) *string {
	effective := a.applySlackThreadAnchor(chatID, replyTo, metadata, "reply_to_message_id")
	a.stampSlackUnfurl(a.chatPlatform(chatID), metadata)
	return effective
}

// Metadata copy with the Slack DM status line anchored on the turn message or the latest inbound ts
func (a *RelayAdapter) withStatusThreadAnchor(chatID string, metadata map[string]any, anchor string) map[string]any {
	md := maps.Clone(metadata)
	if md == nil {
		md = map[string]any{}
	}
	a.mu.Lock()
	platform, chatType, latest := a.platformByChat[chatID], a.chatTypeByChat[chatID], a.lastInboundTSByChat[chatID]
	a.mu.Unlock()
	if !(pyrepr.Truthy(md["thread_id"]) || pyrepr.Truthy(md["thread_ts"])) && platform == relaySlack && chatType == "dm" {
		if anchor == "" {
			anchor = latest
		}
		if anchor != "" {
			md["thread_id"] = anchor
		}
	}
	return md
}

// Typing metadata of a turn with its status anchor fixed on the turn's own message
func (a *RelayAdapter) StatusMetadataForTurn(chatID, anchor string, metadata map[string]any) map[string]any {
	return a.withStatusThreadAnchor(chatID, metadata, anchor)
}

// Edits a relayed message through the connector
func (a *RelayAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (SendResult, error) {
	return a.EditMessageWithMetadata(ctx, chatID, messageID, content, finalize, nil)
}

// Edits a relayed message with its routing metadata, the structured decline carried in the result
func (a *RelayAdapter) EditMessageWithMetadata(ctx context.Context, chatID, messageID, content string, finalize bool, metadata map[string]any) (SendResult, error) {
	if a.transport == nil {
		return SendResult{Success: false, Error: "no transport"}, nil
	}
	result, err := a.outbound(ctx, chatID, map[string]any{"op": "edit", "chat_id": chatID, "message_id": messageID,
		"content": content, "metadata": a.textMetadata(chatID, metadata)})
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{Success: pyrepr.Truthy(result["success"]), MessageID: pyrepr.Or(result["message_id"], messageID),
		Error: result["error"], RawResponse: result}, nil
}

// Deletes a relayed message when the chat's descriptor advertises delete, false otherwise or on failure
func (a *RelayAdapter) DeleteMessage(ctx context.Context, chatID, messageID string) (bool, error) {
	if a.transport == nil {
		return false, nil
	}
	if !containsString(a.descriptorForChat(chatID).SupportedOps, "delete") {
		return false, nil
	}
	result, err := a.outbound(ctx, chatID, map[string]any{"op": "delete", "chat_id": chatID, "message_id": messageID,
		"metadata": a.withScope(chatID, map[string]any{})})
	if err != nil {
		if ctx.Err() != nil {
			return false, err
		}
		relayAdapterLog().Debug("relay delete_message failed", "exc_info", err)
		return false, nil
	}
	return pyrepr.Truthy(result["success"]), nil
}

// Egresses one typing frame carrying the chat's live status phrase when one is set
func (a *RelayAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	if a.transport == nil {
		return nil
	}
	var content *string
	if phrase, ok := a.StatusText(chatID); ok && phrase != "" {
		content = &phrase
	}
	return a.typingFrame(ctx, chatID, metadata, content, "send_typing")
}

// Sends the explicit empty status clear, Slack chats only
func (a *RelayAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.mu.Lock()
	platform := a.platformByChat[chatID]
	a.mu.Unlock()
	if a.transport == nil || platform != relaySlack {
		return nil
	}
	clear := ""
	return a.typingFrame(ctx, chatID, metadata, &clear, "stop_typing")
}

// One status anchored typing frame, content omitted when nil, failures logged and swallowed
func (a *RelayAdapter) typingFrame(ctx context.Context, chatID string, metadata map[string]any, content *string, lane string) error {
	md := a.withStatusThreadAnchor(chatID, metadata, "")
	frame := map[string]any{"op": "typing", "chat_id": chatID, "metadata": a.withScope(chatID, md)}
	if content != nil {
		frame["content"] = *content
	}
	if _, err := a.outbound(ctx, chatID, frame); err != nil {
		if ctx.Err() != nil {
			return err
		}
		relayAdapterLog().Debug(fmt.Sprintf("relay %s failed for %s", lane, chatID), "exc_info", err)
	}
	return nil
}

// Chat info from the connector, a local DM shape when the op is unadvertised or there is no transport
func (a *RelayAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	if a.transport == nil || !a.Descriptor().SupportsOp("get_chat_info") {
		return map[string]any{"name": chatID, "type": "dm"}, nil
	}
	return a.transport.GetChatInfo(ctx, chatID)
}

// Sends through a session bound shared identity capability, the connector resolving the credential
func (a *RelayAdapter) SendFollowUp(ctx context.Context, sessionKey, kind, content string, metadata map[string]any) (SendResult, error) {
	if a.transport == nil {
		return SendResult{Success: false, Error: "no transport"}, nil
	}
	platform := ""
	if prefix, _, found := strings.Cut(kind, "."); found && prefix != "" && a.FrontsPlatform(Platform(prefix)) {
		platform = prefix
	}
	followUpMetadata := maps.Clone(metadata)
	if followUpMetadata == nil {
		followUpMetadata = map[string]any{}
	}
	if !pyrepr.Truthy(followUpMetadata["profile"]) {
		if profile := relayProfileFromSessionKey(sessionKey); profile != nil && *profile != "" {
			followUpMetadata["profile"] = *profile
		}
	}
	result, err := a.transport.SendFollowUp(ctx, map[string]any{"op": "follow_up", "session_key": sessionKey, "kind": kind,
		"content": content, "metadata": followUpMetadata}, platform)
	if err != nil {
		return SendResult{}, err
	}
	if !pyrepr.Truthy(result["success"]) && IsEgressDecline(result) {
		LogDecline("follow_up", sessionKey, result)
	}
	sr := relaySendResult(result)
	sr.RawResponse = result
	return sr, nil
}

// Fresh final delivery for a link bearing final when Slack unfurl is forced on for the chat's platform
func (a *RelayAdapter) PrefersFreshFinalStreaming(content string, metadata map[string]any, chatID string) bool {
	var platform any
	if chatID != "" {
		a.mu.Lock()
		if p, ok := a.platformByChat[chatID]; ok {
			platform = p
		}
		a.mu.Unlock()
	}
	if platform == nil && metadata != nil {
		platform = metadata["platform"]
	}
	if platform == nil {
		platform = a.Descriptor().Platform
	}
	hints := a.slackUnfurlHints(pyrepr.Str(pyrepr.Or(platform, "")))
	forced := false
	for _, v := range hints {
		forced = forced || v
	}
	return forced && relayURLRe.MatchString(content)
}
