package media

// Text held back for the next sentence, the chunker buf the idle flush inspects
func (c *SentenceChunker) Buffered() string { return c.buf }

// Default wait of a live transcription finalize in seconds
const StreamingFinalizeTimeout = streamingFinalizeTimeout
