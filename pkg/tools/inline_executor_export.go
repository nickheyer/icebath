package tools

// Agent level executor installed for a tool name, nil when none is
func InlineToolExecutorOf(name string) InlineToolExecutor { return inlineToolExecutor(name) }
