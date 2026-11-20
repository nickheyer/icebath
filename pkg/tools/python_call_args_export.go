package tools

// Python values of a call's args for handlers outside the package, ints kept apart from floats
func PythonCallArgs(call *Call) map[string]any { return pythonCallArgs(call) }
