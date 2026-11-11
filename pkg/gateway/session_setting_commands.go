package gateway

// Principal and level of a reasoning command that arrived while a turn ran, a nil effort resetting the level
type pendingReasoning struct {
	principal string
	effort    any
}
