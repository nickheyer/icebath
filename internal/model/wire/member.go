package wire

func (m *Message) DropMember(key string) bool {
	sh := m.sourceShape()
	if sh == nil {
		return false
	}
	kept := make([]member, 0, len(sh.members))
	for _, mb := range sh.members {
		if mb.key != key {
			kept = append(kept, mb)
		}
	}
	if len(kept) == len(sh.members) {
		return false
	}
	m.src = &messageSource{shape: &shape{members: kept, layout: sh.layout}, snap: m.src.snap}
	return true
}
