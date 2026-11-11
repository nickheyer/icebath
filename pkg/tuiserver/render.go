package tuiserver

// Incremental renderer of streamed assistant text the turn feeds each delta to
type streamRenderer interface {
	Feed(delta string) (rendered string, ok bool)
}

// Rendered reply text from the agent rich output module, absent upstream so the client renders its own markdown
func renderMessage(text string, cols int) (string, bool) { return "", false }

// Rendered diff text from the agent rich output module, absent upstream so the client renders its own diff
func renderDiff(text string, cols int) (string, bool) { return "", false }

// Streaming renderer from the agent rich output module, absent upstream so deltas carry no rendered copy
func makeStreamRenderer(cols int) streamRenderer { return nil }
