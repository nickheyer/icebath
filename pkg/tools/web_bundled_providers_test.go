package tools

// Registers the eleven bundled web providers globally as plugin discovery does in the linked binary
func installBundledWebProviders() {
	for _, p := range []WebProvider{NewBraveFreeWebProvider(), NewDDGSWebProvider(), NewExaWebProvider(), NewFirecrawlWebProvider(),
		NewKeenableWebProvider(), NewOpenAINativeWebProvider(), NewParallelWebProvider(), NewPerplexityWebProvider(),
		NewSearXNGWebProvider(), NewTavilyWebProvider(), NewXAIWebProvider()} {
		RegisterWebProvider(p)
	}
}
