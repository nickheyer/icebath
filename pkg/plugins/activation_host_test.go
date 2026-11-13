package plugins

import (
	"strings"
)

const declaresFixture = "name: depprobe\nversion: '1.0'\npython_dependencies:\n  - '"

// Warning log lines of one discovery that name the depprobe plugin
func depprobeWarnings(logs string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "depprobe") {
			out = append(out, line)
		}
	}
	return out
}

const importsFixture = "import icebath_depprobe_fixture\n\ndef register(ctx):\n    pass\n"

// Reports whether the manager holds the probe hook on on_session_start
func probeHooked(m *Manager) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.hooks["on_session_start"] {
		if strings.Contains(e.name, "_probe_hook") {
			return true
		}
	}
	return false
}
