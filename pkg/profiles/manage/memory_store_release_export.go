package manage

// Closes every holographic memory store connection under dir in each running plugin host
func MemoryStoreReleaseAllUnder(dir string) (int, error) { return memoryStoreReleaseAllUnder(dir) }
