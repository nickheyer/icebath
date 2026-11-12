package tools

// Runs the progress line, activity stamp, start callbacks and checkpoint preflight of one call
func (e *Executor) BeginToolExecution(ref *ToolCallRef, displayIndex int) {
	e.beginToolExecution(ref, displayIndex)
}
