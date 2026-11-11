package gateway

// Message id a reply to the event should anchor on, nil for none
func ReplyAnchorForEvent(event *MessageEvent) *string { return replyAnchorForEvent(event) }
