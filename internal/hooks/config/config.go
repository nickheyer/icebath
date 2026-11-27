package config

const (
	Key = "hooks"
	// Longest a hook may take before it is cut off, in seconds; 0 for no limit
	CallbackTimeout = Key + ".callback_timeout"
	// Hooks are handed the model's reasoning as it streams
	StreamReasoning = Key + ".stream_reasoning"
	// Most characters of the request and response views a hook is handed
	PayloadMaxChars = Key + ".payload_max_chars"
)

// Where long text a hook hands the turn is saved, a preview kept in its place
const (
	SpillKey         = Key + ".output_spill"
	SpillEnabled     = SpillKey + ".enabled"
	SpillMaxChars    = SpillKey + ".max_chars"
	SpillPreviewHead = SpillKey + ".preview_head"
	SpillPreviewTail = SpillKey + ".preview_tail"
	SpillDirectory   = SpillKey + ".directory"
)
