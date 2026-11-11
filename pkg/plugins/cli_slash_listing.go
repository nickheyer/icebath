package plugins

import "sort"

// QuickPluginListing is the disk discovery and allow and deny lists the classic CLI /plugins glance reads
type QuickPluginListing struct {
	Entries  []PluginEntry
	Enabled  map[string]bool
	Disabled map[string]bool
}

// Discovers every plugin on disk with the configured enabled and disabled sets
func LoadQuickPluginListing(h string) (QuickPluginListing, error) {
	entries, err := DiscoverAllPlugins(h)
	if err != nil {
		return QuickPluginListing{}, err
	}
	return QuickPluginListing{Entries: entries, Enabled: getEnabledSet(h), Disabled: getDisabledSet(h)}, nil
}

// Activation state of one entry by name or key against the allow and deny lists only
func (l QuickPluginListing) Status(h string, e PluginEntry) string {
	status, err := pluginStatus(h, e, l.Enabled, l.Disabled, nil, false)
	if err != nil {
		panic(err)
	}
	return status
}

// Non bundled entries sorted as Python sorts their tuples, with the bundled count
func (l QuickPluginListing) UserEntries() ([]PluginEntry, int) {
	var user []PluginEntry
	for _, e := range l.Entries {
		if e.Source != "bundled" {
			user = append(user, e)
		}
	}
	sort.SliceStable(user, func(i, j int) bool {
		a, b := user[i], user[j]
		for _, pair := range [][2]string{{a.Name, b.Name}, {a.Version, b.Version}, {a.Description, b.Description}, {a.Source, b.Source}, {a.Dir, b.Dir}, {a.Key, b.Key}} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return false
	})
	return user, len(l.Entries) - len(user)
}
