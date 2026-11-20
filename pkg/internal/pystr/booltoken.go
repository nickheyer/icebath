package pystr

// True or false for a recognized truthy or falsy token after strip and lower, ok false for anything else
func BoolToken(s string) (value, ok bool) {
	switch Lower(Strip(s)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}
