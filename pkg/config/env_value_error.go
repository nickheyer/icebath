package config

// Refusal of a .env write for an invalid, denylisted or locked key
type EnvValueError struct{ Msg string }

func (e *EnvValueError) Error() string { return e.Msg }

// Kind of the refusal, the ValueError family invalid values belong to
func (e *EnvValueError) ErrorKind() string { return "ValueError" }
