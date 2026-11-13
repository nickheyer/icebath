package connections

// SaveConnectionConfig writes the v1 settings of a payload and answers their renderer view
func (s *Service) SaveConnectionConfig(payload any) (ConnectionConfigView, error) {
	if err := s.assertCanMutateManagedPrimaryRouting(); err != nil {
		return ConnectionConfigView{}, err
	}
	input := objectOrEmpty(payload)
	s.mu.Lock()
	config, err := s.coerceDesktopConnectionConfig(input, s.readDesktopConnectionConfig(), true)
	if err == nil {
		err = s.writeDesktopConnectionConfig(config)
	}
	s.mu.Unlock()
	if err != nil {
		return ConnectionConfigView{}, err
	}
	return s.sanitizeDesktopConnectionConfig(config, input["profile"]), nil
}
