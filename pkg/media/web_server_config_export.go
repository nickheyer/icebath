package media

import "slices"

// Catalog models of a speech to text provider in display order, nil for an unknown provider
func STTModelCatalog(provider string) []string { return slices.Clone(sttModelCatalog[provider]) }

// Whether a name is reserved for a native speech to text handler
func IsBuiltinSTTProvider(name string) bool { return builtinSTTProviders[name] }

// Whether a name is reserved for a native text to speech handler
func IsBuiltinTTSProvider(name string) bool { return builtinTTSProviders[name] }
