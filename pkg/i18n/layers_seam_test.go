package i18n

// RegisteredPacks returns the registrations in order
func RegisteredPacks() []*PackEntry {
	layerMu.Lock()
	defer layerMu.Unlock()
	return append([]*PackEntry{}, packs...)
}
