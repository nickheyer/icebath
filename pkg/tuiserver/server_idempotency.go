package tuiserver

// Runtime session a create or branch idempotency key answered with, and when it was last used
type idempotencyEntry struct {
	sid string
	at  float64
}

// Seconds an idempotency key keeps answering with its session, longer than any realistic retry window
const idempotencyKeyTTL = 300.0

// Idempotency keys of session.create and session.branch, guarded by sessionsLock
var idempotencyKeys = map[string]idempotencyEntry{}
