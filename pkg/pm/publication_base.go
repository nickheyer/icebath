package pm

// Durable writes and renames at each publication boundary, replaced by crash tests to stop the process there
var (
	publicationWrite = DurableWriteBytes
)
