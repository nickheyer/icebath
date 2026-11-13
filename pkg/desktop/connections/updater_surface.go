package connections

// Whether one remote backend serves this desktop, read by the update hand-off to skip the local gateway restart
func (s *Service) GlobalRemoteActive() bool {
	return s.globalRemoteActive()
}
