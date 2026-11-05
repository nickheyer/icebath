package agent

// Structural label substituted into an empty non final assistant row on the wire copy
const InterruptedPlaceholder = "[interrupt: no assistant output for this turn]"

// Spelling shipped before the structural label, still retired by the replay filters
const LegacyInterruptedPlaceholder = "[response interrupted]"

// Fresh hidden assistant row whose api_content carries the interrupt placeholder
func HiddenInterruptPlaceholderRow() map[string]any {
	return map[string]any{"role": "assistant", "content": "", "display_kind": "hidden", "api_content": InterruptedPlaceholder}
}
