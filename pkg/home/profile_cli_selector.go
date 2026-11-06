package home

// Returns the -p name selector with a trailing space for a named profile home, empty for default or custom homes
func ProfileCLISelector(h string) string {
	name := ProfileNameFor(h)
	if name != "" && name != "default" {
		return "-p " + name + " "
	}
	return ""
}
