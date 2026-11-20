package updaterecord

// Clears the run's refused child and ran flag and returns the call that restores them
func ResetCustodyRun() func() {
	custodyRun.Lock()
	ran, refused, unchanged := custodyRun.ran, custodyRun.refused, custodyRun.unchanged
	custodyRun.ran, custodyRun.refused, custodyRun.unchanged = false, nil, false
	custodyRun.Unlock()
	return func() {
		custodyRun.Lock()
		custodyRun.ran, custodyRun.refused, custodyRun.unchanged = ran, refused, unchanged
		custodyRun.Unlock()
	}
}
