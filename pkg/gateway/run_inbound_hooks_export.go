package gateway

import "context"

// True inside the execution of a claimed admission, where ingress hooks already ran
func ExecutingAdmission(ctx context.Context) bool { return executingAdmissionOf(ctx) }

// Copy of the event with only its dataclass fields and new text, sharing the same source as dataclasses replace does
func ReplaceMessageEventText(event *MessageEvent, text string) *MessageEvent {
	gatewayAcceptedMu.Lock()
	eventTurnStateMu.Lock()
	copied := *event
	eventTurnStateMu.Unlock()
	gatewayAcceptedMu.Unlock()
	copied.Text = text
	copied.gatewayAccepted, copied.OnCommit, copied.admittedBeforeDrain = false, nil, false
	copied.notificationReplyMuted, copied.pendingSTTText, copied.pendingSTTTranscripts = nil, nil, nil
	copied.pendingSTTClips, copied.pendingSTTEchoed = nil, 0
	copied.turnMarkerHandoff, copied.activeTurnToken, copied.markerReleased = false, nil, nil
	copied.heartbeatSessionID, copied.lastChunkLen, copied.busyArrival = nil, 0, nil
	copied.claimedOwner, copied.activeTurnSessionKey, copied.turnRefusal = nil, nil, nil
	copied.heartbeatResolvedID, copied.heartbeatStarted = nil, false
	copied.producerDelivery, copied.ingressSHA256, copied.consumerReply, copied.webhookDuplicate = false, "", nil, false
	copied.botLoopAdmitted, copied.fifoOnly, copied.stopEpoch = false, false, 0
	return &copied
}
