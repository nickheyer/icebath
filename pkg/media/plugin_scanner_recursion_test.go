package media

import (
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Listing row of one plugin key, nil when the manager never recorded it
func scannerPluginRow(m *plugins.Manager, key string) map[string]any {
	for _, row := range m.ListPlugins() {
		if row["key"] == key {
			return row
		}
	}
	return nil
}
