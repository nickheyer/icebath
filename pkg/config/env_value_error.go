package config

// ValueError a .env write raises for an invalid, denylisted or locked key
type EnvValueError struct{ Msg string }

func (e *EnvValueError) Error() string { return e.Msg }

// Python class the error stands for
func (e *EnvValueError) PyTypeName() string { return "ValueError" }
