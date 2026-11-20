package gateway

// Whether code approvals on the platform are refused after too many failed attempts
func (s *PairingStore) IsLockedOut(platform string) (bool, error) {
	return s.isLockedOut(platform)
}
