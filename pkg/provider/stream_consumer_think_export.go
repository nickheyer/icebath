package provider

// Lowercase opening think tags the stream scrubbers recognise
func ThinkOpenTags() [][]rune { return thinkOpenTags }

// Lowercase closing think tags the stream scrubbers recognise
func ThinkCloseTags() [][]rune { return thinkCloseTags }

// Index and length of the earliest case insensitive tag in buf, minus one and zero when none
func FindFirstThinkTag(buf []rune, tags [][]rune) (int, int) { return findFirstThinkTag(buf, tags) }

// Length of the longest buf suffix that is a proper prefix of a tag
func MaxPartialThinkSuffix(buf []rune, tags [][]rune) int { return maxPartialThinkSuffix(buf, tags) }

// Text with close tags lacking a matching open removed with their trailing whitespace
func StripOrphanThinkCloseTags(text string) string { return stripOrphanThinkCloseTags(text) }
