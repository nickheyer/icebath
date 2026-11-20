package plugins

// Keys of every plugin the manager has a record for, sorted
func (m *Manager) PluginKeys() []string {
	keys, _ := m.loadedSnapshot()
	return keys
}
