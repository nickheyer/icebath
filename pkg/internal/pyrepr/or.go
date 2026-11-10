package pyrepr

// Value of the Python expression v or def
func Or(v, def any) any {
	if Truthy(v) {
		return v
	}
	return def
}
