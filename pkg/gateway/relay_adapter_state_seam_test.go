package gateway

func (m *relayOrdered[V]) size() int { return len(m.order) }
