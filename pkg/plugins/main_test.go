package plugins

// Empties the bundled table of plugins that packages linked for the external tests registered at init
func dropImportedBundledPlugins() {
	bundledMu.Lock()
	defer bundledMu.Unlock()
	bundledByKey, bundledOrder = map[string]Plugin{}, nil
}
