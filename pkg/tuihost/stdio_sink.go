package tuihost

// Module stdio sink a request falls back to when no transport is bound
func stdioTransport() Transport {
	return stdioSink
}
