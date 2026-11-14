package env

type groupHandoff interface {
	HandOffGroup() (pgid int, ok bool)
	ReturnGroup(pgid int)
}

var (
	_ groupHandoff = (*cmdProcess)(nil)
	_ groupHandoff = (*pumped)(nil)
)

func (p *cmdProcess) HandOffGroup() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.owned {
		return 0, false
	}
	p.owned = false
	return p.pgid, true
}

func (p *cmdProcess) ReturnGroup(pgid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pgid == p.pgid && p.guard != nil {
		p.owned = true
	}
}

func (p *pumped) HandOffGroup() (int, bool) {
	if h, ok := p.src.(groupHandoff); ok {
		return h.HandOffGroup()
	}
	return 0, false
}

func (p *pumped) ReturnGroup(pgid int) {
	if h, ok := p.src.(groupHandoff); ok {
		h.ReturnGroup(pgid)
	}
}
