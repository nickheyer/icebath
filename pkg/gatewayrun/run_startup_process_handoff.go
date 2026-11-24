package gatewayrun

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Executes one handoff row as an inline synthetic turn and posts its reply, an error marking the row failed
func (r *GatewayRunner) processHandoff(ctx context.Context, row *jsonutil.Object, profileName *string) error {
	rawID, ok := row.Get("id")
	if !ok {
		return errors.New("a process handoff row has no id")
	}
	cliSessionID := anyutil.String(rawID)
	rowMap := make(map[string]any, row.Len())
	for _, key := range row.Keys() {
		value, _ := row.Get(key)
		rowMap[key] = value
	}
	dest, err := r.handoffResolveDestination(ctx, rowMap, profileName)
	if err != nil {
		return err
	}
	sessionKey, err := r.handoffSessionKey(ctx, dest, profileName)
	if err != nil {
		return err
	}
	if err := r.handoffRepoint(ctx, dest, sessionKey, cliSessionID); err != nil {
		return err
	}
	r.evictCachedAgent(ctx, sessionKey, false, false)
	r.releaseRunningAgentState(ctx, sessionKey, nil)
	cliTitle := ""
	if title := row.ValueOr("title", nil); anyutil.IsTrue(title) {
		cliTitle = anyutil.String(title)
	} else {
		runes := []rune(cliSessionID)
		cliTitle = string(runes[:min(8, len(runes))])
	}
	syntheticEvent := gateway.NewMessageEvent(fmt.Sprintf("[Session was just handed off from CLI (\"%s\") to this "+
		"channel. The full prior conversation history is loaded above. "+
		"Briefly confirm you're working here and summarize what we were "+
		"working on, so the user can continue from this device.]", cliTitle))
	syntheticEvent.Source, syntheticEvent.Internal = dest.source, true
	pkgLog().Info(fmt.Sprintf("Handoff: dispatching synthetic turn for CLI session %s → %s (home=%s, thread=%s, session_key=%s)",
		cliSessionID, dest.platformName, dest.homeChatID, derefOrNoneText(dest.effectiveThreadID), sessionKey))
	response, err := r.handleMessage(ctx, syntheticEvent)
	if err != nil {
		return err
	}
	responseText := ""
	switch typed := response.(type) {
	case string:
		responseText = typed
	case *gateway.EphemeralReply:
		if typed != nil {
			responseText = typed.Text()
		}
	}
	if responseText == "" {
		return nil
	}
	var sendMetadata map[string]any
	if dest.effectiveThreadID != nil && *dest.effectiveThreadID != "" {
		sendMetadata = map[string]any{"thread_id": *dest.effectiveThreadID}
	}
	result, err := dest.transport.Send(ctx, dest.platform, dest.homeChatID, responseText, sendMetadata)
	if err != nil {
		return fmt.Errorf("adapter.send failed: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("adapter.send failed: %s", sendError(result))
	}
	return nil
}
