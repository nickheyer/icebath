package gateway

// Flag a counted slash command carries so a requeued busy event is never counted twice
func (e *MessageEvent) SlashCommandCounted() *bool { return &e.slashCommandCounted }
