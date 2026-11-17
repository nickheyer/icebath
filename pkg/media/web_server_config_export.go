package media

import "slices"

// Catalog models of a speech to text provider in display order, nil for an unknown provider
func STTModelCatalog(provider string) []string { return slices.Clone(sttModelCatalog[provider]) }
