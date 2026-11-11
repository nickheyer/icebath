package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Format hinted and scope stamped metadata of a text frame
func (a *RelayAdapter) textMetadata(chatID string, metadata map[string]any) map[string]any {
	return a.withScope(chatID, a.withFormatHintsForChat(chatID, metadata))
}

// Draft frame of one stream update
func (a *RelayAdapter) draftFrame(chatID string, draftID int, content string, final bool, metadata map[string]any) map[string]any {
	md := map[string]any{}
	for k, v := range metadata {
		md[k] = v
	}
	return map[string]any{"op": "draft", "chat_id": chatID, "draft_id": draftID, "content": content, "final": final,
		"metadata": a.textMetadata(chatID, md)}
}

// Streams one draft frame, a sealed stream's straggler swallowed and a declined frame retiring the open draft
func (a *RelayAdapter) SendDraft(ctx context.Context, chatID string, draftID int, content string, metadata map[string]any) (SendResult, error) {
	if !a.SupportsDraftStreaming("", nil, chatID) {
		return SendResult{}, fmt.Errorf("NotImplementedError: connector does not advertise the 'draft' relay op")
	}
	if a.transport == nil {
		return SendResult{Success: false, Error: "no transport"}, nil
	}
	chatKey := relayDraftKey(chatID, metadata)
	a.mu.Lock()
	if sealed, ok := a.sealedDraftByChat.get(chatKey); ok && sealed == draftID {
		first := !a.tombstoneSwallowLogged.has(chatKey)
		if first {
			a.tombstoneSwallowLogged.set(chatKey, draftID)
			a.tombstoneSwallowLogged.evict(relayDraftStateCap)
		}
		a.mu.Unlock()
		if first {
			relayAdapterLog().Warn(fmt.Sprintf("relay: draft frame for %s swallowed by post-seal tombstone (draft_id=%d) — expected for a straggler; "+
				"a live stream freezing NOW means something sealed it mid-flight", chatKey, draftID))
		}
		return SendResult{Success: true}, nil
	}
	a.mu.Unlock()
	if a.StreamIsMessageForChat(chatID) {
		a.mu.Lock()
		a.openDraftByChat.set(chatKey, draftID)
		a.openDraftByChat.evict(relayDraftStateCap)
		a.mu.Unlock()
	}
	result, err := a.outbound(ctx, chatID, a.draftFrame(chatID, draftID, content, false, metadata))
	if err != nil {
		if ctx.Err() != nil {
			return SendResult{}, err
		}
		return SendResult{Success: false, Error: "draft transport error: " + relayExcStr(err)}, nil
	}
	if pyrepr.Truthy(result["success"]) {
		return SendResult{Success: true}, nil
	}
	if pyrepr.Truthy(result["ambiguous"]) {
		return SendResult{Success: false, Error: pyrepr.Str(pyrepr.Or(result["error"], "draft ack lost")), RawResponse: result}, nil
	}
	a.mu.Lock()
	if open, ok := a.openDraftByChat.get(chatKey); ok && open == draftID {
		a.openDraftByChat.pop(chatKey)
	}
	a.mu.Unlock()
	if IsEgressDecline(result) {
		LogDecline("draft", chatID, result)
	}
	errText := pyrepr.Or(result["error"], DeclineError(result))
	return SendResult{Success: false, Error: pyrepr.Str(pyrepr.Or(errText, "draft failed")), RawResponse: result}, nil
}

// Seals the open stream with its final content, retrying once when the ack is lost and restoring it on cancellation
func (a *RelayAdapter) sealOpenDraft(ctx context.Context, chatID, content string, metadata map[string]any, draftKey string) (SendResult, error) {
	if draftKey == "" {
		draftKey = relayDraftKey(chatID, metadata)
	}
	a.mu.Lock()
	draftID, ok := a.openDraftByChat.pop(draftKey)
	if !ok {
		a.mu.Unlock()
		return SendResult{}, &PyError{"KeyError", pyrepr.Repr(draftKey)}
	}
	a.sealedDraftByChat.set(draftKey, draftID)
	a.sealedDraftByChat.evict(relayDraftStateCap)
	platform := a.platformByChat[chatID]
	a.mu.Unlock()
	if a.transport == nil {
		return SendResult{Success: false, Error: "no transport"}, nil
	}
	sealFrame := a.draftFrame(chatID, draftID, content, true, metadata)
	attempt := func() map[string]any {
		r, err := a.transport.SendOutbound(ctx, sealFrame, platform)
		if err != nil {
			relayAdapterLog().Warn(fmt.Sprintf("relay seal transport error (ambiguous): %s", relayExcStr(err)))
			return nil
		}
		if pyrepr.Truthy(r["ambiguous"]) {
			relayAdapterLog().Warn(fmt.Sprintf("relay seal ack lost (ambiguous): %s", pyrepr.Str(r["error"])))
			return nil
		}
		return r
	}
	result := attempt()
	if result == nil && ctx.Err() == nil {
		result = attempt()
	}
	if ctx.Err() != nil {
		a.mu.Lock()
		a.openDraftByChat.set(draftKey, draftID)
		if sealed, ok := a.sealedDraftByChat.get(draftKey); ok && sealed == draftID {
			a.sealedDraftByChat.pop(draftKey)
		}
		a.mu.Unlock()
		return SendResult{}, ctx.Err()
	}
	if result == nil {
		return SendResult{Success: false, Error: "draft seal ambiguous after retry (transport ack lost)",
			RawResponse: map[string]any{"success": false, "ambiguous": true}}, nil
	}
	if pyrepr.Truthy(result["success"]) {
		var messageID any
		if id := pyrepr.Str(pyrepr.Or(result["message_id"], "")); id != "" {
			messageID = id
		}
		return SendResult{Success: true, MessageID: messageID}, nil
	}
	if IsEgressDecline(result) {
		LogDecline("draft_seal", chatID, result)
	}
	errText := pyrepr.Or(result["error"], DeclineError(result))
	return SendResult{Success: false, Error: pyrepr.Str(pyrepr.Or(errText, "draft seal failed")), RawResponse: result}, nil
}

// Final content sealed into the chat's open stream, nil when there is none or the seal failed without a decline
func (a *RelayAdapter) absorbIntoOpenDraft(ctx context.Context, chatID, content string, metadata map[string]any, interim bool) (*SendResult, error) {
	if interim {
		return nil, nil
	}
	key, ok := a.matchOpenDraft(chatID, metadata)
	if !ok {
		return nil, nil
	}
	seal, err := a.sealOpenDraft(ctx, chatID, content, metadata, key)
	if err != nil {
		return nil, err
	}
	if seal.Success {
		return &seal, nil
	}
	if IsEgressDecline(seal.RawResponse) {
		relayAdapterLog().Warn(fmt.Sprintf("relay draft seal DECLINED for %s — not falling back to a plain send (the destination is not approved for this connection)", chatID))
		return &seal, nil
	}
	relayAdapterLog().Warn(fmt.Sprintf("relay seal failed (%s); delivering turn-final as plain send", pyrepr.Str(seal.Error)))
	return nil, nil
}
