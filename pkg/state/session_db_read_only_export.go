package state

// IsReadOnly reports whether the handle was opened read only
func (d *SessionDB) IsReadOnly() bool { return d.isReadOnly }
