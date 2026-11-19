package monitoring

func (m *InMemorySpanExporter) Export(spans []*Span) error {
	m.mu.Lock()
	m.spans = append(m.spans, spans...)
	m.mu.Unlock()
	return nil
}

func (m *InMemorySpanExporter) Shutdown() error { return nil }

// FinishedSpans returns a copy of every span exported so far
func (m *InMemorySpanExporter) FinishedSpans() []*Span {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Span{}, m.spans...)
}

// NewSimpleTracerProvider exports every span as it ends, the in memory test provider
func NewSimpleTracerProvider(resource map[string]string, exporter SpanExporter) *TracerProvider {
	return &TracerProvider{resource: resource, exporter: exporter}
}
