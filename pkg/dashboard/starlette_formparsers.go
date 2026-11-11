package dashboard

// Form body the parser refuses, answered as a 400 with its message inside an app
type MultiPartException struct{ Message string }

func (e *MultiPartException) Error() string      { return e.Message }
func (e *MultiPartException) PyTypeName() string { return "MultiPartException" }
