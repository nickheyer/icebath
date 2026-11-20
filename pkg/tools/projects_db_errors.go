package tools

import "fmt"

// ProjectsValueError is a projects database argument rejection, the ValueError of projects_db
type ProjectsValueError struct{ Msg string }

func (e *ProjectsValueError) Error() string { return e.Msg }

// Formatted argument rejection of the projects database
func projectValueErrorf(format string, a ...any) error {
	return &ProjectsValueError{Msg: fmt.Sprintf(format, a...)}
}
