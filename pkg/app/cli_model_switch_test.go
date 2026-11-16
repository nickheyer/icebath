package app

var pickerModels = []string{
	"anthropic/claude-opus-4.8",
	"anthropic/claude-sonnet-4.6",
	"anthropic/claude-haiku-4.5",
	"openai/gpt-5.5",
	"x-ai/grok-4.6",
	"deepseek/deepseek-v4-flash",
}

// Every model paired with its own index
func allPickerPairs() []pickerEntry {
	out := make([]pickerEntry, len(pickerModels))
	for i, m := range pickerModels {
		out[i] = pickerEntry{Index: i, Model: m}
	}
	return out
}
