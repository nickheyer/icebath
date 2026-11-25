package tools

// Arguments of a call for handlers outside the package, whole numbers kept apart from floats
func PythonCallArgs(call *Call) map[string]any { return pythonCallArgs(call) }
