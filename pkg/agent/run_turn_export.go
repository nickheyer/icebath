package agent

// Whether the latest compression attempt recorded its per attempt signals
func (a *AIAgent) LastCompressionAttemptRecorded() bool { return a.lastCompressionAttemptRecorded }

// Lock skip signal of the latest compression attempt, true or the holder text
func (a *AIAgent) CompressionSkippedDueToLock() any { return a.compressionSkippedDueToLock }

// Transient block reason of the latest compression attempt
func (a *AIAgent) CompressionBlockedTransient() any { return a.compressionBlockedTransient }

// In place outcome of the latest compression attempt, nil when it aborted before a commit boundary
func (a *AIAgent) LastCompressionAttemptInPlace() *bool { return a.lastCompressionAttemptInPlace }

// Whether the latest compression summary timed out
func (a *AIAgent) LastCompressionTimedOut() bool { return a.lastCompressionTimedOut }

// Summary warning of the latest compression attempt
func (a *AIAgent) LastCompressionSummaryWarning() *string { return a.lastCompressionSummaryWarning }

// Whether the agent switched to a fallback provider during its run
func (a *AIAgent) FallbackActivated() bool { return a.fallbackActivated }
