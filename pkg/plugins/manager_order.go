package plugins

import "sort"

// Plugin key with its loaded record
type OrderedPlugin struct {
	Key    string
	Plugin *LoadedPlugin
}

// Stores a loaded record under its key, a new key placed after every key already held, under m.mu
func (m *Manager) putPluginLocked(key string, loaded *LoadedPlugin) {
	if _, held := m.plugins[key]; !held {
		if m.pluginSeq == nil {
			m.pluginSeq = map[string]uint64{}
		}
		m.pluginSeqNext++
		m.pluginSeq[key] = m.pluginSeqNext
	}
	m.plugins[key] = loaded
}

// Removes a key and its place in the order, under m.mu
func (m *Manager) dropPluginLocked(key string) {
	delete(m.plugins, key)
	delete(m.pluginSeq, key)
}

// Every loaded record in the order its key was first stored
func (m *Manager) LoadedPluginsInOrder() []OrderedPlugin {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]OrderedPlugin, 0, len(m.plugins))
	for key, p := range m.plugins {
		out = append(out, OrderedPlugin{Key: key, Plugin: p})
	}
	sort.Slice(out, func(i, j int) bool { return m.pluginSeq[out[i].Key] < m.pluginSeq[out[j].Key] })
	return out
}
