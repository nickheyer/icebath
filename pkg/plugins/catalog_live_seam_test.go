package plugins

// Forgets the resolved in-tree catalog time so the next call resolves it again
func resetInTreeCatalogTime() {
	inTreeCatalogTimeMu.Lock()
	inTreeCatalogTimeResolved, inTreeCatalogTimeValue = false, nil
	inTreeCatalogTimeMu.Unlock()
}
