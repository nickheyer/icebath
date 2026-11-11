package state

// Compression lock acquire answering the raise a worker store can make, the canonical store never raising
func (d *SessionDB) TryAcquireCompressionLockChecked(sessionID, holder string, ttlSeconds float64) (bool, error) {
	return d.TryAcquireCompressionLock(sessionID, holder, ttlSeconds), nil
}

// Compression lock renewal answering the raise a worker store can make
func (d *SessionDB) RefreshCompressionLockChecked(sessionID, holder string, ttlSeconds float64) (bool, error) {
	return d.RefreshCompressionLock(sessionID, holder, ttlSeconds), nil
}

// Compression lock release answering the raise a worker store can make
func (d *SessionDB) ReleaseCompressionLockChecked(sessionID, holder string) error {
	d.ReleaseCompressionLock(sessionID, holder)
	return nil
}

// Cooldown record answering the raise a worker store can make
func (d *SessionDB) RecordCompressionFailureCooldownChecked(sessionID string, cooldownUntil float64, errValue *string) error {
	d.RecordCompressionFailureCooldown(sessionID, cooldownUntil, errValue)
	return nil
}

// Cooldown clear answering the raise a worker store can make
func (d *SessionDB) ClearCompressionFailureCooldownChecked(sessionID string) error {
	d.ClearCompressionFailureCooldown(sessionID)
	return nil
}

// Token count drain answering the raise a worker store can make
func (d *SessionDB) FlushTokenCountsChecked(timeoutS float64) (bool, error) {
	return d.FlushTokenCounts(timeoutS), nil
}
