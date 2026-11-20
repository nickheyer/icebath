package tuiserver

// Handler registered for the method name, nil when none, the lookup of the server _methods table
func Method(name string) Handler { return lookupMethod(name) }
