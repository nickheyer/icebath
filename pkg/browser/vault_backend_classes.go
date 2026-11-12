package browser

// Name and display name of every external password manager backend in hermes order
func ExternalLoginBackendClasses() [][2]string {
	classes := externalBackendClasses()
	out := make([][2]string, len(classes))
	for i, cls := range classes {
		out[i] = [2]string{cls.name, cls.displayName}
	}
	return out
}
