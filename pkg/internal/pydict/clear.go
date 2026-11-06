package pydict

// Removes every key while the dict keeps its identity as dict.clear does
func (d *Dict[K, V]) Clear() {
	d.keys = nil
	clear(d.vals)
}
